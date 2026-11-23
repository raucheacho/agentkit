package event

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tool"
)

func TestBusFanOutReplayAndDrops(t *testing.T) {
	b := NewBus(Config{Backlog: 3, Buffer: 1, TextChars: 5})
	all, cancelAll := b.Subscribe(nil)
	defer cancelAll()
	onlyS1, cancelS1 := b.Subscribe(func(e Event) bool { return e.Session == "s1" })
	var sunk []uint64
	b.Attach(func(e Event) { sunk = append(sunk, e.Seq) })

	b.Publish(Event{Kind: Custom, Session: "s1", Text: "abcdefgh"})
	b.Publish(Event{Kind: Custom, Session: "s2"})
	first := <-all
	if first.Seq != 1 || first.Text != "abcde…" || first.At.IsZero() {
		t.Fatalf("premier : %+v", first)
	}
	if e := <-onlyS1; e.Session != "s1" {
		t.Fatalf("filtre : %+v", e)
	}
	if b.Dropped() != 1 { // all n'a qu'une place : le 2e est perdu pour lui
		t.Fatalf("pertes : %d", b.Dropped())
	}
	cancelS1()
	cancelS1() // idempotent
	for range 3 {
		b.Publish(Event{Kind: Custom})
	}
	if got := b.Since(2, nil); len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("rattrapage borné : %+v", got)
	}
	if len(sunk) != 5 || sunk[4] != 5 {
		t.Fatalf("récepteur synchrone : %v", sunk)
	}
}

func TestMeter(t *testing.T) {
	m := NewMeter(map[string]Price{"gpt-4o": {Input: 2.5, CachedInput: 1.25, Output: 10}, "gpt-4o-mini": {Input: 0.15, Output: 0.6}})
	b := NewBus(Config{})
	b.Attach(m.Observe)
	b.Publish(Event{Kind: Model, Session: "a", Model: "gpt-4o-2024-08-06", Usage: llm.Usage{InputTokens: 1_000_000, CachedTokens: 400_000, OutputTokens: 100_000}})
	b.Publish(Event{Kind: Model, Session: "b", Model: "gpt-4o-mini", Usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}})
	b.Publish(Event{Kind: Model, Session: "b", Model: "llama3.1:8b", Usage: llm.Usage{InputTokens: 10}})
	b.Publish(Event{Kind: Tool, Session: "a"}) // ignoré

	// a : 600k × 2,5 + 400k × 1,25 + 100k × 10 = 1,5 + 0,5 + 1 = 3 $
	if got := m.Session("a"); math.Abs(got.Cost-3) > 1e-9 || got.Calls != 1 || got.CacheRate() != 0.4 {
		t.Fatalf("session a : %+v", got)
	}
	// b : 0,15 + 0,6 = 0,75 $ (le préfixe le plus long l'emporte), plus un appel sans tarif.
	if got := m.Session("b"); math.Abs(got.Cost-0.75) > 1e-9 || got.Calls != 2 || got.Unpriced != 1 {
		t.Fatalf("session b : %+v", got)
	}
	snap := m.Snapshot(1)
	if math.Abs(snap.Total.Cost-3.75) > 1e-9 || len(snap.Top) != 1 || snap.Top[0] != "a" || snap.Models["llama3.1:8b"].Unpriced != 1 {
		t.Fatalf("instantané : %+v", snap)
	}
}

// De bout en bout : les crochets de l'agent alimentent le bus.
func TestAgentHook(t *testing.T) {
	b := NewBus(Config{})
	var buf bytes.Buffer
	b.Attach(JSONL(&buf))
	reg := tool.NewRegistry()
	_ = reg.Add(tool.Func("lookup", "", nil, func(context.Context, tool.Call) (tool.Result, error) { return tool.External("prix : 12 €"), nil }))
	step := 0
	model := llm.ProviderFunc(func(context.Context, *llm.Request) (*llm.Response, error) {
		step++
		u := llm.Usage{InputTokens: 100, OutputTokens: 5, CachedTokens: 50}
		if step == 1 {
			return &llm.Response{Model: "m1", Usage: u, Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "lookup", Arguments: "{}"}}}}, nil
		}
		return &llm.Response{Model: "m1", Usage: u, Message: llm.Message{Role: llm.RoleAssistant, Content: "12 €"}}, nil
	})
	ag := agent.New(agent.Config{Provider: model, Tools: reg, NoTimeNote: true, OnEvent: b.Agent()})
	if _, err := ag.Run(context.Background(), agent.Input{Conversation: tool.Conversation{Session: "web:c1", Channel: "web", ChatID: "c1"}, Content: "prix ?"}); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	sc := bufio.NewScanner(&buf)
	var events []Event
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
		kinds = append(kinds, string(e.Kind))
	}
	if strings.Join(kinds, ",") != "model,tool,model,turn" {
		t.Fatalf("événements : %v", kinds)
	}
	if events[0].Model != "m1" || events[0].Usage.CachedTokens != 50 || events[1].Tool != "lookup" || events[1].Text != "prix : 12 €" ||
		events[3].Status != "done" || events[3].Text != "12 €" || events[3].Session != "web:c1" || events[3].Data["steps"] != "2" {
		t.Fatalf("contenu : %+v", events)
	}
}

func TestSSE(t *testing.T) {
	b := NewBus(Config{})
	b.Publish(Event{Kind: Custom, Session: "s1", Text: "avant"})
	b.Publish(Event{Kind: Custom, Session: "s2", Text: "autre"})
	srv := httptest.NewServer(Handler(b, HandlerConfig{Filter: QueryFilter}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "?session=s1&after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.Publish(Event{Kind: Custom, Session: "s2", Text: "filtré"})
		b.Publish(Event{Kind: Custom, Session: "s1", Text: "après"})
	}()
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() && len(got) < 2 {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var e Event
			_ = json.Unmarshal([]byte(d), &e)
			got = append(got, e.Text)
		}
	}
	if strings.Join(got, ",") != "avant,après" {
		t.Fatalf("flux : %v", got)
	}
}
