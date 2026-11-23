package channel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/runner"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/turn"
)

type fake struct {
	name string
	max  int
	sink chan Sink
	mu   sync.Mutex
	sent []Outgoing
}

func newFake(name string, max int) *fake {
	return &fake{name: name, max: max, sink: make(chan Sink, 1)}
}

func (f *fake) Name() string   { return f.name }
func (f *fake) MaxLength() int { return f.max }
func (f *fake) Run(ctx context.Context, sink Sink) error {
	f.sink <- sink
	<-ctx.Done()
	return nil
}
func (f *fake) Send(_ context.Context, o Outgoing) error {
	f.mu.Lock()
	f.sent = append(f.sent, o)
	f.mu.Unlock()
	return nil
}
func (f *fake) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var t []string
	for _, o := range f.sent {
		t = append(t, o.Text)
	}
	return t
}

func echo() llm.Provider {
	return llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		last := req.Messages[len(req.Messages)-1].Content
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "écho: " + last}}, nil
	})
}

func wait(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("délai dépassé")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func setup(t *testing.T, cfg Config, adapters ...Adapter) (*Gateway, *runner.Runner, *session.Memory) {
	st := session.NewMemory()
	ag := agent.New(agent.Config{Provider: echo(), Sessions: st, NoTimeNote: true})
	gw := NewGateway(cfg, adapters...)
	r := runner.New(runner.Config{Agent: ag, Queue: turn.NewMemory(), Outbox: gw, Holds: gw, Poll: 5 * time.Millisecond})
	gw.Attach(r, ag)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = r.Run(ctx) }()
	go func() { defer wg.Done(); _ = gw.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return gw, r, st
}

func TestMessageBecomesTurnAndReplyIsDelivered(t *testing.T) {
	tg := newFake("telegram", 0)
	_, r, _ := setup(t, Config{Allow: func(m Message) bool { return m.Sender != "spam" }}, tg)
	sink := <-tg.sink
	ctx := context.Background()

	sink(ctx, Message{Channel: "telegram", ChatID: "1", Sender: "u1", Text: "bonjour", ID: "m1",
		Media: []Media{{Kind: "image", Name: "photo.jpg"}}})
	sink(ctx, Message{Channel: "telegram", ChatID: "1", Sender: "u1", Text: "bonjour", ID: "m1"}) // redélivré
	sink(ctx, Message{Channel: "telegram", ChatID: "2", Sender: "spam", Text: "achetez"})

	wait(t, func() bool { return len(tg.texts()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := tg.texts(); len(got) != 1 || got[0] != "écho: bonjour\n[Pièce jointe : image « photo.jpg »]" {
		t.Fatalf("livré : %q", got)
	}
	turns, _ := r.Queue().List(ctx, turn.Filter{})
	if len(turns) != 1 || turns[0].Meta["message_id"] != "m1" || turns[0].Session != "telegram:1" {
		t.Fatalf("tours : %+v", turns)
	}
}

func TestLongReplyIsSplit(t *testing.T) {
	wa := newFake("whatsapp", 40)
	gw, _, _ := setup(t, Config{}, wa)
	<-wa.sink
	text := strings.Repeat("mot ", 8) + "\n\n" + strings.Repeat("autre ", 8)
	if err := gw.SendText(context.Background(), "whatsapp", "1", text); err != nil {
		t.Fatal(err)
	}
	got := wa.texts()
	if len(got) < 2 {
		t.Fatalf("non découpé : %q", got)
	}
	for _, p := range got {
		if len([]rune(p)) > 40 || p == "" {
			t.Fatalf("morceau invalide : %q", p)
		}
	}
	if err := gw.SendText(context.Background(), "inconnu", "1", "x"); err == nil {
		t.Fatal("canal inconnu accepté")
	}
}

// Reprise en main : l'agent se tait, le message reçu et la réponse humaine
// entrent dans l'historique ; après Release l'agent répond de nouveau.
func TestTakeoverAndRelease(t *testing.T) {
	tg := newFake("telegram", 0)
	gw, r, st := setup(t, Config{}, tg)
	sink := <-tg.sink
	ctx := context.Background()

	if err := gw.Take(ctx, "telegram:1"); err != nil {
		t.Fatal(err)
	}
	sink(ctx, Message{Channel: "telegram", ChatID: "1", Text: "il y a quelqu'un ?"})
	wait(t, func() bool {
		ts, _ := r.Queue().List(ctx, turn.Filter{Status: []turn.Status{turn.Held}})
		return len(ts) == 1
	})
	if err := gw.Reply(ctx, "telegram:1", "telegram", "1", "Oui, c'est Paul."); err != nil {
		t.Fatal(err)
	}
	if got := tg.texts(); len(got) != 1 || got[0] != "Oui, c'est Paul." {
		t.Fatalf("livré : %q", got)
	}

	if err := gw.Release(ctx, "telegram:1"); err != nil {
		t.Fatal(err)
	}
	sink(ctx, Message{Channel: "telegram", ChatID: "1", Text: "merci"})
	wait(t, func() bool { return len(tg.texts()) == 2 })
	s, _ := st.Load(ctx, "telegram:1", 0)
	var roles []string
	for _, m := range s.Messages {
		roles = append(roles, string(m.Role)+":"+m.Content)
	}
	want := "user:il y a quelqu'un ?|assistant:Oui, c'est Paul.|user:merci|assistant:écho: merci"
	if strings.Join(roles, "|") != want {
		t.Fatalf("historique : %s", strings.Join(roles, "|"))
	}
}

func TestSplit(t *testing.T) {
	if got := Split("court", 10); len(got) != 1 {
		t.Fatal(got)
	}
	got := Split(strings.Repeat("é", 25), 10)
	if len(got) != 3 || got[0] != strings.Repeat("é", 10) {
		t.Fatalf("%q", got)
	}
}
