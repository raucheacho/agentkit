package session_test

import (
	"testing"

	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/session/sessiontest"
)

func TestMemoryContract(t *testing.T) {
	sessiontest.Run(t, func(*testing.T) sessiontest.Store { return session.NewMemory() })
}
