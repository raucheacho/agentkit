package observe_test

import (
	"testing"

	"github.com/raucheacho/agentkit/observe"
	"github.com/raucheacho/agentkit/observe/observetest"
)

func TestMemorySeen(t *testing.T) {
	observetest.Run(t, func(*testing.T) observe.Seen { return observe.NewMemorySeen() })
}
