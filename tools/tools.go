package tools

import (
	"context"
	"fmt"
)

// ToolDef définit un outil disponible pour l'agent (format OpenAI/Anthropic)
// C'est le format utilisé pour décrire les outils au LLM.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef définit une fonction
type FunctionDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  Parameters `json:"parameters"`
}

// Parameters définit les paramètres
type Parameters struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

// Property définit une propriété
type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// Executor exécute un appel d'outil
type Executor func(ctx context.Context, name string, args map[string]interface{}) (string, error)

// Manager expose au LLM la liste des outils (ToolDef) et route leur
// exécution : vers le registry moderne (inner) pour tout ce qui y est
// enregistré, vers execs pour ce qui n'y est pas (spawn, tools métier de l'application,
// propres à l'agent).
type Manager struct {
	tools     []ToolDef
	execs     map[string]Executor
	workDir   string
	inner     *Registry
	disabled  map[string]bool
	spawnExec SpawnExecutor
}

// NewToolManager crée un nouveau manager d'outils
func NewToolManager(workDir string) *Manager {
	r := &Manager{
		tools:    []ToolDef{},
		execs:    make(map[string]Executor),
		workDir:  workDir,
		inner:    NewRegistryWithDefaults(&Config{WorkspaceDir: workDir}),
		disabled: make(map[string]bool),
	}

	r.registerDefaultTools()
	return r
}

// GetTools retourne la liste des outils
func (r *Manager) GetTools() []ToolDef {
	return r.tools
}

// RegisterTool ajoute un outil géré par un Executor (pas via le registry
// moderne) — utilisé pour spawn et les tools métier qui ont besoin du
// ConversationContext porté par ctx (cf. context.go).
func (r *Manager) RegisterTool(def ToolDef, exec Executor) {
	r.tools = append(r.tools, def)
	r.execs[def.Function.Name] = exec
}

// Execute exécute un outil : d'abord via le registry moderne (get_today),
// sinon via execs (spawn, tools métier).
func (r *Manager) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if r.disabled[name] {
		return "", fmt.Errorf("outil désactivé: %s", name)
	}

	if _, exists := r.inner.Get(name); exists {
		return r.inner.Execute(ctx, name, args)
	}

	exec, ok := r.execs[name]
	if !ok {
		return "", fmt.Errorf("outil inconnu: %s", name)
	}
	return exec(ctx, name, args)
}

// DisableTool désactive un outil (il ne sera plus exécutable ni visible)
func (r *Manager) DisableTool(name string) {
	r.disabled[name] = true
	var filtered []ToolDef
	for _, tool := range r.tools {
		if tool.Function.Name != name {
			filtered = append(filtered, tool)
		}
	}
	r.tools = filtered
}

// EnableTool réactive un outil
func (r *Manager) EnableTool(name string) {
	delete(r.disabled, name)
}

// IsDisabled vérifie si un outil est désactivé
func (r *Manager) IsDisabled(name string) bool {
	return r.disabled[name]
}

// registerDefaultTools déclare au LLM les outils implémentés dans le
// registry moderne (voir NewRegistryWithDefaults / gettoday.go).
func (r *Manager) registerDefaultTools() {
	r.tools = append(r.tools, ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "get_today",
			Description: "Retourne la date et l'heure actuelles",
			Parameters: Parameters{
				Type: "object",
				Properties: map[string]Property{
					"format": {Type: "string", Description: "Format: 'full', 'date', 'time', 'iso'", Enum: []string{"full", "date", "time", "iso"}},
				},
			},
		},
	})
}

// RegisterSpawnTool enregistre l'outil spawn avec son exécuteur.
func (r *Manager) RegisterSpawnTool(spawnFn SpawnExecutor) {
	r.spawnExec = spawnFn

	r.RegisterTool(ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "spawn",
			Description: "Crée un subagent pour exécuter une tâche en arrière-plan et revenir avec le résultat une fois terminée. Utile pour les tâches qui prennent du temps ou qui peuvent s'exécuter sans intervention.",
			Parameters: Parameters{
				Type: "object",
				Properties: map[string]Property{
					"task": {
						Type:        "string",
						Description: "La tâche complète à exécuter par le subagent. Sois précis et détaillé.",
					},
					"label": {
						Type:        "string",
						Description: "Label court pour identifier la tâche (ex: 'Comptage', 'Recherche')",
					},
				},
				Required: []string{"task"},
			},
		},
	}, r.execSpawn)
}

// SpawnExecutor est la fonction d'exécution pour l'outil spawn
type SpawnExecutor func(task string, label string, originChannel string, originChatID string) (string, error)

// execSpawn exécute l'outil spawn
func (r *Manager) execSpawn(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if r.spawnExec == nil {
		return "", fmt.Errorf("spawn non initialisé")
	}

	task, _ := args["task"].(string)
	label, _ := args["label"].(string)

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

	taskID, err := r.spawnExec(task, label, originChannel, originChatID)
	if err != nil {
		return "", err
	}

	shortID := taskID
	if len(taskID) > 8 {
		shortID = taskID[:8]
	}
	return fmt.Sprintf("🔍 Tâche de fond lancée (ID: %s). Je m'en occupe, je reviens vers vous dès que c'est prêt.", shortID), nil
}
