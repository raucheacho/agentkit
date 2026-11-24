package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

// Submitter crée les tours de reprise (en pratique runner.Runner).
type Submitter interface {
	Submit(ctx context.Context, t *turn.Turn) error
}

// Texts regroupe les textes adressés au modèle. Les valeurs par défaut sont en français.
type Texts struct {
	// Asked : résultat d'outil quand une validation est demandée.
	Asked func(r *Request) string
	// Denied : résultat d'outil quand la porte refuse.
	Denied func(reason string) string
	// Question : résultat de l'outil de question.
	Question func(r *Request) string
	// Resume : message du tour de reprise, une fois la demande décidée.
	Resume func(r *Request) string
}

// DefaultTexts renvoie les textes par défaut.
func DefaultTexts() Texts {
	return Texts{
		Asked: func(r *Request) string {
			return fmt.Sprintf("Validation demandée (référence %s). L'action n'est PAS encore faite. "+
				"Dis à ton interlocuteur que sa demande est transmise et qu'il recevra une réponse dès la décision.", r.ID)
		},
		Denied: func(reason string) string {
			if reason == "" {
				reason = "non autorisée"
			}
			return "Action refusée : " + reason + ". Ne la tente pas autrement ; explique-le à ton interlocuteur."
		},
		Question: func(r *Request) string {
			return fmt.Sprintf("Question transmise (référence %s). Tu n'as pas encore la réponse : "+
				"dis à ton interlocuteur que tu te renseignes et que tu reviens vers lui.", r.ID)
		},
		Resume: defaultResume,
	}
}

func defaultResume(r *Request) string {
	const head = "[Suivi interne — ce n'est pas un message de ton interlocuteur] "
	by := r.DecidedBy
	if by == "" {
		by = "le responsable"
	}
	switch r.Status {
	case Executed:
		return head + fmt.Sprintf("La demande %s (%s) a été validée par %s et exécutée. Résultat : %s\n"+
			"Informe maintenant ton interlocuteur du résultat.", r.ID, r.Summary, by, r.Result)
	case Failed:
		return head + fmt.Sprintf("La demande %s (%s) a été validée, mais son exécution a échoué : %s\n"+
			"Informe ton interlocuteur, sans promettre que c'est fait.", r.ID, r.Summary, r.Result)
	case Denied:
		reason := r.Result
		if reason == "" {
			reason = "aucun motif donné"
		}
		return head + fmt.Sprintf("La demande %s (%s) a été refusée par %s (motif : %s). "+
			"Informe ton interlocuteur ; ne l'exécute pas.", r.ID, r.Summary, by, reason)
	case Expired:
		return head + fmt.Sprintf("La demande %s (%s) n'a pas été décidée à temps et n'est pas exécutée. "+
			"Informe ton interlocuteur.", r.ID, r.Summary)
	case Answered:
		return head + fmt.Sprintf("Réponse de %s à ta question « %s » : %s\n"+
			"Reprends la conversation avec ton interlocuteur.", by, r.Summary, r.Result)
	}
	return head + fmt.Sprintf("La demande %s est passée à l'état %s.", r.ID, r.Status)
}

// Config configure un Manager.
type Config struct {
	Store Store
	// Tools sert à exécuter les actions validées (le registre de l'agent).
	Tools *tool.Registry
	// Policy décide de chaque appel (nil : Default).
	Policy Policy
	// Resume crée les tours de reprise (nil : aucun, ou plus tard avec Attach).
	Resume Submitter
	// TTL d'une demande (0 : n'expire pas).
	TTL time.Duration
	// ExecTimeout borne l'exécution d'une action validée (défaut 60 s).
	ExecTimeout time.Duration
	// Summarize décrit une action pour l'humain qui valide (défaut : outil et arguments).
	Summarize func(call tool.Call) string
	// OnChange est appelé à chaque création ou changement d'état (tableau de
	// bord, notification du responsable).
	OnChange func(ctx context.Context, r *Request)
	// AskTool : nom et description de l'outil de question (défauts : "ask_human").
	AskToolName        string
	AskToolDescription string
	Texts              Texts
	// Notice rédige elle-même l'annonce d'une décision (validée et exécutée,
	// refusée, échouée, expirée) : si elle renvoie un texte, il est livré tel
	// quel à l'interlocuteur par un tour à réponse fixe (turn.MetaReply), sans
	// appel au modèle. nil, ou ok = false : le modèle rédige l'annonce à
	// partir de Texts.Resume. Ne concerne pas les réponses aux questions.
	Notice func(ctx context.Context, r *Request) (text string, ok bool)
	Logger *slog.Logger
}

