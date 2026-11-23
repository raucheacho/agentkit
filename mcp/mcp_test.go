package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/tool"
)

// fake est un faux serveur MCP : outils echo (lecture seule), delete
// (destructif), slow (attend l'annulation), fail (isError) ; deux pages.
type fake struct {
	mu          sync.Mutex
	initialized bool
	cancelled   int
}

func (f *fake) handle(m message) *message {
	out := &message{JSONRPC: "2.0", ID: m.ID}
	switch m.Method {
	case "initialize":
		out.Result = json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1.0"},"instructions":"Sers-toi de echo."}`)
	case "notifications/initialized":
		f.mu.Lock()
		f.initialized = true
		f.mu.Unlock()
		return nil
	case "notifications/cancelled":
		f.mu.Lock()
		f.cancelled++
		f.mu.Unlock()
		return nil
	case "tools/list":
		var p struct{ Cursor string }
		_ = json.Unmarshal(m.Params, &p)
		if p.Cursor == "" {
			out.Result = json.RawMessage(`{"tools":[
				{"name":"echo","description":"Répète.","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]},"annotations":{"readOnlyHint":true}},
				{"name":"delete.all","description":"Efface tout.","inputSchema":{"type":"object"},"annotations":{"destructiveHint":true}}
			],"nextCursor":"p2"}`)
		} else {
			out.Result = json.RawMessage(`{"tools":[{"name":"slow","inputSchema":{"type":"object"}},{"name":"fail","description":"Échoue.","inputSchema":null}]}`)
		}
	case "tools/call":
		var p struct {
			Name      string
			Arguments map[string]any
		}
		_ = json.Unmarshal(m.Params, &p)
		switch p.Name {
		case "echo":
			b, _ := json.Marshal(fmt.Sprint(p.Arguments["text"]))
			out.Result = json.RawMessage(`{"content":[{"type":"text","text":` + string(b) + `},{"type":"image","mimeType":"image/png","data":"AA=="}]}`)
		case "fail":
			out.Result = json.RawMessage(`{"content":[{"type":"text","text":"quota dépassé"}],"isError":true}`)
		case "slow":
			time.Sleep(300 * time.Millisecond)
			out.Result = json.RawMessage(`{"content":[]}`)
		default:
			out.Error = &RPCError{Code: -32602, Message: "outil inconnu"}
		}
	default:
		if len(m.ID) == 0 {
			return nil
		}
		out.Error = &RPCError{Code: -32601, Message: "méthode inconnue"}
	}
	return out
}

// Le binaire de test sert de serveur stdio quand MCP_FAKE_SERVER=1.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_SERVER") == "1" {
		f := &fake{}
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		w := bufio.NewWriter(os.Stdout)
		// Une requête du serveur au client, à laquelle il doit répondre.
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":"srv-1","method":"ping"}`)
		w.Flush()
		fmt.Fprintln(os.Stderr, "serveur prêt")
		for sc.Scan() {
			var msg message
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" {
				continue // réponses du client (ping)
			}
			if out := f.handle(msg); out != nil {
				b, _ := json.Marshal(out)
				w.Write(append(b, '\n'))
				w.Flush()
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func checkClient(t *testing.T, c *Client) {
	t.Helper()
	if c.Instructions() != "Sers-toi de echo." || len(c.Infos()) != 4 {
		t.Fatalf("initialisation : %q %d outils", c.Instructions(), len(c.Infos()))
	}
	set := c.Toolset()
	specs := map[string]tool.Spec{}
	for _, tl := range set.Tools {
		specs[tl.Spec().Name] = tl.Spec()
	}
	if len(specs) != 3 { // "slow" exclu
		t.Fatalf("outils : %v", specs)
	}
	if specs["fake_echo"].Effect != tool.Write || specs["fake_delete_all"].Effect != tool.Sensitive || specs["fake_fail"].Effect != tool.Write {
		t.Fatalf("effets : %+v", specs)
	}
	reg := tool.NewRegistry()
	if err := reg.AddSet(set); err != nil {
		t.Fatal(err)
	}
	echo, _, _ := reg.Get("fake_echo")
	res, err := echo.Run(context.Background(), tool.Call{Raw: json.RawMessage(`{"text":"bonjour"}`)})
	if err != nil || !res.Untrusted || res.Content != "bonjour\n[image image/png omis]" {
		t.Fatalf("echo : %+v %v", res, err)
	}
	fail, _, _ := reg.Get("fake_fail")
	if res, _ := fail.Run(context.Background(), tool.Call{}); !strings.HasPrefix(res.Content, "Erreur renvoyée par l'outil : quota") {
		t.Fatalf("isError : %+v", res)
	}
	if !set.Available(context.Background()) {
		t.Fatal("groupe indisponible")
	}
}

func TestStdio(t *testing.T) {
	exe, _ := os.Executable()
	c, err := Connect(context.Background(), Config{Name: "fake", Command: exe, Env: []string{"MCP_FAKE_SERVER=1"}, Exclude: []string{"slow"}})
	if err != nil {
		t.Fatal(err)
	}
	checkClient(t, c)

	// Annulation : le serveur en est informé.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "slow", nil); err == nil {
		t.Fatal("annulation attendue")
	}
	// La connexion reste utilisable après l'annulation.
	if res, err := c.Call(context.Background(), "echo", json.RawMessage(`{"text":"encore"}`)); err != nil || !strings.HasPrefix(res.Content, "encore") {
		t.Fatalf("après annulation : %+v %v", res, err)
	}
	_ = c.Close()
	if c.Alive() || c.Toolset().Available(context.Background()) {
		t.Fatal("encore vivant après Close")
	}
	if _, err := c.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("appel après Close")
	}
}

