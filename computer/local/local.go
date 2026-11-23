// Package local est un computer.Workspace sur la machine elle-même : un
// dossier par conversation, des commandes lancées par bash.
//
// AUCUNE ISOLATION : les commandes ont les droits du processus de
// l'application et voient tout le système de fichiers et le réseau. Seul
// l'environnement est vidé (pas de clés). Réservé au développement, aux
// tests et aux agents de confiance ; en production, utilisez computer/docker.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/computer"
)

// Config configure l'espace de travail.
type Config struct {
	// Root contient un dossier par conversation et un HOME commun.
	Root string
	// Shell (défaut "/bin/bash").
	Shell string
	// Env : variables ajoutées à l'environnement minimal (« CLE=valeur »).
	Env []string
	// DefaultTimeout (défaut 10 min) et MaxOutput (défaut 1 Mio).
	DefaultTimeout time.Duration
	MaxOutput      int
}

// Workspace est un computer.Workspace local.
type Workspace struct {
	cfg  Config
	mu   sync.Mutex
	open map[string]*session
}

// New crée l'espace de travail (Root est créé au besoin).
func New(cfg Config) (*Workspace, error) {
	if cfg.Root == "" {
		return nil, errors.New("local : Root est obligatoire")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	cfg.Root = root
	if cfg.Shell == "" {
		cfg.Shell = "/bin/bash"
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 1 << 20
	}
	for _, d := range []string{root, filepath.Join(root, ".home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	// Chemin réel (sur macOS, /var est un lien vers /private/var) : c'est
	// celui que voient les commandes.
	if cfg.Root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	return &Workspace{cfg: cfg, open: map[string]*session{}}, nil
}

func (w *Workspace) Open(_ context.Context, key string) (computer.Session, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.open[key]; s != nil {
		return s, nil
	}
	dir := filepath.Join(w.cfg.Root, computer.Folder(key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &session{w: w, dir: dir}
	w.open[key] = s
	return s, nil
}

func (w *Workspace) Close() error { return nil }

type session struct {
	w   *Workspace
	dir string
}

func (s *session) Dir() string { return s.dir }

func (s *session) env() []string {
	return append([]string{
		"PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + filepath.Join(s.w.cfg.Root, ".home"),
		"LANG=C.UTF-8",
		"TERM=dumb",
		"TMPDIR=" + os.TempDir(),
	}, s.w.cfg.Env...)
}

func (s *session) Exec(ctx context.Context, command string, opts computer.ExecOptions) (computer.ExecResult, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = s.w.cfg.DefaultTimeout
	}
	max := opts.MaxOutput
	if max <= 0 {
		max = s.w.cfg.MaxOutput
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := computer.NewOutput(max)
	cmd := exec.CommandContext(cctx, s.w.cfg.Shell, "--noprofile", "--norc", "-c", command)
	cmd.Dir, cmd.Env = s.dir, s.env()
	cmd.Stdout, cmd.Stderr = out, out
	if opts.Stdin != nil {
		cmd.Stdin = strings.NewReader(string(opts.Stdin))
	}
	start := time.Now()
	res, err := run(cctx, cmd)
	res.Duration = time.Since(start)
	res.Output, res.Truncated = out.Bytes()
	if err != nil {
		return res, err
	}
	if cctx.Err() != nil && ctx.Err() == nil {
		res.TimedOut = true
	} else if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

func (s *session) root() (*os.Root, error) { return os.OpenRoot(s.dir) }

func (s *session) ReadFile(_ context.Context, name string, max int64) ([]byte, error) {
	p, err := computer.Clean(s.dir, name)
	if err != nil {
		return nil, err
	}
	r, err := s.root()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(p) // os.Root refuse les liens qui sortent du dossier
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("fichier %s introuvable", name)
	}
	if err != nil {
		return nil, fmt.Errorf("fichier %s illisible", name)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.IsDir() {
		return nil, fmt.Errorf("%s est un dossier", name)
	}
	if max <= 0 {
		max = 4 << 20
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("fichier %s trop gros (plus de %d octets) : utilise bash", name, max)
	}
	return data, nil
}

func (s *session) WriteFile(_ context.Context, name string, data []byte) error {
	p, err := computer.Clean(s.dir, name)
	if err != nil {
		return err
	}
	r, err := s.root()
	if err != nil {
		return err
	}
	defer r.Close()
	if d := filepath.Dir(p); d != "." {
		if err := r.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("dossier %s : %w", d, err)
		}
	}
	return r.WriteFile(p, data, 0o600)
}

var _ computer.Workspace = (*Workspace)(nil)
