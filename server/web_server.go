// Package server exposes two things on the same HTTP server:
//   - a small admin API for apps/admin (JWT-protected, RequireAdmin) to
//     check status and change the model,
//   - the wotp webhook (validated by its own shared secret, not admin JWT —
//     see pkg/channels.Wotp.Webhook) for inbound WhatsApp messages.
package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/httpauth"
)

// WebServer serves the agent's admin API and the wotp webhook.
type WebServer struct {
	router *chi.Mux
	port   string
}

// NewWebServer creates the HTTP server. jwtSecret protects only the /api
// routes (see SetupRoutes) — the webhook route mounted separately validates
// its own shared secret.
func NewWebServer(port string, jwtSecret string, corsOrigins []string) *WebServer {
	if port == "" {
		port = "8090"
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   corsOrigins,
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "apikey"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	return &WebServer{router: r, port: port}
}

// SetupRoutes configures the admin API routes, behind RequireAdmin.
func (s *WebServer) SetupRoutes(h *APIHandlers, jwtSecret string) {
	s.router.Route("/api", func(r chi.Router) {
		r.Use(httpauth.RequireAdmin(jwtSecret))

		r.Get("/status", h.Status)
		r.Get("/config", h.GetConfig)
		r.Post("/config", h.UpdateConfig)
	})
}

// WebhookHandler mounts a webhook route outside the admin-protected /api
// group — it's called by wotp, not an admin user, and authenticates via its
// own shared secret (internal/config.WotpConfig.WebhookSecret).
func (s *WebServer) WebhookHandler(path string, handler http.HandlerFunc) {
	s.router.Post(path, handler)
}

// Start starts the server.
func (s *WebServer) Start() error {
	return http.ListenAndServe(":"+s.port, s.router)
}

// APIHandlers contains the admin API handlers. It tracks the current model
// name itself (guarded by mu) instead of reaching into any app config, so
// this package stays app-agnostic.
type APIHandlers struct {
	Agent *agent.Agent

	mu    sync.RWMutex
	model string
}

// NewAPIHandlers creates the admin API handlers. initialModel is the model
// name the provider was configured with at startup.
func NewAPIHandlers(ag *agent.Agent, initialModel string) *APIHandlers {
	return &APIHandlers{Agent: ag, model: initialModel}
}

func (h *APIHandlers) currentModel() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.model
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func decodeJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// Status returns the current model in use. WhatsApp connection status now
// lives in wotp's own dashboard, not here.
func (h *APIHandlers) Status(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{
		"model": h.currentModel(),
	})
}

// GetConfig returns the editable configuration (model name only — the LLM
// API key stays environment-only, never exposed or editable via the API).
func (h *APIHandlers) GetConfig(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, 200, map[string]interface{}{"model": h.currentModel()})
}

// UpdateConfig updates the model name at runtime.
func (h *APIHandlers) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	if req.Model != "" {
		h.mu.Lock()
		h.model = req.Model
		h.mu.Unlock()
		if h.Agent != nil {
			h.Agent.UpdateModel(req.Model)
		}
	}

	respondJSON(w, 200, map[string]interface{}{"model": h.currentModel()})
}
