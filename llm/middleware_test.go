package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flaky struct {
	fails int
	calls int
	err   error
}

func (f *flaky) Chat(context.Context, *Request) (*Response, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, f.err
	}
	return &Response{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
}

func (f *flaky) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	resp, err := f.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Response: resp}
	close(ch)
	return ch, nil
}

func TestRetry(t *testing.T) {
	f := &flaky{fails: 2, err: &StatusError{Status: 503}}
	p := Retry(f, 3, time.Millisecond)
	if _, err := p.Chat(context.Background(), &Request{}); err != nil || f.calls != 3 {
		t.Fatalf("err=%v calls=%d", err, f.calls)
	}
	if _, ok := p.(Streamer); !ok {
		t.Fatal("Retry doit conserver le flux")
	}

	f = &flaky{fails: 5, err: &StatusError{Status: 401}}
	if _, err := Retry(f, 3, time.Millisecond).Chat(context.Background(), &Request{}); err == nil || f.calls != 1 {
		t.Fatalf("une erreur définitive ne doit pas être réessayée : calls=%d", f.calls)
	}
	if _, ok := Retry(ProviderFunc(f.Chat), 2, 0).(Streamer); ok {
		t.Fatal("un fournisseur sans flux ne doit pas en gagner un")
	}
}

func TestFallback(t *testing.T) {
	a := &flaky{fails: 9, err: &StatusError{Status: 429}}
	b := &flaky{}
	if _, err := Fallback(a, b).Chat(context.Background(), &Request{}); err != nil || b.calls != 1 {
		t.Fatalf("err=%v", err)
	}
	a = &flaky{fails: 9, err: &StatusError{Status: 400}}
	b = &flaky{}
	if _, err := Fallback(a, b).Chat(context.Background(), &Request{}); err == nil || b.calls != 0 {
		t.Fatal("une erreur définitive ne doit pas basculer")
	}
	if !Transient(errors.Join(errors.New("x"), &StatusError{Status: 502})) {
		t.Fatal("502 enveloppé doit rester transitoire")
	}
	if Transient(context.Canceled) {
		t.Fatal("annulation non transitoire")
	}
}

// Le délai demandé par le service est respecté ; s'il dépasse MaxRetryAfter
// (quota du jour épuisé), on n'attend pas : l'erreur est renvoyée tout de suite.
func TestRetryHonorsRetryAfter(t *testing.T) {
	f := &flaky{fails: 1, err: &StatusError{Status: 429, RetryAfter: 50 * time.Millisecond}}
	start := time.Now()
	if _, err := Retry(f, 3, time.Millisecond).Chat(context.Background(), &Request{}); err != nil || f.calls != 2 {
		t.Fatalf("err=%v calls=%d", err, f.calls)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("le délai demandé n'a pas été respecté")
	}
	f = &flaky{fails: 5, err: &StatusError{Status: 429, RetryAfter: 20 * time.Hour}}
	start = time.Now()
	if _, err := Retry(f, 3, time.Millisecond).Chat(context.Background(), &Request{}); err == nil || f.calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("quota épuisé : err=%v calls=%d", err, f.calls)
	}
}
