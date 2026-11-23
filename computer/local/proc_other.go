//go:build !unix

package local

import (
	"context"
	"errors"
	"os/exec"

	"github.com/raucheacho/agentkit/computer"
)

func run(ctx context.Context, cmd *exec.Cmd) (computer.ExecResult, error) {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	err := cmd.Run()
	res := computer.ExecResult{ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	var exit *exec.ExitError
	if err == nil || errors.As(err, &exit) || ctx.Err() != nil {
		return res, nil
	}
	return res, err
}
