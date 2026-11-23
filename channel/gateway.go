package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/runner"
	"github.com/raucheacho/agentkit/turn"
)

// Engine est ce dont la passerelle a besoin du runner.
type Engine interface {
	Submit(ctx context.Context, t *turn.Turn) error
	StopSession(ctx context.Context, session string) (int, error)
}

// Recorder inscrit un message dans l'historique d'une session (agent.Agent).
type Recorder interface {
	Record(ctx context.Context, key string, msgs ...llm.Message) error
}

// Config configure une passerelle.
type Config struct {
	// Allow filtre les messages reçus (nil : tous acceptés). Un message refusé
	// est ignoré sans réponse.
	Allow func(m Message) bool
	// Session donne la clé de session d'un message (défaut Channel:ChatID).
	// Une application peut regrouper plusieurs canaux d'une même personne.
	Session func(m Message) string
	// Describe transforme un message en texte pour l'agent (défaut : le texte,
	// suivi d'une ligne par pièce jointe).
	Describe func(m Message) string
	// Holds conserve les reprises en main pour qu'elles survivent à un
	// redémarrage (nil : en mémoire seulement).
	Holds HoldStore
	// DedupWindow : durée pendant laquelle un identifiant de message déjà vu
	// est ignoré (défaut 10 min). Les webhooks redélivrent souvent.
	DedupWindow time.Duration
	Logger      *slog.Logger
}

// HoldStore conserve les sessions reprises en main.
type HoldStore interface {
	List(ctx context.Context) ([]string, error)
	Set(ctx context.Context, session string, held bool) error
}

// Gateway relie des adaptateurs à un runner. Elle implémente runner.Outbox,
// runner.Typer et runner.Holds.
type Gateway struct {
	cfg      Config
	log      *slog.Logger
	adapters map[string]Adapter

	mu     sync.Mutex
	engine Engine
	rec    Recorder
	seen   map[string]time.Time
	held   map[string]bool
}

