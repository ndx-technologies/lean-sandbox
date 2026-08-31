// Package agent implements the in-pod sandbox agent: persistent bash
// sessions, command execution, and file read/write over HTTP.
//
// Session persistence model: every Run spawns a fresh `bash -c` that first
// re-exports the session's persisted environment, cd's to the persisted
// working directory, runs the requested command, then dumps the resulting
// env/cwd/exit-code via markers. So `cd` and `export` survive across runs
// without keeping a long-lived interactive shell process.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/ndx-technologies/lean-sandbox/api"
)

const (
	markerEnvStart  = "__LEAN_ENV_START__"
	markerEnvEnd    = "__LEAN_ENV_END__"
	markerPwdPrefix = "__LEAN_PWD__:"
	markerExitPref  = "__LEAN_EXIT__:"
)

// sessionVarsNeverPersist are shell display vars that should not carry over.
var sessionVarsNeverPersist = map[string]bool{
	"PS1": true, "PS2": true, "PS3": true, "PS4": true,
	"PROMPT_COMMAND": true, "_": true, "SHLVL": true,
}

// Session is a persistent bash session. All methods are safe for concurrent use.
type Session struct {
	ID api.SessionID

	mu  sync.Mutex
	env map[string]string
	cwd string
}

// NewSession creates a session starting in the container's default working
// directory (the process cwd); use `cd`/`pwd` to navigate.
func NewSession() *Session {
	return &Session{
		ID:  api.NewSessionID(),
		env: snapshotEnv(os.Environ()),
	}
}

// Result is the outcome of a single command run. Cwd/env are persisted on the
// session server-side but not exposed; read them from stdout if the caller
// needs them.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes command in the session, persisting env/cwd across calls.
// It is a convenience wrapper around Stream that aggregates all events.
func (s *Session) Run(ctx context.Context, command string, extraEnv []string) (*Result, error) {
	events, err := s.Stream(ctx, command, extraEnv)
	if err != nil {
		return nil, err
	}
	var stdout, stderr strings.Builder
	var exitCode int
	var runErr error
	for ev := range events {
		switch ev.Type {
		case "stdout":
			stdout.WriteString(ev.Data)
		case "stderr":
			stderr.WriteString(ev.Data)
		case "done":
			exitCode = ev.ExitCode
			if ev.Error != "" {
				runErr = errors.New(ev.Error)
			}
		}
	}
	return &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, runErr
}

// Stream runs command and streams stdout/stderr events, ending with a "done"
// event carrying the exit code. The channel is closed after the "done" event.
// The session env/cwd are updated server-side when the stream ends. Canceling
// ctx (e.g. context.WithTimeout) kills the whole process group.
func (s *Session) Stream(ctx context.Context, command string, extraEnv []string) (<-chan api.StreamEvent, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("empty command")
	}
	s.mu.Lock()
	script := buildScript(command, s.env, s.cwd, extraEnv)
	s.mu.Unlock()

	scriptFile, err := os.CreateTemp("", "lean-sandbox-*.sh")
	if err != nil {
		return nil, fmt.Errorf("create script: %w", err)
	}
	scriptPath := scriptFile.Name()
	if _, err := scriptFile.WriteString(script); err != nil {
		_ = scriptFile.Close()
		return nil, fmt.Errorf("write script: %w", err)
	}
	_ = scriptFile.Close()

	cmd := exec.Command("bash", "--noprofile", "--norc", scriptPath)
	cmd.SysProcAttr = procAttr()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = os.Remove(scriptPath)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = os.Remove(scriptPath)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = os.Remove(scriptPath)
		return nil, fmt.Errorf("start: %w", err)
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	events := make(chan api.StreamEvent, 64)
	go s.pump(ctx, cmd, scriptPath, stdoutR, stderrR, events)
	return events, nil
}

