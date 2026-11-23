package event

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HandlerConfig configure le flux SSE.
type HandlerConfig struct {
	// Filter choisit les événements d'une requête (nil : tous ; par exemple
	// ?session=… ou ?kind=…, voir QueryFilter).
	Filter func(r *http.Request) func(Event) bool
	// KeepAlive : commentaire envoyé pour garder la connexion (défaut 25 s).
	KeepAlive time.Duration
}

// QueryFilter filtre sur les paramètres ?session=, ?kind= et ?source=.
func QueryFilter(r *http.Request) func(Event) bool {
	q := r.URL.Query()
	session, kind, source := q.Get("session"), q.Get("kind"), q.Get("source")
	if session == "" && kind == "" && source == "" {
		return nil
	}
	return func(ev Event) bool {
		return (session == "" || ev.Session == session) && (kind == "" || string(ev.Kind) == kind) && (source == "" || ev.Source == source)
	}
}

// Handler sert le flux en SSE (text/event-stream), avec rattrapage depuis
// Last-Event-ID ou ?after=. À protéger : il expose des extraits de
// conversations.
func Handler(b *Bus, cfg HandlerConfig) http.Handler {
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 25 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flux non pris en charge", http.StatusInternalServerError)
			return
		}
		var filter func(Event) bool
		if cfg.Filter != nil {
			filter = cfg.Filter(r)
		}
		after := r.Header.Get("Last-Event-ID")
		if after == "" {
			after = r.URL.Query().Get("after")
		}
		ch, cancel := b.Subscribe(filter)
		defer cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var last uint64
		send := func(ev Event) bool {
			if ev.Seq <= last {
				return true // déjà envoyé par le rattrapage
			}
			last = ev.Seq
			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, data); err != nil {
				return false
			}
			fl.Flush()
			return true
		}
		if seq, err := strconv.ParseUint(after, 10, 64); err == nil {
			for _, ev := range b.Since(seq, filter) {
				if !send(ev) {
					return
				}
			}
		}
		fl.Flush()
		tick := time.NewTicker(cfg.KeepAlive)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				if !send(ev) {
					return
				}
			case <-tick.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	})
}

// CostHandler sert l'état du compteur en JSON (?top=N).
func CostHandler(m *Meter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		top, _ := strconv.Atoi(r.URL.Query().Get("top"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot(top))
	})
}
