package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/raucheacho/agentkit/bus"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tools"
	"github.com/rs/zerolog/log"
)

// Valeurs par défaut de la boucle.
const (
	defaultMaxToolResultChars = 12000
	defaultFallbackReply      = "Je n'ai pas pu finaliser votre demande. Pouvez-vous la reformuler ou la préciser ?"

	// finalAnswerRequest : demandé au modèle (sans outils) quand le budget
	// d'étapes est épuisé ou qu'il ne répond que du vide. Jamais enregistré.
	finalAnswerRequest = "[Consigne interne] Tu ne peux plus appeler d'outil pour ce message. " +
		"Réponds maintenant directement à ton interlocuteur : dis ce qui a réellement été fait " +
		"et ce qui reste à faire. N'affirme rien qui n'a pas été fait par un outil."
)

// turn : les messages d'un tour. api est ce qui part au modèle (prompt système,
// historique, note éphémère) ; saved est ce qui sera ajouté à l'historique
// (message d'origine, appels d'outils, résultats, réponse finale).
type turn struct {
	api   []llm.Message
	saved []llm.Message
}

func (t *turn) add(m llm.Message) {
	t.api = append(t.api, m)
	t.saved = append(t.saved, m)
}

func (a *Agent) process(ctx context.Context, msg *bus.InboundMessage, sessionKey string) (*bus.OutboundMessage, error) {
	history, err := a.config.SessionStore.Get(sessionKey)
	if err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur lecture session, on repart de zéro")
	}

	// Porter l'identité de la conversation (channel + chat ID — pour
	// WhatsApp, chat ID est le numéro de téléphone) via le contexte plutôt
	// qu'un champ partagé : voir tools.ConversationContext pour le pourquoi.
	ctx = tools.WithConversationContext(ctx, tools.ConversationContext{Channel: msg.Channel, ChatID: msg.ChatID})

	t := &turn{}
	t.api = append(t.api, llm.Message{Role: "system", Content: a.buildSystemPrompt()})
	t.api = append(t.api, llm.SanitizeHistory(history)...)

	// La note éphémère accompagne le message à l'envoi, l'historique garde le
	// message d'origine.
	user := llm.Message{Role: "user", Content: msg.Content}
	t.saved = append(t.saved, user)
	if note := strings.TrimSpace(msg.Note); note != "" {
		user.Content = note + "\n\n" + msg.Content
	}
	t.api = append(t.api, user)

	final, err := a.run(ctx, t)
	if err != nil {
		return nil, err
	}
	t.saved = append(t.saved, llm.Message{Role: "assistant", Content: final})
	a.persist(sessionKey, t.saved)
	return &bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: final}, nil
}

// run exécute la boucle ReAct et renvoie la réponse finale (jamais vide).
func (a *Agent) run(ctx context.Context, t *turn) (string, error) {
	emptyRetried := false
	for iter := 0; iter < a.config.MaxIter; iter++ {
		resp, err := a.config.Provider.Chat(ctx, t.api, a.convertTools(a.tools.GetTools()))
		if err != nil {
			return "", fmt.Errorf("erreur LLM: %w", err)
		}

		calls := resp.ToolCalls
		if len(calls) == 0 && a.config.ParseInlineTools {
			if tc, ok := a.parseInlineToolCall(resp.Content); ok {
				calls = []llm.ToolCall{*tc}
				resp.Content = ""
			}
		}

		if len(calls) == 0 {
			text := cleanReply(resp.Content)
			if text != "" {
				return text, nil
			}
			// Réponse vide (fréquent avec les modèles qui « réfléchissent ») :
			// une nouvelle tentative, puis la réponse finale forcée.
			if emptyRetried {
				break
			}
			emptyRetried = true
			log.Warn().Msg("Réponse vide du modèle, nouvelle tentative")
			continue
		}

		assignCallIDs(calls, iter)
		t.add(llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: calls})

		for _, tc := range calls {
			result, ok := a.runTool(ctx, tc)
			t.add(llm.Message{Role: "tool", Content: result, ToolCallID: tc.ID})

			// Le spawn confirme tout de suite à l'utilisateur ; le vrai
			// résultat arrivera plus tard via processSubagentResult.
			if tc.Function.Name == "spawn" && ok {
				return result, nil
			}
		}
	}
	return a.finalAnswer(ctx, t), nil
}

