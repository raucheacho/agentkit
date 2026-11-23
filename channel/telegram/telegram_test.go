package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

type fakeAPI struct {
	mu       sync.Mutex
	updates  []string
	sent     []map[string]any
	offsets  []float64
	conflict bool
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botSECRET/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/botSECRET/")
		raw, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case "getUpdates":
			if f.conflict {
				_, _ = io.WriteString(w, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`)
				return
			}
			f.offsets = append(f.offsets, p["offset"].(float64))
			if len(f.updates) == 0 {
				time.Sleep(5 * time.Millisecond)
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
				return
			}
			u := f.updates[0]
			f.updates = f.updates[1:]
			_, _ = io.WriteString(w, `{"ok":true,"result":[`+u+`]}`)
		case "sendMessage":
			f.sent = append(f.sent, p)
			if p["parse_mode"] == "HTML" && strings.Contains(p["text"].(string), "casse") {
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
				return
			}
			if p["text"] == "bloqué" {
				_, _ = io.WriteString(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	})
}

func TestReceive(t *testing.T) {
	api := &fakeAPI{updates: []string{
		`{"update_id":10,"message":{"message_id":1,"date":1700000000,"from":{"id":7,"username":"Ana","first_name":"Ana"},"chat":{"id":7,"type":"private"},"text":"salut"}}`,
		`{"update_id":11,"message":{"message_id":2,"from":{"id":8,"username":"intrus"},"chat":{"id":8,"type":"private"},"text":"spam"}}`,
		`{"update_id":12,"message":{"message_id":3,"from":{"id":7,"username":"ana"},"chat":{"id":-5,"type":"group"},"caption":"regarde","photo":[{"file_id":"small"},{"file_id":"big"}]}}`,
	}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	tg := New(Config{Token: "SECRET", BaseURL: srv.URL, AllowFrom: []string{"@ana"}, PollTimeout: time.Second})

	got := make(chan channel.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tg.Run(ctx, func(_ context.Context, m channel.Message) { got <- m }) }()

	a, b := <-got, <-got
	cancel()
	<-done
	if a.Text != "salut" || a.ChatID != "7" || a.ID != "7:1" || a.Group || a.SenderName != "Ana" {
		t.Fatalf("premier : %+v", a)
	}
	if b.Text != "regarde" || !b.Group || len(b.Media) != 1 || b.Media[0].Ref != "big" || b.Media[0].URL != "" {
		t.Fatalf("second : %+v", b)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.offsets[0] != 0 || api.offsets[1] != 11 || api.offsets[2] != 12 {
		t.Fatalf("offsets : %v", api.offsets)
	}
}

func TestConflictStopsAndTokenNeverLeaks(t *testing.T) {
	api := &fakeAPI{conflict: true}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	err := New(Config{Token: "SECRET", BaseURL: srv.URL}).Run(context.Background(), func(context.Context, channel.Message) {})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 409 {
		t.Fatalf("erreur : %v", err)
	}

	// Serveur injoignable : l'erreur réseau ne doit pas contenir le jeton.
	dead := New(Config{Token: "SECRET", BaseURL: "http://127.0.0.1:1"})
	err = dead.Send(context.Background(), channel.Outgoing{ChatID: "1", Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("erreur : %v", err)
	}
}

func TestSend(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	tg := New(Config{Token: "SECRET", BaseURL: srv.URL})
	ctx := context.Background()

	if err := tg.Send(ctx, channel.Outgoing{ChatID: "7", Text: "**Prix** : 3 < 5 & `x`"}); err != nil {
		t.Fatal(err)
	}
	if err := tg.Send(ctx, channel.Outgoing{ChatID: "7", Text: "mise en forme casse"}); err != nil {
		t.Fatal(err)
	}
	err := tg.Send(ctx, channel.Outgoing{ChatID: "7", Text: "bloqué"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 403 {
		t.Fatalf("erreur : %v", err)
	}
	if err := tg.Send(ctx, channel.Outgoing{ChatID: "abc", Text: "x"}); err == nil {
		t.Fatal("conversation invalide acceptée")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.sent[0]["text"] != "<b>Prix</b> : 3 &lt; 5 &amp; <code>x</code>" {
		t.Fatalf("html : %v", api.sent[0]["text"])
	}
	if len(api.sent) != 4 || api.sent[2]["parse_mode"] != nil || api.sent[2]["text"] != "mise en forme casse" {
		t.Fatalf("repli texte brut : %+v", api.sent)
	}
}
