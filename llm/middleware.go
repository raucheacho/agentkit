package llm

import (
	"context"
	"errors"
	"time"
)

// MaxRetryAfter est l'attente la plus longue que Retry accepte quand le
// service indique lui-même quand réessayer (StatusError.RetryAfter). Au-delà
// (quota du jour épuisé, par exemple), l'erreur est renvoyée tout de suite.
var MaxRetryAfter = time.Minute

// Retry réessaie les erreurs transitoires (voir Transient) avec un délai
// exponentiel, ou le délai demandé par le service s'il en donne un (jusqu'à
// MaxRetryAfter). Une requête qui a réussi n'est jamais rejouée. Si p sait
// répondre en flux, le Provider renvoyé aussi : seule l'ouverture du flux est
// réessayée, jamais un flux déjà commencé.
func Retry(p Provider, attempts int, baseDelay time.Duration) Provider {
	if attempts < 1 {
		attempts = 1
	}
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	r := &retry{inner: p, attempts: attempts, delay: baseDelay}
	if s, ok := p.(Streamer); ok {
		return &retryStreamer{retry: r, stream: s}
	}
	return r
}

type retry struct {
	inner    Provider
	attempts int
	delay    time.Duration
}

func (r *retry) Chat(ctx context.Context, req *Request) (*Response, error) {
	var resp *Response
	err := r.do(ctx, func() (err error) {
		resp, err = r.inner.Chat(ctx, req)
		return err
	})
	return resp, err
}

func (r *retry) do(ctx context.Context, f func() error) error {
	delay := r.delay
	var err error
	for i := 0; i < r.attempts; i++ {
		if err = f(); err == nil || !Transient(err) || i == r.attempts-1 {
			return err
		}
		wait := delay
		var se *StatusError
		if errors.As(err, &se) && se.RetryAfter > 0 {
			if se.RetryAfter > MaxRetryAfter {
				return err
			}
			wait = max(wait, se.RetryAfter)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay *= 2
	}
	return err
}

type retryStreamer struct {
	*retry
	stream Streamer
}

func (r *retryStreamer) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	var ch <-chan StreamEvent
	err := r.do(ctx, func() (err error) {
		ch, err = r.stream.Stream(ctx, req)
		return err
	})
	return ch, err
}

// Fallback essaie chaque fournisseur dans l'ordre et passe au suivant sur une
// erreur transitoire. Une erreur définitive (requête invalide, clé refusée) est
// renvoyée telle quelle : elle échouerait pareil ailleurs ou masquerait un bug.
func Fallback(providers ...Provider) Provider {
	return ProviderFunc(func(ctx context.Context, req *Request) (*Response, error) {
		var errs []error
		for _, p := range providers {
			resp, err := p.Chat(ctx, req)
			if err == nil {
				return resp, nil
			}
			errs = append(errs, err)
			if !Transient(err) {
				break
			}
		}
		return nil, errors.Join(errs...)
	})
}
