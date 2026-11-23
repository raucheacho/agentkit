// Package docker est un computer.Workspace dans un conteneur Docker
// verrouillé, piloté par la commande docker (aucun SDK).
//
// Sécurité, à la manière de Nod :
//   - conteneur sans aucune capacité (cap-drop ALL), sans élévation de
//     privilèges, racine en lecture seule, mémoire, CPU et processus bornés ;
//   - réseau au choix : "public" (Internet public seulement : un conteneur
//     voisin pose des règles iptables qui refusent les réseaux privés, le
//     réseau local et les métadonnées du cloud), "none" (aucun réseau) ou
//     "open" (réseau Docker par défaut, sans filtre) ;
//   - aucune variable de l'application n'entre dans le conteneur ;
//   - chaque conversation a son dossier et son propre utilisateur Linux
//     (dossier en 700) : une conversation ne lit pas les fichiers d'une
//     autre, même par bash ;
//   - un superviseur lance chaque commande dans sa session de processus,
//     applique le délai et arrête tout le groupe à la fin ; si la liaison
//     est perdue en cours de route, l'issue est inconnue (ErrUncertain) et
//     la commande n'est pas relancée.
package docker

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"hash/fnv"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/turn"
)

//go:embed Dockerfile
var dockerfile []byte

// DefaultImage est l'image construite par Ensure à partir du Dockerfile embarqué.
const DefaultImage = "agentkit-workspace:2"

// Config configure l'espace de travail.
type Config struct {
	// Name nomme le conteneur et ses volumes (défaut "agentkit-workspace").
	Name string
	// Image (défaut DefaultImage, construite au besoin).
	Image string
	// Network : "public" (défaut), "none" ou "open".
	Network string
	// Limites (défauts : "2g", "2", 1024).
	Memory string
	CPUs   string
	Pids   int
	// DefaultTimeout (défaut 10 min) et MaxOutput (défaut 1 Mio).
	DefaultTimeout time.Duration
	MaxOutput      int
	// Docker : chemin de la commande docker (défaut "docker").
	Docker string
}

// Workspace est un computer.Workspace Docker.
type Workspace struct {
	cfg   Config
	mu    sync.Mutex
	ready bool
	open  map[string]*session
}

