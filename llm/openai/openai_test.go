package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/llm"
)

func TestChatSendsRawSchemaAndParsesResponse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("clé absente")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"model":"m1","choices":[{"message":{"role":"assistant","content":null,
			"reasoning_content":"je réfléchis",
			"tool_calls":[{"id":"c1","type":"function","function":{"name":"book","arguments":"{\"slots\":[1,2]}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":8}}}`)
	}))
	defer srv.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"slots":{"type":"array","items":{"type":"integer"}}},"required":["slots"]}`)
	p := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m0"})
	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "salut"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x", Name: "book"}}},
			{Role: llm.RoleTool, ToolCallID: "x", Content: "ok"},
		},
		Tools: []llm.ToolSpec{{Name: "book", Description: "d", Parameters: schema}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tools := got["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	items := params["properties"].(map[string]any)["slots"].(map[string]any)["items"]
	if items == nil {
		t.Fatalf("schéma imbriqué perdu : %v", params)
	}
	msgs := got["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["content"] != nil {
		t.Fatalf("contenu assistant attendu nul : %v", asst["content"])
	}
	if args := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]; args != "{}" {
		t.Fatalf("arguments vides non normalisés : %v", args)
	}
	if got["model"] != "m0" {
		t.Fatalf("modèle : %v", got["model"])
	}

	if resp.Message.Reasoning != "je réfléchis" || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Arguments != `{"slots":[1,2]}` {
		t.Fatalf("réponse : %+v", resp.Message)
	}
	if resp.Usage.CachedTokens != 8 || resp.Usage.InputTokens != 10 {
		t.Fatalf("usage : %+v", resp.Usage)
	}
}

func TestStatusErrorKeepsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit reached"}}`)
	}))
	defer srv.Close()
	_, err := New(Config{Name: "groq", BaseURL: srv.URL}).Chat(context.Background(), &llm.Request{})
	var se *llm.StatusError
	if !errors.As(err, &se) || se.Status != 429 || se.Message != "rate limit reached" {
		t.Fatalf("erreur : %v", err)
	}
	if !llm.Transient(err) {
		t.Fatal("429 doit être transitoire")
	}
}

func TestStreamAssemblesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := []string{
			`{"choices":[{"delta":{"content":"Je "}}]}`,
			`{"choices":[{"delta":{"content":"cherche."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"sea","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"rch","arguments":"\"go\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := New(Config{BaseURL: srv.URL}).Stream(context.Background(), &llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var final *llm.Response
	for ev := range ch {
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		text.WriteString(ev.Delta)
		if ev.Response != nil {
			final = ev.Response
		}
	}
	if text.String() != "Je cherche." || final == nil {
		t.Fatalf("flux : %q %v", text.String(), final)
	}
	tc := final.Message.ToolCalls
	if len(tc) != 1 || tc[0].Name != "search" || tc[0].Arguments != `{"q":"go"}` || tc[0].ID != "c1" {
		t.Fatalf("appel assemblé : %+v", tc)
	}
	if final.Usage.InputTokens != 5 || final.FinishReason != "tool_calls" {
		t.Fatalf("final : %+v", final)
	}
}

// Gemini (API compatible OpenAI) : les appels arrivent entiers, sans index,
// avec une signature (extra_content) à renvoyer telle quelle avec l'appel.
func TestGeminiToolCallsRoundTrip(t *testing.T) {
	var second map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			_ = json.NewDecoder(r.Body).Decode(&second)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		for _, c := range []string{
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"SIG1"}},"function":{"arguments":"{\"q\":\"a\"}","name":"search"},"id":"c1","type":"function"}]},"index":0}]}`,
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"q\":\"b\"}","name":"search"},"id":"c2","type":"function"}]},"index":0}]}`,
			`{"choices":[{"delta":{"role":"assistant"},"finish_reason":"stop","index":0}]}`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), &llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var final *llm.Response
	for ev := range ch {
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		if ev.Response != nil {
			final = ev.Response
		}
	}
	tc := final.Message.ToolCalls
	if len(tc) != 2 || tc[0].ID != "c1" || tc[1].ID != "c2" || tc[0].Arguments != `{"q":"a"}` || tc[1].Arguments != `{"q":"b"}` {
		t.Fatalf("appels : %+v", tc)
	}
	if string(tc[0].Extra) != `{"google":{"thought_signature":"SIG1"}}` || tc[1].Extra != nil {
		t.Fatalf("signature : %s / %s", tc[0].Extra, tc[1].Extra)
	}

	// L'historique stocké (JSON) puis renvoyé garde la signature.
	b, _ := json.Marshal(final.Message)
	var stored llm.Message
	_ = json.Unmarshal(b, &stored)
	if _, err := p.Chat(context.Background(), &llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "cherche"}, stored,
		{Role: llm.RoleTool, ToolCallID: "c1", Content: "A"}, {Role: llm.RoleTool, ToolCallID: "c2", Content: "B"},
	}}); err != nil {
		t.Fatal(err)
	}
	sent := second["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)
	extra, _ := json.Marshal(sent[0].(map[string]any)["extra_content"])
	if string(extra) != `{"google":{"thought_signature":"SIG1"}}` {
		t.Fatalf("signature renvoyée : %s", extra)
	}
	if _, ok := sent[1].(map[string]any)["extra_content"]; ok {
		t.Fatal("pas de signature inventée")
	}
}

// Erreur Google : enveloppée dans un tableau, délai dans RetryInfo ; ailleurs,
// l'en-tête Retry-After.
func TestStatusErrorRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/h/") {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"message":"trop vite"}}`)
			return
		}
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `[{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.Help"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"41s"}]}}]`)
	}))
	defer srv.Close()
	var se *llm.StatusError
	_, err := New(Config{BaseURL: srv.URL}).Chat(context.Background(), &llm.Request{})
	if !errors.As(err, &se) || se.Status != 429 || se.Message != "Quota exceeded" || se.RetryAfter != 41*time.Second {
		t.Fatalf("Google : %#v", err)
	}
	_, err = New(Config{BaseURL: srv.URL + "/h"}).Chat(context.Background(), &llm.Request{})
	if !errors.As(err, &se) || se.Message != "trop vite" || se.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After : %#v", err)
	}
}
