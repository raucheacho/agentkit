package memory_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/memory"
	"github.com/raucheacho/agentkit/memory/notes"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
)

var conv = tool.Conversation{Session: "web:c1", Channel: "web", ChatID: "c1"}

type fakeProvider struct {
	name   string
	recall string
	err    error
	mu     sync.Mutex
	synced []llm.Message
	pre    int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Prompt(context.Context, tool.Conversation) (string, error) {
	return "bloc " + f.name, nil
}
func (f *fakeProvider) Recall(context.Context, tool.Conversation, string) (string, error) {
	return f.recall, f.err
}
func (f *fakeProvider) Sync(_ context.Context, _ tool.Conversation, msgs []llm.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = append(f.synced, msgs...)
	return nil
}
func (f *fakeProvider) PreCompact(context.Context, tool.Conversation, []llm.Message) { f.pre++ }

func TestSetLifecycle(t *testing.T) {
	a := &fakeProvider{name: "a", recall: "souvenir A"}
	b := &fakeProvider{name: "b", err: errors.New("panne")}
	set := memory.New(memory.Config{}, a, b)

	p, err := set.Section()(context.Background(), conv)
	if err != nil || p != "bloc a\n\nbloc b" {
		t.Fatalf("prompt : %q %v", p, err)
	}
	r, err := set.Recall(context.Background(), conv, "bonjour")
	if err != nil || r != "souvenir A" {
		t.Fatalf("rappel (le fournisseur en panne est ignoré) : %q %v", r, err)
	}
	set.AfterTurn(context.Background(), conv, &agent.Outcome{Saved: []llm.Message{{Role: llm.RoleUser, Content: "x"}}})
	set.Wait()
	if len(a.synced) != 1 || len(b.synced) != 1 {
		t.Fatal("synchronisation")
	}
	set.OnCompact(context.Background(), conv, nil)
	if a.pre != 1 {
		t.Fatal("pré-compression")
	}
}

// Embeddings factices : une dimension par thème.
var themes = []string{"cafe", "velo", "chat"}

func fakeEmbed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		v := make([]float32, len(themes))
		low := session.Fold(s)
		for j, th := range themes {
			if strings.Contains(low, th) {
				v[j] = 1
			}
		}
		if strings.Contains(low, "expresso") {
			v[0] = 1 // synonyme que le plein texte ne trouve pas
		}
		out[i] = v
	}
	return out, nil
}

func run(t *testing.T, tl tool.Tool, ctx context.Context, args string) string {
	t.Helper()
	call := tool.Call{Name: tl.Spec().Name, Raw: []byte(args)}
	res, err := tl.Run(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content
}

func byName(ts []tool.Tool, name string) tool.Tool {
	for _, t := range ts {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

func TestNotesProvider(t *testing.T) {
	store := notes.NewMemory()
	n := notes.New(notes.Config{Store: store, Embedder: llm.EmbedderFunc(fakeEmbed)})
	ctx := tool.WithConversation(context.Background(), conv)
	tools := n.Tools()

	if out := run(t, byName(tools, "memory_save"), ctx, `{"path":"Préférences/Café","content":"Aime le café serré."}`); !strings.Contains(out, "preferences/cafe") {
		t.Fatalf("enregistrement : %s", out)
	}
	run(t, byName(tools, "memory_save"), ctx, `{"path":"loisirs/velo","content":"Fait du vélo le dimanche."}`)
	if out := run(t, byName(tools, "memory_save"), ctx, `{"path":"sans-zone","content":"x"}`); !strings.HasPrefix(out, "Erreur") {
		t.Fatalf("chemin invalide accepté : %s", out)
	}
	if out := run(t, byName(tools, "memory_read"), ctx, `{"path":"preferences/cafe"}`); !strings.Contains(out, "café serré") {
		t.Fatalf("lecture : %s", out)
	}

	// Un sous-agent partage la mémoire de son parent.
	child := tool.WithConversation(context.Background(), tool.Conversation{Session: "delegate:x", Parent: "web:c1"})
	if out := run(t, byName(tools, "memory_read"), child, `{"path":"loisirs/velo"}`); !strings.Contains(out, "dimanche") {
		t.Fatalf("lecture depuis un sous-agent : %s", out)
	}
	// Une autre conversation ne voit rien.
	other := tool.WithConversation(context.Background(), tool.Conversation{Session: "web:c2"})
	if out := run(t, byName(tools, "memory_read"), other, `{"path":"loisirs/velo"}`); !strings.HasPrefix(out, "Aucune") {
		t.Fatalf("fuite entre conversations : %s", out)
	}

	p, _ := n.Prompt(context.Background(), conv)
	if !strings.Contains(p, "- loisirs/velo — Fait du vélo le dimanche.") || !strings.Contains(p, "- preferences/cafe") {
		t.Fatalf("index : %s", p)
	}
	// Hybride : « expresso » n'est dans aucune note, les vecteurs la trouvent.
	r, _ := n.Recall(context.Background(), conv, "Un expresso ?")
	if !strings.Contains(r, "preferences/cafe") || strings.Contains(r, "loisirs/velo") {
		t.Fatalf("rappel hybride : %s", r)
	}
	if out := run(t, byName(tools, "memory_delete"), ctx, `{"path":"loisirs/velo"}`); !strings.Contains(out, "supprimée") {
		t.Fatal(out)
	}
}

func TestHistoryTool(t *testing.T) {
	st := session.NewMemory()
	_ = st.Append(context.Background(), "web:c1", llm.Message{Role: llm.RoleUser, Content: "Ma commande 4512 est en retard"})
	_ = st.Append(context.Background(), "web:c2", llm.Message{Role: llm.RoleUser, Content: "commande 9999 d'un autre client"})
	h := memory.HistoryTool(memory.HistoryConfig{Searcher: st})
	ctx := tool.WithConversation(context.Background(), conv)
	res, err := h.Run(ctx, tool.Call{Raw: []byte(`{"query":"commande"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Untrusted || !strings.Contains(res.Content, "4512") || strings.Contains(res.Content, "9999") {
		t.Fatalf("recherche limitée à la conversation : %+v", res)
	}
}

// De bout en bout : l'agent reçoit le rappel dans sa note, l'index dans son
// prompt, et la mémoire est synchronisée après le tour.
func TestWithAgent(t *testing.T) {
	store := notes.NewMemory()
	n := notes.New(notes.Config{Store: store})
	_, _ = n.Save(context.Background(), "web:c1", "clients/ana", "Ana est allergique aux noix.")
	sync := &fakeProvider{name: "sync"}
	set := memory.New(memory.Config{}, n, sync)

	var req *llm.Request
	model := llm.ProviderFunc(func(_ context.Context, r *llm.Request) (*llm.Response, error) {
		req = r
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "noté"}}, nil
	})
	reg := tool.NewRegistry()
	_ = reg.Add(set.Tools()...)
	ag := agent.New(agent.Config{Provider: model, Tools: reg, NoTimeNote: true,
		System: set.Section(), Recall: set.Recall, AfterTurn: set.AfterTurn})
	if _, err := ag.Run(context.Background(), agent.Input{Conversation: conv, Content: "Un gâteau aux noix pour Ana ?"}); err != nil {
		t.Fatal(err)
	}
	set.Wait()
	if !strings.Contains(req.Messages[0].Content, "clients/ana") {
		t.Fatalf("index dans le prompt : %s", req.Messages[0].Content)
	}
	if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "allergique") {
		t.Fatalf("rappel dans la note : %s", last)
	}
	if len(sync.synced) != 2 {
		t.Fatalf("synchronisation : %+v", sync.synced)
	}
}
