package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raucheacho/agentkit/tool"
)

func TestBuilder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Règles du projet."), 0o600); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 50) + strings.Repeat("z", 50)
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(
		Text("# Assistant"),
		Heading("Contexte", Files(dir, 30, "AGENTS.md", "ABSENT.md", "SOUL.md")),
		Heading("Vide", Text("  ")),
		Func(func(_ context.Context, c tool.Conversation) string { return "Canal : " + c.Channel }),
	)
	got, err := b.Build(context.Background(), tool.Conversation{Channel: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Assistant\n\n## Contexte\n\nRègles du projet.", "SOUL.md tronqué", "Canal : telegram"} {
		if !strings.Contains(got, want) {
			t.Errorf("absent : %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "Vide") {
		t.Error("une section vide ne doit pas laisser son titre")
	}
}
