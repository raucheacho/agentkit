// Package schedule planifie des tâches : à chaque échéance, un tour
// source=schedule porte la consigne dans la conversation qui l'a créée, et
// la réponse de l'agent y est livrée.
//
// Invariants :
//   - une échéance s'exécute au plus une fois, même avec plusieurs processus
//     ou après un redémarrage (Store.Advance conditionnel) ; elle est
//     consommée avant que le tour soit soumis : en cas d'arrêt entre les
//     deux, elle est perdue plutôt que doublée ;
//   - une échéance manquée de plus de Grace (application arrêtée) est sautée,
//     pas rattrapée en rafale ;
//   - un tour planifié ne peut pas créer de planification, et un sous-agent
//     non plus (outils MainOnly).
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

// Submitter crée les tours planifiés (en pratique runner.Runner).
type Submitter interface {
	Submit(ctx context.Context, t *turn.Turn) error
}

// Texts regroupe les textes adressés au modèle. Les valeurs par défaut sont en français.
type Texts struct {
	// Turn est le message d'un tour planifié.
	Turn func(j *Job, silence string) string
}

// DefaultTexts renvoie les textes par défaut.
func DefaultTexts() Texts {
	return Texts{Turn: func(j *Job, silence string) string {
		s := "[Échéance de la tâche planifiée « " + j.Name + " » — ce message vient du planificateur, pas de ton " +
			"interlocuteur. La tâche est déjà planifiée : ne la replanifie pas.]\n" +
			"C'est le moment. Exécute maintenant cette consigne et adresse le résultat directement à ton interlocuteur :\n" + j.Task
		if j.Silent {
			s += "\nS'il n'y a rien d'utile à lui dire, réponds exactement " + silence + " et rien d'autre."
		}
		return s
	}}
}

// Config configure un Scheduler.
type Config struct {
	Store Store
	// Submit crée les tours (voir aussi Attach).
	Submit Submitter
	// Location : fuseau des expressions cron et des dates (défaut time.Local).
	Location *time.Location
	// Tick : fréquence de vérification des échéances (défaut 30 s).
	Tick time.Duration
	// Grace : retard au-delà duquel une échéance manquée est sautée (défaut 1 h).
	Grace time.Duration
	// MinInterval : intervalle minimal entre deux exécutions d'une tâche
	// créée par l'agent (défaut 5 min).
	MinInterval time.Duration
	// MaxPerSession borne les tâches d'une conversation (défaut 20).
	MaxPerSession int
	// Silence est le jeton de silence des tâches Silent (défaut "[SILENCE]").
	Silence string
	// Now remplace l'horloge (tests).
	Now func() time.Time
	// OnRun est appelé pour chaque échéance prise (ran : exécutée ou sautée).
	OnRun  func(ctx context.Context, j *Job, slot time.Time, ran bool)
	Texts  Texts
	Logger *slog.Logger
}

// Scheduler crée les tours planifiés.
type Scheduler struct {
	cfg    Config
	log    *slog.Logger
	mu     sync.Mutex
	submit Submitter
}

// New crée un Scheduler.
func New(cfg Config) *Scheduler {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 30 * time.Second
	}
	if cfg.Grace <= 0 {
		cfg.Grace = time.Hour
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = 5 * time.Minute
	}
	if cfg.MaxPerSession <= 0 {
		cfg.MaxPerSession = 20
	}
	if cfg.Silence == "" {
		cfg.Silence = "[SILENCE]"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Texts.Turn == nil {
		cfg.Texts.Turn = DefaultTexts().Turn
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{cfg: cfg, log: log, submit: cfg.Submit}
}

// Attach branche le Submitter après coup (le runner a besoin de l'agent, qui
// a besoin des outils de ce Scheduler).
func (s *Scheduler) Attach(sub Submitter) {
	s.mu.Lock()
	s.submit = sub
	s.mu.Unlock()
}

// Store renvoie le Store du Scheduler.
func (s *Scheduler) Store() Store { return s.cfg.Store }

// Add crée une tâche dans la conversation conv. MinInterval ne s'applique
// pas : c'est l'application qui planifie (voir Tools pour l'agent).
func (s *Scheduler) Add(ctx context.Context, conv tool.Conversation, name, when, task string, silent bool) (*Job, error) {
	return s.add(ctx, conv, name, when, task, silent, 0)
}

func (s *Scheduler) add(ctx context.Context, conv tool.Conversation, name, when, task string, silent bool, minGap time.Duration) (*Job, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, errors.New("consigne vide")
	}
	if conv.Session == "" {
		return nil, errors.New("conversation inconnue")
	}
	now := s.cfg.Now()
	spec, err := Parse(when, s.cfg.Location, now)
	if err != nil {
		return nil, err
	}
	next := spec.Next(now)
	if next.IsZero() {
		return nil, errors.New("cette date est déjà passée")
	}
	if minGap > 0 {
		if after := spec.Next(next); !after.IsZero() && after.Sub(next) < minGap {
			return nil, fmt.Errorf("trop fréquent : au moins %s entre deux exécutions", minGap)
		}
	}
	if name = strings.TrimSpace(name); name == "" {
		name = clip(task, 40)
	}
	j := &Job{Name: name, Spec: spec.String(), Task: task, Conversation: tool.Conversation{
		Session: conv.Session, Channel: conv.Channel, ChatID: conv.ChatID, Sender: conv.Sender,
	}, Silent: silent, Enabled: true, NextRun: next}
	if err := s.cfg.Store.Create(ctx, j); err != nil {
		return nil, err
	}
	return j, nil
}

