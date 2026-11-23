package kit

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/raucheacho/agentkit/channel"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
)

// fakeChannel est une messagerie en mémoire.
type fakeChannel struct {
	mu   sync.Mutex
	sink channel.Sink
	sent []string
	in   chan struct{}
}

func newFake() *fakeChannel { return &fakeChannel{in: make(chan struct{})} }

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Run(ctx context.Context, sink channel.Sink) error {
	f.mu.Lock()
	f.sink = sink
	f.mu.Unlock()
	close(f.in)
	<-ctx.Done()
	return nil
}
func (f *fakeChannel) Send(_ context.Context, out channel.Outgoing) error {
	f.mu.Lock()
	f.sent = append(f.sent, out.Text)
	f.mu.Unlock()
	return nil
}
func (f *fakeChannel) say(ctx context.Context, chat, text string) {
	<-f.in
	f.mu.Lock()
	sink := f.sink
	f.mu.Unlock()
	sink(ctx, channel.Message{Channel: "fake", ChatID: chat, Sender: chat, Text: text, ID: text, At: time.Now()})
}
func (f *fakeChannel) replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// model : « réserve » → appel de book ; résultat d'outil → résumé ; sinon écho.
var model = llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	msg := llm.Message{Role: llm.RoleAssistant}
	switch {
	case last.Role == llm.RoleTool:
		msg.Content = "fait : " + last.Content
	case strings.Contains(last.Content, "réserve") && len(req.Tools) > 0:
		msg.ToolCalls = []llm.ToolCall{{ID: "1", Name: "book", Arguments: `{"day":"lundi"}`}}
	default:
		msg.Content = "écho : " + last.Content[strings.LastIndex(last.Content, "\n")+1:]
	}
	return &llm.Response{Message: msg, Model: "m"}, nil
})

type bookIn struct {
	Day string `json:"day"`
}

func book(runs *int) tool.Tool {
	return tool.New("book", "Réserve.", func(_ context.Context, in bookIn) (tool.Result, error) {
		*runs++
		return tool.Text("réservé " + in.Day), nil
	}, tool.WithEffect(tool.Sensitive))
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("délai dépassé : %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func start(t *testing.T, cfg Config) (*Kit, context.CancelFunc) {
	t.Helper()
	k, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = k.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done; _ = k.Close() }
	t.Cleanup(stop)
	return k, stop
}

func TestKitEndToEnd(t *testing.T) {
	runs := 0
	fc := newFake()
	k, _ := start(t, Config{
		Provider: model, Instructions: "Tu es un test.", Channels: []channel.Adapter{fc},
		Tools: []tool.Tool{book(&runs)}, Trust: Trust{AskHuman: true},
		Memory: &Memory{}, Delegate: &Delegate{Background: true}, Schedule: &Schedule{AgentTools: true},
	})
	var names []string
	for _, s := range k.Tools.Specs(context.Background(), nil) {
		names = append(names, s.Name)
	}
	for _, want := range []string{"book", "ask_human", "memory_save", "history_search", "delegate", "start_task", "schedule_create"} {
		if !slices.Contains(names, want) {
			t.Errorf("outil %s absent : %v", want, names)
		}
	}

	ctx := context.Background()
	fc.say(ctx, "ana", "bonjour")
	waitFor(t, "écho", func() bool { return len(fc.replies()) == 1 })
	if fc.replies()[0] != "écho : bonjour" {
		t.Fatal(fc.replies())
	}

	// Action sensible : une demande, validée par l'API d'administration.
	fc.say(ctx, "ana", "réserve pour lundi")
	var pending []*trust.Request
	waitFor(t, "demande", func() bool {
		pending, _ = k.Requests.List(ctx, trust.RequestFilter{Status: []trust.Status{trust.Pending}})
		return len(pending) == 1
	})
	srv := httptest.NewServer(k.Admin("secret"))
	defer srv.Close()
	if resp, _ := http.Post(srv.URL+"/requests/"+pending[0].ID+"/approve", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sans jeton : %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/requests/"+pending[0].ID+"/approve", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("validation : %v %v", resp.Status, err)
	}
	waitFor(t, "compte rendu", func() bool { return len(fc.replies()) == 3 })
	if runs != 1 {
		t.Fatalf("exécutions : %d", runs)
	}
	resp, _ = http.DefaultClient.Do(req) // seconde validation
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("seconde validation : %d", resp.StatusCode)
	}

	// Coûts et événements.
	creq, _ := http.NewRequest(http.MethodGet, srv.URL+"/costs?token=secret", nil)
	cresp, _ := http.DefaultClient.Do(creq)
	var snap struct{ Total struct{ Calls int } }
	_ = json.NewDecoder(cresp.Body).Decode(&snap)
	if snap.Total.Calls < 3 {
		t.Fatalf("coûts : %+v", snap)
	}
}

// Avec une base, l'état survit à un redémarrage.
func TestKitDurable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "k.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fc := newFake()
	_, stop := start(t, Config{Provider: model, DB: db, Channels: []channel.Adapter{fc}, Memory: &Memory{}})
	fc.say(context.Background(), "ana", "premier")
	waitFor(t, "réponse", func() bool { return len(fc.replies()) == 1 })
	stop()

	fc2 := newFake()
	k2, _ := start(t, Config{Provider: model, DB: db, Channels: []channel.Adapter{fc2}, Memory: &Memory{}})
	sess, err := k2.Sessions.Load(context.Background(), "fake:ana", 0)
	if err != nil || len(sess.Messages) != 2 || sess.Messages[0].Content != "premier" {
		t.Fatalf("session relue : %+v %v", sess, err)
	}
}

func TestKitRequiresProvider(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("erreur attendue")
	}
}
