// Package event réunit ce qui se passe dans un agent en un seul flux :
// appels au modèle, outils, tours, validations, sous-agents, planifications.
// Le flux alimente les traces (JSONL), les journaux, un tableau de bord en
// direct (SSE) et le comptage des coûts (Meter).
//
// Les branchements (Bus.Agent, Bus.Runner, Bus.Trust…) ont la signature des
// crochets de chaque package : on les passe tels quels dans leurs Config.
//
// Les événements contiennent des extraits de conversations : le flux SSE est
// à réserver aux administrateurs.
package event

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/trust"
	"github.com/raucheacho/agentkit/turn"
)

// Kind est le type d'un événement.
type Kind string

const (
	Model    Kind = "model"    // un appel au modèle a répondu
	Tool     Kind = "tool"     // un outil a été exécuté (ou refusé)
	Turn     Kind = "turn"     // l'agent a fini un tour
	Finished Kind = "finished" // le runner a clos un tour (statut final, livraison)
	Approval Kind = "approval" // une demande de validation ou une question a changé d'état
	Delegate Kind = "delegate" // un sous-agent a terminé
	Schedule Kind = "schedule" // une échéance planifiée a été prise
	Command  Kind = "command"  // une commande a tourné sur l'ordinateur de l'agent
	Custom   Kind = "custom"   // émis par l'application
)

// Event est un événement.
type Event struct {
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
	Kind Kind      `json:"kind"`

	Session string `json:"session,omitempty"`
	Channel string `json:"channel,omitempty"`
	ChatID  string `json:"chat_id,omitempty"`
	Source  string `json:"source,omitempty"`
	Turn    string `json:"turn,omitempty"`

	Model    string        `json:"model,omitempty"`
	Usage    llm.Usage     `json:"usage,omitzero"`
	Duration time.Duration `json:"duration,omitempty"`
	Tool     string        `json:"tool,omitempty"`
	Status   string        `json:"status,omitempty"`
	Error    string        `json:"error,omitempty"`
	// Text : extrait (réponse, résultat, demande), borné.
	Text string            `json:"text,omitempty"`
	Data map[string]string `json:"data,omitempty"`
}

// Config configure un Bus.
type Config struct {
	// Backlog : événements gardés pour le rattrapage (défaut 500).
	Backlog int
	// Buffer : file de chaque abonné (défaut 256) ; un abonné trop lent perd
	// des événements plutôt que de ralentir l'agent.
	Buffer int
	// TextChars borne Event.Text (défaut 300, < 0 : pas de texte).
	TextChars int
	Now       func() time.Time
}

type sub struct {
	ch     chan Event
	filter func(Event) bool
}

// Bus diffuse les événements.
type Bus struct {
	cfg     Config
	mu      sync.Mutex
	seq     uint64
	ring    []Event
	subs    map[*sub]struct{}
	sinks   []func(Event)
	dropped uint64
}

// NewBus crée un Bus.
func NewBus(cfg Config) *Bus {
	if cfg.Backlog <= 0 {
		cfg.Backlog = 500
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 256
	}
	if cfg.TextChars == 0 {
		cfg.TextChars = 300
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Bus{cfg: cfg, subs: map[*sub]struct{}{}}
}

// Attach ajoute un récepteur synchrone, appelé pour chaque événement dans
// l'ordre (comptage, trace) : il ne perd rien mais doit rendre la main vite,
// et ne doit pas publier lui-même.
func (b *Bus) Attach(sink func(Event)) {
	b.mu.Lock()
	b.sinks = append(b.sinks, sink)
	b.mu.Unlock()
}

// Publish numérote, date et diffuse un événement.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	b.seq++
	ev.Seq = b.seq
	if ev.At.IsZero() {
		ev.At = b.cfg.Now().UTC()
	}
	ev.Text = clip(ev.Text, b.cfg.TextChars)
	b.ring = append(b.ring, ev)
	if len(b.ring) > b.cfg.Backlog {
		b.ring = b.ring[len(b.ring)-b.cfg.Backlog:]
	}
	sinks := b.sinks
	for s := range b.subs {
		if s.filter != nil && !s.filter(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			b.dropped++
		}
	}
	// Sous le verrou : les récepteurs voient les événements dans l'ordre.
	for _, sink := range sinks {
		sink(ev)
	}
	b.mu.Unlock()
}

// Subscribe renvoie un canal d'événements (filter nil : tous) et la fonction
// qui s'en désabonne.
func (b *Bus) Subscribe(filter func(Event) bool) (<-chan Event, func()) {
	s := &sub{ch: make(chan Event, b.cfg.Buffer), filter: filter}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
		})
	}
}

// Since renvoie les événements gardés de numéro supérieur à seq.
func (b *Bus) Since(seq uint64, filter func(Event) bool) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Event
	for _, ev := range b.ring {
		if ev.Seq > seq && (filter == nil || filter(ev)) {
			out = append(out, ev)
		}
	}
	return out
}

// Dropped renvoie le nombre d'événements perdus par des abonnés trop lents.
func (b *Bus) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// --- branchements ---------------------------------------------------------

