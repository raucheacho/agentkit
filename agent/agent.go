package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/raucheacho/agentkit/bootstrap"
	"github.com/raucheacho/agentkit/bus"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tools"
)

// SessionStore interface for conversation persistence
type SessionStore interface {
	Get(key string) ([]llm.Message, error)
	Save(key string, messages []llm.Message) error
}

// Config configuration de l'agent. Le runtime est générique : tout ce qui
// est spécifique à une application (tools métier, persona, instructions)
// est injecté ici par l'assemblage (cmd/), jamais importé par ce package.
type Config struct {
	Provider     llm.Provider
	SessionStore SessionStore

	// Tools est le manager d'outils, déjà garni des tools métier par
	// l'application appelante. Si nil, un manager avec les seuls tools
	// génériques est créé.
	Tools *tools.Manager

	// Identity est la persona en tête de prompt système ("qui je suis").
	// Instructions est le mode d'emploi métier (flux d'auth, règles de
	// confirmation, ...) — il évolue avec les tools enregistrés, donc il
	// vit à côté d'eux chez l'appelant.
	Identity     string
	Instructions string

	Workspace            string
	MaxIter              int
	Bus                  *bus.Bus // Message bus pour les subagents
	SubagentsMaxParallel int

	// Crochets d'interception d'outils
	BeforeTool func(ctx context.Context, call llm.ToolCall) (allow bool, reason string)
	AfterTool  func(ctx context.Context, call llm.ToolCall, result string)

	// Filtrage d'outils actifs (nil = tous)
	EnabledTools func(name string) bool

	// ParseInlineTools si le modèle émet l'appel d'outil en TEXTE (petits modèles)
	ParseInlineTools bool

	// Composition du prompt système
	ExtraPrompt func() string
}

// Valeurs par défaut quand Identity/Instructions ne sont pas fournis.
const (
	defaultIdentity = `# Assistant

Tu es un assistant conversationnel utile et concis.`

	defaultInstructions = `## Instructions

Réponds de façon concise et utile, en français par défaut.

Quand tu reçois un message système annonçant qu'une tâche de fond (subagent) est terminée, résume le résultat en 1-2 phrases pour l'utilisateur, sans mentionner de détails techniques (IDs, "subagent", etc.).`
)

// Agent représente l'assistant
type Agent struct {
	config       Config
	tools        *tools.Manager
	sessionLocks *keyedMutex
	subagents    *SubagentManager // Manager des subagents
}

// New crée un nouvel agent
func New(cfg Config) *Agent {
	if cfg.MaxIter == 0 {
		cfg.MaxIter = 20
	}
	if cfg.Identity == "" {
		cfg.Identity = defaultIdentity
	}
	if cfg.Instructions == "" {
		cfg.Instructions = defaultInstructions
	}

	// S'assurer que les fichiers bootstrap existent
	if err := bootstrap.EnsureBootstrapFiles(cfg.Workspace); err != nil {
		log.Warn().Err(err).Msg("Erreur création fichiers bootstrap")
	}

	toolMgr := cfg.Tools
	if toolMgr == nil {
		toolMgr = tools.NewManager()
		_ = toolMgr.Register(&tools.GetTodayTool{})
		log.Warn().Msg("Aucun tool manager fourni : seuls les tools génériques sont disponibles")
	}

	agent := &Agent{
		config:       cfg,
		tools:        toolMgr,
		sessionLocks: newKeyedMutex(),
	}

	// Initialiser le subagent manager si un bus est fourni
	if cfg.Bus != nil {
		maxParallel := cfg.SubagentsMaxParallel
		if maxParallel <= 0 {
			maxParallel = 2
		}

		agent.subagents = NewSubagentManager(cfg.Provider, cfg.Workspace, cfg.Bus, "", maxParallel)
		agent.subagents.LoadTasksFromFile()

		// Enregistrer l'outil spawn
		_ = toolMgr.Register(tools.NewSpawnTool(func(task, label, originChannel, originChatID string) (string, error) {
			return agent.subagents.Spawn(task, label, originChannel, originChatID)
		}))
	}

	return agent
}

// ProcessMessage traite un message entrant
func (a *Agent) ProcessMessage(ctx context.Context, msg *bus.InboundMessage) (*bus.OutboundMessage, error) {
	log.Info().Str("channel", msg.Channel).Str("sender", msg.SenderID).Msg("Message reçu")

	// Gérer les messages système (résultats de subagents)
	if msg.Channel == "system" && msg.SenderID == "subagent" {
		return a.processSubagentResult(ctx, msg)
	}

	sessionKey := msg.SessionKey()

	// Sérialiser le traitement par conversation : deux messages de la même
	// conversation ne s'exécutent jamais en parallèle (évite les mises à jour
	// perdues), tandis que des conversations différentes restent parallèles.
	unlock := a.sessionLocks.Lock(sessionKey)
	defer unlock()

	return a.process(ctx, msg, sessionKey)
}