// Manager applique la politique et suit les demandes.
type Manager struct {
	cfg    Config
	log    *slog.Logger
	mu     sync.Mutex
	resume Submitter

	// La récupération des actions interrompues par un arrêt précédent a lieu
	// une seule fois, avant toute exécution de ce processus : sinon une
	// décision prise avant Run serait prise pour une action interrompue.
	recovery    sync.Once
	recoveryErr error
}

// recoverOnce passe en échec, une seule fois, les actions restées en cours
// d'exécution après l'arrêt précédent.
func (m *Manager) recoverOnce(ctx context.Context) error {
	m.recovery.Do(func() {
		n, err := m.cfg.Store.Recover(ctx)
		if err != nil {
			m.recoveryErr = err
			return
		}
		if n > 0 {
			m.log.Warn("trust : actions interrompues par l'arrêt précédent, non relancées", "count", n)
		}
	})
	return m.recoveryErr
}

// New crée un Manager.
func New(cfg Config) *Manager {
	if cfg.Policy == nil {
		cfg.Policy = Default()
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 60 * time.Second
	}
	if cfg.Summarize == nil {
		cfg.Summarize = func(c tool.Call) string { return c.Name + " " + string(c.Raw) }
	}
	if cfg.AskToolName == "" {
		cfg.AskToolName = "ask_human"
	}
	if cfg.AskToolDescription == "" {
		cfg.AskToolDescription = "Pose une question au responsable humain quand tu ne peux pas répondre seul " +
			"(information manquante, décision qui ne t'appartient pas). La réponse arrivera plus tard."
	}
	def := DefaultTexts()
	if cfg.Texts.Asked == nil {
		cfg.Texts.Asked = def.Asked
	}
	if cfg.Texts.Denied == nil {
		cfg.Texts.Denied = def.Denied
	}
	if cfg.Texts.Question == nil {
		cfg.Texts.Question = def.Question
	}
	if cfg.Texts.Resume == nil {
		cfg.Texts.Resume = def.Resume
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Manager{cfg: cfg, log: log, resume: cfg.Resume}
}

// Attach branche le Submitter des tours de reprise après coup : le runner a
// besoin de l'agent, qui a besoin du Middleware de ce Manager.
func (m *Manager) Attach(s Submitter) {
	m.mu.Lock()
	m.resume = s
	m.mu.Unlock()
}

// Store renvoie le Store du Manager.
func (m *Manager) Store() Store { return m.cfg.Store }

// Middleware est la porte : à placer en premier dans agent.Config.Middleware.
func (m *Manager) Middleware() agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call tool.Call) (tool.Result, error) {
			conv, _ := tool.ConversationFrom(ctx)
			v := m.cfg.Policy.Check(ctx, Action{Call: call, Conversation: conv})
			switch v.Decision {
			case Allow:
				return next(ctx, call)
			case Ask:
				r, err := m.requestApproval(ctx, call, conv)
				if err != nil {
					return tool.Result{}, fmt.Errorf("demande de validation impossible : %w", err)
				}
				return tool.Pause(m.cfg.Texts.Asked(r), tool.Wait{Kind: string(KindApproval), Ref: r.ID}), nil
			default:
				return tool.Text(m.cfg.Texts.Denied(v.Reason)), nil
			}
		}
	}
}

