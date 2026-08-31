//go:build linux

package agent

import (
	"context"
	"os/exec"
	"syscall"
)

// procAttr gives the command its own process group so a timeout kill does not
// take down the agent itself, and SIGKILL the whole group on cancellation.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// runGroup waits for a started cmd, killing the whole process group if ctx is
// cancelled first. This matters for `sleep 30`-style commands whose child
// survives bash being killed by exec.CommandContext alone.
func runGroup(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return <-done
	}
}