// pump drives the process to completion, streaming events and finalizing the
// session env/cwd. It owns cleanup of the temp script and pipes.
func (s *Session) pump(
	ctx context.Context,
	cmd *exec.Cmd,
	scriptPath string,
	stdoutR, stderrR *os.File,
	events chan<- api.StreamEvent,
) {
	defer os.Remove(scriptPath)
	defer close(events)
	defer stdoutR.Close()
	defer stderrR.Close()

	// stdout: forward user output, capture env/pwd markers.
	envOut := map[string]string{}
	var pwdOut string
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		inEnv := false
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == markerEnvStart:
				inEnv = true
				continue
			case line == markerEnvEnd:
				inEnv = false
				continue
			case strings.HasPrefix(line, markerPwdPrefix):
				pwdOut = strings.TrimPrefix(line, markerPwdPrefix)
				continue
			case strings.HasPrefix(line, markerExitPref):
				continue
			}
			if inEnv {
				if k, v, ok := parseExportLine(line); ok {
					envOut[k] = v
				}
				continue
			}
			select {
			case events <- api.StreamEvent{Type: "stdout", Data: line + "\n"}:
			case <-ctx.Done():
				return
			}
		}
		_ = sc.Err() // read error on the pipe is surfaced via the process exit
	}()

	// stderr: forward user error output.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderrR)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			select {
			case events <- api.StreamEvent{Type: "stderr", Data: line + "\n"}:
			case <-ctx.Done():
				return
			}
		}
		_ = sc.Err() // read error on the pipe is surfaced via the process exit
	}()

	err := runGroup(ctx, cmd)
	<-stdoutDone
	<-stderrDone

	if ctx.Err() != nil {
		events <- api.StreamEvent{Type: "done", ExitCode: -1, Error: ctx.Err().Error()}
		return
	}

	exitCode := 0
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = ee.ExitCode()
		} else {
			events <- api.StreamEvent{Type: "done", ExitCode: -1, Error: err.Error()}
			return
		}
	}

	s.mu.Lock()
	if len(envOut) > 0 {
		s.env = envOut
	}
	if pwdOut != "" {
		s.cwd = pwdOut
	}
	s.mu.Unlock()

	events <- api.StreamEvent{
		Type:     "done",
		ExitCode: exitCode,
	}
}

// Close is a no-op placeholder for the session lifecycle (no long-lived process).
func (s *Session) Close() {}

// buildScript wraps the user command so env/cwd persist and markers delimit state.
// Extra env vars are applied for the run but unset before the snapshot dump so
// they never leak into the persisted session env.
func buildScript(command string, env map[string]string, cwd string, extraEnv []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	extraKeys := make([]string, 0, len(extraEnv))
	for _, kv := range extraEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		b.WriteString("export ")
		b.WriteString(shellEscape(k))
		b.WriteString("=")
		b.WriteString(shellEscape(v))
		b.WriteString("\n")
		extraKeys = append(extraKeys, k)
	}
	for _, k := range sortedKeys(env) {
		if sessionVarsNeverPersist[k] {
			continue
		}
		b.WriteString("export ")
		b.WriteString(shellEscape(k))
		b.WriteString("=")
		b.WriteString(shellEscape(env[k]))
		b.WriteString("\n")
	}
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(shellEscape(cwd))
		b.WriteString("\n")
	}
	b.WriteString(command)
	if !strings.HasSuffix(command, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("__lean_exit=$?\n")
	// Drop per-run extra env from the persisted snapshot.
	for _, k := range extraKeys {
		b.WriteString("unset ")
		b.WriteString(shellEscape(k))
		b.WriteString("\n")
	}
	b.WriteString("printf '%s\\n' \"" + markerEnvStart + "\"\n")
	b.WriteString("export -p\n")
	b.WriteString("printf '%s\\n' \"" + markerEnvEnd + "\"\n")
	b.WriteString("printf '" + markerPwdPrefix + "%s\\n' \"$(pwd)\"\n")
	b.WriteString("printf '" + markerExitPref + "%s\\n' \"$__lean_exit\"\n")
	b.WriteString("exit $__lean_exit\n")
	return b.String()
}

// parseExportLine parses `declare -x NAME="value"` from `export -p`.
func parseExportLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "declare -x ")
	line = strings.TrimPrefix(line, "export ")
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", "", false
	}
	v = strings.Trim(v, "\"")
	// Unescape basic \n \t \\ \" sequences.
	v = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\\`, `\`, `\"`, `"`).Replace(v)
	return k, v, true
}

func snapshotEnv(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shellEscape single-quotes a value for safe embedding in the script.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
