package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/raucheacho/agentkit/bus"
	"github.com/raucheacho/agentkit/tools"
	"github.com/raucheacho/agentkit/llm"
)

// SubagentTask représente une tâche de subagent en cours
type SubagentTask struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Task      string    `json:"task"`
	Status    string    `json:"status"` // "running", "completed", "failed"
	Result    string    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SubagentNotification représente une notification pour le client
type SubagentNotification struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Message   string    `json:"message"`
	Type      string    `json:"type"` // "started", "completed"
	Timestamp time.Time `json:"timestamp"`
}

// SubagentManager gère l'exécution des subagents en arrière-plan
type SubagentManager struct {
	provider      llm.Provider
	workspace     string
	bus           *bus.Bus
	model         string
	modelMu       sync.RWMutex // Protects model
	maxParallel   int          // Nombre max de subagents simultanés
	tasks         map[string]*SubagentTask
	tasksMu       sync.RWMutex
	onComplete    func(task *SubagentTask)
	notifications []SubagentNotification // Notifications pour le web
	notifMu       sync.RWMutex
}

// NewSubagentManager crée un nouveau manager de subagents
func NewSubagentManager(provider llm.Provider, workspace string, bus *bus.Bus, model string, maxParallel int) *SubagentManager {
	if model == "" {
		model = provider.GetDefaultModel()
	}
	if maxParallel <= 0 {
		maxParallel = 2 // Valeur par défaut
	}
	return &SubagentManager{
		provider:      provider,
		workspace:     workspace,
		bus:           bus,
		model:         model,
		maxParallel:   maxParallel,
		tasks:         make(map[string]*SubagentTask),
		notifications: make([]SubagentNotification, 0),
	}
}

// SetOnComplete définit le callback appelé quand un subagent termine
func (sm *SubagentManager) SetOnComplete(fn func(task *SubagentTask)) {
	sm.onComplete = fn
}

// SetModel met à jour le modèle utilisé par les subagents (thread-safe)
func (sm *SubagentManager) SetModel(model string) {
	sm.modelMu.Lock()
	defer sm.modelMu.Unlock()
	if model != "" {
		sm.model = model
	}
}