// NewGateway crée une passerelle.
func NewGateway(cfg Config, adapters ...Adapter) *Gateway {
	if cfg.DedupWindow <= 0 {
		cfg.DedupWindow = 10 * time.Minute
	}
	if cfg.Session == nil {
		cfg.Session = func(m Message) string { return m.Channel + ":" + m.ChatID }
	}
	if cfg.Describe == nil {
		cfg.Describe = Describe
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	g := &Gateway{cfg: cfg, log: log, adapters: map[string]Adapter{}, seen: map[string]time.Time{}, held: map[string]bool{}}
	for _, a := range adapters {
		g.adapters[a.Name()] = a
	}
	return g
}

// Attach branche la passerelle sur le runner qui exécute les tours et sur
// l'agent qui tient l'historique.
func (g *Gateway) Attach(e Engine, rec Recorder) {
	g.mu.Lock()
	g.engine, g.rec = e, rec
	g.mu.Unlock()
}

// Adapter renvoie un adaptateur par son nom.
func (g *Gateway) Adapter(name string) (Adapter, bool) {
	a, ok := g.adapters[name]
	return a, ok
}

// Run démarre tous les adaptateurs et attend leur fin. Si l'un d'eux échoue,
// l'erreur est journalisée et les autres continuent ; Run renvoie les erreurs
// réunies une fois ctx annulé ou tous les adaptateurs arrêtés.
func (g *Gateway) Run(ctx context.Context) error {
	if err := g.LoadHolds(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, a := range g.adapters {
		wg.Add(1)
		go func(a Adapter) {
			defer wg.Done()
			if err := a.Run(ctx, g.receive); err != nil && !errors.Is(err, context.Canceled) {
				g.log.Error("channel : adaptateur arrêté", "channel", a.Name(), "err", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s : %w", a.Name(), err))
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// receive transforme un message reçu en tour.
func (g *Gateway) receive(ctx context.Context, m Message) {
	if g.cfg.Allow != nil && !g.cfg.Allow(m) {
		g.log.Info("channel : message refusé", "channel", m.Channel)
		return
	}
	if g.duplicate(m) {
		return
	}
	text := strings.TrimSpace(g.cfg.Describe(m))
	if text == "" {
		return
	}
	g.mu.Lock()
	e := g.engine
	g.mu.Unlock()
	if e == nil {
		g.log.Error("channel : passerelle non branchée (Attach), message perdu", "channel", m.Channel)
		return
	}
	meta := map[string]string{}
	for k, v := range m.Meta {
		meta[k] = v
	}
	if m.ID != "" {
		meta["message_id"] = m.ID
	}
	if m.SenderName != "" {
		meta["sender_name"] = m.SenderName
	}
	t := &turn.Turn{
		Session: g.cfg.Session(m), Source: turn.SourceUser,
		Channel: m.Channel, ChatID: m.ChatID, Sender: m.Sender,
		Content: text, Meta: meta,
	}
	if err := e.Submit(ctx, t); err != nil {
		g.log.Error("channel : message non enregistré", "channel", m.Channel, "err", err)
	}
}

func (g *Gateway) duplicate(m Message) bool {
	if m.ID == "" {
		return false
	}
	key := m.Channel + "\x00" + m.ID
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if at, ok := g.seen[key]; ok && now.Sub(at) < g.cfg.DedupWindow {
		return true
	}
	g.seen[key] = now
	if len(g.seen) > 10000 {
		for k, at := range g.seen {
			if now.Sub(at) >= g.cfg.DedupWindow {
				delete(g.seen, k)
			}
		}
	}
	return false
}

// Send livre une réponse sur son canal, découpée si le canal limite la
// longueur des messages. Implémente runner.Outbox.
func (g *Gateway) Send(ctx context.Context, out runner.Outbound) error {
	return g.SendText(ctx, out.Channel, out.ChatID, out.Text)
}

// SendText envoie un texte sur un canal.
func (g *Gateway) SendText(ctx context.Context, channel, chatID, text string) error {
	a, ok := g.adapters[channel]
	if !ok {
		return fmt.Errorf("canal inconnu : %s", channel)
	}
	max := 0
	if l, ok := a.(Limiter); ok {
		max = l.MaxLength()
	}
	for _, part := range Split(text, max) {
		if err := a.Send(ctx, Outgoing{ChatID: chatID, Text: part}); err != nil {
			return err
		}
	}
	return nil
}

// Typing affiche « en train d'écrire » si le canal le permet. Implémente runner.Typer.
func (g *Gateway) Typing(ctx context.Context, channel, chatID string) {
	if a, ok := g.adapters[channel]; ok {
		if t, ok := a.(Typer); ok {
			if err := t.Typing(ctx, chatID); err != nil {
				g.log.Debug("channel : indicateur de frappe", "channel", channel, "err", err)
			}
		}
	}
}

// LoadHolds relit les reprises en main conservées (appelé par Run).
func (g *Gateway) LoadHolds(ctx context.Context) error {
	if g.cfg.Holds == nil {
		return nil
	}
	list, err := g.cfg.Holds.List(ctx)
	if err != nil {
		return fmt.Errorf("channel : lecture des reprises en main : %w", err)
	}
	g.mu.Lock()
	for _, s := range list {
		g.held[s] = true
	}
	g.mu.Unlock()
	return nil
}

// Take reprend une session en main : le tour en cours est arrêté, les
// messages en attente annulés, et l'agent ne répond plus jusqu'à Release.
// Les messages reçus entre-temps sont enregistrés dans l'historique.
func (g *Gateway) Take(ctx context.Context, session string) error {
	if g.cfg.Holds != nil {
		if err := g.cfg.Holds.Set(ctx, session, true); err != nil {
			return err
		}
	}
	g.mu.Lock()
	g.held[session] = true
	e := g.engine
	g.mu.Unlock()
	if e != nil {
		if _, err := e.StopSession(ctx, session); err != nil {
			return err
		}
	}
	return nil
}

// Release rend la session à l'agent.
func (g *Gateway) Release(ctx context.Context, session string) error {
	if g.cfg.Holds != nil {
		if err := g.cfg.Holds.Set(ctx, session, false); err != nil {
			return err
		}
	}
	g.mu.Lock()
	delete(g.held, session)
	g.mu.Unlock()
	return nil
}

// Held indique si une session est reprise en main. Implémente runner.Holds.
func (g *Gateway) Held(session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[session]
}

// HeldSessions renvoie les sessions reprises en main.
func (g *Gateway) HeldSessions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.held))
	for s := range g.held {
		out = append(out, s)
	}
	return out
}

// Reply envoie un message écrit par un humain et l'inscrit dans
// l'historique, pour que l'agent sache ce qui a été dit quand il reprendra.
func (g *Gateway) Reply(ctx context.Context, session, channel, chatID, text string) error {
	if err := g.SendText(ctx, channel, chatID, text); err != nil {
		return err
	}
	g.mu.Lock()
	rec := g.rec
	g.mu.Unlock()
	if rec == nil {
		return nil
	}
	return rec.Record(ctx, session, llm.Message{Role: llm.RoleAssistant, Content: text})
}

// Describe met un message en texte : son contenu, puis une ligne par pièce jointe.
func Describe(m Message) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(m.Text))
	for _, md := range m.Media {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		desc := md.Kind
		if md.Name != "" {
			desc += " « " + md.Name + " »"
		}
		if md.URL != "" {
			desc += " " + md.URL
		}
		fmt.Fprintf(&b, "[Pièce jointe : %s]", desc)
	}
	return b.String()
}

// Split découpe un texte en morceaux d'au plus max caractères, de préférence
// entre deux paragraphes, puis entre deux lignes, puis entre deux mots.
// max <= 0 : pas de découpe.
func Split(text string, max int) []string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	var parts []string
	rest := []rune(text)
	for len(rest) > max {
		cut := lastBreak(rest[:max])
		parts = append(parts, strings.TrimSpace(string(rest[:cut])))
		rest = []rune(strings.TrimLeft(string(rest[cut:]), " \n"))
	}
	if s := strings.TrimSpace(string(rest)); s != "" {
		parts = append(parts, s)
	}
	return parts
}

func lastBreak(r []rune) int {
	s := string(r)
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(s, sep); i > len(s)/3 {
			return utf8.RuneCountInString(s[:i]) + utf8.RuneCountInString(sep)
		}
	}
	return len(r)
}

var (
	_ runner.Outbox = (*Gateway)(nil)
	_ runner.Typer  = (*Gateway)(nil)
	_ runner.Holds  = (*Gateway)(nil)
)
