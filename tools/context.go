package tools

import "context"

type contextKey struct{}

// ConversationContext identifies which conversation a tool call belongs to
// (for WhatsApp, ChatID is the phone number). Carried via context.Context —
// a value scoped to a single call/goroutine — rather than a mutable field on
// the shared Manager: the Manager is one instance reused by every
// conversation, and concurrent conversations run in separate goroutines
// (see internal/agent), so a shared mutable field would let one
// conversation's tool calls observe another's identity mid-flight.
type ConversationContext struct {
	Channel string
	ChatID  string
}

// WithConversationContext attaches cc to ctx.
func WithConversationContext(ctx context.Context, cc ConversationContext) context.Context {
	return context.WithValue(ctx, contextKey{}, cc)
}

// ConversationFromContext retrieves the ConversationContext attached by
// WithConversationContext, if any.
func ConversationFromContext(ctx context.Context) (ConversationContext, bool) {
	cc, ok := ctx.Value(contextKey{}).(ConversationContext)
	return cc, ok
}
