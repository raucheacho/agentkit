package trust_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/runner"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
	"github.com/raucheacho/agentkit/trust/trusttest"
	"github.com/raucheacho/agentkit/turn"
)

func TestMemoryStore(t *testing.T) {
	trusttest.Run(t, func(*testing.T) trust.Store { return trust.NewMemoryStore() })
}

func TestRules(t *testing.T) {
	p := trust.Rules(nil,
		trust.Rule{Tools: []string{"shell"}, Channels: []string{"whatsapp"}, Decision: trust.Deny, Reason: "interdit aux clients"},
		trust.Rule{Effects: []tool.Effect{tool.Sensitive}, Sources: []string{"schedule"}, Decision: trust.Deny, Reason: "personne pour valider"},
	)
	check := func(name string, eff tool.Effect, source, ch string) trust.Verdict {
		return p.Check(context.Background(), trust.Action{
			Call:         tool.Call{Name: name, Spec: tool.Spec{Name: name, Effect: eff}},
			Conversation: tool.Conversation{Source: source, Channel: ch},
		})
	}
	cases := []struct {
		got  trust.Verdict
		want trust.Decision
	}{
		{check("shell", tool.Write, "user", "whatsapp"), trust.Deny},
		{check("shell", tool.Write, "user", "cli"), trust.Allow},
		{check("pay", tool.Sensitive, "schedule", "web"), trust.Deny},
		{check("pay", tool.Sensitive, "user", "web"), trust.Ask},
		{check("search", tool.Read, "user", "web"), trust.Allow},
	}
	for i, c := range cases {
		if c.got.Decision != c.want {
			t.Errorf("cas %d : %s, attendu %s", i, c.got.Decision, c.want)
		}
	}
}

type submitter struct {
	mu    sync.Mutex
	turns []*turn.Turn
}

func (s *submitter) Submit(_ context.Context, t *turn.Turn) error {
	s.mu.Lock()
	s.turns = append(s.turns, t)
	s.mu.Unlock()
	return nil
}

type bookIn struct {
	Date string `json:"date"`
}

func bookTool(runs *int, mu *sync.Mutex) tool.Tool {
	return tool.New("book", "Réserve.", func(ctx context.Context, in bookIn) (tool.Result, error) {
		mu.Lock()
		*runs++
		mu.Unlock()
		c, _ := tool.ConversationFrom(ctx)
		return tool.Text("RDV REF-42 le " + in.Date + " pour " + c.ChatID), nil
	}, tool.WithEffect(tool.Sensitive))
}

func call(m *trust.Manager, ctx context.Context, name, raw string, eff tool.Effect, run agent.ToolHandler) (tool.Result, error) {
	h := m.Middleware()(run)
	return h(ctx, tool.Call{ID: "c1", Name: name, Raw: []byte(raw), Spec: tool.Spec{Name: name, Effect: eff}})
}

func TestGateDecisions(t *testing.T) {
	sub := &submitter{}
	var changes []trust.Status
	m := trust.New(trust.Config{Store: trust.NewMemoryStore(), Resume: sub, Policy: trust.Rules(nil,
		trust.Rule{Tools: []string{"rm"}, Decision: trust.Deny, Reason: "dangereux"}),
		OnChange: func(_ context.Context, r *trust.Request) { changes = append(changes, r.Status) }})
	ctx := tool.WithConversation(context.Background(), tool.Conversation{Session: "s", Channel: "tg", ChatID: "1"})
	ran := 0
	run := func(context.Context, tool.Call) (tool.Result, error) { ran++; return tool.Text("fait"), nil }

	if res, _ := call(m, ctx, "search", `{}`, tool.Read, run); res.Content != "fait" || ran != 1 {
		t.Fatalf("lecture : %+v", res)
	}
	if res, _ := call(m, ctx, "rm", `{}`, tool.Write, run); !strings.Contains(res.Content, "dangereux") || ran != 1 || res.Wait != nil {
		t.Fatalf("refus : %+v", res)
	}
	res, _ := call(m, ctx, "book", `{"date":"lundi"}`, tool.Sensitive, run)
	if res.Wait == nil || res.Wait.Kind != "approval" || ran != 1 || !strings.Contains(res.Content, res.Wait.Ref) {
		t.Fatalf("demande : %+v", res)
	}
	again, _ := call(m, ctx, "book", `{"date":"lundi"}`, tool.Sensitive, run)
	other, _ := call(m, ctx, "book", `{"date":"mardi"}`, tool.Sensitive, run)
	if again.Wait.Ref != res.Wait.Ref || other.Wait.Ref == res.Wait.Ref {
		t.Fatal("même action : même demande ; autres arguments : nouvelle demande")
	}
	if len(changes) != 2 {
		t.Fatalf("notifications : %v", changes)
	}
}

