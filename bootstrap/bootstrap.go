// Package bootstrap ensures the agent's workspace has a customizable
// instructions file, and loads it into the system prompt.
package bootstrap

import (
	"os"
	"path/filepath"
)

// AgentsFile is the name of the bootstrap instructions file.
const AgentsFile = "AGENTS.md"

// defaultContent is written the first time a workspace is created; it is a
// placeholder meant to be edited at runtime with app-specific instructions
// (the persona and tool usage rules live in agent.Config, not here).
const defaultContent = `# Instructions pour l'Agent

Ce fichier est chargé dans le prompt système à chaque message. Éditez-le pour
ajuster le comportement de l'agent sans redéployer.

## Règles
- Sois concis mais complet.
- Pour toute action irréversible, demande toujours confirmation explicite avant d'agir.
`

// EnsureBootstrapFiles crée AGENTS.md dans le workspace s'il n'existe pas déjà.
func EnsureBootstrapFiles(workspaceDir string) error {
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(workspaceDir, AgentsFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.WriteFile(path, []byte(defaultContent), 0644)
	}
	return nil
}

// LoadBootstrapFiles charge AGENTS.md pour l'injecter dans le prompt système.
func LoadBootstrapFiles(workspaceDir string) string {
	path := filepath.Join(workspaceDir, AgentsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return "# Contexte\n\n## " + AgentsFile + "\n\n" + string(data)
}
