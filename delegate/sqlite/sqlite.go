// Package sqlite implémente delegate.Store sur une base SQL compatible
// SQLite, sans imposer de pilote (voir session/sqlite).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/tool"
)

// Store est un delegate.Store SQLite.
type Store struct {
	db    *sql.DB
	table string
}

var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// New crée la table si besoin. prefix vide : "agentkit_".
func New(ctx context.Context, db *sql.DB, prefix string) (*Store, error) {
	if prefix == "" {
		prefix = "agentkit_"
	}
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("préfixe de table invalide : %q", prefix)
	}
	s := &Store{db: db, table: prefix + "tasks"}
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			seq          INTEGER PRIMARY KEY AUTOINCREMENT,
			id           TEXT NOT NULL UNIQUE,
			goal         TEXT NOT NULL,
			context      TEXT NOT NULL DEFAULT '',
			conversation TEXT NOT NULL,
			session      TEXT NOT NULL,
			status       TEXT NOT NULL,
			result       TEXT NOT NULL DEFAULT '',
			error        TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			finished_at  INTEGER NOT NULL DEFAULT 0)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_status ON %s (status, seq)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_session ON %s (session, seq)`, s.table, s.table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("delegate sqlite : création de la table : %w", err)
		}
	}
	return s, nil
}

const cols = `id, goal, context, conversation, status, result, error, created_at, finished_at`

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

type scanner interface{ Scan(...any) error }

func scan(sc scanner) (*delegate.Task, error) {
	var t delegate.Task
	var conv, status string
	var created, finished int64
	if err := sc.Scan(&t.ID, &t.Goal, &t.Context, &conv, &status, &t.Result, &t.Error, &created, &finished); err != nil {
		return nil, err
	}
	t.Status = delegate.TaskStatus(status)
	var c tool.Conversation
	if err := json.Unmarshal([]byte(conv), &c); err != nil {
		return nil, fmt.Errorf("delegate sqlite : conversation illisible : %w", err)
	}
	t.Conversation = c
	t.CreatedAt, t.FinishedAt = fromNanos(created), fromNanos(finished)
	return &t, nil
}

func (s *Store) Create(ctx context.Context, t *delegate.Task) error {
	delegate.PrepareTask(t, time.Now())
	conv, err := json.Marshal(t.Conversation)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s, session) VALUES (?,?,?,?,?,?,?,?,?,?)`, s.table, cols),
		t.ID, t.Goal, t.Context, string(conv), string(t.Status), t.Result, t.Error, nanos(t.CreatedAt), nanos(t.FinishedAt),
		t.Conversation.Session)
	if err != nil {
		return fmt.Errorf("delegate sqlite : création : %w", err)
	}
	return nil
}

// Finish s'appuie sur une mise à jour conditionnelle : seule une tâche encore
// en cours change, ce qui départage une fin et une annulation simultanées.
func (s *Store) Finish(ctx context.Context, id string, status delegate.TaskStatus, result, errMsg string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = ?, result = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`, s.table),
		string(status), result, errMsg, nanos(at.UTC()), id, string(delegate.TaskRunning))
	if err != nil {
		return false, fmt.Errorf("delegate sqlite : fin de %s : %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) Get(ctx context.Context, id string) (*delegate.Task, error) {
	t, err := scan(s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, cols, s.table), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *Store) List(ctx context.Context, f delegate.TaskFilter) ([]*delegate.Task, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var where []string
	var args []any
	if f.Session != "" {
		where, args = append(where, "session = ?"), append(args, f.Session)
	}
	if len(f.Status) > 0 {
		ph := make([]string, len(f.Status))
		for i, st := range f.Status {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}
	q := fmt.Sprintf(`SELECT %s FROM %s`, cols, s.table)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY seq DESC LIMIT ?"
	args = append(args, limit)
	return s.query(ctx, q, args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]*delegate.Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*delegate.Task
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Recover(ctx context.Context, at time.Time) ([]*delegate.Task, error) {
	running, err := s.query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE status = ? ORDER BY seq`, cols, s.table), string(delegate.TaskRunning))
	if err != nil {
		return nil, err
	}
	var out []*delegate.Task
	for _, t := range running {
		ok, err := s.Finish(ctx, t.ID, delegate.TaskFailed, "", delegate.Interrupted, at)
		if err != nil {
			return out, err
		}
		if ok {
			t.Status, t.Error, t.FinishedAt = delegate.TaskFailed, delegate.Interrupted, at.UTC()
			out = append(out, t)
		}
	}
	return out, nil
}