func (a *Agent) process(ctx context.Context, msg *bus.InboundMessage, sessionKey string) (*bus.OutboundMessage, error) {
	systemPrompt := a.buildSystemPrompt()

	history, err := a.config.SessionStore.Get(sessionKey)
	if err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur lecture session, on repart de zéro")
	}

	// Porter l'identité de la conversation (channel + chat ID — pour
	// WhatsApp, chat ID est le numéro de téléphone) via le contexte plutôt
	// qu'un champ partagé : voir tools.ConversationContext pour le pourquoi.
	ctx = tools.WithConversationContext(ctx, tools.ConversationContext{Channel: msg.Channel, ChatID: msg.ChatID})

	var messages []llm.Message
	messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: "user", Content: msg.Content})

	var finalContent string
	iterations := 0

	for iterations < a.config.MaxIter {
		iterations++

		resp, err := a.config.Provider.Chat(ctx, messages, a.convertTools(a.tools.GetTools()))
		if err != nil {
			return nil, fmt.Errorf("erreur LLM: %w", err)
		}

		if len(resp.ToolCalls) == 0 {
			if a.config.ParseInlineTools {
				if tc, ok := a.parseInlineToolCall(resp.Content); ok {
					resp.ToolCalls = []llm.ToolCall{*tc}
				}
			}
			if len(resp.ToolCalls) == 0 {
				finalContent = resp.Content
				break
			}
		}

		// L'assistant a appelé des tools : conserver tool_calls dans l'historique,
		// requis par les API compatibles OpenAI (Groq) pour rattacher les
		// réponses "tool" qui suivent.
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			log.Info().Str("tool", tc.Function.Name).Msg("Exécution outil")

			// Interception avant outil
			if a.config.BeforeTool != nil {
				allow, reason := a.config.BeforeTool(ctx, tc)
				if !allow {
					log.Warn().Str("tool", tc.Function.Name).Str("reason", reason).Msg("Outil refusé par BeforeTool")
					messages = append(messages, llm.Message{
						Role:       "tool",
						Content:    fmt.Sprintf("Erreur: exécution refusée (%s)", reason),
						ToolCallID: tc.ID,
					})
					continue
				}
			}

			var args map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				log.Error().Err(err).Msg("Erreur parsing arguments")
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("Erreur: arguments invalides (%s)", err),
					ToolCallID: tc.ID,
				})
				continue
			}

			result, err := a.executeTool(ctx, tc.Function.Name, args)
			if err != nil {
				result = fmt.Sprintf("Erreur: %s", err)
			}

			// Interception après outil
			if a.config.AfterTool != nil && err == nil {
				a.config.AfterTool(ctx, tc, result)
			}

			// Le spawn renvoie immédiatement une confirmation à l'utilisateur ;
			// le vrai résultat arrivera plus tard via processSubagentResult.
			if tc.Function.Name == "spawn" && err == nil {
				finalContent = result
				a.persist(sessionKey, msg.Content, finalContent)
				return &bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: finalContent}, nil
			}

			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	if finalContent == "" {
		finalContent = "J'ai traité votre demande mais n'ai pas de réponse spécifique."
	}

	a.persist(sessionKey, msg.Content, finalContent)

	return &bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: finalContent}, nil
}

// persist ajoute l'échange à l'historique de la conversation et le sauvegarde.
func (a *Agent) persist(sessionKey, userContent, assistantContent string) {
	history, err := a.config.SessionStore.Get(sessionKey)
	if err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur lecture session avant sauvegarde")
	}

	history = append(history,
		llm.Message{Role: "user", Content: userContent},
		llm.Message{Role: "assistant", Content: assistantContent},
	)

	if err := a.config.SessionStore.Save(sessionKey, history); err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur sauvegarde session")
	}
}

