//go:build unix

package local

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/raucheacho/agentkit/computer"
)

// run lance la commande dans son propre groupe de processus : à la fin, au
// délai ou à l'annulation, tout le groupe est arrêté, y compris ce que la
// commande a laissé en arrière-plan.
func run(ctx context.Context, cmd *exec.Cmd) (computer.ExecResult, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := -cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		go func() {
			time.Sleep(3 * time.Second)
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}()
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return computer.ExecResult{ExitCode: -1}, err
	}
	err := cmd.Wait()
	killGroup(cmd.Process.Pid) // restes en arrière-plan
	res := computer.ExecResult{ExitCode: cmd.ProcessState.ExitCode()}
	var exit *exec.ExitError
	if err == nil || errors.As(err, &exit) || ctx.Err() != nil {
		return res, nil
	}
	return res, err
}

// killGroup tue le groupe jusqu'à ce qu'il soit vide : un enfant créé
// pendant la délivrance du signal peut y échapper la première fois.
func killGroup(pgid int) {
	for i := 0; i < 40; i++ {
		if syscall.Kill(-pgid, syscall.SIGKILL) != nil { // ESRCH : groupe vide
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}