// requestApproval crée la demande, ou renvoie celle qui attend déjà pour la
// même action dans la même session (le modèle redemande souvent).
func (m *Manager) requestApproval(ctx context.Context, call tool.Call, conv tool.Conversation) (*Request, error) {
	hash := HashArgs(call.Raw)
	pending, err := m.cfg.Store.List(ctx, RequestFilter{Session: conv.Session, Kind: KindApproval, Status: []Status{Pending}})
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		if p.Tool == call.Name && p.ArgsHash == hash {
			return p, nil
		}
	}
	r := &Request{Kind: KindApproval, Tool: call.Name, Args: call.Raw, ArgsHash: hash, Summary: m.cfg.Summarize(call), Conversation: conv}
	if m.cfg.TTL > 0 {
		r.ExpiresAt = time.Now().UTC().Add(m.cfg.TTL)
	}
	if err := m.cfg.Store.Create(ctx, r); err != nil {
		return nil, err
	}
	m.changed(ctx, r)
	return r, nil
}

type askIn struct {
	Question string `json:"question" desc:"la question, claire et complète, pour le responsable"`
}

// AskTool renvoie l'outil par lequel l'agent pose une question à un humain :
// le tour s'arrête en attente, la réponse (Answer) relance la conversation.
func (m *Manager) AskTool() tool.Tool {
	return tool.New(m.cfg.AskToolName, m.cfg.AskToolDescription, func(ctx context.Context, in askIn) (tool.Result, error) {
		q := strings.TrimSpace(in.Question)
		if q == "" {
			return tool.Text("Erreur : la question est vide."), nil
		}
		conv, _ := tool.ConversationFrom(ctx)
		r := &Request{Kind: KindQuestion, Summary: q, Conversation: conv}
		if m.cfg.TTL > 0 {
			r.ExpiresAt = time.Now().UTC().Add(m.cfg.TTL)
		}
		if err := m.cfg.Store.Create(ctx, r); err != nil {
			return tool.Result{}, err
		}
		m.changed(ctx, r)
		return tool.Pause(m.cfg.Texts.Question(r), tool.Wait{Kind: string(KindQuestion), Ref: r.ID}), nil
	}, tool.MainOnly())
}

// Approve valide une demande : l'action enregistrée (outil et arguments
// exacts) s'exécute une seule fois, puis un tour de reprise informe
// l'interlocuteur. Une seconde décision sur la même demande échoue (ErrNotPending).
func (m *Manager) Approve(ctx context.Context, id, by string) (*Request, error) {
	if err := m.recoverOnce(ctx); err != nil {
		return nil, err
	}
	r, err := m.decide(ctx, id, KindApproval, Approved, by, "")
	if err != nil {
		return nil, err
	}
	r, err = m.cfg.Store.Transition(ctx, id, Approved, Executing, "", "", time.Now().UTC())
	if err != nil {
		return nil, err
	}
	m.changed(ctx, r)
	status, result := m.execute(ctx, r)
	r, err = m.cfg.Store.Transition(ctx, id, Executing, status, "", result, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	m.changed(ctx, r)
	m.resumeTurn(ctx, r)
	return r, nil
}

// Deny refuse une demande de validation.
func (m *Manager) Deny(ctx context.Context, id, by, reason string) (*Request, error) {
	r, err := m.decide(ctx, id, KindApproval, Denied, by, reason)
	if err != nil {
		return nil, err
	}
	m.changed(ctx, r)
	m.resumeTurn(ctx, r)
	return r, nil
}

// Answer répond à une question de l'agent.
func (m *Manager) Answer(ctx context.Context, id, by, answer string) (*Request, error) {
	if strings.TrimSpace(answer) == "" {
		return nil, errors.New("trust : réponse vide")
	}
	r, err := m.decide(ctx, id, KindQuestion, Answered, by, answer)
	if err != nil {
		return nil, err
	}
	m.changed(ctx, r)
	m.resumeTurn(ctx, r)
	return r, nil
}

func (m *Manager) decide(ctx context.Context, id string, kind Kind, to Status, by, result string) (*Request, error) {
	cur, err := m.cfg.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("trust : demande %s introuvable", id)
	}
	if cur.Kind != kind {
		return nil, fmt.Errorf("trust : la demande %s est de type %s", id, cur.Kind)
	}
	now := time.Now().UTC()
	if !cur.ExpiresAt.IsZero() && now.After(cur.ExpiresAt) {
		m.expire(ctx, cur)
		return nil, ErrNotPending
	}
	return m.cfg.Store.Transition(ctx, id, Pending, to, by, result, now)
}

