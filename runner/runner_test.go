package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

type outbox struct {
	mu     sync.Mutex
	sent   []Outbound
	typing int
	fail   error
}

func (o *outbox) Send(_ context.Context, out Outbound) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.sent = append(o.sent, out)
	return nil
}

func (o *outbox) Typing(context.Context, string, string) {
	o.mu.Lock()
	o.typing++
	o.mu.Unlock()
}

func (o *outbox) texts() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var t []string
	for _, s := range o.sent {
		t = append(t, s.Text)
	}
	return t
}

type holds map[string]bool

func (h holds) Held(s string) bool { return h[s] }

// echo répond "écho: <dernier message user>" ; gate bloque le premier appel
// jusqu'à ce qu'on le relâche.
type echo struct {
	mu    sync.Mutex
	gate  chan struct{}
	seen  []string
	fail  bool
	calls int
}

func (e *echo) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	e.mu.Lock()
	e.calls++
	first := e.calls == 1
	gate := e.gate
	last := req.Messages[len(req.Messages)-1]
	e.seen = append(e.seen, last.Content)
	fail := e.fail
	e.mu.Unlock()
	if first && gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("panne du modèle")
	}
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "fini"}}, nil
	}
	if strings.Contains(last.Content, "outil") {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "noop", Arguments: "{}"}}}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "écho: " + last.Content}}, nil
}

