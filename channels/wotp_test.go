package channels

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/bus"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func newWebhookRequest(t *testing.T, body []byte, signature string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/wotp/message", bytes.NewReader(body))
	if signature != "" {
		req.Header.Set("X-Wotp-Signature", signature)
	}
	return req
}

const samplePayload = `{
	"event": "message.received",
	"timestamp": 1234567890,
	"data": {
		"type": "message.received",
		"phone": "212612345678",
		"message_id": "ABC123",
		"at": "2026-07-22T10:00:00Z",
		"data": {
			"text": "33.589200, -7.631800",
			"pushName": "Karim",
			"conversation_id": "conv-1",
			"conversation_state": "bot"
		}
	}
}`

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	b := bus.New()
	w := NewWotp(WotpConfig{WebhookSecret: "shh"}, b)

	req := newWebhookRequest(t, []byte(samplePayload), "not-the-right-signature")
	rec := httptest.NewRecorder()
	w.Webhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhookAcceptsValidSignature(t *testing.T) {
	b := bus.New()
	w := NewWotp(WotpConfig{WebhookSecret: "shh"}, b)

	body := []byte(samplePayload)
	req := newWebhookRequest(t, body, sign("shh", body))
	rec := httptest.NewRecorder()
	w.Webhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	select {
	case msg := <-b.Inbound():
		if msg.ChatID != "212612345678" || msg.Content != "33.589200, -7.631800" {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a message on the bus, got none")
	}
}

func TestWebhookSkipsWhenSecretUnset(t *testing.T) {
	b := bus.New()
	w := NewWotp(WotpConfig{}, b) // no WebhookSecret configured (local dev)

	body := []byte(samplePayload)
	req := newWebhookRequest(t, body, "") // no signature header at all
	rec := httptest.NewRecorder()
	w.Webhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestWebhookSkipsWhenConversationIsHuman(t *testing.T) {
	b := bus.New()
	w := NewWotp(WotpConfig{}, b)

	humanPayload := `{
		"event": "message.received",
		"data": {
			"type": "message.received",
			"phone": "212612345678",
			"data": {"text": "hello", "conversation_state": "human"}
		}
	}`
	req := newWebhookRequest(t, []byte(humanPayload), "")
	rec := httptest.NewRecorder()
	w.Webhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	select {
	case msg := <-b.Inbound():
		t.Fatalf("expected no message published while conversation is human-owned, got %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing published
	}
}

func TestWebhookIgnoresNonMessageEvents(t *testing.T) {
	b := bus.New()
	w := NewWotp(WotpConfig{}, b)

	statusPayload := `{"event": "message.delivered", "data": {"phone": "212612345678"}}`
	req := newWebhookRequest(t, []byte(statusPayload), "")
	rec := httptest.NewRecorder()
	w.Webhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	select {
	case msg := <-b.Inbound():
		t.Fatalf("expected no message published for a non message.received event, got %+v", msg)
	case <-time.After(200 * time.Millisecond):
	}
}
