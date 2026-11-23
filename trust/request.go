package trust

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

// Kind est le type d'une demande.
type Kind string

const (
	KindApproval Kind = "approval" // valider une action
	KindQuestion Kind = "question" // répondre à une question de l'agent
)

// Status est l'état d'une demande.
type Status string

const (
	Pending   Status = "pending"   // en attente d'une décision
	Approved  Status = "approved"  // validée, pas encore exécutée
	Executing Status = "executing" // en cours d'exécution
	Executed  Status = "executed"  // validée et exécutée avec succès
	Failed    Status = "failed"    // validée, mais l'exécution a échoué ou son issue est inconnue
	Denied    Status = "denied"    // refusée
	Answered  Status = "answered"  // question répondue
	Expired   Status = "expired"   // non décidée à temps
)

// Request est une demande adressée à un humain.
type Request struct {
	ID   string
	Kind Kind
	// Tool, Args et ArgsHash : l'action exacte à valider (KindApproval).
	Tool     string
	Args     json.RawMessage
	ArgsHash string
	// Summary décrit l'action ou porte la question, pour l'humain.
	Summary string
	// Conversation d'origine : où reprendre une fois la décision prise.
	Conversation tool.Conversation

	Status    Status
	Result    string // résultat de l'exécution, ou réponse à la question
	DecidedBy string
	CreatedAt time.Time
	DecidedAt time.Time
	ExpiresAt time.Time // zéro : n'expire pas
}

// HashArgs renvoie l'empreinte d'arguments JSON. Les arguments d'un appel
// (tool.Call.Raw) sont déjà canoniques : clés triées, sans espaces.
func HashArgs(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ErrNotPending : la demande n'attend plus de décision (déjà décidée, expirée…).
var ErrNotPending = errors.New("trust : la demande n'est plus en attente")

// RequestFilter sélectionne des demandes.
type RequestFilter struct {
	Session string
	Kind    Kind
	Status  []Status
	Limit   int // 0 : 100
}

// Store conserve les demandes. Les transitions sont atomiques : deux
// décisions simultanées sur la même demande ne peuvent pas réussir toutes
// les deux, et une action ne passe qu'une fois en exécution.
type Store interface {
	Create(ctx context.Context, r *Request) error
	Get(ctx context.Context, id string) (*Request, error)
	List(ctx context.Context, f RequestFilter) ([]*Request, error)
	// Transition passe la demande de l'état from à l'état to, avec les champs
	// fournis ; ErrNotPending si elle n'est plus dans l'état from.
	Transition(ctx context.Context, id string, from, to Status, by, result string, at time.Time) (*Request, error)
	// Recover passe en Failed les demandes restées Executing après un arrêt
	// brutal : leur issue est inconnue, elles ne sont jamais réexécutées.
	Recover(ctx context.Context) (int, error)
}

// MemoryStore est un Store en mémoire.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]*Request
	seq map[string]int
	n   int
}

// NewMemoryStore crée un Store en mémoire.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: map[string]*Request{}, seq: map[string]int{}}
}

func cloneReq(r *Request) *Request {
	c := *r
	c.Args = append(json.RawMessage(nil), r.Args...)
	return &c
}

func (s *MemoryStore) Create(_ context.Context, r *Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	Prepare(r)
	if _, dup := s.m[r.ID]; dup {
		return fmt.Errorf("trust : %s existe déjà", r.ID)
	}
	s.n++
	s.m[r.ID], s.seq[r.ID] = cloneReq(r), s.n
	return nil
}

// Prepare remplit les valeurs par défaut d'une nouvelle demande (identifiant,
// date, statut, type, empreinte des arguments). Les Store l'appellent dans Create.
func Prepare(r *Request) {
	if r.ID == "" {
		r.ID = turn.NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = Pending
	}
	if r.Kind == "" {
		r.Kind = KindApproval
	}
	if r.Kind == KindApproval && r.ArgsHash == "" {
		r.ArgsHash = HashArgs(r.Args)
	}
}

func (s *MemoryStore) Get(_ context.Context, id string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[id]; ok {
		return cloneReq(r), nil
	}
	return nil, nil
}

func (s *MemoryStore) List(_ context.Context, f RequestFilter) ([]*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	all := make([]*Request, 0, len(s.m))
	for _, r := range s.m {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return s.seq[all[i].ID] > s.seq[all[j].ID] })
	var out []*Request
	for _, r := range all {
		if len(out) >= limit {
			break
		}
		if f.Session != "" && r.Conversation.Session != f.Session || f.Kind != "" && r.Kind != f.Kind {
			continue
		}
		if len(f.Status) > 0 && !containsStatus(f.Status, r.Status) {
			continue
		}
		out = append(out, cloneReq(r))
	}
	return out, nil
}

func containsStatus(list []Status, s Status) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *MemoryStore) Transition(_ context.Context, id string, from, to Status, by, result string, at time.Time) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return nil, fmt.Errorf("trust : demande %s introuvable", id)
	}
	if r.Status != from {
		return nil, ErrNotPending
	}
	r.Status = to
	if by != "" {
		r.DecidedBy = by
	}
	if result != "" {
		r.Result = result
	}
	if from == Pending {
		r.DecidedAt = at
	}
	return cloneReq(r), nil
}

func (s *MemoryStore) Recover(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.m {
		if r.Status == Executing {
			r.Status, r.Result = Failed, ErrUncertain
			n++
		}
	}
	return n, nil
}

// ErrUncertain est le résultat d'une action interrompue pendant son exécution.
const ErrUncertain = "exécution interrompue : issue inconnue, l'action n'est pas relancée"
