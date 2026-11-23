package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

func start(t *testing.T, cfg Config) (*Web, *httptest.Server, chan channel.Message) {
	w := New(cfg)
	got := make(chan channel.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = w.Run(ctx, func(_ context.Context, m channel.Message) { got <- m }) }()
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(func() { srv.Close(); cancel() })
	for {
		w.mu.Lock()
		ready := w.sink != nil
		w.mu.Unlock()
		if ready {
			return w, srv, got
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPostAndIdentify(t *testing.T) {
	_, srv, got := start(t, Config{Identify: func(r *http.Request) (string, bool) {
		c, err := r.Cookie("chat")
		if err != nil {
			return "", false
		}
		return c.Value, true
	}})
	post := func(cookie, body string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/messages", strings.NewReader(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "chat", Value: cookie})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("", `{"text":"x"}`); c != http.StatusUnauthorized {
		t.Fatalf("sans identité : %d", c)
	}
	if c := post("c9", `{"text":"  "}`); c != http.StatusBadRequest {
		t.Fatalf("vide : %d", c)
	}
	if c := post("c9", `{"text":"bonjour","chat_id":"autre","id":"m1"}`); c != http.StatusAccepted {
		t.Fatalf("code : %d", c)
	}
	m := <-got
	if m.ChatID != "c9" || m.Text != "bonjour" || m.ID != "m1" || m.Channel != "web" {
		t.Fatalf("message : %+v", m)
	}
}

func readEvents(t *testing.T, url, lastID string, n int) []string {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && len(lines) < n {
		if l := sc.Text(); strings.HasPrefix(l, "id:") || strings.HasPrefix(l, "event: typing") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestEventsLiveAndReplay(t *testing.T) {
	w, srv, _ := start(t, Config{})
	ctx := context.Background()
	_ = w.Send(ctx, channel.Outgoing{ChatID: "c1", Text: "un"})
	_ = w.Send(ctx, channel.Outgoing{ChatID: "c1", Text: "deux"})
	_ = w.Send(ctx, channel.Outgoing{ChatID: "c2", Text: "autre conversation"})

	if got := readEvents(t, srv.URL+"/events?chat_id=c1", "1", 1); len(got) != 1 || got[0] != "id: 2" {
		t.Fatalf("rattrapage : %v", got)
	}

	for w.Connected("c1") { // la première connexion se ferme côté serveur
		time.Sleep(time.Millisecond)
	}
	done := make(chan []string)
	go func() { done <- readEvents(t, srv.URL+"/events?chat_id=c1", "2", 2) }()
	for !w.Connected("c1") {
		time.Sleep(time.Millisecond)
	}
	_ = w.Typing(ctx, "c1")
	_ = w.Send(ctx, channel.Outgoing{ChatID: "c1", Text: "trois"})
	if got := <-done; len(got) != 2 || got[0] != "event: typing" || got[1] != "id: 3" {
		t.Fatalf("direct : %v", got)
	}
}