// runTool valide puis exécute un appel d'outil. Toute erreur devient un
// résultat lisible par le modèle, pour qu'il se corrige au lieu d'échouer.
// ok indique que l'outil a réellement été exécuté sans erreur.
func (a *Agent) runTool(ctx context.Context, tc llm.ToolCall) (result string, ok bool) {
	name := a.resolveToolName(tc.Function.Name)
	if name == "" {
		log.Warn().Str("tool", tc.Function.Name).Msg("Outil inconnu ou indisponible")
		return fmt.Sprintf("Erreur : l'outil %q n'existe pas ou n'est pas disponible ici. Outils disponibles : %s.",
			tc.Function.Name, strings.Join(a.enabledToolNames(), ", ")), false
	}
	tc.Function.Name = name
	log.Info().Str("tool", name).Msg("Exécution outil")

	if a.config.BeforeTool != nil {
		if allow, reason := a.config.BeforeTool(ctx, tc); !allow {
			log.Warn().Str("tool", name).Str("reason", reason).Msg("Outil refusé par BeforeTool")
			return fmt.Sprintf("Erreur: exécution refusée (%s)", reason), false
		}
	}

	args, err := parseArguments(tc.Function.Arguments)
	if err != nil {
		log.Warn().Err(err).Str("tool", name).Msg("Arguments d'outil invalides")
		return fmt.Sprintf("Erreur : arguments invalides pour %s (%s). Renvoie un objet JSON conforme au schéma de l'outil.", name, err), false
	}

	result, err = a.executeTool(ctx, name, args)
	if err != nil {
		return a.truncate(fmt.Sprintf("Erreur: %s", err)), false
	}
	if a.config.AfterTool != nil {
		a.config.AfterTool(ctx, tc, result)
	}
	return a.truncate(result), true
}

// finalAnswer demande au modèle, sans outils, une réponse à l'interlocuteur
// (budget d'étapes épuisé ou réponses vides). La consigne n'est pas enregistrée.
func (a *Agent) finalAnswer(ctx context.Context, t *turn) string {
	log.Warn().Int("max_iter", a.config.MaxIter).Msg("Réponse finale forcée (budget épuisé ou réponse vide)")
	msgs := append([]llm.Message(nil), t.api...)
	if last := len(msgs) - 1; msgs[last].Role == "user" {
		// Jamais deux messages "user" d'affilée.
		msgs[last].Content += "\n\n" + finalAnswerRequest
	} else {
		msgs = append(msgs, llm.Message{Role: "user", Content: finalAnswerRequest})
	}
	resp, err := a.config.Provider.Chat(ctx, msgs, nil)
	if err == nil {
		if text := cleanReply(resp.Content); text != "" {
			return text
		}
	} else {
		log.Warn().Err(err).Msg("Réponse finale forcée impossible")
	}
	return a.config.FallbackReply
}

// persist ajoute les messages du tour à l'historique et le sauvegarde. La
// session est relue : un message ajouté pendant le tour (reprise en main) est
// conservé.
func (a *Agent) persist(sessionKey string, saved []llm.Message) {
	history, err := a.config.SessionStore.Get(sessionKey)
	if err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur lecture session avant sauvegarde")
	}
	history = append(history, saved...)
	if err := a.config.SessionStore.Save(sessionKey, history); err != nil {
		log.Warn().Err(err).Str("session", sessionKey).Msg("Erreur sauvegarde session")
	}
}

// resolveToolName renvoie le nom réel d'un outil activé, en réparant les
// variantes courantes émises par les modèles (« functions.x », casse, tirets,
// jetons de format collés au nom). "" si l'outil n'existe pas ou est désactivé :
// un modèle ne peut jamais exécuter un outil qui ne lui est pas exposé.
func (a *Agent) resolveToolName(raw string) string {
	enabled := a.enabledToolNames()
	if contains(enabled, raw) {
		return raw
	}
	n := strings.TrimSpace(raw)
	if i := strings.Index(n, "<|"); i >= 0 {
		n = n[:i]
	}
	n = strings.TrimPrefix(n, "functions.")
	n = strings.TrimPrefix(n, "tools.")
	n = strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(n))
	for _, name := range enabled {
		if strings.ToLower(name) == n {
			return name
		}
	}
	return ""
}

// enabledToolNames : les outils exposés au modèle pour ce tour.
func (a *Agent) enabledToolNames() []string {
	var names []string
	for _, t := range a.tools.GetTools() {
		if a.config.EnabledTools == nil || a.config.EnabledTools(t.Function.Name) {
			names = append(names, t.Function.Name)
		}
	}
	return names
}

func (a *Agent) truncate(s string) string {
	limit := a.config.MaxToolResultChars
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head, tail := limit*2/3, limit/3
	// Couper sur des frontières de rune.
	for head > 0 && !utf8Start(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n[... %d caractères omis ...]\n", start-head) + s[start:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// assignCallIDs donne un identifiant unique aux appels qui n'en ont pas
// (Ollama, appels écrits en texte) : sans lui, un résultat ne peut pas être
// rattaché à son appel dans l'historique.
func assignCallIDs(calls []llm.ToolCall, iter int) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", iter, i)
		}
		if calls[i].Type == "" {
			calls[i].Type = "function"
		}
	}
}

var trailingComma = regexp.MustCompile(`,\s*([}\]])`)

// parseArguments lit les arguments JSON d'un appel, en réparant les défauts
// fréquents des modèles (vide, "null", virgule finale).
func parseArguments(raw string) (map[string]interface{}, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" || s == "None" {
		return map[string]interface{}{}, nil
	}
	var args map[string]interface{}
	err := json.Unmarshal([]byte(s), &args)
	if err != nil {
		if json.Unmarshal([]byte(trailingComma.ReplaceAllString(s, "$1")), &args) == nil {
			return args, nil
		}
		return nil, err
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	return args, nil
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// cleanReply retire le raisonnement exposé par certains modèles.
func cleanReply(s string) string {
	return strings.TrimSpace(thinkBlock.ReplaceAllString(s, ""))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
