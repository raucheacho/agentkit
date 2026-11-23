// Package runner fait tourner un agent sur une file de tours.
//
// Il prend les tours prêts dans la file (un seul à la fois par session,
// plusieurs sessions en parallèle), les exécute avec l'agent, livre la
// réponse par l'Outbox et enregistre l'issue. Il gère l'arrêt d'un tour
// (Stop), l'absorption des messages arrivés pendant un tour (steering), la
// reprise en main par un humain et l'indicateur « en train d'écrire ».
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

// Outbound est une réponse à livrer.
type Outbound struct {
	Channel string
	ChatID  string
	Text    string
	Session string
	TurnID  string
	Source  turn.Source
}

// Outbox livre les réponses (en pratique, la passerelle de canaux).
type Outbox interface {
	Send(ctx context.Context, out Outbound) error
}

// Typer est implémenté par une Outbox capable d'afficher « en train d'écrire ».
type Typer interface {
	Typing(ctx context.Context, channel, chatID string)
}

// Holds dit si une session est reprise en main par un humain : l'agent n'y
// répond plus, les messages reçus sont seulement enregistrés.
type Holds interface {
	Held(session string) bool
}

// Config configure un runner.
type Config struct {
	Agent *agent.Agent
	Queue turn.Queue
	// Outbox livre les réponses (nil : rien n'est livré, l'issue reste dans la file).
	Outbox Outbox
	// Holds : reprises en main (nil : aucune).
	Holds Holds
	// Workers borne les tours exécutés en même temps (défaut 8).
	Workers int
	// Poll : intervalle de vérification de la file, pour les tours reportés
	// (défaut 1 s). Un Submit réveille le runner sans attendre.
	Poll time.Duration
	// TypingEvery : rafraîchissement de l'indicateur « en train d'écrire » (défaut 4 s).
	TypingEvery time.Duration
	// Prepare ajoute une note éphémère propre au tour (fiche du client,
	// mission en cours). Elle s'ajoute à Turn.Note.
	Prepare func(ctx context.Context, t *turn.Turn) string
	// FailureReply est envoyé à l'interlocuteur quand son message n'a pas pu
	// être traité ("" : rien n'est envoyé).
	FailureReply string
	// OnFinish est appelé à la fin de chaque tour (journal, tableau de bord).
	OnFinish func(ctx context.Context, t *turn.Turn, out *agent.Outcome, err error)
	Logger   *slog.Logger
}

type running struct {
	session string
	cancel  context.CancelFunc
}

// Runner exécute les tours d'une file.
type Runner struct {
	cfg  Config
	log  *slog.Logger
	wake chan struct{}

	mu      sync.Mutex
	running map[string]*running // par identifiant de tour
	wg      sync.WaitGroup
}

// New crée un runner.
func New(cfg Config) *Runner {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.TypingEvery <= 0 {
		cfg.TypingEvery = 4 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cfg: cfg, log: log, wake: make(chan struct{}, 1), running: map[string]*running{}}
}

// Agent renvoie l'agent du runner.
func (r *Runner) Agent() *agent.Agent { return r.cfg.Agent }

// Queue renvoie la file du runner.
func (r *Runner) Queue() turn.Queue { return r.cfg.Queue }

// Submit ajoute un tour à la file et réveille le runner.
func (r *Runner) Submit(ctx context.Context, t *turn.Turn) error {
	if err := r.cfg.Queue.Enqueue(ctx, t); err != nil {
		return err
	}
	r.Wake()
	return nil
}

// Wake demande au runner de regarder la file sans attendre.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run exécute les tours jusqu'à l'annulation de ctx, puis arrête les tours en
// cours et attend leur fin. Les tours restés « en cours » après un arrêt
// brutal précédent sont d'abord marqués en échec, jamais rejoués.
func (r *Runner) Run(ctx context.Context) error {
	if n, err := r.cfg.Queue.Recover(ctx); err != nil {
		return fmt.Errorf("runner : reprise de la file : %w", err)
	} else if n > 0 {
		r.log.Warn("runner : tours interrompus par l'arrêt précédent", "count", n)
	}
	tick := time.NewTicker(r.cfg.Poll)
	defer tick.Stop()
	for {
		r.dispatch(ctx)
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for _, run := range r.running {
				run.cancel()
			}
			r.mu.Unlock()
			r.wg.Wait()
			return nil
		case <-r.wake:
		case <-tick.C:
		}
	}
}

func (r *Runner) dispatch(ctx context.Context) {
	r.mu.Lock()
	free := r.cfg.Workers - len(r.running)
	r.mu.Unlock()
	if free <= 0 || ctx.Err() != nil {
		return
	}
	turns, err := r.cfg.Queue.Claim(ctx, time.Now().UTC(), free)
	if err != nil {
		r.log.Error("runner : lecture de la file", "err", err)
		return
	}
	for _, t := range turns {
		tctx, cancel := context.WithCancel(ctx)
		r.mu.Lock()
		r.running[t.ID] = &running{session: t.Session, cancel: cancel}
		r.mu.Unlock()
		r.wg.Add(1)
		go func(t *turn.Turn) {
			defer r.wg.Done()
			defer func() {
				cancel()
				r.mu.Lock()
				delete(r.running, t.ID)
				r.mu.Unlock()
				r.Wake() // le tour suivant de la session peut démarrer
			}()
			r.exec(tctx, t)
		}(t)
	}
}

