package kit

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/event"
	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/trust"
)

// Admin renvoie l'API d'administration, protégée par un jeton
// (« Authorization: Bearer <token> » ou ?token=). Sans jeton, tout est refusé.
//
//	GET  /requests                 demandes en attente
//	POST /requests/{id}/approve    valider (exécute l'action une fois)
//	POST /requests/{id}/deny       refuser (corps : raison)
//	POST /requests/{id}/answer     répondre à une question (corps : réponse)
//	POST /sessions/take?session=…  reprise en main
//	POST /sessions/release?session=…
//	POST /sessions/reply?session=…&channel=…&chat_id=…   (corps : texte)
//	GET  /tasks  /jobs             tâches de fond, planifications
//	POST /jobs/{id}/pause  /jobs/{id}/resume  ;  DELETE /jobs/{id}
//	GET  /events                   flux SSE (?session=, ?kind=, ?source=)
//	GET  /costs                    coûts et usage (?top=)
func (k *Kit) Admin(token string) http.Handler {
	mux := http.NewServeMux()
	jsonOut := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, trust.ErrNotPending) || errors.Is(err, schedule.ErrNotFound) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	body := func(r *http.Request) string {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		return strings.TrimSpace(string(b))
	}
	by := func(r *http.Request) string {
		if v := r.Header.Get("X-Admin-User"); v != "" {
			return v
		}
		return "admin"
	}

	mux.HandleFunc("GET /requests", func(w http.ResponseWriter, r *http.Request) {
		list, err := k.Requests.List(r.Context(), trust.RequestFilter{Status: []trust.Status{trust.Pending}, Session: r.URL.Query().Get("session")})
		jsonOut(w, list, err)
	})
	decide := func(f func(ctx context.Context, id, by, text string) (*trust.Request, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req, err := f(r.Context(), r.PathValue("id"), by(r), body(r))
			jsonOut(w, req, err)
		}
	}
	mux.HandleFunc("POST /requests/{id}/approve", decide(func(ctx context.Context, id, by, _ string) (*trust.Request, error) {
		return k.Gate.Approve(ctx, id, by)
	}))
	mux.HandleFunc("POST /requests/{id}/deny", decide(k.Gate.Deny))
	mux.HandleFunc("POST /requests/{id}/answer", decide(k.Gate.Answer))

	mux.HandleFunc("POST /sessions/take", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]bool{"held": true}, k.Gateway.Take(r.Context(), r.URL.Query().Get("session")))
	})
	mux.HandleFunc("POST /sessions/release", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]bool{"held": false}, k.Gateway.Release(r.Context(), r.URL.Query().Get("session")))
	})
	mux.HandleFunc("POST /sessions/reply", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		jsonOut(w, map[string]bool{"sent": true}, k.Gateway.Reply(r.Context(), q.Get("session"), q.Get("channel"), q.Get("chat_id"), body(r)))
	})
	mux.HandleFunc("GET /sessions/held", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, k.Gateway.HeldSessions(), nil)
	})

	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		if k.Tasks == nil {
			jsonOut(w, []any{}, nil)
			return
		}
		list, err := k.Tasks.List(r.Context(), delegate.TaskFilter{Session: r.URL.Query().Get("session"), Limit: 50})
		jsonOut(w, list, err)
	})
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		if k.Jobs == nil {
			jsonOut(w, []any{}, nil)
			return
		}
		list, err := k.Jobs.List(r.Context(), schedule.Filter{Session: r.URL.Query().Get("session")})
		jsonOut(w, list, err)
	})
	jobAction := func(f func(ctx context.Context, id string) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if k.Scheduler == nil {
				http.Error(w, "planificateur désactivé", http.StatusNotFound)
				return
			}
			jsonOut(w, map[string]string{"id": r.PathValue("id")}, f(r.Context(), r.PathValue("id")))
		}
	}
	mux.HandleFunc("POST /jobs/{id}/pause", jobAction(func(ctx context.Context, id string) error { return k.Scheduler.Pause(ctx, id) }))
	mux.HandleFunc("POST /jobs/{id}/resume", jobAction(func(ctx context.Context, id string) error { return k.Scheduler.Resume(ctx, id) }))
	mux.HandleFunc("DELETE /jobs/{id}", jobAction(func(ctx context.Context, id string) error {
		ok, err := k.Jobs.Delete(ctx, id)
		if err == nil && !ok {
			err = schedule.ErrNotFound
		}
		return err
	}))

	mux.Handle("GET /events", event.Handler(k.Bus, event.HandlerConfig{Filter: event.QueryFilter}))
	mux.Handle("GET /costs", event.CostHandler(k.Meter))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "non autorisé", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
