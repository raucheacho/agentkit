// Package archtest vérifie les règles d'import du framework :
//   - tout le module (hors exemples et tests) ne dépend que de la bibliothèque
//     standard : aucun SDK, aucun pilote imposé à l'application ;
//   - les sous-modules (contrib/…) peuvent dépendre d'autres modules, cgo compris ;
//   - le noyau n'importe ni fournisseur (llm/openai), ni stockage (*/sqlite),
//     ni adaptateur de canal (channel/*) : ce sont des choix de l'application.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/raucheacho/agentkit"

var core = map[string]bool{
	"llm": true, "tool": true, "session": true, "prompt": true, "agent": true,
	"turn": true, "runner": true, "channel": true, "trust": true, "delegate": true,
	"memory": true, "memory/notes": true, "skills": true, "schedule": true, "observe": true,
	"mcp": true, "event": true, "computer": true,
}

func TestImportRules(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "examples" || strings.HasPrefix(rel, ".") && rel != "." {
				return filepath.SkipDir
			}
			// Un sous-module (contrib/vectix…) a ses propres dépendances.
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkg := filepath.ToSlash(filepath.Dir(rel))
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			first := strings.SplitN(p, "/", 2)[0]
			if !strings.Contains(first, ".") {
				continue // bibliothèque standard
			}
			if !strings.HasPrefix(p, module+"/") {
				t.Errorf("%s importe %s : le module ne dépend que de la bibliothèque standard", rel, p)
				continue
			}
			target := strings.TrimPrefix(p, module+"/")
			if core[pkg] && !core[target] && !strings.HasSuffix(target, "test") {
				t.Errorf("%s (noyau) importe %s : le noyau ne choisit ni fournisseur, ni stockage, ni canal", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
