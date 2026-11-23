// Package computertest vérifie qu'une implémentation de computer.Workspace
// respecte le contrat.
package computertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/computer"
)

// Run exécute le contrat sur un Workspace neuf produit par open.
func Run(t *testing.T, open func(t *testing.T) computer.Workspace) {
	ctx := context.Background()
	ws := open(t)
	a, err := ws.Open(ctx, "web:ana")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ws.Open(ctx, "web:bob")

	t.Run("commande, code de sortie, entrée standard", func(t *testing.T) {
		res, err := a.Exec(ctx, "echo bonjour; echo erreur >&2; exit 3", computer.ExecOptions{})
		if err != nil || res.ExitCode != 3 || !strings.Contains(string(res.Output), "bonjour") || !strings.Contains(string(res.Output), "erreur") {
			t.Fatalf("%+v %v", res, err)
		}
		res, _ = a.Exec(ctx, "tr a-z A-Z", computer.ExecOptions{Stdin: []byte("salut")})
		if strings.TrimSpace(string(res.Output)) != "SALUT" {
			t.Fatalf("stdin : %q", res.Output)
		}
		res, _ = a.Exec(ctx, "pwd", computer.ExecOptions{})
		if strings.TrimSpace(string(res.Output)) != a.Dir() {
			t.Fatalf("dossier : %q, attendu %q", res.Output, a.Dir())
		}
	})

	t.Run("aucune variable de l'application", func(t *testing.T) {
		t.Setenv("AGENTKIT_TEST_SECRET", "sk-ne-doit-pas-fuir")
		res, _ := a.Exec(ctx, "env", computer.ExecOptions{})
		if strings.Contains(string(res.Output), "sk-ne-doit-pas-fuir") {
			t.Fatal("variable de l'application visible")
		}
	})

	t.Run("fichiers et dossiers séparés", func(t *testing.T) {
		if err := a.WriteFile(ctx, "notes/a.txt", []byte("A")); err != nil {
			t.Fatal(err)
		}
		if data, err := a.ReadFile(ctx, "notes/a.txt", 0); err != nil || string(data) != "A" {
			t.Fatalf("relu : %q %v", data, err)
		}
		if data, err := a.ReadFile(ctx, a.Dir()+"/notes/a.txt", 0); err != nil || string(data) != "A" {
			t.Fatalf("chemin absolu dans le dossier : %q %v", data, err)
		}
		res, _ := a.Exec(ctx, "cat notes/a.txt", computer.ExecOptions{})
		if string(res.Output) != "A" {
			t.Fatalf("vu par la commande : %q", res.Output)
		}
		if _, err := b.ReadFile(ctx, "notes/a.txt", 0); err == nil {
			t.Fatal("bob lit le fichier d'ana")
		}
		// Persistance : un nouvel Open retrouve les fichiers.
		again, _ := ws.Open(ctx, "web:ana")
		if data, _ := again.ReadFile(ctx, "notes/a.txt", 0); string(data) != "A" {
			t.Fatal("fichier perdu")
		}
		if _, err := a.ReadFile(ctx, "inconnu.txt", 0); err == nil {
			t.Fatal("fichier inconnu")
		}
		if _, err := a.ReadFile(ctx, "gros", 0); err == nil {
			t.Fatal("fichier inconnu")
		}
		_, _ = a.Exec(ctx, "head -c 5000 /dev/zero > gros", computer.ExecOptions{})
		if _, err := a.ReadFile(ctx, "gros", 100); err == nil {
			t.Fatal("limite de lecture ignorée")
		}
	})

	t.Run("aucun chemin ne sort du dossier", func(t *testing.T) {
		for _, p := range []string{"../web_bob-x/notes", "/etc/passwd", "..", "a/../../x", ""} {
			if _, err := a.ReadFile(ctx, p, 0); err == nil {
				t.Errorf("lecture de %q acceptée", p)
			}
			if err := a.WriteFile(ctx, p, []byte("x")); err == nil {
				t.Errorf("écriture de %q acceptée", p)
			}
		}
		_, _ = a.Exec(ctx, "ln -sf /etc/passwd lien; ln -sfn / racine", computer.ExecOptions{})
		if _, err := a.ReadFile(ctx, "lien", 0); err == nil {
			t.Error("lien symbolique suivi hors du dossier")
		}
		if err := a.WriteFile(ctx, "racine/tmp/agentkit-evasion", []byte("x")); err == nil {
			_ = os.Remove("/tmp/agentkit-evasion")
			t.Error("écriture à travers un lien hors du dossier")
		}
	})

	t.Run("délai, annulation, processus laissés en arrière-plan", func(t *testing.T) {
		start := time.Now()
		res, err := a.Exec(ctx, "sleep 30", computer.ExecOptions{Timeout: 500 * time.Millisecond})
		if err != nil || !res.TimedOut || time.Since(start) > 10*time.Second {
			t.Fatalf("délai : %+v %v en %s", res, err, time.Since(start))
		}
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if _, err := a.Exec(cctx, "sleep 30", computer.ExecOptions{}); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("annulation : %v", err)
		}
		// Un processus détaché ne survit pas à sa commande. Durée propre à ce
		// test (d'autres tests peuvent tourner en parallèle sur la machine) ;
		// le motif « …[d] » ne se reconnaît pas lui-même.
		d := fmt.Sprintf("30.%d", time.Now().UnixNano()%1000000)
		_, _ = a.Exec(ctx, "(sleep "+d+" && touch survivant) >/dev/null 2>&1 &", computer.ExecOptions{})
		res, _ = a.Exec(ctx, "pgrep -f 'sleep "+d[:len(d)-1]+"["+d[len(d)-1:]+"]' || echo aucun", computer.ExecOptions{})
		if !strings.Contains(string(res.Output), "aucun") {
			ps, _ := a.Exec(ctx, "ps -o pid,ppid,pgid,etime,args -p $(pgrep -d, -f 'sleep "+d[:len(d)-1]+"["+d[len(d)-1:]+"]')", computer.ExecOptions{})
			t.Fatalf("processus survivant : %q\n%s", res.Output, ps.Output)
		}
	})

	t.Run("sortie bornée", func(t *testing.T) {
		res, err := a.Exec(ctx, "yes abcdefghij | head -c 300000", computer.ExecOptions{MaxOutput: 1000})
		if err != nil || !res.Truncated || len(res.Output) > 1200 || !strings.Contains(string(res.Output), "octets omis") {
			t.Fatalf("troncature : %d octets, %v %v", len(res.Output), res.Truncated, err)
		}
	})
}
