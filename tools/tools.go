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

// Manager expose au LLM la liste des outils (ToolDef) et route leur
// exécution vers le registry moderne (inner).
type Manager struct {
	tools    []ToolDef
	inner    *Registry
	disabled map[string]bool
}

// NewManager crée un nouveau manager d'outils
func NewManager() *Manager {
	return &Manager{
		tools:    []ToolDef{},
		inner:    NewRegistry(),
		disabled: make(map[string]bool),
	}
}

// GetTools retourne la liste des outils
func (r *Manager) GetTools() []ToolDef {
	return r.tools
}

// Register enregistre un outil implémentant l'interface Tool.
// Il construit automatiquement le schéma ToolDef à partir de Parameters().
func (r *Manager) Register(tool Tool) error {
	if r.inner == nil {
		r.inner = NewRegistry()
	}
	if err := r.inner.Register(tool); err != nil {
		return err
	}

	// Ajouter à la liste des outils exposés au LLM
	toolDef := ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  schemaToParameters(tool.Parameters()),
		},
	}
	r.tools = append(r.tools, toolDef)
	return nil
}

// schemaToParameters convertit un JSON schema standard en Parameters.
func schemaToParameters(schema map[string]interface{}) Parameters {
	params := Parameters{
		Type:       "object",
		Properties: make(map[string]Property),
		Required:   []string{},
	}
	if schema == nil {
		return params
	}

	if req, ok := schema["required"].([]interface{}); ok {
		for _, r := range req {
			if str, ok := r.(string); ok {
				params.Required = append(params.Required, str)
			}
		}
	} else if req, ok := schema["required"].([]string); ok {
		params.Required = append(params.Required, req...)
	}

	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for k, v := range props {
			if propMap, ok := v.(map[string]interface{}); ok {
				prop := Property{}
				if t, ok := propMap["type"].(string); ok {
					prop.Type = t
				}
				if d, ok := propMap["description"].(string); ok {
					prop.Description = d
				}
				if enum, ok := propMap["enum"].([]interface{}); ok {
					for _, e := range enum {
						if str, ok := e.(string); ok {
							prop.Enum = append(prop.Enum, str)
						}
					}
				} else if enum, ok := propMap["enum"].([]string); ok {
					prop.Enum = append(prop.Enum, enum...)
				}
				params.Properties[k] = prop
			}
		}
	}
	return params
}

// Execute exécute un outil via le registry.
func (r *Manager) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if r.disabled[name] {
		return "", fmt.Errorf("outil désactivé: %s", name)
	}

	return r.inner.Execute(ctx, name, args)
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