// Une validation exécute l'action enregistrée une seule fois, même si deux
// personnes valident en même temps, puis relance la conversation.
func TestApproveRunsOnceAndResumes(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	reg := tool.NewRegistry()
	_ = reg.Add(bookTool(&runs, &mu))
	sub := &submitter{}
	m := trust.New(trust.Config{Store: trust.NewMemoryStore(), Tools: reg, Resume: sub})
	ctx := tool.WithConversation(context.Background(), tool.Conversation{Session: "tg:1", Channel: "tg", ChatID: "1", Source: "user"})
	res, _ := call(m, ctx, "book", `{"date":"lundi"}`, tool.Sensitive, nil)
	id := res.Wait.Ref

	var wg sync.WaitGroup
	var errs []error
	var emu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Approve(context.Background(), id, "patron"); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		}()
	}
	wg.Wait()
	if runs != 1 || len(errs) != 4 || !errors.Is(errs[0], trust.ErrNotPending) {
		t.Fatalf("runs=%d errs=%v", runs, errs)
	}
	r, _ := m.Store().Get(context.Background(), id)
	if r.Status != trust.Executed || r.Result != "RDV REF-42 le lundi pour 1" || r.DecidedBy != "patron" {
		t.Fatalf("demande : %+v", r)
	}
	if len(sub.turns) != 1 {
		t.Fatalf("reprises : %d", len(sub.turns))
	}
	rt := sub.turns[0]
	if rt.Source != turn.SourceResume || rt.Session != "tg:1" || rt.Channel != "tg" || !strings.Contains(rt.Content, "REF-42") || rt.Meta["request_id"] != id || rt.Meta[turn.MetaTools] != "none" {
		t.Fatalf("tour de reprise : %+v", rt)
	}
	if _, err := m.Deny(context.Background(), id, "patron", "trop tard"); !errors.Is(err, trust.ErrNotPending) {
		t.Fatalf("refus après validation : %v", err)
	}
}

func TestDenyExpireAndQuestion(t *testing.T) {
	sub := &submitter{}
	m := trust.New(trust.Config{Store: trust.NewMemoryStore(), Resume: sub, TTL: time.Hour})
	ctx := tool.WithConversation(context.Background(), tool.Conversation{Session: "s", Channel: "web", ChatID: "c"})
	a, _ := call(m, ctx, "pay", `{"eur":10}`, tool.Sensitive, nil)
	b, _ := call(m, ctx, "pay", `{"eur":20}`, tool.Sensitive, nil)

	if r, err := m.Deny(context.Background(), a.Wait.Ref, "patron", "budget"); err != nil || r.Status != trust.Denied || r.Result != "budget" {
		t.Fatalf("refus : %+v %v", r, err)
	}
	if n, _ := m.Expire(context.Background(), time.Now().Add(2*time.Hour)); n != 1 {
		t.Fatalf("expirées : %d", n)
	}
	if _, err := m.Approve(context.Background(), b.Wait.Ref, "patron"); !errors.Is(err, trust.ErrNotPending) {
		t.Fatalf("validation d'une demande expirée : %v", err)
	}

	ask := m.AskTool()
	res, err := ask.Run(ctx, tool.Call{Raw: []byte(`{"question":"Le cabinet est-il ouvert samedi ?"}`)})
	if err != nil || res.Wait == nil || res.Wait.Kind != "question" {
		t.Fatalf("question : %+v %v", res, err)
	}
	if _, err := m.Answer(context.Background(), res.Wait.Ref, "Paul", " "); err == nil {
		t.Fatal("réponse vide acceptée")
	}
	if _, err := m.Approve(context.Background(), res.Wait.Ref, "Paul"); err == nil {
		t.Fatal("une question ne se valide pas")
	}
	if r, err := m.Answer(context.Background(), res.Wait.Ref, "Paul", "Oui, de 9h à 12h."); err != nil || r.Status != trust.Answered {
		t.Fatalf("réponse : %+v %v", r, err)
	}
	if len(sub.turns) != 3 || sub.turns[2].Meta[turn.MetaTools] != "" {
		t.Fatalf("reprises : %+v", sub.turns)
	}
	for i, want := range []string{"refusée par patron (motif : budget)", "pas été décidée à temps", "Réponse de Paul"} {
		if !strings.Contains(sub.turns[i].Content, want) {
			t.Errorf("reprise %d : %q", i, sub.turns[i].Content)
		}
	}
}

