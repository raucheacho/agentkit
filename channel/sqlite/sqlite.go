// Package sqlite implémente channel.HoldStore sur une base SQL compatible
// SQLite, sans imposer de pilote (voir session/sqlite).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

// Holds conserve les sessions reprises en main.
type Holds struct {
	db    *sql.DB
	table string
}

var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// NewHolds crée la table si besoin. prefix vide : "agentkit_".
func NewHolds(ctx context.Context, db *sql.DB, prefix string) (*Holds, error) {
	if prefix == "" {
		prefix = "agentkit_"
	}
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("préfixe de table invalide : %q", prefix)
	}
	h := &Holds{db: db, table: prefix + "holds"}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		session TEXT PRIMARY KEY,
		since   INTEGER NOT NULL)`, h.table)); err != nil {
		return nil, fmt.Errorf("channel sqlite : création de la table : %w", err)
	}
	return h, nil
}

func (h *Holds) List(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, fmt.Sprintf(`SELECT session FROM %s ORDER BY since`, h.table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *Holds) Set(ctx context.Context, session string, held bool) error {
	var err error
	if held {
		_, err = h.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (session, since) VALUES (?, ?) ON CONFLICT(session) DO NOTHING`, h.table),
			session, time.Now().UTC().UnixNano())
	} else {
		_, err = h.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE session = ?`, h.table), session)
	}
	if err != nil {
		return fmt.Errorf("channel sqlite : reprise en main de %s : %w", session, err)
	}
	return nil
}

var _ channel.HoldStore = (*Holds)(nil)