// execute lance l'action validée, sans repasser par la porte : la validation
// porte sur ces arguments exacts.
func (m *Manager) execute(ctx context.Context, r *Request) (Status, string) {
	if m.cfg.Tools == nil {
		return Failed, "aucun registre d'outils configuré"
	}
	impl, spec, ok := m.cfg.Tools.Get(r.Tool)
	if !ok {
		return Failed, "outil introuvable : " + r.Tool
	}
	var args map[string]any
	if err := json.Unmarshal(r.Args, &args); err != nil {
		return Failed, "arguments illisibles : " + err.Error()
	}
	conv := r.Conversation
	conv.Source = string(turn.SourceResume)
	ectx, cancel := context.WithTimeout(tool.WithConversation(context.WithoutCancel(ctx), conv), m.cfg.ExecTimeout)
	defer cancel()
	res, err := impl.Run(ectx, tool.Call{ID: "approval_" + r.ID, Name: r.Tool, Args: args, Raw: r.Args, Spec: spec})
	if err != nil {
		return Failed, err.Error()
	}
	return Executed, res.Content
}

// Expire passe en Expired les demandes dont le délai est dépassé et relance
// leurs conversations. Renvoie le nombre de demandes expirées.
func (m *Manager) Expire(ctx context.Context, now time.Time) (int, error) {
	pending, err := m.cfg.Store.List(ctx, RequestFilter{Status: []Status{Pending}, Limit: 1000})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range pending {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) && m.expire(ctx, r) {
			n++
		}
	}
	return n, nil
}

func (m *Manager) expire(ctx context.Context, r *Request) bool {
	r, err := m.cfg.Store.Transition(ctx, r.ID, Pending, Expired, "", "", time.Now().UTC())
	if err != nil {
		return false
	}
	m.changed(ctx, r)
	m.resumeTurn(ctx, r)
	return true
}

// Run fait expirer les demandes chaque minute jusqu'à l'annulation de ctx,
// après avoir marqué en échec les actions interrompues par un arrêt brutal.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.recoverOnce(ctx); err != nil {
		return err
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if _, err := m.Expire(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
			m.log.Error("trust : expiration des demandes", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (m *Manager) resumeTurn(ctx context.Context, r *Request) {
	m.mu.Lock()
	sub := m.resume
	m.mu.Unlock()
	if sub == nil || r.Conversation.Session == "" {
		return
	}
	c := r.Conversation
	t := &turn.Turn{
		Session: c.Session, Source: turn.SourceResume, Channel: c.Channel, ChatID: c.ChatID, Sender: c.Sender,
		Content: m.cfg.Texts.Resume(r), Meta: map[string]string{"request_id": r.ID, "request_status": string(r.Status)},
	}
	// Rendre compte d'une décision ne demande aucun outil ; après la réponse
	// à une question, l'agent peut avoir besoin d'agir.
	if r.Kind == KindApproval {
		t.Meta[turn.MetaTools] = "none"
		if m.cfg.Notice != nil {
			if text, ok := m.cfg.Notice(ctx, r); ok {
				t.Meta[turn.MetaReply] = text
			}
		}
	}
	if err := sub.Submit(context.WithoutCancel(ctx), t); err != nil {
		m.log.Error("trust : tour de reprise non créé", "request", r.ID, "err", err)
	}
}

func (m *Manager) changed(ctx context.Context, r *Request) {
	if m.cfg.OnChange != nil {
		m.cfg.OnChange(ctx, r)
	}
}