// Spawn crée un nouveau subagent pour exécuter une tâche en arrière-plan
// Retourne le taskID et un booléen indiquant si le spawn a réussi
func (sm *SubagentManager) Spawn(task string, label string, originChannel string, originChatID string) (string, error) {
	// Vérifier si on a atteint la limite de subagents
	runningCount := sm.GetRunningCount()
	if runningCount >= sm.maxParallel {
		return "", fmt.Errorf("limite de subagents atteinte (%d/%d). Attendez qu'un subagent termine avant d'en lancer un nouveau", runningCount, sm.maxParallel)
	}

	taskID := uuid.New().String()[:8]
	displayLabel := label
	if displayLabel == "" {
		displayLabel = task
		if len(displayLabel) > 30 {
			displayLabel = displayLabel[:30] + "..."
		}
	}

	// Créer la tâche
	subagentTask := &SubagentTask{
		ID:        taskID,
		Label:     displayLabel,
		Task:      task,
		Status:    "running",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	sm.tasksMu.Lock()
	sm.tasks[taskID] = subagentTask
	sm.tasksMu.Unlock()

	// Ajouter une notification
	sm.addNotification(SubagentNotification{
		ID:        uuid.New().String()[:8],
		TaskID:    taskID,
		Message:   fmt.Sprintf("Tâche '%s' démarrée", displayLabel),
		Type:      "started",
		Timestamp: time.Now(),
	})

	// Notifier via le bus pour SSE
	sm.bus.PublishOutbound(&bus.OutboundMessage{
		Channel: originChannel,
		ChatID:  originChatID,
		Content: fmt.Sprintf("🚀 Subagent lancé: %s", displayLabel),
	})

	// Lancer la tâche en arrière-plan
	go sm.runSubagent(subagentTask, originChannel, originChatID)

	log.Info().Str("task_id", taskID).Str("label", displayLabel).Msg("Subagent spawned")
	return taskID, nil
}

// runSubagent exécute la tâche du subagent
func (sm *SubagentManager) runSubagent(task *SubagentTask, originChannel string, originChatID string) {
	log.Info().Str("task_id", task.ID).Msg("Subagent started")

	// Créer les outils pour le subagent (sans spawn et sans message)
	toolMgr := tools.NewToolManager(sm.workspace)
	toolMgr.DisableTool("spawn")   // Un subagent ne peut pas créer d'autres subagents
	toolMgr.DisableTool("message") // Un subagent ne peut pas envoyer de messages directement

	// Construire le prompt système spécifique au subagent
	systemPrompt := sm.buildSubagentPrompt(task.Task)

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task.Task},
	}

	var finalResult string
	maxIterations := 15
	ctx := context.Background()

	for i := 0; i < maxIterations; i++ {
		response, err := sm.provider.Chat(ctx, messages, sm.convertTools(toolMgr.GetTools()))
		if err != nil {
			finalResult = fmt.Sprintf("Erreur LLM: %s", err)
			task.Status = "failed"
			break
		}

		if len(response.ToolCalls) > 0 {
			// Ajouter la réponse de l'assistant (avec tool_calls, requis par les
			// API compatibles OpenAI pour rattacher les réponses "tool" qui suivent)
			messages = append(messages, llm.Message{
				Role:      "assistant",
				Content:   response.Content,
				ToolCalls: response.ToolCalls,
			})

			// Exécuter les outils
			for _, tc := range response.ToolCalls {
				log.Debug().Str("task_id", task.ID).Str("tool", tc.Function.Name).Msg("Subagent executing tool")

				var args map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					log.Error().Err(err).Str("task_id", task.ID).Msg("Erreur parsing arguments")
					messages = append(messages, llm.Message{
						Role:       "tool",
						Content:    fmt.Sprintf("Erreur: arguments invalides (%s)", err),
						ToolCallID: tc.ID,
					})
					continue
				}

				result, err := toolMgr.Execute(ctx, tc.Function.Name, args)
				if err != nil {
					result = fmt.Sprintf("Erreur: %s", err)
				}

				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}
		} else {
			finalResult = response.Content
			task.Status = "completed"
			break
		}
	}

	if task.Status == "running" {
		task.Status = "completed"
		if finalResult == "" {
			finalResult = "Tâche terminée mais aucun résultat généré."
		}
	}

	task.Result = finalResult
	task.UpdatedAt = time.Now()

	sm.tasksMu.Lock()
	sm.tasks[task.ID] = task
	sm.tasksMu.Unlock()

	log.Info().Str("task_id", task.ID).Str("status", task.Status).Msg("Subagent completed")

	// Ajouter une notification
	statusMsg := "terminée"
	if task.Status == "failed" {
		statusMsg = "échouée"
	}
	sm.addNotification(SubagentNotification{
		ID:        uuid.New().String()[:8],
		TaskID:    task.ID,
		Message:   fmt.Sprintf("Tâche '%s' %s", task.Label, statusMsg),
		Type:      "completed",
		Timestamp: time.Now(),
	})

	// Notifier la fin via le bus pour SSE
	sm.bus.PublishOutbound(&bus.OutboundMessage{
		Channel: originChannel,
		ChatID:  originChatID,
		Content: fmt.Sprintf("✅ Subagent terminé: %s\n\n%s", task.Label, finalResult),
	})

	// Annoncer le résultat via le bus pour traitement par l'agent principal
	sm.announceResult(task, originChannel, originChatID)

	// Appeler le callback si défini
	if sm.onComplete != nil {
		sm.onComplete(task)
	}
}

// announceResult publie le résultat du subagent sur le bus
func (sm *SubagentManager) announceResult(task *SubagentTask, originChannel string, originChatID string) {
	statusText := "terminée avec succès"
	if task.Status == "failed" {
		statusText = "échouée"
	}

	announceContent := fmt.Sprintf(`[Tâche de fond '%s' %s]

Tâche: %s

Résultat:
%s

Résume cela naturellement pour l'utilisateur (1-2 phrases). Ne mentionne pas les détails techniques comme "subagent" ou les IDs.`,
		task.Label, statusText, task.Task, task.Result)

	// Publier comme message système
	sm.bus.PublishInbound(&bus.InboundMessage{
		Channel:   "system",
		SenderID:  "subagent",
		ChatID:    fmt.Sprintf("%s:%s", originChannel, originChatID),
		Content:   announceContent,
		Timestamp: time.Now(),
	})
}

// buildSubagentPrompt construit le prompt système pour un subagent
func (sm *SubagentManager) buildSubagentPrompt(task string) string {
	return fmt.Sprintf(`# Subagent

Tu es un assistant spécialisé qui exécute une tâche spécifique en arrière-plan.

## Ta Tâche
%s

## Règles
1. Reste concentré - accomplis uniquement la tâche assignée, rien d'autre
2. Ta réponse finale sera rapportée à l'agent principal
3. N'initie pas de conversations ou ne prends pas de tâches annexes
4. Sois concis mais informatif dans tes conclusions

## Ce Que Tu Peux Faire
- Lire et écrire des fichiers dans le workspace
- Exécuter des commandes shell
- Faire des recherches web et récupérer des pages
- Compléter la tâche minutieusement

## Ce Que Tu Ne Peux PAS Faire
- Envoyer des messages directement aux utilisateurs (pas d'outil message disponible)
- Créer d'autres subagents
- Accéder à l'historique de conversation de l'agent principal

## Workspace
Ton workspace est: %s

Quand tu as terminé la tâche, fournis un résumé clair de tes trouvailles ou actions.`, task, sm.workspace)
}

