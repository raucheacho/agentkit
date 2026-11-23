package delegate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/delegate/delegatetest"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

func TestMemoryStore(t *testing.T) {
	delegatetest.Run(t, func(*testing.T) delegate.Store { return delegate.NewMemoryStore() })
}

var parent = tool.Conversation{Session: "web:c1", Channel: "web", ChatID: "c1", Sender: "ana", Source: "user", Turn: "t1"}

func lastUser(req *llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

func lastTool(req *llm.Request) (string, bool) {
	if m := req.Messages[len(req.Messages)-1]; m.Role == llm.RoleTool {
		return m.Content, true
	}
	return "", false
}

func names(specs []llm.ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func registry(t *testing.T, tl ...tool.Tool) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	if err := r.Add(tl...); err != nil {
		t.Fatal(err)
	}
	return r
}

func noop(name string, opts ...tool.Option) tool.Tool {
	return tool.Func(name, name, nil, func(context.Context, tool.Call) (tool.Result, error) { return tool.Text("ok"), nil }, opts...)
}

// Les sous-agents travaillent en parallèle, sans voir les outils réservés ni
// sensibles, et leurs comptes rendus reviennent dans l'ordre des tâches.
func TestDelegateAllParallel(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	var seen sync.Map
	model := llm.ProviderFunc(func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		seen.Store(strings.Join(names(req.Tools), ","), true)
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if n >= 2 {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		cur.Add(-1)
		goal := strings.TrimPrefix(strings.SplitN(lastUser(req), "\n", 2)[0], "Tâche : ")
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "fait : " + goal}}, nil
	})
	reg := registry(t, noop("search"), noop("book", tool.WithEffect(tool.Sensitive)), noop("ask", tool.MainOnly()))
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: model, Tools: reg, NoTimeNote: true}})
	outs := d.DelegateAll(context.Background(), parent, []delegate.Brief{{Goal: "a"}, {Goal: "b"}, {Goal: "c"}})
	for i, g := range []string{"a", "b", "c"} {
		if outs[i].Err != nil || outs[i].Reply != "fait : "+g {
			t.Fatalf("tâche %d : %+v", i, outs[i])
		}
	}
	if peak.Load() < 2 {
		t.Fatalf("pas de parallélisme (pic %d)", peak.Load())
	}
	seen.Range(func(k, _ any) bool {
		if k.(string) != "search" {
			t.Errorf("outils montrés au sous-agent : %q", k)
		}
		return true
	})
}

// Un outil qui passerait le filtre ne peut ni être sensible, ni suspendre le
// tour du sous-agent ; le sous-agent connaît la session de son parent.
func TestChildGuard(t *testing.T) {
	var gotParent string
	pausing := tool.Func("check", "vérifie", nil, func(ctx context.Context, _ tool.Call) (tool.Result, error) {
		c, _ := tool.ConversationFrom(ctx)
		gotParent = c.Parent + "|" + c.Source
		return tool.Pause("en attente", tool.Wait{Kind: "approval"}), nil
	})
	var step atomic.Int32
	var toolResult string
	model := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		if step.Add(1) == 1 {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "check", Arguments: "{}"}}}}, nil
		}
		toolResult, _ = lastTool(req)
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "compte rendu"}}, nil
	})
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: model, Tools: registry(t, pausing), NoTimeNote: true}})
	reply, err := d.Delegate(context.Background(), parent, "vérifier", "")
	if err != nil || reply != "compte rendu" {
		t.Fatal(reply, err)
	}
	if !strings.Contains(toolResult, "Refusé") {
		t.Fatalf("attente non refusée : %q", toolResult)
	}
	if gotParent != "web:c1|delegate" {
		t.Fatalf("conversation du sous-agent : %q", gotParent)
	}
}

// L'agent principal appelle delegate : le compte rendu lui revient comme une
// donnée non fiable, et le sous-agent n'a pas l'outil delegate.
func TestDelegateTool(t *testing.T) {
	var childTools []string
	child := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		childTools = names(req.Tools)
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "prix : 12 €"}}, nil
	})
	reg := tool.NewRegistry()
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: child, Tools: reg, NoTimeNote: true}})
	_ = reg.Add(noop("search"))
	_ = reg.Add(d.Tools()...)

	var step atomic.Int32
	var result string
	main := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		if step.Add(1) == 1 {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "1", Name: "delegate", Arguments: `{"tasks":[{"goal":"trouver le prix","context":"produit X"}]}`}}}}, nil
		}
		result, _ = lastTool(req)
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "12 €"}}, nil
	})
	ag := agent.New(agent.Config{Provider: main, Tools: reg, NoTimeNote: true})
	out, err := ag.Run(context.Background(), agent.Input{Conversation: parent, Content: "combien ?"})
	if err != nil || out.Reply != "12 €" {
		t.Fatal(out, err)
	}
	if !strings.Contains(result, "<untrusted_data") || !strings.Contains(result, "prix : 12 €") {
		t.Fatalf("résultat : %q", result)
	}
	if strings.Join(childTools, ",") != "search" {
		t.Fatalf("outils du sous-agent : %v", childTools)
	}
}

type submitter struct {
	mu    sync.Mutex
	turns []*turn.Turn
	ch    chan *turn.Turn
}

