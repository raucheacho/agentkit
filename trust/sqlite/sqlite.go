// Package sqlite implémente trust.Store sur une base SQL compatible SQLite,
// sans imposer de pilote (voir session/sqlite).
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

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
)

// Store est un trust.Store SQLite.
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
	s := &Store{db: db, table: prefix + "requests"}
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			seq          INTEGER PRIMARY KEY AUTOINCREMENT,
			id           TEXT NOT NULL UNIQUE,
			kind         TEXT NOT NULL,
			tool         TEXT NOT NULL DEFAULT '',
			args         TEXT NOT NULL DEFAULT '',
			args_hash    TEXT NOT NULL DEFAULT '',
			summary      TEXT NOT NULL DEFAULT '',
			conversation TEXT NOT NULL,
			session      TEXT NOT NULL,
			status       TEXT NOT NULL,
			result       TEXT NOT NULL DEFAULT '',
			decided_by   TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			decided_at   INTEGER NOT NULL DEFAULT 0,
			expires_at   INTEGER NOT NULL DEFAULT 0)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_status ON %s (status, seq)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_session ON %s (session, seq)`, s.table, s.table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("trust sqlite : création de la table : %w", err)
		}
	}
	return s, nil
}

const cols = `id, kind, tool, args, args_hash, summary, conversation, status, result, decided_by, created_at, decided_at, expires_at`

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

func scan(sc scanner) (*trust.Request, error) {
	var r trust.Request
	var kind, args, conv, status string
	var created, decided, expires int64
	if err := sc.Scan(&r.ID, &kind, &r.Tool, &args, &r.ArgsHash, &r.Summary, &conv, &status, &r.Result, &r.DecidedBy, &created, &decided, &expires); err != nil {
		return nil, err
	}
	r.Kind, r.Status = trust.Kind(kind), trust.Status(status)
	if args != "" {
		r.Args = json.RawMessage(args)
	}
	var c tool.Conversation
	if err := json.Unmarshal([]byte(conv), &c); err != nil {
		return nil, fmt.Errorf("trust sqlite : conversation illisible : %w", err)
	}
	r.Conversation = c
	r.CreatedAt, r.DecidedAt, r.ExpiresAt = fromNanos(created), fromNanos(decided), fromNanos(expires)
	return &r, nil
}

func (s *Store) Create(ctx context.Context, r *trust.Request) error {
	trust.Prepare(r)
	conv, err := json.Marshal(r.Conversation)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s, session) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.table, cols),
		r.ID, string(r.Kind), r.Tool, string(r.Args), r.ArgsHash, r.Summary, string(conv), string(r.Status), r.Result, r.DecidedBy,
		nanos(r.CreatedAt), nanos(r.DecidedAt), nanos(r.ExpiresAt), r.Conversation.Session)
	if err != nil {
		return fmt.Errorf("trust sqlite : création : %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (*trust.Request, error) {
	r, err := scan(s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, cols, s.table), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) List(ctx context.Context, f trust.RequestFilter) ([]*trust.Request, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var where []string
	var args []any
	if f.Session != "" {
		where, args = append(where, "session = ?"), append(args, f.Session)
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, string(f.Kind))
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
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*trust.Request
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Transition s'appuie sur une mise à jour conditionnelle : la ligne ne change
// que si elle est encore dans l'état from, ce qui rend la transition atomique
// même entre plusieurs processus.
func (s *Store) Transition(ctx context.Context, id string, from, to trust.Status, by, result string, at time.Time) (*trust.Request, error) {
	set := []string{"status = ?"}
	args := []any{string(to)}
	if by != "" {
		set, args = append(set, "decided_by = ?"), append(args, by)
	}
	if result != "" {
		set, args = append(set, "result = ?"), append(args, result)
	}
	if from == trust.Pending {
		set, args = append(set, "decided_at = ?"), append(args, nanos(at))
	}
	args = append(args, id, string(from))
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s WHERE id = ? AND status = ?`, s.table, strings.Join(set, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("trust sqlite : transition de %s : %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("trust : demande %s introuvable", id)
		}
		return nil, trust.ErrNotPending
	}
	return s.Get(ctx, id)
}

func (s *Store) Recover(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = ?, result = ? WHERE status = ?`, s.table),
		string(trust.Failed), trust.ErrUncertain, string(trust.Executing))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

var _ trust.Store = (*Store)(nil)
