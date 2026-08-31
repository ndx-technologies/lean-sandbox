//go:build !linux

package agent

import (
	"context"
	"os/exec"
	"syscall"
)

// Setpgid + process-group kill works on all Unix (Linux target, macOS dev).
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// runGroup waits for a started cmd, killing the whole process group if ctx is
// cancelled first.
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
