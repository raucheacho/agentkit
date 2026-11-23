package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/computer/computertest"
)

// Ces tests demandent un démon Docker ; ils sont sautés sinon (et avec -short).
// AGENTKIT_REQUIRE_DOCKER=1 (la CI) fait échouer au lieu de sauter.
func needDocker(t *testing.T) {
	required := os.Getenv("AGENTKIT_REQUIRE_DOCKER") != ""
	if testing.Short() && !required {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if required {
			t.Fatal("Docker indisponible : ", err)
		}
		t.Skip("Docker indisponible")
	}
}

func workspace(t *testing.T, network string) *Workspace {
	needDocker(t)
	w := New(Config{Name: "agentkit-test-" + network, Network: network})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute) // première construction de l'image
	defer cancel()
	if err := w.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Remove(context.Background()) })
	return w
}

func TestContract(t *testing.T) {
	w := workspace(t, "none")
	computertest.Run(t, func(*testing.T) computer.Workspace { return w })
}

// Une conversation ne lit pas les fichiers d'une autre, même par bash.
func TestSessionsIsolated(t *testing.T) {
	w := workspace(t, "none")
	ctx := context.Background()
	a, _ := w.Open(ctx, "client:ana")
	b, _ := w.Open(ctx, "client:bob")
	_ = a.WriteFile(ctx, "secret.txt", []byte("dossier médical"))
	res, err := b.Exec(ctx, "cat "+a.Dir()+"/secret.txt; ls "+a.Dir(), computer.ExecOptions{})
	if err != nil || strings.Contains(string(res.Output), "médical") || res.ExitCode == 0 {
		t.Fatalf("bob lit ana : %+v %v", res, err)
	}
	res, _ = a.Exec(ctx, "id -u; touch /etc/x 2>&1; touch /x 2>&1", computer.ExecOptions{})
	if strings.HasPrefix(string(res.Output), "0") || !strings.Contains(string(res.Output), "Read-only") && !strings.Contains(string(res.Output), "Permission denied") {
		t.Fatalf("droits : %s", res.Output)
	}
}

// Réseau "public" : Internet oui, réseau privé et hôte non.
func TestPublicNetwork(t *testing.T) {
	w := workspace(t, "public")
	ctx := context.Background()
	s, _ := w.Open(ctx, "web:net")
	res, _ := s.Exec(ctx, "curl -s -o /dev/null -w '%{http_code}' --max-time 15 https://example.com", computer.ExecOptions{})
	if !strings.Contains(string(res.Output), "200") {
		t.Skipf("pas d'accès Internet depuis le conteneur : %s", res.Output)
	}
	for _, target := range []string{"http://192.168.1.1", "http://host.docker.internal", "http://169.254.169.254", "http://10.0.0.1"} {
		res, _ := s.Exec(ctx, "curl -s -o /dev/null -w '%{http_code}' --max-time 5 "+target+" || echo bloqué", computer.ExecOptions{})
		if !strings.Contains(string(res.Output), "bloqué") {
			t.Errorf("%s joignable : %s", target, res.Output)
		}
	}
}

// Le superviseur ne laisse aucun processus derrière lui (le conteneur est
// isolé : ps n'y voit que ses propres processus).
func TestNoLeftovers(t *testing.T) {
	w := workspace(t, "none")
	ctx := context.Background()
	s, _ := w.Open(ctx, "web:leak")
	for range 3 {
		_, _ = s.Exec(ctx, "true", computer.ExecOptions{Timeout: 10 * time.Minute})
	}
	_, _ = s.Exec(ctx, "sleep 5", computer.ExecOptions{Timeout: time.Second})
	time.Sleep(4 * time.Second) // le -k 3 de timeout
	res, _ := s.Exec(ctx, "ps -eo user=,args= | grep -v -e 'ps -eo' -e agentkit-run -e 'sleep infinity' -e docker-init -e grep || true", computer.ExecOptions{})
	if strings.TrimSpace(string(res.Output)) != "" {
		t.Fatalf("processus restants :\n%s", res.Output)
	}
}
