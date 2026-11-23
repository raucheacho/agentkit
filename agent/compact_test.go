package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
)

// Au-delà du seuil, le début de l'historique est résumé puis archivé ; le
// résumé suit le prompt système, la fin reste intacte et commence par un
// message user.
func TestCompaction(t *testing.T) {
	p := &scripted{replies: []llm.Message{text("r1"), text("r2"), text("r3"), text("RÉSUMÉ-1"), text("r4")}}
	var compacted []llm.Message
	a, st := newAgent(t, p, func(c *Config) {
		c.Compaction = &Compaction{MaxMessages: 5, Keep: 9, // ramené à 2
			OnCompact: func(_ context.Context, _ tool.Conversation, msgs []llm.Message) { compacted = msgs }}
	})
	for _, m := range []string{"un", "deux", "trois"} {
		send(t, a, m, "")
	}
	// 6 messages actifs > 5 : compression avant le 4e tour.
	send(t, a, "quatre", "")

	sum := p.reqs[3]
	if sum.Messages[0].Content != a.cfg.Texts.Summarize || !strings.Contains(sum.Messages[1].Content, "user : un") || !strings.HasPrefix(sum.Messages[1].Content, a.cfg.Texts.Summarize) || sum.Tools != nil {
		t.Fatalf("requête de résumé : %+v", sum.Messages)
	}
	if len(compacted) != 4 || compacted[0].Content != "un" {
		t.Fatalf("OnCompact : %+v", compacted)
	}
	last := p.reqs[4]
	if !strings.HasSuffix(last.Messages[0].Content, a.cfg.Texts.SummaryHeading+"\n\nRÉSUMÉ-1") {
		t.Fatalf("prompt après compression : %q", last.Messages[0].Content)
	}
	if len(last.Messages) != 4 || last.Messages[1].Content != "trois" || last.Messages[3].Content != "quatre" {
		t.Fatalf("historique envoyé : %+v", last.Messages)
	}
	sess, _ := st.Load(context.Background(), "web:c1", 0)
	if sess.Summary != "RÉSUMÉ-1" || len(sess.Messages) != 4 {
		t.Fatalf("session : %+v", sess)
	}
	// Les archives restent consultables.
	hits, _ := st.Search(context.Background(), sessionQuery("deux"))
	if len(hits) != 1 {
		t.Fatalf("recherche dans les archives : %+v", hits)
	}
}

// Un échec du résumé laisse l'historique intact.
func TestCompactionFailureKeepsHistory(t *testing.T) {
	p := &scripted{replies: []llm.Message{text("r1"), text("r2"), text("r3"), text(""), text("r4")}}
	a, st := newAgent(t, p, func(c *Config) { c.Compaction = &Compaction{MaxMessages: 5, Keep: 2} })
	if a.cfg.Compaction.Keep != 2 {
		t.Fatal(a.cfg.Compaction.Keep)
	}
	for _, m := range []string{"un", "deux", "trois", "quatre"} {
		send(t, a, m, "")
	}
	sess, _ := st.Load(context.Background(), "web:c1", 0)
	if sess.Summary != "" || len(sess.Messages) != 8 {
		t.Fatalf("session : %+v", sess)
	}
}

// Recall complète la note éphémère, jamais enregistrée ; AfterTurn reçoit le tour.
func TestRecallAndAfterTurn(t *testing.T) {
	p := &scripted{replies: []llm.Message{text("ok")}}
	var after *Outcome
	a, st := newAgent(t, p, func(c *Config) {
		c.Recall = func(_ context.Context, _ tool.Conversation, content string) (string, error) {
			return "Souvenir lié à : " + content, nil
		}
		c.AfterTurn = func(_ context.Context, _ tool.Conversation, out *Outcome) { after = out }
	})
	send(t, a, "bonjour", "NOTE")
	got := p.reqs[0].Messages[len(p.reqs[0].Messages)-1].Content
	if got != "NOTE\n\nSouvenir lié à : bonjour\n\nbonjour" {
		t.Fatalf("message envoyé : %q", got)
	}
	sess, _ := st.Load(context.Background(), "web:c1", 0)
	if sess.Messages[0].Content != "bonjour" {
		t.Fatalf("enregistré : %q", sess.Messages[0].Content)
	}
	if after == nil || after.Reply != "ok" {
		t.Fatalf("AfterTurn : %+v", after)
	}
}

func sessionQuery(q string) session.Query { return session.Query{Text: q} }