// processSubagentResult traite un résultat de subagent
func (a *Agent) processSubagentResult(ctx context.Context, msg *bus.InboundMessage) (*bus.OutboundMessage, error) {
	log.Info().Str("chat_id", msg.ChatID).Msg("🔄 Traitement résultat subagent")

	// Parser l'origine depuis chat_id (format: "channel:chat_id")
	var originChannel, originChatID string
	if idx := strings.Index(msg.ChatID, ":"); idx != -1 {
		originChannel = msg.ChatID[:idx]
		originChatID = msg.ChatID[idx+1:]
	} else {
		originChannel = "cli"
		originChatID = msg.ChatID
	}

	sessionKey := fmt.Sprintf("%s:%s", originChannel, originChatID)

	// Même verrou que les messages normaux de cette conversation, pour éviter
	// une écriture concurrente si l'utilisateur écrit pile au même moment.
	unlock := a.sessionLocks.Lock(sessionKey)
	defer unlock()

	history, err := a.config.SessionStore.Get(sessionKey)
	if err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur lecture session")
	}

	brutResult := msg.Content
	systemPrompt := a.buildSystemPrompt()

	ctx = tools.WithConversationContext(ctx, tools.ConversationContext{Channel: originChannel, ChatID: originChatID})

	var messages []llm.Message
	messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: "user", Content: msg.Content})

	resp, err := a.config.Provider.Chat(ctx, messages, a.convertTools(a.tools.GetTools()))
	if err != nil {
		log.Error().Err(err).Msg("❌ Erreur LLM lors du résumé du subagent, utilisation du résultat brut")
		fallbackContent := fmt.Sprintf("✅ Tâche terminée !\n\n%s", brutResult)
		a.persist(sessionKey, "[Tâche en arrière-plan terminée]", fallbackContent)
		return &bus.OutboundMessage{Channel: originChannel, ChatID: originChatID, Content: fallbackContent}, nil
	}

	log.Info().Str("chat_id", originChatID).Msg("✅ Résultat subagent résumé et prêt à être envoyé")
	a.persist(sessionKey, "[Tâche en arrière-plan terminée]", resp.Content)

	return &bus.OutboundMessage{Channel: originChannel, ChatID: originChatID, Content: resp.Content}, nil
}

// GetSubagentManager retourne le manager des subagents
func (a *Agent) GetSubagentManager() *SubagentManager {
	return a.subagents
}

// buildSystemPrompt construit le prompt système : persona (Identity) +
// contexte du workspace (AGENTS.md) + instructions métier (Instructions) +
// heure courante. Persona et instructions viennent de la Config — ce package
// ne connaît aucun métier.
func (a *Agent) buildSystemPrompt() string {
	var parts []string

	parts = append(parts, a.config.Identity)

	if bootstrapCtx := bootstrap.LoadBootstrapFiles(a.config.Workspace); bootstrapCtx != "" {
		parts = append(parts, bootstrapCtx)
	}

	if a.config.ExtraPrompt != nil {
		if extra := a.config.ExtraPrompt(); extra != "" {
			parts = append(parts, extra)
		}
	}

	parts = append(parts, fmt.Sprintf("%s\n\nHeure actuelle: %s",
		a.config.Instructions, time.Now().Format("2006-01-02 15:04:05")))

	return strings.Join(parts, "\n\n---\n\n")
}

// executeTool exécute un outil par son nom
func (a *Agent) executeTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	return a.tools.Execute(ctx, name, args)
}

// convertTools convertit les outils internes au format provider
// (partagé avec SubagentManager.convertTools, cf. subagent.go)
func (a *Agent) convertTools(toolDefs []tools.ToolDef) []llm.Tool {
	return convertToolDefs(toolDefs, a.config.EnabledTools)
}

// convertToolDefs convertit les outils internes au format provider.
func convertToolDefs(toolDefs []tools.ToolDef, enabledTools func(name string) bool) []llm.Tool {
	var result []llm.Tool
	for _, t := range toolDefs {
		if enabledTools != nil && !enabledTools(t.Function.Name) {
			continue // Outil désactivé pour ce tour
		}

		result = append(result, llm.Tool{
			Type: t.Type,
			Function: llm.FunctionDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters: llm.Parameters{
					Type:       t.Function.Parameters.Type,
					Properties: convertProperties(t.Function.Parameters.Properties),
					Required:   t.Function.Parameters.Required,
				},
			},
		})
	}
	return result
}

func convertProperties(props map[string]tools.Property) map[string]llm.Property {
	result := make(map[string]llm.Property)
	for k, v := range props {
		result[k] = llm.Property{
			Type:        v.Type,
			Description: v.Description,
			Enum:        v.Enum,
		}
	}
	return result
}

// UpdateModel met à jour le modèle utilisé par le provider (thread-safe)
func (a *Agent) UpdateModel(model string) {
	if model != "" && a.config.Provider != nil {
		a.config.Provider.SetModel(model)
		if a.subagents != nil {
			a.subagents.SetModel(model)
		}
	}
}

// parseInlineToolCall parse un outil écrit en texte dans la réponse
func (a *Agent) parseInlineToolCall(content string) (*llm.ToolCall, bool) {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(s[i:j+1]), &raw) != nil {
		return nil, false
	}
	nameRaw, ok := raw["name"]
	if !ok {
		return nil, false
	}
	var name string
	if json.Unmarshal(nameRaw, &name) != nil || name == "" {
		return nil, false
	}
	known := false
	for _, t := range a.tools.GetTools() {
		if t.Function.Name == name {
			known = true
			break
		}
	}
	if !known {
		return nil, false
	}
	argsRaw := json.RawMessage("{}")
	if v, ok := raw["parameters"]; ok {
		argsRaw = v
	} else if v, ok := raw["arguments"]; ok {
		argsRaw = v
	}
	tc := &llm.ToolCall{Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = string(argsRaw)
	return tc, true
}