// Pause suspend une tâche ; Resume la réactive à sa prochaine échéance.
func (s *Scheduler) Pause(ctx context.Context, id string) error {
	return s.cfg.Store.SetEnabled(ctx, id, false, time.Time{})
}

// Resume réactive une tâche à partir de maintenant.
func (s *Scheduler) Resume(ctx context.Context, id string) error {
	j, err := s.cfg.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return ErrNotFound
	}
	spec, err := Parse(j.Spec, s.cfg.Location, s.cfg.Now())
	if err != nil {
		return err
	}
	next := spec.Next(s.cfg.Now())
	if next.IsZero() {
		return errors.New("plus aucune échéance pour cette tâche")
	}
	return s.cfg.Store.SetEnabled(ctx, id, true, next)
}

// Run vérifie les échéances jusqu'à l'arrêt de ctx.
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("schedule : vérification des échéances", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick traite les échéances atteintes (appelé par Run ; exposé pour les tests
// et les applications qui ont leur propre horloge).
func (s *Scheduler) Tick(ctx context.Context) error {
	now := s.cfg.Now()
	due, err := s.cfg.Store.Due(ctx, now, 100)
	if err != nil {
		return err
	}
	for _, j := range due {
		s.fire(ctx, j, now)
	}
	return nil
}

func (s *Scheduler) fire(ctx context.Context, j *Job, now time.Time) {
	slot := j.NextRun
	spec, err := Parse(j.Spec, s.cfg.Location, j.CreatedAt)
	var next time.Time
	if err != nil {
		s.log.Error("schedule : planification illisible, tâche suspendue", "job", j.ID, "spec", j.Spec, "err", err)
	} else {
		// Prochaine échéance future : les échéances manquées ne s'empilent pas.
		for next = spec.Next(slot); !next.IsZero() && !next.After(now); next = spec.Next(next) {
		}
	}
	ran := err == nil && now.Sub(slot) <= s.cfg.Grace
	ok, aerr := s.cfg.Store.Advance(ctx, j.ID, slot, next, ran)
	if aerr != nil {
		s.log.Error("schedule : échéance non consommée", "job", j.ID, "err", aerr)
		return
	}
	if !ok {
		return // prise par un autre processus
	}
	if s.cfg.OnRun != nil {
		s.cfg.OnRun(ctx, j, slot, ran)
	}
	if !ran {
		if err == nil {
			s.log.Warn("schedule : échéance manquée, sautée", "job", j.ID, "slot", slot, "late", now.Sub(slot).Round(time.Second))
		}
		return
	}
	s.mu.Lock()
	sub := s.submit
	s.mu.Unlock()
	if sub == nil {
		s.log.Error("schedule : aucun Submitter, échéance perdue", "job", j.ID)
		return
	}
	c := j.Conversation
	tr := &turn.Turn{
		Session: c.Session, Source: turn.SourceSchedule, Channel: c.Channel, ChatID: c.ChatID, Sender: c.Sender,
		Content: s.cfg.Texts.Turn(j, s.cfg.Silence),
		Meta:    map[string]string{"job_id": j.ID, "slot": slot.UTC().Format(time.RFC3339)},
	}
	if j.Silent {
		tr.Meta[turn.MetaSilence] = s.cfg.Silence
	}
	if err := sub.Submit(ctx, tr); err != nil {
		s.log.Error("schedule : tour planifié non soumis", "job", j.ID, "err", err)
	}
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
