package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/session/sessiontest"
	_ "modernc.org/sqlite"
)

func TestStoreAppendOnly(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}

	empty, err := s.Load(ctx, "k", 0)
	if err != nil || empty.System != "" || len(empty.Messages) != 0 {
		t.Fatalf("session vide : %+v %v", empty, err)
	}
	if err := s.SetSystem(ctx, "k", "SYS"); err != nil {
		t.Fatal(err)
	}
	err = s.Append(ctx, "k",
		llm.Message{Role: llm.RoleUser, Content: "un"},
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "x", Arguments: `{"a":1}`}}, Reasoning: "r"},
		llm.Message{Role: llm.RoleTool, ToolCallID: "c1", Content: "res"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "k", llm.Message{Role: llm.RoleAssistant, Content: "fin"}); err != nil {
		t.Fatal(err)
	}

	all, _ := s.Load(ctx, "k", 0)
	if all.System != "SYS" || len(all.Messages) != 4 {
		t.Fatalf("session : %+v", all)
	}
	if tc := all.Messages[1].ToolCalls; len(tc) != 1 || tc[0].Arguments != `{"a":1}` || all.Messages[1].Reasoning != "r" {
		t.Fatalf("appel d'outil : %+v", all.Messages[1])
	}
	last, _ := s.Load(ctx, "k", 2)
	if len(last.Messages) != 2 || last.Messages[0].Role != llm.RoleTool || last.Messages[1].Content != "fin" {
		t.Fatalf("limite : %+v", last.Messages)
	}

	if err := s.Reset(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx, "k", 0); got.System != "" || len(got.Messages) != 0 {
		t.Fatalf("après reset : %+v", got)
	}
	if _, err := New(ctx, db, "bad-prefix;"); err == nil {
		t.Fatal("préfixe invalide accepté")
	}
}

func TestStoreContract(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) sessiontest.Store {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := New(context.Background(), db, "")
		if err != nil {
			t.Fatal(err)
		}
		if !s.hasFTS {
			t.Log("FTS5 indisponible : recherche par LIKE")
		}
		return s
	})
}

// Une base créée avant la compression est migrée, et l'index plein texte est
// rempli avec les messages existants.
func TestMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE agentkit_sessions (key TEXT PRIMARY KEY, system TEXT NOT NULL DEFAULT '', created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE agentkit_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL,
			tool_calls TEXT, tool_call_id TEXT, name TEXT, reasoning TEXT, created_at TIMESTAMP NOT NULL)`,
		`INSERT INTO agentkit_sessions VALUES ('k', 'SYS', '2026-01-01', '2026-01-01')`,
		`INSERT INTO agentkit_messages (key, role, content, created_at) VALUES ('k', 'user', 'ancienne baleine', '2026-01-01')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "k", 0)
	if err != nil || got.System != "SYS" || len(got.Messages) != 1 {
		t.Fatalf("après migration : %+v %v", got, err)
	}
	hits, err := s.Search(ctx, session.Query{Text: "baleine"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("index rempli : %+v %v", hits, err)
	}
	if _, err := New(ctx, db, ""); err != nil {
		t.Fatalf("seconde ouverture : %v", err)
	}
}
