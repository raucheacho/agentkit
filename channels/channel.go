package channels

import (
	"context"

	"github.com/raucheacho/agentkit/bus"
)

// Channel définit une interface de canal de communication
type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop() error
	Send(ctx context.Context, msg *bus.OutboundMessage) error
}
