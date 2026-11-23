package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestWebhook(t *testing.T) {
	w := New(Config{WebhookSecret: "s"})
	got := make(chan channel.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx, func(_ context.Context, m channel.Message) { got <- m }) }()
	time.Sleep(10 * time.Millisecond)
	h := w.Handler()

	post := func(body, sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if sig != "" {
			req.Header.Set("X-Wotp-Signature", sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	msg := `{"event":"message.received","data":{"phone":"33600","message_id":"m1","data":{"text":" Bonjour ","pushName":"Ana"}}}`
	if code := post(msg, "bad"); code != http.StatusUnauthorized {
		t.Fatalf("signature fausse : %d", code)
	}
	if code := post(msg, sign("s", msg)); code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	human := `{"event":"message.received","data":{"phone":"33600","data":{"text":"x","conversation_state":"human"}}}`
	if code := post(human, sign("s", human)); code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	m := <-got
	if m.Channel != "whatsapp" || m.ChatID != "33600" || m.Text != "Bonjour" || m.SenderName != "Ana" || m.ID != "m1" {
		t.Fatalf("message : %+v", m)
	}
	select {
	case extra := <-got:
		t.Fatalf("message d'une conversation reprise en main transmis : %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSendAndTyping(t *testing.T) {
	var paths []string
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "k" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]string
		_ = json.Unmarshal(raw, &b)
		paths, bodies = append(paths, r.URL.Path), append(bodies, b)
		if b["text"] == "boom" {
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(rw, "passerelle hors ligne")
		}
	}))
	defer srv.Close()
	w := New(Config{BaseURL: srv.URL + "/", APIKey: "k"})
	ctx := context.Background()
	if err := w.Send(ctx, channel.Outgoing{ChatID: "33600", Text: "salut"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Typing(ctx, "33600"); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(ctx, channel.Outgoing{ChatID: "1", Text: "boom"}); err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "hors ligne") {
		t.Fatalf("erreur : %v", err)
	}
	if paths[0] != "/v1/messages/send" || bodies[0]["phone"] != "33600" || paths[1] != "/v1/messages/presence" || bodies[1]["state"] != "typing" {
		t.Fatalf("%v %v", paths, bodies)
	}
}