// scénario complet : le client demande, la porte suspend le tour, le patron
// valide, l'action s'exécute et un tour de reprise prévient le client.
type outbox struct {
	mu   sync.Mutex
	sent []string
}

func (o *outbox) Send(_ context.Context, out runner.Outbound) error {
	o.mu.Lock()
	o.sent = append(o.sent, out.Text)
	o.mu.Unlock()
	return nil
}

func (o *outbox) texts() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sent...)
}

func TestEndToEndWithRunner(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	reg := tool.NewRegistry()
	_ = reg.Add(bookTool(&runs, &mu))

	model := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		reply := func(s string) (*llm.Response, error) {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: s}}, nil
		}
		switch {
		case last.Role == llm.RoleUser && strings.Contains(last.Content, "Suivi interne"):
			return reply("Bonne nouvelle : c'est confirmé, " + strings.SplitN(strings.SplitN(last.Content, "Résultat : ", 2)[1], "\n", 2)[0])
		case last.Role == llm.RoleUser && strings.Contains(last.Content, "Consigne interne"):
			return reply("Votre demande est transmise, je reviens vers vous.")
		case last.Role == llm.RoleUser:
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "book", Arguments: `{"date":"lundi"}`}}}}, nil
		}
		return reply("?")
	})

	m := trust.New(trust.Config{Store: trust.NewMemoryStore(), Tools: reg})
	ag := agent.New(agent.Config{Provider: model, Tools: reg, Sessions: session.NewMemory(), Middleware: []agent.ToolMiddleware{m.Middleware()}, NoTimeNote: true})
	ob := &outbox{}
	rn := runner.New(runner.Config{Agent: ag, Queue: turn.NewMemory(), Outbox: ob, Poll: 5 * time.Millisecond})
	m.Attach(rn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rn.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	first := &turn.Turn{Channel: "tg", ChatID: "7", Content: "Un rendez-vous lundi svp"}
	_ = rn.Submit(context.Background(), first)
	waitFor(t, func() bool { return len(ob.texts()) == 1 })
	if got, _ := rn.Queue().Get(context.Background(), first.ID); got.Status != turn.Waiting {
		t.Fatalf("premier tour : %s", got.Status)
	}
	if runs != 0 || ob.texts()[0] != "Votre demande est transmise, je reviens vers vous." {
		t.Fatalf("runs=%d livré=%v", runs, ob.texts())
	}
	pending, _ := m.Store().List(context.Background(), trust.RequestFilter{Status: []trust.Status{trust.Pending}})
	if len(pending) != 1 {
		t.Fatalf("demandes : %+v", pending)
	}
	if _, err := m.Approve(context.Background(), pending[0].ID, "patron"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(ob.texts()) == 2 })
	if runs != 1 || ob.texts()[1] != "Bonne nouvelle : c'est confirmé, RDV REF-42 le lundi pour 7" {
		t.Fatalf("runs=%d livré=%v", runs, ob.texts())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("délai dépassé")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
