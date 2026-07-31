package tools

import (
	"context"
	"fmt"
)

// SpawnExecutor est la fonction d'exécution pour l'outil spawn
type SpawnExecutor func(task string, label string, originChannel string, originChatID string) (string, error)

// SpawnTool permet de créer un subagent en arrière-plan
type SpawnTool struct {
	exec SpawnExecutor
}

// NewSpawnTool crée un nouvel outil de spawn
func NewSpawnTool(exec SpawnExecutor) *SpawnTool {
	return &SpawnTool{exec: exec}
}

func (t *SpawnTool) Name() string {
	return "spawn"
}

func (t *SpawnTool) Description() string {
	return "Crée un subagent pour exécuter une tâche en arrière-plan et revenir avec le résultat une fois terminée. Utile pour les tâches qui prennent du temps ou qui peuvent s'exécuter sans intervention."
}

func (t *SpawnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{
				"type":        "string",
				"description": "La tâche complète à exécuter par le subagent. Sois précis et détaillé.",
			},
			"label": map[string]interface{}{
				"type":        "string",
				"description": "Label court pour identifier la tâche (ex: 'Comptage', 'Recherche')",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SpawnTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	if t.exec == nil {
		return "", fmt.Errorf("spawn non initialisé")
	}

	task, _ := params["task"].(string)
	label, _ := params["label"].(string)

	if task == "" {
		return "", fmt.Errorf("paramètre 'task' requis")
	}

	originChannel := "cli"
	originChatID := "direct"
	if cc, ok := ConversationFromContext(ctx); ok {
		if cc.Channel != "" {
			originChannel = cc.Channel
		}
		if cc.ChatID != "" {
			originChatID = cc.ChatID
		}
	}

	taskID, err := t.exec(task, label, originChannel, originChatID)
	if err != nil {
		return "", err
	}

	shortID := taskID
	if len(taskID) > 8 {
		shortID = taskID[:8]
	}
	return fmt.Sprintf("🔍 Tâche de fond lancée (ID: %s). Je m'en occupe, je reviens vers vous dès que c'est prêt.", shortID), nil
}