// Stop arrête un tour : en cours, il est annulé (ce qu'il a déjà fait reste
// enregistré) ; en attente, il est retiré de la file.
func (r *Runner) Stop(ctx context.Context, turnID string) (bool, error) {
	r.mu.Lock()
	run, ok := r.running[turnID]
	r.mu.Unlock()
	if ok {
		run.cancel()
		return true, nil
	}
	return r.cfg.Queue.Cancel(ctx, turnID)
}

// StopSession arrête le tour en cours d'une session et annule ceux qui
// attendent. Renvoie le nombre de tours arrêtés ou annulés.
func (r *Runner) StopSession(ctx context.Context, session string) (int, error) {
	n := 0
	r.mu.Lock()
	for _, run := range r.running {
		if run.session == session {
			run.cancel()
			n++
		}
	}
	r.mu.Unlock()
	queued, err := r.cfg.Queue.List(ctx, turn.Filter{Session: session, Status: []turn.Status{turn.Queued}, Limit: 1000})
	if err != nil {
		return n, err
	}
	for _, t := range queued {
		if ok, err := r.cfg.Queue.Cancel(ctx, t.ID); err != nil {
			return n, err
		} else if ok {
			n++
		}
	}
	return n, nil
}

func (r *Runner) exec(ctx context.Context, t *turn.Turn) {
	finishCtx := context.WithoutCancel(ctx)
	if t.Source == turn.SourceUser && r.cfg.Holds != nil && r.cfg.Holds.Held(t.Session) {
		err := r.cfg.Agent.Record(finishCtx, t.Session, llm.Message{Role: llm.RoleUser, Content: t.Content})
		r.finish(finishCtx, t, turn.Held, "", errText(err), nil, err)
		return
	}

	stopTyping := r.typing(ctx, t)
	in := agent.Input{
		Conversation: tool.Conversation{Session: t.Session, Channel: t.Channel, ChatID: t.ChatID, Sender: t.Sender, Source: string(t.Source), Turn: t.ID},
		Content:      t.Content,
		Note:         t.Note,
		NoTools:      t.Meta[turn.MetaTools] == "none",
	}
	if r.cfg.Prepare != nil {
		if extra := strings.TrimSpace(r.cfg.Prepare(ctx, t)); extra != "" {
			in.Note = strings.TrimSpace(in.Note + "\n" + extra)
		}
	}
	if t.Source == turn.SourceUser && t.Channel != "" {
		in.Steer = func() []string {
			taken, err := r.cfg.Queue.TakeQueued(finishCtx, t.Session, t.Channel, t.ID)
			if err != nil {
				r.log.Warn("runner : absorption des messages en attente", "turn", t.ID, "err", err)
				return nil
			}
			out := make([]string, 0, len(taken))
			for _, x := range taken {
				out = append(out, x.Content)
			}
			return out
		}
	}

	out, err := r.cfg.Agent.Run(ctx, in)
	stopTyping()

	status, reply := turn.Done, ""
	switch {
	case err == nil:
		reply = out.Reply
		if out.Status == agent.Waiting {
			status = turn.Waiting
		}
	case errors.Is(err, context.Canceled):
		status = turn.Cancelled
	default:
		status = turn.Failed
		r.log.Error("runner : tour en échec", "turn", t.ID, "session", t.Session, "err", err)
		if t.Source == turn.SourceUser {
			reply = r.cfg.FailureReply
		}
	}

	errMsg := errText(err)
	if turn.Silent(t, reply) {
		r.log.Info("runner : le tour a choisi de ne rien dire", "turn", t.ID, "session", t.Session, "source", t.Source)
	} else if reply != "" && t.Channel != "" && r.cfg.Outbox != nil {
		if derr := r.cfg.Outbox.Send(finishCtx, Outbound{Channel: t.Channel, ChatID: t.ChatID, Text: reply, Session: t.Session, TurnID: t.ID, Source: t.Source}); derr != nil {
			r.log.Error("runner : livraison impossible", "turn", t.ID, "channel", t.Channel, "err", derr)
			errMsg = strings.TrimSpace(errMsg + " ; livraison : " + derr.Error())
		}
	}
	r.finish(finishCtx, t, status, reply, errMsg, out, err)
}

func (r *Runner) finish(ctx context.Context, t *turn.Turn, status turn.Status, reply, errMsg string, out *agent.Outcome, err error) {
	if ferr := r.cfg.Queue.Finish(ctx, t.ID, status, reply, errMsg); ferr != nil {
		r.log.Error("runner : enregistrement de l'issue", "turn", t.ID, "err", ferr)
	}
	t.Status, t.Reply, t.Error = status, reply, errMsg
	if r.cfg.OnFinish != nil {
		r.cfg.OnFinish(ctx, t, out, err)
	}
}

// typing affiche « en train d'écrire » pendant le tour, si le canal le permet.
func (r *Runner) typing(ctx context.Context, t *turn.Turn) func() {
	ty, ok := r.cfg.Outbox.(Typer)
	if !ok || t.Channel == "" || t.Source != turn.SourceUser {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		tick := time.NewTicker(r.cfg.TypingEvery)
		defer tick.Stop()
		for {
			ty.Typing(ctx, t.Channel, t.ChatID)
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
