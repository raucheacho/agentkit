// Package memory branche des fournisseurs de mémoire sur un agent.
//
// Un fournisseur n'implémente que ce qui le concerne, à chaque moment du
// cycle de vie d'une session :
//   - Prompter : un bloc figé dans le prompt système, à la création de la
//     session (index des notes, profil de l'interlocuteur) ;
//   - Recaller : à chaque tour, des souvenirs pertinents pour le message
//     reçu, ajoutés à la note éphémère (jamais enregistrés) ;
//   - ToolProvider : des outils (noter, relire, chercher) ;
//   - Syncer : après chaque tour, en arrière-plan (extraction de faits,
//     indexation) ;
//   - PreCompactor : juste avant la compression, les messages qui vont être
//     archivés (dernière chance d'en garder l'essentiel).
//
// Set les assemble et fournit les crochets attendus par agent.Config
// (Recall, AfterTurn, Compaction.OnCompact) et une section de prompt.
package memory

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/prompt"
	"github.com/raucheacho/agentkit/tool"
)

// Provider est un fournisseur de mémoire.
type Provider interface {
	Name() string
}

// Prompter ajoute un bloc au prompt système d'une nouvelle session.
type Prompter interface {
	Prompt(ctx context.Context, conv tool.Conversation) (string, error)
}

// Recaller renvoie des souvenirs pertinents pour un message.
type Recaller interface {
	Recall(ctx context.Context, conv tool.Conversation, query string) (string, error)
}

// ToolProvider fournit des outils.
type ToolProvider interface {
	Tools() []tool.Tool
}

// Syncer reçoit chaque tour terminé (messages ajoutés à l'historique).
type Syncer interface {
	Sync(ctx context.Context, conv tool.Conversation, msgs []llm.Message) error
}

// PreCompactor reçoit les messages sur le point d'être archivés.
type PreCompactor interface {
	PreCompact(ctx context.Context, conv tool.Conversation, msgs []llm.Message)
}

// Scope renvoie la clé de mémoire par défaut d'une conversation : sa
// session, ou celle de l'agent parent pour un sous-agent.
func Scope(conv tool.Conversation) string {
	if conv.Parent != "" {
		return conv.Parent
	}
	return conv.Session
}

// Config configure un Set.
type Config struct {
	// RecallTimeout borne le rappel d'un fournisseur à chaque tour (défaut 5 s) :
	// un fournisseur lent ne doit pas retarder la réponse.
	RecallTimeout time.Duration
	// SyncTimeout borne une synchronisation (défaut 2 min).
	SyncTimeout time.Duration
	Logger      *slog.Logger
}

// Set assemble des fournisseurs.
type Set struct {
	cfg       Config
	providers []Provider
	log       *slog.Logger
	wg        sync.WaitGroup
}

// New crée un Set.
func New(cfg Config, providers ...Provider) *Set {
	if cfg.RecallTimeout <= 0 {
		cfg.RecallTimeout = 5 * time.Second
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = 2 * time.Minute
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Set{cfg: cfg, providers: providers, log: log}
}

// Section est la section de prompt des fournisseurs Prompter.
func (s *Set) Section() prompt.Section {
	return func(ctx context.Context, conv tool.Conversation) (string, error) {
		var parts []string
		for _, p := range s.providers {
			pr, ok := p.(Prompter)
			if !ok {
				continue
			}
			out, err := pr.Prompt(ctx, conv)
			if err != nil {
				return "", err
			}
			if out = strings.TrimSpace(out); out != "" {
				parts = append(parts, out)
			}
		}
		return strings.Join(parts, "\n\n"), nil
	}
}

// Recall interroge les fournisseurs Recaller en parallèle (signature de
// agent.Config.Recall). Un fournisseur en erreur ou trop lent est ignoré.
func (s *Set) Recall(ctx context.Context, conv tool.Conversation, content string) (string, error) {
	var rs []Recaller
	for _, p := range s.providers {
		if r, ok := p.(Recaller); ok {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 || strings.TrimSpace(content) == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RecallTimeout)
	defer cancel()
	outs := make([]string, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.Recall(ctx, conv, content)
			if err != nil {
				s.log.Warn("memory : rappel impossible", "provider", r.(Provider).Name(), "err", err)
				return
			}
			outs[i] = strings.TrimSpace(out)
		}()
	}
	wg.Wait()
	var parts []string
	for _, o := range outs {
		if o != "" {
			parts = append(parts, o)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// AfterTurn transmet le tour aux fournisseurs Syncer, en arrière-plan
// (signature de agent.Config.AfterTurn). Wait attend leur fin.
func (s *Set) AfterTurn(ctx context.Context, conv tool.Conversation, out *agent.Outcome) {
	if out == nil || len(out.Saved) == 0 {
		return
	}
	msgs := append([]llm.Message(nil), out.Saved...)
	for _, p := range s.providers {
		sy, ok := p.(Syncer)
		if !ok {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SyncTimeout)
			defer cancel()
			if err := sy.Sync(sctx, conv, msgs); err != nil {
				s.log.Warn("memory : synchronisation impossible", "provider", p.Name(), "session", conv.Session, "err", err)
			}
		}()
	}
}

// OnCompact transmet les messages archivés aux fournisseurs PreCompactor
// (signature de agent.Compaction.OnCompact).
func (s *Set) OnCompact(ctx context.Context, conv tool.Conversation, msgs []llm.Message) {
	for _, p := range s.providers {
		if pc, ok := p.(PreCompactor); ok {
			pc.PreCompact(ctx, conv, msgs)
		}
	}
}

// Tools renvoie les outils des fournisseurs ToolProvider.
func (s *Set) Tools() []tool.Tool {
	var out []tool.Tool
	for _, p := range s.providers {
		if tp, ok := p.(ToolProvider); ok {
			out = append(out, tp.Tools()...)
		}
	}
	return out
}

// Wait attend la fin des synchronisations en cours (arrêt, tests).
func (s *Set) Wait() { s.wg.Wait() }