// Agent renvoie le crochet agent.Config.OnEvent.
func (b *Bus) Agent() func(ctx context.Context, ev agent.Event) {
	return func(_ context.Context, ev agent.Event) {
		c := ev.Conversation
		out := Event{Session: c.Session, Channel: c.Channel, ChatID: c.ChatID, Source: c.Source, Turn: c.Turn,
			Duration: ev.Duration, Usage: ev.Usage, Model: ev.Model}
		if ev.Err != nil {
			out.Error = ev.Err.Error()
		}
		switch ev.Kind {
		case agent.EventModel:
			out.Kind = Model
		case agent.EventTool:
			out.Kind = Tool
			if ev.Call != nil {
				out.Tool = ev.Call.Name
				out.Data = map[string]string{"effect": ev.Call.Spec.Effect.String()}
			}
			if ev.Result != nil {
				out.Text = ev.Result.Content
				if ev.Result.Wait != nil {
					out.Status = "waiting"
				}
			}
		case agent.EventTurn:
			out.Kind = Turn
			if ev.Outcome != nil {
				out.Status, out.Text = ev.Outcome.Status.String(), ev.Outcome.Reply
				out.Data = map[string]string{"steps": itoa(ev.Outcome.Steps)}
			}
		default:
			return
		}
		b.Publish(out)
	}
}

// Runner renvoie le crochet runner.Config.OnFinish.
func (b *Bus) Runner() func(ctx context.Context, t *turn.Turn, out *agent.Outcome, err error) {
	return func(_ context.Context, t *turn.Turn, out *agent.Outcome, err error) {
		ev := Event{Kind: Finished, Session: t.Session, Channel: t.Channel, ChatID: t.ChatID, Source: string(t.Source),
			Turn: t.ID, Status: string(t.Status), Text: t.Reply, Error: t.Error}
		if !t.StartedAt.IsZero() {
			ev.Duration = time.Since(t.StartedAt)
		}
		if out != nil {
			ev.Usage = out.Usage
		}
		if turn.Silent(t, t.Reply) {
			ev.Data = map[string]string{"silent": "true"}
		}
		b.Publish(ev)
	}
}

// Trust renvoie le crochet trust.Config.OnChange.
func (b *Bus) Trust() func(ctx context.Context, r *trust.Request) {
	return func(_ context.Context, r *trust.Request) {
		c := r.Conversation
		b.Publish(Event{Kind: Approval, Session: c.Session, Channel: c.Channel, ChatID: c.ChatID, Source: c.Source, Turn: c.Turn,
			Tool: r.Tool, Status: string(r.Status), Text: r.Summary,
			Data: map[string]string{"request_id": r.ID, "request_kind": string(r.Kind), "decided_by": r.DecidedBy}})
	}
}

// Delegate renvoie le crochet delegate.Config.OnFinish.
func (b *Bus) Delegate() func(ctx context.Context, r delegate.Report) {
	return func(_ context.Context, r delegate.Report) {
		c := r.Parent
		ev := Event{Kind: Delegate, Session: c.Session, Channel: c.Channel, ChatID: c.ChatID, Source: c.Source, Turn: c.Turn,
			Duration: r.Duration, Text: r.Goal, Status: "done", Data: map[string]string{"task_id": r.Task, "child": r.Child.Session}}
		if r.Outcome != nil {
			ev.Usage = r.Outcome.Usage
		}
		if r.Err != nil {
			ev.Status, ev.Error = "failed", r.Err.Error()
		}
		b.Publish(ev)
	}
}

// Schedule renvoie le crochet schedule.Config.OnRun.
func (b *Bus) Schedule() func(ctx context.Context, j *schedule.Job, slot time.Time, ran bool) {
	return func(_ context.Context, j *schedule.Job, slot time.Time, ran bool) {
		c := j.Conversation
		status := "ran"
		if !ran {
			status = "skipped"
		}
		b.Publish(Event{Kind: Schedule, Session: c.Session, Channel: c.Channel, ChatID: c.ChatID, Source: "schedule",
			Status: status, Text: j.Name, Data: map[string]string{"job_id": j.ID, "slot": slot.UTC().Format(time.RFC3339)}})
	}
}

// Computer renvoie le crochet computer.ToolsConfig.OnExec : l'historique des
// commandes (commande et fin de sortie déjà expurgées).
func (b *Bus) Computer() func(ctx context.Context, r computer.Record) {
	return func(_ context.Context, r computer.Record) {
		c := r.Conversation
		status := "exit " + itoa(r.ExitCode)
		switch {
		case r.Err != "":
			status = "failed"
		case r.TimedOut:
			status = "timeout"
		}
		b.Publish(Event{Kind: Command, Session: c.Session, Channel: c.Channel, ChatID: c.ChatID, Source: c.Source, Turn: c.Turn,
			Tool: "bash", Duration: r.Duration, Status: status, Error: r.Err, Text: r.Command,
			Data: map[string]string{"output": r.Output}})
	}
}

// --- récepteurs -----------------------------------------------------------

// JSONL renvoie un récepteur qui écrit chaque événement sur une ligne JSON
// (trace rejouable, à passer à Attach).
func JSONL(w io.Writer) func(Event) {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	return func(ev Event) {
		mu.Lock()
		_ = enc.Encode(ev)
		mu.Unlock()
	}
}

// Log renvoie un récepteur qui journalise les événements (sans leur texte).
func Log(log *slog.Logger) func(Event) {
	return func(ev Event) {
		attrs := []any{"kind", ev.Kind, "session", ev.Session}
		for _, kv := range [][2]string{{"source", ev.Source}, {"tool", ev.Tool}, {"status", ev.Status}, {"model", ev.Model}, {"error", ev.Error}} {
			if kv[1] != "" {
				attrs = append(attrs, kv[0], kv[1])
			}
		}
		if ev.Duration > 0 {
			attrs = append(attrs, "duration", ev.Duration.Round(time.Millisecond))
		}
		if ev.Usage.InputTokens+ev.Usage.OutputTokens > 0 {
			attrs = append(attrs, "in", ev.Usage.InputTokens, "out", ev.Usage.OutputTokens, "cached", ev.Usage.CachedTokens)
		}
		level := slog.LevelInfo
		if ev.Error != "" {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "événement", attrs...)
	}
}

func clip(s string, max int) string {
	if max < 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
