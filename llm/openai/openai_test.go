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
