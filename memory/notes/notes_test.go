package notes_test

import (
	"testing"

	"github.com/raucheacho/agentkit/memory/notes"
	"github.com/raucheacho/agentkit/memory/notes/notestest"
)

func TestMemoryContract(t *testing.T) {
	notestest.Run(t, func(*testing.T) notestest.Store { return notes.NewMemory() })
}
