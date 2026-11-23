// Package web est un canal pour un site ou une application : le navigateur
// envoie ses messages en POST et reçoit les réponses en temps réel par SSE
// (Server-Sent Events). Après une coupure, il rattrape les événements manqués
// grâce à l'en-tête Last-Event-ID.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

// Config configure le canal.
type Config struct {
	// Name du canal (défaut "web").
	Name string
	// Identify renvoie la conversation de la requête (cookie, jeton…) ; false
	// refuse la requête (401). nil : chat_id est lu dans la requête, sans
	// contrôle — à réserver au développement.
	Identify func(r *http.Request) (chatID string, ok bool)
	// Backlog : événements gardés par conversation pour le rattrapage (défaut 50).
	Backlog int
	// KeepAlive : intervalle des commentaires SSE qui gardent la connexion
	// ouverte derrière un proxy (défaut 25 s).
	KeepAlive time.Duration
}

// Event est un événement envoyé au navigateur.
type Event struct {
	ID   int64     `json:"id"`
	Type string    `json:"type"` // message | typing
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

type conv struct {
	seq     int64
	backlog []Event
	subs    map[chan Event]struct{}
}

// Web est le canal.
type Web struct {
	cfg Config

	mu    sync.Mutex
	convs map[string]*conv
	sink  channel.Sink
	ctx   context.Context
}

// New crée le canal. Montez Handler sur votre serveur HTTP.
func New(cfg Config) *Web {
	if cfg.Name == "" {
		cfg.Name = "web"
	}
	if cfg.Backlog <= 0 {
		cfg.Backlog = 50
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 25 * time.Second
	}
	return &Web{cfg: cfg, convs: map[string]*conv{}}
}

func (w *Web) Name() string { return w.cfg.Name }

// Run attend les requêtes jusqu'à l'annulation de ctx.
func (w *Web) Run(ctx context.Context, sink channel.Sink) error {
	w.mu.Lock()
	w.sink, w.ctx = sink, ctx
	w.mu.Unlock()
	<-ctx.Done()
	w.mu.Lock()
	w.sink = nil
	w.mu.Unlock()
	return nil
}

// Send publie une réponse vers les navigateurs de la conversation.
func (w *Web) Send(_ context.Context, out channel.Outgoing) error {
	w.publish(out.ChatID, Event{Type: "message", Text: out.Text}, true)
	return nil
}

// Typing publie « en train d'écrire » (non conservé pour le rattrapage).
func (w *Web) Typing(_ context.Context, chatID string) error {
	w.publish(chatID, Event{Type: "typing"}, false)
	return nil
}

// Connected indique si un navigateur écoute la conversation.
func (w *Web) Connected(chatID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.convs[chatID]
	return c != nil && len(c.subs) > 0
}

func (w *Web) get(chatID string) *conv {
	c := w.convs[chatID]
	if c == nil {
		c = &conv{subs: map[chan Event]struct{}{}}
		w.convs[chatID] = c
	}
	return c
}

func (w *Web) publish(chatID string, ev Event, keep bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.get(chatID)
	ev.At = time.Now().UTC()
	if keep {
		c.seq++
		ev.ID = c.seq
		c.backlog = append(c.backlog, ev)
		if len(c.backlog) > w.cfg.Backlog {
			c.backlog = c.backlog[len(c.backlog)-w.cfg.Backlog:]
		}
	}
	for ch := range c.subs {
		select {
		case ch <- ev:
		default: // navigateur trop lent : il rattrapera à la reconnexion
		}
	}
}

func (w *Web) subscribe(chatID string, after int64) (<-chan Event, []Event, func()) {
	ch := make(chan Event, 32)
	w.mu.Lock()
	c := w.get(chatID)
	c.subs[ch] = struct{}{}
	var missed []Event
	for _, ev := range c.backlog {
		if ev.ID > after {
			missed = append(missed, ev)
		}
	}
	w.mu.Unlock()
	var once sync.Once
	return ch, missed, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(c.subs, ch)
			w.mu.Unlock()
		})
	}
}

func (w *Web) identify(r *http.Request, fromBody string) (string, bool) {
	if w.cfg.Identify != nil {
		return w.cfg.Identify(r)
	}
	id := r.URL.Query().Get("chat_id")
	if id == "" {
		id = fromBody
	}
	return id, id != ""
}

// Handler sert POST /messages et GET /events. Montez-le sous un préfixe avec
// http.StripPrefix si besoin.
func (w *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", w.postMessage)
	mux.HandleFunc("GET /events", w.events)
	return mux
}

type postBody struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
	ID     string `json:"id"`
}

func (w *Web) postMessage(rw http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(rw, "corps illisible", http.StatusBadRequest)
		return
	}
	var b postBody
	if err := json.Unmarshal(raw, &b); err != nil {
		http.Error(rw, "JSON invalide", http.StatusBadRequest)
		return
	}
	chatID, ok := w.identify(r, b.ChatID)
	if !ok {
		http.Error(rw, "conversation non identifiée", http.StatusUnauthorized)
		return
	}
	text := strings.TrimSpace(b.Text)
	if text == "" {
		http.Error(rw, "message vide", http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	sink, ctx := w.sink, w.ctx
	w.mu.Unlock()
	if sink == nil {
		http.Error(rw, "canal arrêté", http.StatusServiceUnavailable)
		return
	}
	sink(ctx, channel.Message{Channel: w.cfg.Name, ChatID: chatID, Sender: chatID, Text: text, ID: b.ID, At: time.Now()})
	rw.WriteHeader(http.StatusAccepted)
}

func (w *Web) events(rw http.ResponseWriter, r *http.Request) {
	chatID, ok := w.identify(r, "")
	if !ok {
		http.Error(rw, "conversation non identifiée", http.StatusUnauthorized)
		return
	}
	fl, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "flux non pris en charge", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q := r.URL.Query().Get("after"); q != "" {
		after, _ = strconv.ParseInt(q, 10, 64)
	}
	ch, missed, cancel := w.subscribe(chatID, after)
	defer cancel()

	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	write := func(ev Event) bool {
		data, _ := json.Marshal(ev)
		var err error
		if ev.ID > 0 {
			_, err = fmt.Fprintf(rw, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data)
		} else {
			_, err = fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", ev.Type, data)
		}
		fl.Flush()
		return err == nil
	}
	for _, ev := range missed {
		if !write(ev) {
			return
		}
	}
	fl.Flush()
	keep := time.NewTicker(w.cfg.KeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !write(ev) {
				return
			}
		case <-keep.C:
			if _, err := io.WriteString(rw, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

var (
	_ channel.Adapter = (*Web)(nil)
	_ channel.Typer   = (*Web)(nil)
)
