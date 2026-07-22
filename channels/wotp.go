package channels

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
	"time"

	"github.com/rs/zerolog/log"

	"github.com/raucheacho/agentkit/bus"
)

// WotpConfig configures the wotp (github.com/raucheacho/wotp) instance
// dedicated to chat transport — a separate instance/number than the one the
// backend uses for OTP (see backend/pkg/wotpclient), to keep the two traffic
// patterns (conversational replies vs. OTP sends) from sharing a ban risk.
type WotpConfig struct {
	ChatURL       string
	ChatAPIKey    string
	WebhookSecret string
}

// Wotp implements Channel using wotp as the WhatsApp transport: inbound
// messages arrive via Webhook (mounted on the agent's own HTTP server),
// outbound messages go through wotp's REST API. Unlike a whatsmeow-style
// channel, there is no in-process WhatsApp session/pairing to manage here —
// that lives in wotp itself (with its own dashboard).
type Wotp struct {
	config WotpConfig
	bus    *bus.Bus
	client *http.Client
}

func NewWotp(cfg WotpConfig, b *bus.Bus) *Wotp {
	return &Wotp{
		config: cfg,
		bus:    b,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (w *Wotp) Name() string { return "whatsapp" }

// Start is a no-op: inbound messages arrive via Webhook, mounted separately
// on the agent's HTTP server (see cmd/agent/main.go) — there's no polling
// loop to run here.
func (w *Wotp) Start(ctx context.Context) error { return nil }

func (w *Wotp) Stop() error { return nil }

// Send posts a text message to wotp's outbound API (POST /v1/messages/send).
func (w *Wotp) Send(ctx context.Context, msg *bus.OutboundMessage) error {
	body, err := json.Marshal(map[string]string{
		"phone": msg.ChatID,
		"type":  "text",
		"text":  msg.Content,
	})
	if err != nil {
		return fmt.Errorf("marshal wotp send request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.config.ChatURL+"/v1/messages/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build wotp send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", w.config.ChatAPIKey)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("wotp send failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wotp send failed: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SetPresence sets the typing indicator for a chat via POST /v1/messages/presence,
// without sending a message — call before a slow LLM turn so the customer
// sees "typing…" while the agent thinks. state is "typing" or "paused".
func (w *Wotp) SetPresence(ctx context.Context, phone, state string) error {
	body, err := json.Marshal(map[string]string{"phone": phone, "state": state})
	if err != nil {
		return fmt.Errorf("marshal wotp presence request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.config.ChatURL+"/v1/messages/presence", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build wotp presence request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", w.config.ChatAPIKey)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("wotp presence failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wotp presence failed: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// wotpWebhookPayload mirrors wotp's outbound webhook shape: an envelope
// ({event, timestamp, data}) wrapping the whatsapp.Event it forwards
// (itself {type, phone, message_id, at, data}), whose innermost Data carries
// the actual message content plus, once routeInboundMessage has enriched it,
// conversation_id/conversation_state. Two nesting levels, both named "data" —
// see core/internal/webhooks/webhooks.go and core/internal/api/conversations.go
// in the wotp repo.
type wotpWebhookPayload struct {
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

// Webhook handles wotp's message.received callbacks. Deliberately minimal —
// verify, translate, push to the bus, respond — so it stays fast under
// concurrent conversation load; the actual LLM processing happens
// asynchronously off the bus (see cmd/agent/main.go's dispatch loop).
//
// wotp never stops forwarding message.received just because a human has
// taken the conversation over (see wotp's README, "Conversations & human
// takeover") — it's up to us to read conversation_state and stay quiet
// instead. A shared-location message already arrives as plain text here
// ("latitude, longitude", or a place name if the sender picked a named pin
// instead of their live position) — wotp renders that server-side, we don't
// need to parse a location payload ourselves.
func (w *Wotp) Webhook(wr http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		wr.WriteHeader(http.StatusBadRequest)
		return
	}

	if !w.validSignature(r, body) {
		wr.WriteHeader(http.StatusUnauthorized)
		return
	}

	var payload wotpWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		wr.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Event != "" && payload.Event != "message.received" {
		wr.WriteHeader(http.StatusOK)
		return
	}
	if payload.Data.Phone == "" {
		wr.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Data.Data.ConversationState == "human" {
		log.Info().Str("phone", payload.Data.Phone).Str("conversation_id", payload.Data.Data.ConversationID).
			Msg("wotp: conversation held by a human, skipping auto-reply")
		wr.WriteHeader(http.StatusOK)
		return
	}

	content := strings.TrimSpace(payload.Data.Data.Text)
	if content == "" {
		wr.WriteHeader(http.StatusOK)
		return
	}

	w.bus.PublishInbound(&bus.InboundMessage{
		Channel:   "whatsapp",
		SenderID:  payload.Data.Phone,
		ChatID:    payload.Data.Phone,
		Content:   content,
		Timestamp: time.Now(),
	})

	wr.WriteHeader(http.StatusOK)
}

// validSignature checks the HMAC-SHA256 of the raw request body against the
// hex-encoded X-Wotp-Signature header (see core/internal/webhooks/webhooks.go
// in the wotp repo — it signs with the same secret configured as this
// project's webhook secret). Skipped (always valid) when no secret is
// configured, e.g. for local testing.
func (w *Wotp) validSignature(r *http.Request, body []byte) bool {
	if w.config.WebhookSecret == "" {
		return true
	}

	mac := hmac.New(sha256.New, []byte(w.config.WebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	got := r.Header.Get("X-Wotp-Signature")
	return got != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
