// Package session provides a SQLite-backed store for per-conversation message
// history, safe for concurrent access from many simultaneous
// conversations (unlike an in-process map, which is not).
package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/raucheacho/agentkit/llm"
)

// maxHistory bounds how many messages are kept per conversation.
const maxHistory = 20

// SQLiteStore persists conversation history, one row per session key (channel:chatID).
// It shares its *sql.DB with the app's other stores (see pkg/store.Open).
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the sessions table if needed on the shared db.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			key        TEXT PRIMARY KEY,
			messages   TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`); err != nil {
		return nil, fmt.Errorf("create sessions table: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Get returns the message history for a session key, or nil if none exists yet.
func (s *SQLiteStore) Get(key string) ([]llm.Message, error) {
	var raw string
	err := s.db.QueryRow(`SELECT messages FROM sessions WHERE key = ?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query session: %w", err)
	}

	var messages []llm.Message
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	return messages, nil
}

// Save persists the message history for a session key, truncating it to the
// most recent messages.
func (s *SQLiteStore) Save(key string, messages []llm.Message) error {
	if len(messages) > maxHistory {
		messages = messages[len(messages)-maxHistory:]
	}

	raw, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO sessions (key, messages, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET messages = excluded.messages, updated_at = excluded.updated_at
	`, key, string(raw), time.Now())
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}
