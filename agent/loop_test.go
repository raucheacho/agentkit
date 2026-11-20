package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/raucheacho/agentkit/bus"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tools"
)

// scripted : faux modèle qui joue une suite de réponses et note ce qu'il reçoit.
type scripted struct {
	mu        sync.Mutex
	replies   []*llm.Response
	calls     [][]llm.Message
	toolsSent [][]llm.Tool
}

func (s *scripted) Chat(_ context.Context, msgs []llm.Message, tl []llm.Tool) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]llm.Message(nil), msgs...))
	s.toolsSent = append(s.toolsSent, tl)
	if len(s.replies) == 0 {
		return nil, errors.New("plus de réponse")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}
func (s *scripted) ChatStream(context.Context, []llm.Message, []llm.Tool) (<-chan llm.StreamingResponse, error) {
	return nil, errors.New("non")
}
func (s *scripted) GetDefaultModel() string                      { return "test" }
func (s *scripted) SetModel(string)                              {}
func (s *scripted) ListModels(context.Context) ([]string, error) { return nil, nil }

type memStore struct {
	mu sync.Mutex
	m  map[string][]llm.Message
}

func (m *memStore) Get(k string) ([]llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Message(nil), m.m[k]...), nil
}
func (m *memStore) Save(k string, msgs []llm.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[k] = append([]llm.Message(nil), msgs...)
	return nil
}

type countTool struct {
	name string
	runs int
}

func (c *countTool) Name() string        { return c.name }
func (c *countTool) Description() string { return "test" }
func (c *countTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"date": map[string]interface{}{"type": "string", "description": "jour"},
	}, "required": []string{"date"}}
}
func (c *countTool) Execute(_ context.Context, p map[string]interface{}) (string, error) {
	c.runs++
	return "rendez-vous REF-42 le " + p["date"].(string), nil
}