// GetTask retourne une tâche par son ID
func (sm *SubagentManager) GetTask(taskID string) (*SubagentTask, bool) {
	sm.tasksMu.RLock()
	defer sm.tasksMu.RUnlock()
	task, exists := sm.tasks[taskID]
	return task, exists
}

// GetAllTasks retourne toutes les tâches (nettoie les vieilles tâches terminées automatiquement)
func (sm *SubagentManager) GetAllTasks() []*SubagentTask {
	sm.tasksMu.Lock()
	defer sm.tasksMu.Unlock()

	// Nettoyer automatiquement les tâches terminées de plus de 1 heure
	now := time.Now()
	for id, task := range sm.tasks {
		if task.Status != "running" && now.Sub(task.UpdatedAt) > 1*time.Hour {
			delete(sm.tasks, id)
		}
	}

	tasks := make([]*SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// GetRunningTasks retourne les tâches en cours
func (sm *SubagentManager) GetRunningTasks() []*SubagentTask {
	sm.tasksMu.RLock()
	defer sm.tasksMu.RUnlock()

	var running []*SubagentTask
	for _, task := range sm.tasks {
		if task.Status == "running" {
			running = append(running, task)
		}
	}
	return running
}

// GetRunningCount retourne le nombre de tâches en cours
func (sm *SubagentManager) GetRunningCount() int {
	sm.tasksMu.RLock()
	defer sm.tasksMu.RUnlock()

	count := 0
	for _, task := range sm.tasks {
		if task.Status == "running" {
			count++
		}
	}
	return count
}

// CleanupOldTasks supprime les tâches terminées de plus de X heures
func (sm *SubagentManager) CleanupOldTasks(maxAge time.Duration) {
	sm.tasksMu.Lock()
	defer sm.tasksMu.Unlock()

	now := time.Now()
	for id, task := range sm.tasks {
		if task.Status != "running" && now.Sub(task.UpdatedAt) > maxAge {
			delete(sm.tasks, id)
		}
	}
}

// addNotification ajoute une notification
func (sm *SubagentManager) addNotification(notif SubagentNotification) {
	sm.notifMu.Lock()
	defer sm.notifMu.Unlock()

	sm.notifications = append(sm.notifications, notif)

	// Garder seulement les 50 dernières notifications
	if len(sm.notifications) > 50 {
		sm.notifications = sm.notifications[len(sm.notifications)-50:]
	}
}

// GetNotifications retourne toutes les notifications
func (sm *SubagentManager) GetNotifications() []SubagentNotification {
	sm.notifMu.RLock()
	defer sm.notifMu.RUnlock()

	// Filtrer les notifications de plus de 5 minutes
	now := time.Now()
	var recent []SubagentNotification
	for _, n := range sm.notifications {
		if now.Sub(n.Timestamp) < 5*time.Minute {
			recent = append(recent, n)
		}
	}
	return recent
}

// ClearNotifications supprime toutes les notifications
func (sm *SubagentManager) ClearNotifications() {
	sm.notifMu.Lock()
	defer sm.notifMu.Unlock()

	sm.notifications = make([]SubagentNotification, 0)
}

// convertTools convertit les outils internes au format provider
// (partagé avec Agent.convertTools, cf. agent.go)
func (sm *SubagentManager) convertTools(toolDefs []tools.ToolDef) []llm.Tool {
	return convertToolDefs(toolDefs)
}

// SaveTasksToFile sauvegarde les tâches dans un fichier JSON
func (sm *SubagentManager) SaveTasksToFile() error {
	tasksFile := filepath.Join(sm.workspace, "subagents.json")

	sm.tasksMu.RLock()
	defer sm.tasksMu.RUnlock()

	data, err := json.MarshalIndent(sm.tasks, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(tasksFile, data, 0644)
}

// LoadTasksFromFile charge les tâches depuis un fichier JSON
func (sm *SubagentManager) LoadTasksFromFile() error {
	tasksFile := filepath.Join(sm.workspace, "subagents.json")

	data, err := os.ReadFile(tasksFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	sm.tasksMu.Lock()
	defer sm.tasksMu.Unlock()

	return json.Unmarshal(data, &sm.tasks)
}
