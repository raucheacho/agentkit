package turn_test

import (
	"testing"

	"github.com/raucheacho/agentkit/turn"
	"github.com/raucheacho/agentkit/turn/turntest"
)

func TestMemoryQueue(t *testing.T) {
	turntest.Run(t, func(*testing.T) turn.Queue { return turn.NewMemory() })
}