func call(id, name, args string) llm.ToolCall {
	tc := llm.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func newTestAgent(t *testing.T, p llm.Provider, enabled func(string) bool, tl ...tools.Tool) (*Agent, *memStore) {
	t.Helper()
	mgr := tools.NewManager()
	for _, x := range tl {
		if err := mgr.Register(x); err != nil {
			t.Fatal(err)
		}
	}
	st := &memStore{m: map[string][]llm.Message{}}
	return New(Config{Provider: p, SessionStore: st, Tools: mgr, Workspace: t.TempDir(), MaxIter: 3, EnabledTools: enabled}), st
}

func send(t *testing.T, a *Agent, content, note string) string {
	t.Helper()
	out, err := a.ProcessMessage(context.Background(), &bus.InboundMessage{Channel: "web", ChatID: "c1", Content: content, Note: note})
	if err != nil {
		t.Fatal(err)
	}
	return out.Content
}

// Le tour suivant voit les appels d'outils et leurs résultats du tour précédent :
// le modèle sait ce qu'il a réellement fait (référence, date…).
func TestToolCallsArePersisted(t *testing.T) {
	book := &countTool{name: "book"}
	p := &scripted{replies: []*llm.Response{
		{ToolCalls: []llm.ToolCall{call("a1", "book", `{"date":"lundi"}`)}},
		{Content: "C'est noté."},
		{Content: "Votre référence est REF-42."},
	}}
	a, st := newTestAgent(t, p, nil, book)
	send(t, a, "Je veux lundi", "")
	send(t, a, "Ma référence ?", "")

	second := p.calls[2]
	var sawResult bool
	for _, m := range second {
		if m.Role == "tool" && strings.Contains(m.Content, "REF-42") && m.ToolCallID == "a1" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("le résultat d'outil du tour 1 manque au tour 2 : %+v", second)
	}
	h := st.m["web:c1"]
	roles := []string{}
	for _, m := range h {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,assistant,user,assistant" {
		t.Fatalf("historique inattendu : %s", got)
	}
}

// La note éphémère est vue par le modèle mais jamais enregistrée.
func TestNoteIsNotPersisted(t *testing.T) {
	p := &scripted{replies: []*llm.Response{{Content: "Bonjour"}}}
	a, st := newTestAgent(t, p, nil)
	send(t, a, "Salut", "[Note interne] mission en cours")

	last := p.calls[0][len(p.calls[0])-1]
	if !strings.Contains(last.Content, "mission en cours") || !strings.HasSuffix(last.Content, "Salut") {
		t.Fatalf("note absente de l'appel : %q", last.Content)
	}
	for _, m := range st.m["web:c1"] {
		if strings.Contains(m.Content, "mission") {
			t.Fatalf("note enregistrée dans l'historique : %+v", m)
		}
	}
}

// Un outil désactivé n'est jamais exécuté, même si le modèle l'appelle.
func TestDisabledToolIsNeverExecuted(t *testing.T) {
	book := &countTool{name: "book"}
	p := &scripted{replies: []*llm.Response{
		{ToolCalls: []llm.ToolCall{call("a1", "book", `{"date":"lundi"}`)}},
		{Content: "Je ne peux pas réserver."},
	}}
	a, _ := newTestAgent(t, p, func(string) bool { return false }, book)
	send(t, a, "Réserve", "")
	if book.runs != 0 {
		t.Fatal("outil désactivé exécuté")
	}
	res := p.calls[1][len(p.calls[1])-1]
	if res.Role != "tool" || !strings.Contains(res.Content, "n'existe pas ou n'est pas disponible") {
		t.Fatalf("résultat attendu : erreur d'outil, obtenu %+v", res)
	}
}

// Les variantes de nom courantes sont réparées ; un appel sans identifiant en reçoit un.
func TestToolNameRepairAndIDs(t *testing.T) {
	book := &countTool{name: "book"}
	p := &scripted{replies: []*llm.Response{
		{ToolCalls: []llm.ToolCall{call("", "functions.book<|channel|>commentary", `{"date":"mardi",}`)}},
		{Content: "Fait."},
	}}
	a, st := newTestAgent(t, p, nil, book)
	send(t, a, "Réserve mardi", "")
	if book.runs != 1 {
		t.Fatalf("outil exécuté %d fois, attendu 1", book.runs)
	}
	h := st.m["web:c1"]
	if h[1].ToolCalls[0].ID == "" || h[2].ToolCallID != h[1].ToolCalls[0].ID {
		t.Fatalf("appel et résultat non rattachés : %+v", h)
	}
}

// Un paramètre obligatoire manquant renvoie une erreur au modèle sans exécuter l'outil.
func TestMissingRequiredParam(t *testing.T) {
	book := &countTool{name: "book"}
	p := &scripted{replies: []*llm.Response{
		{ToolCalls: []llm.ToolCall{call("a1", "book", `{}`)}},
		{Content: "Quel jour ?"},
	}}
	a, _ := newTestAgent(t, p, nil, book)
	send(t, a, "Réserve", "")
	if book.runs != 0 {
		t.Fatal("outil exécuté sans paramètre obligatoire")
	}
}

// Budget épuisé : une réponse finale est demandée au modèle, sans outils.
func TestBudgetExhaustedAsksFinalAnswer(t *testing.T) {
	book := &countTool{name: "book"}
	loop := &llm.Response{ToolCalls: []llm.ToolCall{call("", "book", `{"date":"lundi"}`)}}
	p := &scripted{replies: []*llm.Response{loop, loop, loop, {Content: "J'ai fait la réservation, il reste à confirmer."}}}
	a, _ := newTestAgent(t, p, nil, book)
	got := send(t, a, "Réserve", "")
	if got != "J'ai fait la réservation, il reste à confirmer." {
		t.Fatalf("réponse : %q", got)
	}
	if p.toolsSent[3] != nil {
		t.Fatal("la réponse finale ne doit pas proposer d'outils")
	}
	last := p.calls[3][len(p.calls[3])-1]
	if last.Role != "user" || !strings.Contains(last.Content, "Consigne interne") {
		t.Fatalf("consigne finale absente : %+v", last)
	}
}

// Réponse vide : une nouvelle tentative, puis la réponse de secours si rien ne vient.
func TestEmptyReplies(t *testing.T) {
	p := &scripted{replies: []*llm.Response{{Content: ""}, {Content: "<think>hmm</think>Bonjour !"}}}
	a, _ := newTestAgent(t, p, nil)
	if got := send(t, a, "Salut", ""); got != "Bonjour !" {
		t.Fatalf("réponse : %q", got)
	}

	p = &scripted{replies: []*llm.Response{{Content: ""}, {Content: " "}, {Content: ""}}}
	a, st := newTestAgent(t, p, nil)
	if got := send(t, a, "Salut", ""); got != defaultFallbackReply {
		t.Fatalf("réponse : %q", got)
	}
	// Pas deux messages "user" d'affilée dans la demande finale.
	final := p.calls[2]
	if final[len(final)-2].Role == "user" {
		t.Fatal("deux messages user consécutifs")
	}
	if h := st.m["web:c1"]; len(h) != 2 {
		t.Fatalf("historique : %+v", h)
	}
}