func httpServer(t *testing.T, sse bool) (*httptest.Server, *fake) {
	f := &fake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "interdit", http.StatusUnauthorized)
			return
		}
		var m message
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m.Method != "initialize" && m.Method != "" {
			if r.Header.Get("Mcp-Session-Id") != "sess-1" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				http.Error(w, "session manquante", http.StatusBadRequest)
				return
			}
		}
		out := f.handle(m)
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
		}
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		b, _ := json.Marshal(out)
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestHTTP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint("sse=", sse), func(t *testing.T) {
			srv, f := httpServer(t, sse)
			c, err := Connect(context.Background(), Config{Name: "fake", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer secret"}, Exclude: []string{"slow"}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			f.mu.Lock()
			inited := f.initialized
			f.mu.Unlock()
			if !inited {
				t.Fatal("notifications/initialized non reçue")
			}
			checkClient(t, c)
		})
	}
	srv, _ := httpServer(t, false)
	if _, err := Connect(context.Background(), Config{Name: "fake", URL: srv.URL}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("sans authentification : %v", err)
	}
}

func TestEffects(t *testing.T) {
	srv, _ := httpServer(t, false)
	c, err := Connect(context.Background(), Config{Name: "fake", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer secret"},
		TrustAnnotations: true, Effects: map[string]tool.Effect{"fail": tool.Read}, Prefix: "-", Include: []string{"echo", "fail"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := map[string]tool.Effect{}
	for _, tl := range c.Toolset().Tools {
		got[tl.Spec().Name] = tl.Spec().Effect
	}
	if len(got) != 2 || got["echo"] != tool.Read || got["fail"] != tool.Read {
		t.Fatalf("effets : %v", got)
	}
}

// De bout en bout : l'agent appelle un outil MCP, le résultat lui revient
// comme une donnée non fiable.
func TestWithAgent(t *testing.T) {
	srv, _ := httpServer(t, true)
	c, err := Connect(context.Background(), Config{Name: "fake", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reg := tool.NewRegistry()
	_ = reg.AddSet(c.Toolset())
	step := 0
	var result string
	model := llm.ProviderFunc(func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		step++
		if step == 1 {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "fake_echo", Arguments: `{"text":"ping"}`}}}}, nil
		}
		result = req.Messages[len(req.Messages)-1].Content
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
	})
	ag := agent.New(agent.Config{Provider: model, Tools: reg, NoTimeNote: true})
	if _, err := ag.Run(context.Background(), agent.Input{Conversation: tool.Conversation{Session: "s"}, Content: "go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `<untrusted_data source="fake_echo">`) || !strings.Contains(result, "ping") {
		t.Fatalf("résultat : %s", result)
	}
}
