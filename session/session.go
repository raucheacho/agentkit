// Package session conserve l'historique des conversations.
//
// Une session a un prompt système, figé à sa création, et un historique en
// ajout seul : un tour ajoute ses messages, il ne réécrit jamais les
// précédents. Ces deux règles gardent le début de la conversation identique
// d'un tour à l'autre, ce qui permet au fournisseur de servir ce préfixe
// depuis son cache de prompt.
//
// Seule exception : la compression (Compactor). Les messages anciens sont
// archivés — jamais effacés, ils restent consultables par Searcher — et
// remplacés par un résumé.
package session

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/llm"
)

// Session est l'état d'une conversation.
type Session struct {
	Key string
	// System est le prompt système figé à la création de la session ("" tant
	// qu'aucun tour n'a eu lieu).
	System string
	// Summary résume les messages archivés par la compression ("" : aucun).
	Summary  string
	Messages []llm.Message
}

// Store persiste les sessions. Il doit supporter des appels concurrents sur
// des clés différentes ; l'agent sérialise déjà les appels sur une même clé.
type Store interface {
	// Load renvoie la session key avec au plus ses limit derniers messages
	// non archivés (limit <= 0 : tous). Une session inconnue est renvoyée
	// vide, sans erreur.
	Load(ctx context.Context, key string, limit int) (*Session, error)
	// Append ajoute des messages à la fin de l'historique.
	Append(ctx context.Context, key string, msgs ...llm.Message) error
	// SetSystem fixe le prompt système de la session.
	SetSystem(ctx context.Context, key, system string) error
	// Reset efface la session : le prochain tour repart de zéro, avec un
	// prompt système recalculé.
	Reset(ctx context.Context, key string) error
}

// Compactor est implémenté par les Store qui savent compresser une session.
type Compactor interface {
	// Compact archive les messages non archivés de key sauf les keep
	// derniers, et fixe le résumé de la session.
	Compact(ctx context.Context, key, summary string, keep int) error
}

// Hit est un message retrouvé par une recherche.
type Hit struct {
	Key     string
	Role    llm.Role
	Content string
	At      time.Time
}

// Query est une recherche dans l'historique.
type Query struct {
	Text string
	// Keys restreint la recherche à ces sessions (vide : toutes).
	Keys  []string
	Limit int // 0 : 10
}

// Searcher est implémenté par les Store qui savent chercher dans tout
// l'historique, archives comprises.
type Searcher interface {
	Search(ctx context.Context, q Query) ([]Hit, error)
}

type stored struct {
	msg      llm.Message
	at       time.Time
	archived bool
}

type memSession struct {
	system, summary string
	msgs            []stored
}

// Memory est un Store en mémoire, pour les tests et les agents éphémères.
// Il implémente Compactor et Searcher.
type Memory struct {
	mu sync.Mutex
	m  map[string]*memSession
}

// NewMemory crée un Store en mémoire.
func NewMemory() *Memory { return &Memory{m: map[string]*memSession{}} }

func (s *Memory) Load(_ context.Context, key string, limit int) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &Session{Key: key}
	if cur, ok := s.m[key]; ok {
		out.System, out.Summary = cur.system, cur.summary
		var msgs []llm.Message
		for _, m := range cur.msgs {
			if !m.archived {
				msgs = append(msgs, m.msg)
			}
		}
		if limit > 0 && len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
		out.Messages = msgs
	}
	return out, nil
}

func (s *Memory) Append(_ context.Context, key string, msgs ...llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.get(key)
	now := time.Now().UTC()
	for _, m := range msgs {
		cur.msgs = append(cur.msgs, stored{msg: m, at: now})
	}
	return nil
}

func (s *Memory) SetSystem(_ context.Context, key, system string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).system = system
	return nil
}

func (s *Memory) Reset(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *Memory) Compact(_ context.Context, key, summary string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.get(key)
	var live []int
	for i, m := range cur.msgs {
		if !m.archived {
			live = append(live, i)
		}
	}
	if keep < 0 {
		keep = 0
	}
	for n := 0; n < len(live)-keep; n++ {
		cur.msgs[live[n]].archived = true
	}
	cur.summary = summary
	return nil
}

func (s *Memory) Search(_ context.Context, q Query) ([]Hit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	terms := Terms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	type scored struct {
		hit   Hit
		score int
	}
	var all []scored
	for key, sess := range s.m {
		if len(q.Keys) > 0 && !slices.Contains(q.Keys, key) {
			continue
		}
		for _, m := range sess.msgs {
			if m.msg.Role != llm.RoleUser && m.msg.Role != llm.RoleAssistant || m.msg.Content == "" {
				continue
			}
			low := Fold(m.msg.Content)
			score := 0
			for _, t := range terms {
				if strings.Contains(low, t) {
					score++
				}
			}
			if score > 0 {
				all = append(all, scored{Hit{Key: key, Role: m.msg.Role, Content: m.msg.Content, At: m.at}, score})
			}
		}
	}
	slices.SortStableFunc(all, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return b.hit.At.Compare(a.hit.At)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	var out []Hit
	for i := 0; i < len(all) && i < limit; i++ {
		out = append(out, all[i].hit)
	}
	return out, nil
}

func (s *Memory) get(key string) *memSession {
	cur, ok := s.m[key]
	if !ok {
		cur = &memSession{}
		s.m[key] = cur
	}
	return cur
}

// Terms découpe une recherche en termes significatifs (minuscules sans
// accents, au moins trois caractères).
func Terms(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(Fold(q), func(r rune) bool {
		return !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127)
	}) {
		if len([]rune(f)) >= 3 && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

var accents = strings.NewReplacer(
	"à", "a", "â", "a", "ä", "a", "á", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"î", "i", "ï", "i", "í", "i", "ì", "i",
	"ô", "o", "ö", "o", "ó", "o", "ò", "o", "õ", "o",
	"ù", "u", "û", "u", "ü", "u", "ú", "u",
	"ç", "c", "ñ", "n", "ÿ", "y", "œ", "oe", "æ", "ae",
)

// Fold met un texte en minuscules sans accents, pour comparer des termes.
func Fold(s string) string { return accents.Replace(strings.ToLower(s)) }

var (
	_ Compactor = (*Memory)(nil)
	_ Searcher  = (*Memory)(nil)
)
