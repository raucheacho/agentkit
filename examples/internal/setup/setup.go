// Package setup regroupe ce que les exemples partagent : base SQLite,
// fournisseur de modèle, serveur HTTP et arrêt propre.
package setup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/llm/openai"
)

// Env renvoie une variable d'environnement ou sa valeur par défaut.
func Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Main lance run avec un contexte annulé par Ctrl-C, et journalise l'erreur.
func Main(run func(ctx context.Context) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(ctx); err != nil {
		slog.Error("arrêt", "err", err)
		os.Exit(1)
	}
}

// Open ouvre la base SQLite du dossier de données.
func Open(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", filepath.Join(dir, "agent.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
}

// Model renvoie le modèle configuré par AGENTKIT_BASE_URL, AGENTKIT_API_KEY
// et AGENTKIT_MODEL (défaut : Ollama local), avec nouvelles tentatives.
func Model() (llm.Provider, openai.Config) {
	cfg := openai.Config{
		BaseURL: Env("AGENTKIT_BASE_URL", "http://localhost:11434/v1"),
		APIKey:  os.Getenv("AGENTKIT_API_KEY"),
		Model:   Env("AGENTKIT_MODEL", "llama3.1:8b"),
	}
	return llm.Retry(openai.New(cfg), 3, time.Second), cfg
}

// AdminToken renvoie ADMIN_TOKEN, ou en tire un au hasard et l'affiche.
func AdminToken() string {
	if t := os.Getenv("ADMIN_TOKEN"); t != "" {
		return t
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	slog.Info("jeton d'administration généré (définissez ADMIN_TOKEN pour le fixer)", "token", t)
	return t
}

// Serve sert h jusqu'à l'arrêt de ctx, en parallèle de run.
func Serve(ctx context.Context, addr string, h http.Handler, run func(ctx context.Context) error) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	slog.Info("prêt", "addr", addr)
	err := run(ctx)
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	select {
	case e := <-errc:
		return errors.Join(err, e)
	default:
		return err
	}
}
