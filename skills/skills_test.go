package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raucheacho/agentkit/tool"
)

func view(t *testing.T, l *Library, args string) string {
	t.Helper()
	res, err := l.ViewTool().Run(context.Background(), tool.Call{Raw: []byte(args)})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content
}

func TestLoadAndView(t *testing.T) {
	l, err := Load(filepath.Join("testdata", "app"), filepath.Join("testdata", "absent"))
	if err != nil {
		t.Fatal(err)
	}
	list := l.List()
	if len(list) != 1 || list[0].Name != "resume" || list[0].Description != "Résumer un long document en points clés" || list[0].Meta["license"] != "MIT" {
		t.Fatalf("skills : %+v", list)
	}
	if len(l.Errors()) != 1 {
		t.Fatalf("skill invalide signalé : %v", l.Errors())
	}
	sec, _ := l.Section(context.Background(), tool.Conversation{})
	if !strings.Contains(sec, "- resume : Résumer un long document") {
		t.Fatal(sec)
	}
	if out := view(t, l, `{"name":"resume"}`); !strings.Contains(out, "1. Lire le document.") || strings.Contains(out, "license") || !strings.Contains(out, "modele.md") {
		t.Fatalf("lecture : %s", out)
	}
	if out := view(t, l, `{"name":"resume","file":"modele.md"}`); !strings.Contains(out, "point 1") {
		t.Fatalf("annexe : %s", out)
	}
	for _, bad := range []string{"../../secret.txt", "/etc/passwd", "../broken/SKILL.md"} {
		if out := view(t, l, `{"name":"resume","file":"`+bad+`"}`); !strings.HasPrefix(out, "Erreur") {
			t.Fatalf("sortie du dossier avec %s : %s", bad, out)
		}
	}
	if out := view(t, l, `{"name":"inconnu"}`); !strings.Contains(out, "resume") {
		t.Fatal(out)
	}
}

func TestSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	sk := filepath.Join(dir, "s")
	_ = os.MkdirAll(sk, 0o755)
	_ = os.WriteFile(filepath.Join(sk, "SKILL.md"), []byte("---\nname: s\ndescription: d\n---\nx"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret"), []byte("SECRET"), 0o644)
	if err := os.Symlink(filepath.Join(dir, "secret"), filepath.Join(sk, "lien")); err != nil {
		t.Skip(err)
	}
	l, _ := Load(dir)
	if out := view(t, l, `{"name":"s","file":"lien"}`); strings.Contains(out, "SECRET") {
		t.Fatal("lien symbolique suivi hors du dossier")
	}
}

func TestSave(t *testing.T) {
	own := t.TempDir()
	l, _ := Load(filepath.Join("testdata", "app"), own)
	save := l.SaveTool(own)
	run := func(args string) string {
		res, err := save.Run(context.Background(), tool.Call{Raw: []byte(args)})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	if out := run(`{"name":"devis","description":"Rédiger un \"devis\"","body":"Étapes…"}`); !strings.Contains(out, "enregistré") {
		t.Fatal(out)
	}
	sk, ok := l.Get("devis")
	if !ok || sk.Description != "Rédiger un 'devis'" || sk.Body != "Étapes…" {
		t.Fatalf("relu : %+v", sk)
	}
	if out := run(`{"name":"resume","description":"x","body":"y"}`); !strings.HasPrefix(out, "Erreur") {
		t.Fatalf("skill de l'application remplacé : %s", out)
	}
	if out := run(`{"name":"../x","description":"x","body":"y"}`); !strings.HasPrefix(out, "Erreur") {
		t.Fatal(out)
	}
}
