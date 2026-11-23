package schedule_test

import (
	"testing"

	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/schedule/scheduletest"
)

func TestMemoryContract(t *testing.T) {
	scheduletest.Run(t, func(*testing.T) schedule.Store { return schedule.NewMemoryStore() })
}
