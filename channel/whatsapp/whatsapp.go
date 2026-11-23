// Package whatsapp est un canal WhatsApp passant par une passerelle HTTP
// (wotp) : les messages reçus arrivent par webhook, les réponses partent par
// son API REST. La session WhatsApp elle-même (appairage, numéro) est gérée
// par la passerelle, pas par l'agent.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

// Config configure le canal.
type Config struct {
	// BaseURL de la passerelle (ex. https://wa.example.com).
	BaseURL string
	APIKey  string
	// WebhookSecret vérifie la signature HMAC-SHA256 des webhooks
	// (en-tête X-Wotp-Signature). Vide : pas de vérification (tests locaux).
	WebhookSecret string
	// Name du canal (défaut "whatsapp").
	Name       string
	HTTPClient *http.Client
}

// WhatsApp est le canal.
type WhatsApp struct {
	cfg  Config
	http *http.Client

	mu   sync.Mutex
	sink channel.Sink
	ctx  context.Context
}

// New crée le canal. Montez Handler sur votre serveur HTTP pour recevoir les webhooks.
func New(cfg Config) *WhatsApp {
	if cfg.Name == "" {
		cfg.Name = "whatsapp"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &WhatsApp{cfg: cfg, http: hc}
}

func (w *WhatsApp) Name() string { return w.cfg.Name }

// MaxLength : WhatsApp accepte jusqu'à 4096 caractères par message.
func (w *WhatsApp) MaxLength() int { return 4096 }

// Run attend les webhooks jusqu'à l'annulation de ctx.
func (w *WhatsApp) Run(ctx context.Context, sink channel.Sink) error {
	w.mu.Lock()
	w.sink, w.ctx = sink, ctx
	w.mu.Unlock()
	<-ctx.Done()
	w.mu.Lock()
	w.sink = nil
	w.mu.Unlock()
	return nil
}

// Send envoie un texte.
func (w *WhatsApp) Send(ctx context.Context, out channel.Outgoing) error {
	return w.post(ctx, "/v1/messages/send", map[string]string{"phone": out.ChatID, "type": "text", "text": out.Text})
}

// Typing affiche « en train d'écrire ».
func (w *WhatsApp) Typing(ctx context.Context, chatID string) error {
	return w.post(ctx, "/v1/messages/presence", map[string]string{"phone": chatID, "state": "typing"})
}

func (w *WhatsApp) post(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", w.cfg.APIKey)
	resp, err := w.http.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp %s : %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("whatsapp %s : statut %d : %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// payload : enveloppe {event, data} autour de l'événement WhatsApp
// {type, phone, message_id, at, data}, dont le data intérieur porte le contenu.
type payload struct {
	Event string `json:"event"`
	Data  struct {
		Type      string    `json:"type"`
		Phone     string    `json:"phone"`
		MessageID string    `json:"message_id"`
		At        time.Time `json:"at"`
		Data      struct {
			Text              string `json:"text"`
			PushName          string `json:"pushName"`
			ConversationID    string `json:"conversation_id"`
			ConversationState string `json:"conversation_state"`
		} `json:"data"`
	} `json:"data"`
}

// Handler reçoit les webhooks message.received. Il vérifie, traduit, remet le
// message et répond tout de suite : le traitement se fait hors de la requête.
//
// Quand la passerelle indique qu'un humain a repris la conversation
// (conversation_state "human"), le message est ignoré.
func (w *WhatsApp) Handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		if !w.validSignature(r, body) {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		if p.Event != "" && p.Event != "message.received" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		if p.Data.Phone == "" {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		text := strings.TrimSpace(p.Data.Data.Text)
		if p.Data.Data.ConversationState == "human" || text == "" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		w.mu.Lock()
		sink, ctx := w.sink, w.ctx
		w.mu.Unlock()
		if sink == nil {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		at := p.Data.At
		if at.IsZero() {
			at = time.Now()
		}
		meta := map[string]string{}
		if p.Data.Data.ConversationID != "" {
			meta["conversation_id"] = p.Data.Data.ConversationID
		}
		sink(ctx, channel.Message{
			Channel: w.cfg.Name, ChatID: p.Data.Phone, Sender: p.Data.Phone, SenderName: p.Data.Data.PushName,
			Text: text, ID: p.Data.MessageID, At: at, Meta: meta,
		})
		rw.WriteHeader(http.StatusOK)
	})
}

func (w *WhatsApp) validSignature(r *http.Request, body []byte) bool {
	if w.cfg.WebhookSecret == "" {
		return true
	}
	mac := hmac.New(sha256.New, []byte(w.cfg.WebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	got := r.Header.Get("X-Wotp-Signature")
	return got != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

var (
	_ channel.Adapter = (*WhatsApp)(nil)
	_ channel.Typer   = (*WhatsApp)(nil)
	_ channel.Limiter = (*WhatsApp)(nil)
)