func setup(t *testing.T, p llm.Provider, h Holds) (*Runner, *outbox, *session.Memory, context.CancelFunc) {
	t.Helper()
	reg := tool.NewRegistry()
	_ = reg.Add(tool.Func("noop", "", nil, func(context.Context, tool.Call) (tool.Result, error) { return tool.Text("ok"), nil }))
	st := session.NewMemory()
	ag := agent.New(agent.Config{Provider: p, Tools: reg, Sessions: st, NoTimeNote: true})
	ob := &outbox{}
	r := New(Config{Agent: ag, Queue: turn.NewMemory(), Outbox: ob, Holds: h, Poll: 5 * time.Millisecond, TypingEvery: time.Millisecond, FailureReply: "Désolé, réessayez."})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r, ob, st, cancel
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("délai dépassé : %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func status(t *testing.T, r *Runner, id string) turn.Status {
	got, err := r.Queue().Get(context.Background(), id)
	if err != nil || got == nil {
		t.Fatalf("tour %s : %v", id, err)
	}
	return got.Status
}

func TestDeliversReplyAndFinishes(t *testing.T) {
	r, ob, _, _ := setup(t, &echo{}, nil)
	tr := &turn.Turn{Channel: "tg", ChatID: "1", Content: "salut"}
	if err := r.Submit(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fin du tour", func() bool { return status(t, r, tr.ID) == turn.Done })
	if got := ob.texts(); len(got) != 1 || got[0] != "écho: salut" {
		t.Fatalf("livré : %v", got)
	}
	ob.mu.Lock()
	typed := ob.typing
	ob.mu.Unlock()
	if typed == 0 {
		t.Fatal("indicateur de frappe non envoyé")
	}
}

// Les messages arrivés pendant un tour (après un appel d'outil) sont absorbés
// par ce tour : une seule réponse, qui en tient compte.
func TestSteeringAbsorbsQueuedMessages(t *testing.T) {
	p := &echo{gate: make(chan struct{})}
	r, ob, _, _ := setup(t, p, nil)
	ctx := context.Background()
	first := &turn.Turn{Channel: "tg", ChatID: "1", Content: "lance un outil"}
	_ = r.Submit(ctx, first)
	waitFor(t, "démarrage", func() bool { return status(t, r, first.ID) == turn.Running })
	second := &turn.Turn{Channel: "tg", ChatID: "1", Content: "et aussi ceci"}
	_ = r.Submit(ctx, second)
	close(p.gate)
	waitFor(t, "fin", func() bool { return status(t, r, first.ID) == turn.Done })
	if st := status(t, r, second.ID); st != turn.Merged {
		t.Fatalf("second tour : %s", st)
	}
	if got := ob.texts(); len(got) != 1 || got[0] != "écho: et aussi ceci" {
		t.Fatalf("livré : %v", got)
	}
}

// Stop annule le tour en cours ; la session continue avec le tour suivant.
func TestStopCancelsRunningTurn(t *testing.T) {
	p := &echo{gate: make(chan struct{})}
	r, ob, _, _ := setup(t, p, nil)
	ctx := context.Background()
	a := &turn.Turn{Session: "s", Channel: "tg", ChatID: "1", Content: "long", Source: turn.SourceSchedule}
	_ = r.Submit(ctx, a)
	waitFor(t, "démarrage", func() bool { return status(t, r, a.ID) == turn.Running })
	b := &turn.Turn{Session: "s", Channel: "tg", ChatID: "1", Content: "suivant", Source: turn.SourceSchedule}
	_ = r.Submit(ctx, b)
	if ok, _ := r.Stop(ctx, a.ID); !ok {
		t.Fatal("stop refusé")
	}
	waitFor(t, "annulation", func() bool { return status(t, r, a.ID) == turn.Cancelled })
	waitFor(t, "tour suivant", func() bool { return status(t, r, b.ID) == turn.Done })
	if got := ob.texts(); len(got) != 1 || got[0] != "écho: suivant" {
		t.Fatalf("livré : %v", got)
	}
}

// Pendant une reprise en main, un message est enregistré sans réponse de l'agent.
func TestHeldSessionRecordsWithoutReplying(t *testing.T) {
	p := &echo{}
	r, ob, st, _ := setup(t, p, holds{"tg:1": true})
	tr := &turn.Turn{Channel: "tg", ChatID: "1", Content: "je parle à un humain"}
	_ = r.Submit(context.Background(), tr)
	waitFor(t, "fin", func() bool { return status(t, r, tr.ID) == turn.Held })
	if len(ob.texts()) != 0 || p.calls != 0 {
		t.Fatalf("l'agent a répondu : %v", ob.texts())
	}
	s, _ := st.Load(context.Background(), "tg:1", 0)
	if len(s.Messages) != 1 || s.Messages[0].Content != "je parle à un humain" {
		t.Fatalf("historique : %+v", s.Messages)
	}
}

// Un tour en échec : l'interlocuteur reçoit FailureReply, l'erreur est gardée.
func TestFailureReply(t *testing.T) {
	r, ob, _, _ := setup(t, &echo{fail: true}, nil)
	tr := &turn.Turn{Channel: "tg", ChatID: "1", Content: "x"}
	_ = r.Submit(context.Background(), tr)
	waitFor(t, "échec", func() bool { return status(t, r, tr.ID) == turn.Failed })
	got, _ := r.Queue().Get(context.Background(), tr.ID)
	if !strings.Contains(got.Error, "panne du modèle") || ob.texts()[0] != "Désolé, réessayez." {
		t.Fatalf("tour : %+v livré : %v", got, ob.texts())
	}
}

// Une livraison impossible est notée sur le tour.
func TestDeliveryErrorIsRecorded(t *testing.T) {
	r, ob, _, _ := setup(t, &echo{}, nil)
	ob.fail = errors.New("bot bloqué")
	tr := &turn.Turn{Channel: "tg", ChatID: "1", Content: "x"}
	_ = r.Submit(context.Background(), tr)
	waitFor(t, "fin", func() bool { return status(t, r, tr.ID) == turn.Done })
	got, _ := r.Queue().Get(context.Background(), tr.ID)
	if !strings.Contains(got.Error, "livraison : bot bloqué") {
		t.Fatalf("erreur : %q", got.Error)
	}
}

// StopSession arrête le tour en cours et annule ceux qui attendent.
func TestStopSessionCancelsQueued(t *testing.T) {
	p := &echo{gate: make(chan struct{})}
	r, _, _, _ := setup(t, p, nil)
	ctx := context.Background()
	a := &turn.Turn{Session: "s", Channel: "x", ChatID: "1", Source: turn.SourceSchedule, Content: "a"}
	b := &turn.Turn{Session: "s", Channel: "x", ChatID: "1", Source: turn.SourceSchedule, Content: "b"}
	_ = r.Submit(ctx, a)
	waitFor(t, "démarrage", func() bool { return status(t, r, a.ID) == turn.Running })
	_ = r.Submit(ctx, b)
	n, err := r.StopSession(ctx, "s")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	waitFor(t, "annulation", func() bool { return status(t, r, a.ID) == turn.Cancelled })
	if status(t, r, b.ID) != turn.Cancelled {
		t.Fatal("tour en attente non annulé")
	}
}

// Un tour proactif qui répond par son jeton de silence ne livre rien.
func TestSilentTurnDeliversNothing(t *testing.T) {
	r, ob, _, _ := setup(t, &echo{}, nil)
	quiet := &turn.Turn{Channel: "tg", ChatID: "1", Source: turn.SourceEvent, Content: "[SILENCE]", Meta: map[string]string{turn.MetaSilence: "[SILENCE]"}}
	loud := &turn.Turn{Channel: "tg", ChatID: "2", Source: turn.SourceEvent, Content: "alerte", Meta: map[string]string{turn.MetaSilence: "[SILENCE]"}}
	_ = r.Submit(context.Background(), quiet)
	_ = r.Submit(context.Background(), loud)
	waitFor(t, "fin des tours", func() bool { return status(t, r, quiet.ID) == turn.Done && status(t, r, loud.ID) == turn.Done })
	if got := ob.texts(); len(got) != 1 || got[0] != "écho: alerte" {
		t.Fatalf("livré : %v", got)
	}
}
