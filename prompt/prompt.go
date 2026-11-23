// Package prompt assemble le prompt système d'une session à partir de
// sections : identité, instructions, fichiers de contexte (AGENTS.md,
// SOUL.md…), index de skills ou de mémoire.
//
// Le prompt est calculé une fois, à la création de la session (voir
// agent.Config.System), puis figé. Une section ne doit donc contenir que ce
// qui reste vrai pendant toute la session : ce qui change à chaque tour passe
// par la note éphémère de l'agent.
package prompt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/raucheacho/agentkit/tool"
)

// Section produit un morceau de prompt ("" : rien à ajouter).
type Section func(ctx context.Context, conv tool.Conversation) (string, error)

// Builder assemble des sections, dans l'ordre, séparées par une ligne vide.
type Builder struct {
	sections []Section
}

// New crée un Builder.
func New(sections ...Section) *Builder { return &Builder{sections: sections} }

// Add ajoute des sections.
func (b *Builder) Add(sections ...Section) *Builder {
	b.sections = append(b.sections, sections...)
	return b
}

// Build assemble le prompt. Sa signature correspond à agent.Config.System.
func (b *Builder) Build(ctx context.Context, conv tool.Conversation) (string, error) {
	var parts []string
	for _, s := range b.sections {
		out, err := s(ctx, conv)
		if err != nil {
			return "", err
		}
		if out = strings.TrimSpace(out); out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// Text est une section fixe.
func Text(s string) Section {
	return func(context.Context, tool.Conversation) (string, error) { return s, nil }
}

// Func adapte une fonction sans erreur.
func Func(f func(ctx context.Context, conv tool.Conversation) string) Section {
	return func(ctx context.Context, conv tool.Conversation) (string, error) { return f(ctx, conv), nil }
}

// Heading préfixe une section d'un titre Markdown ; rien si la section est vide.
func Heading(title string, s Section) Section {
	return func(ctx context.Context, conv tool.Conversation) (string, error) {
		out, err := s(ctx, conv)
		if err != nil || strings.TrimSpace(out) == "" {
			return "", err
		}
		return "## " + title + "\n\n" + out, nil
	}
}

// File lit un fichier de contexte à la création de la session. Un fichier
// absent ne produit rien ; un fichier trop long est tronqué à maxChars
// (0 : 20 000), début et fin conservés.
func File(path string, maxChars int) Section {
	if maxChars <= 0 {
		maxChars = 20000
	}
	return func(context.Context, tool.Conversation) (string, error) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("prompt : lecture de %s : %w", path, err)
		}
		return clip(string(data), maxChars, filepath.Base(path)), nil
	}
}

// Files lit plusieurs fichiers de contexte d'un même dossier, dans l'ordre.
func Files(dir string, maxChars int, names ...string) Section {
	return func(ctx context.Context, conv tool.Conversation) (string, error) {
		var parts []string
		for _, n := range names {
			out, err := File(filepath.Join(dir, n), maxChars)(ctx, conv)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(out) != "" {
				parts = append(parts, out)
			}
		}
		return strings.Join(parts, "\n\n"), nil
	}
}

func clip(s string, max int, name string) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	head, tail := max*2/3, max/3
	return string(r[:head]) + fmt.Sprintf("\n\n[… %s tronqué : %d caractères omis …]\n\n", name, len(r)-head-tail) + string(r[len(r)-tail:])
}
