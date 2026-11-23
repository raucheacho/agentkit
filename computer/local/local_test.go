package local

import (
	"testing"

	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/computer/computertest"
)

func TestContract(t *testing.T) {
	computertest.Run(t, func(t *testing.T) computer.Workspace {
		ws, err := New(Config{Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	})
}