// New prépare l'espace de travail ; le conteneur est lancé au premier Open
// (ou par Ensure).
func New(cfg Config) *Workspace {
	if cfg.Name == "" {
		cfg.Name = "agentkit-workspace"
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.Network == "" {
		cfg.Network = "public"
	}
	if cfg.Memory == "" {
		cfg.Memory = "2g"
	}
	if cfg.CPUs == "" {
		cfg.CPUs = "2"
	}
	if cfg.Pids <= 0 {
		cfg.Pids = 1024
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 1 << 20
	}
	if cfg.Docker == "" {
		cfg.Docker = "docker"
	}
	return &Workspace{cfg: cfg, open: map[string]*session{}}
}

func (w *Workspace) docker(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, w.cfg.Docker, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s : %w : %s", args[0], err, strings.TrimSpace(lastLine(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Ensure construit l'image si besoin et lance les conteneurs.
func (w *Workspace) Ensure(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready {
		return nil
	}
	if _, err := w.docker(ctx, nil, "image", "inspect", w.cfg.Image); err != nil {
		if w.cfg.Image != DefaultImage {
			return fmt.Errorf("docker : image %s introuvable", w.cfg.Image)
		}
		if _, err := w.docker(ctx, dockerfile, "build", "-t", w.cfg.Image, "-"); err != nil {
			return fmt.Errorf("docker : construction de l'image : %w", err)
		}
	}
	network := "--network=none"
	switch w.cfg.Network {
	case "open":
		network = "--network=bridge"
	case "public":
		net := w.cfg.Name + "-net"
		if err := w.start(ctx, net, "--init", "--cap-drop=ALL", "--cap-add=NET_ADMIN", "--security-opt=no-new-privileges",
			"--read-only", "--tmpfs=/tmp:size=1m", "--memory=64m", "--pids-limit=32", "--dns=1.1.1.1", "--dns=8.8.8.8",
			"--user=0", "--entrypoint=/usr/local/bin/agentkit-network", w.cfg.Image); err != nil {
			return err
		}
		ok := false
		for i := 0; i < 50 && !ok; i++ {
			_, err := w.docker(ctx, nil, "exec", net, "test", "-f", "/tmp/network-ready")
			if ok = err == nil; !ok {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if !ok {
			return errors.New("docker : le filtrage réseau n'est pas prêt")
		}
		network = "--network=container:" + net
	case "none":
	default:
		return fmt.Errorf("docker : réseau %q inconnu (public, none, open)", w.cfg.Network)
	}
	// Seul root (utilisé par l'hôte pour créer les dossiers) garde CHOWN ;
	// les commandes tournent avec des utilisateurs sans privilège.
	if err := w.start(ctx, w.cfg.Name, "--init", "--cap-drop=ALL", "--cap-add=CHOWN", "--cap-add=FOWNER",
		"--security-opt=no-new-privileges", "--read-only", "--tmpfs=/tmp:size=512m,mode=1777",
		"--memory="+w.cfg.Memory, "--cpus="+w.cfg.CPUs, "--pids-limit="+strconv.Itoa(w.cfg.Pids),
		"--volume="+w.cfg.Name+"-data:/workspace", network, w.cfg.Image, "sleep", "infinity"); err != nil {
		return err
	}
	if _, err := w.docker(ctx, nil, "exec", "-u", "0", w.cfg.Name, "chmod", "711", "/workspace"); err != nil {
		return err
	}
	w.ready = true
	return nil
}

// start lance un conteneur s'il ne tourne pas déjà.
func (w *Workspace) start(ctx context.Context, name string, args ...string) error {
	state, err := w.docker(ctx, nil, "inspect", "-f", "{{.State.Running}}", name)
	switch {
	case err == nil && state == "true":
		return nil
	case err == nil:
		_, err = w.docker(ctx, nil, "start", name)
		return err
	}
	_, err = w.docker(ctx, nil, append([]string{"run", "-d", "--name", name, "--restart=unless-stopped"}, args...)...)
	return err
}

// Remove supprime les conteneurs et le volume de données (tests, désinstallation).
func (w *Workspace) Remove(ctx context.Context) error {
	w.mu.Lock()
	w.ready, w.open = false, map[string]*session{}
	w.mu.Unlock()
	_, _ = w.docker(ctx, nil, "rm", "-f", w.cfg.Name, w.cfg.Name+"-net")
	_, err := w.docker(ctx, nil, "volume", "rm", "-f", w.cfg.Name+"-data")
	return err
}

// uid donne à chaque conversation son utilisateur Linux.
func uid(folder string) int {
	h := fnv.New32a()
	h.Write([]byte(folder))
	return 100000 + int(h.Sum32()%900000000)
}

func (w *Workspace) Open(ctx context.Context, key string) (computer.Session, error) {
	if err := w.Ensure(ctx); err != nil {
		return nil, err
	}
	w.mu.Lock()
	if s := w.open[key]; s != nil {
		w.mu.Unlock()
		return s, nil
	}
	w.mu.Unlock()
	folder := computer.Folder(key)
	s := &session{w: w, dir: "/workspace/" + folder, user: strconv.Itoa(uid(folder))}
	if _, err := w.docker(ctx, nil, "exec", "-u", "0", w.cfg.Name, "sh", "-c",
		`mkdir -p "$1/.home" && chown -R "$2:$2" "$1" && chmod 700 "$1"`, "sh", s.dir, s.user); err != nil {
		return nil, fmt.Errorf("docker : dossier de %s : %w", key, err)
	}
	w.mu.Lock()
	w.open[key] = s
	w.mu.Unlock()
	return s, nil
}

func (w *Workspace) Close() error { return nil }

type session struct {
	w    *Workspace
	dir  string
	user string
}

func (s *session) Dir() string { return s.dir }

func (s *session) execArgs(interactive bool, args ...string) []string {
	base := []string{"exec"}
	if interactive {
		base = append(base, "-i")
	}
	base = append(base, "-u", s.user+":"+s.user, "-w", s.dir, "-e", "HOME="+s.dir+"/.home", "-e", "TMPDIR=/tmp", s.w.cfg.Name)
	return append(base, args...)
}

var markers = regexp.MustCompile(`@@agentkit:[0-9a-f]+:(start|exit:[a-z0-9]+)@@\n?`)

func (s *session) Exec(ctx context.Context, command string, opts computer.ExecOptions) (computer.ExecResult, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = s.w.cfg.DefaultTimeout
	}
	maxOut := opts.MaxOutput
	if maxOut <= 0 {
		maxOut = s.w.cfg.MaxOutput
	}
	id := turn.NewID()
	secs := strconv.Itoa(max(int(timeout.Seconds()), 1))
	out := computer.NewOutput(maxOut)
	// L'hôte attend un peu plus que le superviseur, qui applique le délai.
	cctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.w.cfg.Docker, s.execArgs(true, "/usr/local/bin/agentkit-run", id, secs, command)...)
	cmd.Stdin = bytes.NewReader(opts.Stdin)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	runErr := cmd.Run()
	res := computer.ExecResult{Duration: time.Since(start), ExitCode: -1}
	raw, truncated := out.Bytes()
	text := string(raw)

	if ctx.Err() != nil || cctx.Err() != nil {
		// Annulation : le superviseur arrête le groupe dans le conteneur.
		kctx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, _ = s.w.docker(kctx, nil, s.execArgs(false, "/usr/local/bin/agentkit-kill", id)...)
		kcancel()
	}
	started := strings.Contains(text, "@@agentkit:"+id+":start@@")
	exitTag := regexp.MustCompile(`@@agentkit:` + id + `:exit:([a-z0-9]+)@@`).FindStringSubmatch(text)
	res.Output, res.Truncated = []byte(strings.TrimSuffix(markers.ReplaceAllString(text, ""), "\n")), truncated
	switch {
	case exitTag != nil && exitTag[1] == "timeout":
		res.TimedOut, res.ExitCode = true, 124
		return res, nil
	case exitTag != nil:
		res.ExitCode, _ = strconv.Atoi(exitTag[1])
		return res, nil
	case ctx.Err() != nil:
		return res, ctx.Err()
	case !started:
		return res, fmt.Errorf("docker : commande non lancée : %v %s", runErr, strings.TrimSpace(lastLine(text)))
	}
	return res, computer.ErrUncertain
}

// Les fichiers passent par le conteneur, avec l'utilisateur de la
// conversation : un lien symbolique ne mène pas hors de son dossier
// (vérification par realpath) ni vers les fichiers d'une autre conversation
// (droits Unix).
const readScript = `f=$(realpath -e -- "$1" 2>/dev/null) || { echo introuvable >&2; exit 3; }
case "$f" in "$2"/*) ;; *) echo hors >&2; exit 4;; esac
[ -f "$f" ] || { echo dossier >&2; exit 5; }
s=$(stat -c %s -- "$f"); [ "$s" -le "$3" ] || { echo trop >&2; exit 6; }
exec cat -- "$f"`

const writeScript = `f=$(realpath -m -- "$1") || exit 4
case "$f" in "$2"/*) ;; *) echo hors >&2; exit 4;; esac
mkdir -p -- "$(dirname -- "$f")" && f2=$(realpath -m -- "$1") && [ "$f" = "$f2" ] || { echo hors >&2; exit 4; }
exec cat > "$f"`

func (s *session) ReadFile(ctx context.Context, name string, maxBytes int64) ([]byte, error) {
	p, err := computer.Clean(s.dir, name)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	cmd := exec.CommandContext(ctx, s.w.cfg.Docker, s.execArgs(false, "sh", "-c", readScript, "sh", s.dir+"/"+p, s.dir, strconv.FormatInt(maxBytes, 10))...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		switch strings.TrimSpace(stderr.String()) {
		case "introuvable":
			return nil, fmt.Errorf("fichier %s introuvable", name)
		case "hors":
			return nil, computer.ErrPath
		case "dossier":
			return nil, fmt.Errorf("%s n'est pas un fichier", name)
		case "trop":
			return nil, fmt.Errorf("fichier %s trop gros (plus de %d octets) : utilise bash", name, maxBytes)
		}
		return nil, fmt.Errorf("lecture de %s : %v", name, err)
	}
	return stdout.Bytes(), nil
}

func (s *session) WriteFile(ctx context.Context, name string, data []byte) error {
	p, err := computer.Clean(s.dir, name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.w.cfg.Docker, s.execArgs(true, "sh", "-c", writeScript, "sh", s.dir+"/"+p, s.dir)...)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.TrimSpace(stderr.String()) == "hors" {
			return computer.ErrPath
		}
		return fmt.Errorf("écriture de %s : %v %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var _ computer.Workspace = (*Workspace)(nil)