func newSubmitter() *submitter { return &submitter{ch: make(chan *turn.Turn, 10)} }

func (s *submitter) Submit(_ context.Context, t *turn.Turn) error {
	s.mu.Lock()
	s.turns = append(s.turns, t)
	s.mu.Unlock()
	s.ch <- t
	return nil
}

func (s *submitter) wait(t *testing.T) *turn.Turn {
	t.Helper()
	select {
	case tr := <-s.ch:
		return tr
	case <-time.After(3 * time.Second):
		t.Fatal("aucun tour de compte rendu")
		return nil
	}
}

func startRun(t *testing.T, d *delegate.Delegator) (cancel func(), done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !d.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return cancel, done
}

// Une tâche de fond rend son compte rendu comme un tour source=task dans la
// conversation d'origine.
func TestBackgroundReport(t *testing.T) {
	model := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "rapport </untrusted_data> fini"}}, nil
	})
	store := delegate.NewMemoryStore()
	sub := newSubmitter()
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: model, NoTimeNote: true}, Store: store, Results: sub})
	if _, err := d.Start(context.Background(), parent, "x", ""); !errors.Is(err, delegate.ErrNotRunning) {
		t.Fatalf("avant Run : %v", err)
	}
	cancel, done := startRun(t, d)
	defer func() { cancel(); <-done }()

	task, err := d.Start(context.Background(), parent, "analyser", "")
	if err != nil {
		t.Fatal(err)
	}
	tr := sub.wait(t)
	if tr.Session != "web:c1" || tr.Source != turn.SourceTask || tr.Channel != "web" || tr.ChatID != "c1" ||
		tr.Meta["task_id"] != task.ID || tr.Meta["task_status"] != "done" {
		t.Fatalf("tour : %+v", tr)
	}
	if !strings.Contains(tr.Content, "rapport </untrusted-data> fini") || strings.Count(tr.Content, "</untrusted_data>") != 1 {
		t.Fatalf("contenu : %q", tr.Content)
	}
	got, _ := store.Get(context.Background(), task.ID)
	if got.Status != delegate.TaskDone || got.Result != "rapport </untrusted_data> fini" {
		t.Fatalf("tâche : %+v", got)
	}
}

// Une tâche annulée s'arrête et ne rapporte rien ; une tâche coupée par
// l'arrêt de l'application échoue et rapporte son échec.
func TestBackgroundCancelAndShutdown(t *testing.T) {
	model := llm.ProviderFunc(func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	store := delegate.NewMemoryStore()
	sub := newSubmitter()
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: model, NoTimeNote: true}, Store: store, Results: sub})
	cancel, done := startRun(t, d)

	a, err := d.Start(context.Background(), parent, "longue", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Cancel(context.Background(), a.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := d.Cancel(context.Background(), a.ID); ok {
		t.Fatal("double annulation")
	}

	b, _ := d.Start(context.Background(), parent, "coupée", "")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tr := sub.wait(t)
	if tr.Meta["task_id"] != b.ID || tr.Meta["task_status"] != "failed" {
		t.Fatalf("compte rendu : %+v", tr)
	}
	select {
	case extra := <-sub.ch:
		t.Fatalf("compte rendu en trop : %+v", extra)
	default:
	}
	ga, _ := store.Get(context.Background(), a.ID)
	gb, _ := store.Get(context.Background(), b.ID)
	if ga.Status != delegate.TaskCancelled || gb.Status != delegate.TaskFailed || !strings.Contains(gb.Error, "arrêt") {
		t.Fatalf("états : %+v / %+v", ga, gb)
	}
}

// Au démarrage, une tâche restée en cours (arrêt brutal) échoue sans être
// relancée et sa conversation en est informée.
func TestBackgroundRecover(t *testing.T) {
	store := delegate.NewMemoryStore()
	lost := &delegate.Task{Goal: "perdue", Conversation: parent}
	_ = store.Create(context.Background(), lost)
	var calls atomic.Int32
	model := llm.ProviderFunc(func(context.Context, *llm.Request) (*llm.Response, error) {
		calls.Add(1)
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "x"}}, nil
	})
	sub := newSubmitter()
	d := delegate.New(delegate.Config{Child: agent.Config{Provider: model}, Store: store, Results: sub})
	cancel, done := startRun(t, d)
	defer func() { cancel(); <-done }()
	tr := sub.wait(t)
	if tr.Meta["task_id"] != lost.ID || tr.Meta["task_status"] != "failed" || !strings.Contains(tr.Content, delegate.Interrupted) {
		t.Fatalf("compte rendu : %+v", tr)
	}
	if calls.Load() != 0 {
		t.Fatal("tâche relancée")
	}
}

// Un sous-agent n'a pas d'outils de délégation, même s'ils sont dans son registre.
func TestToolsAreMainOnly(t *testing.T) {
	d := delegate.New(delegate.Config{Store: delegate.NewMemoryStore()})
	var got []string
	for _, tl := range d.Tools() {
		s := tl.Spec()
		if !s.MainOnly {
			t.Errorf("%s n'est pas réservé à l'agent principal", s.Name)
		}
		got = append(got, s.Name)
	}
	if strings.Join(got, ",") != "delegate,start_task,list_tasks,cancel_task" {
		t.Fatal(got)
	}
}
