package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSessionPersistence verifies env + cwd survive across Run calls.
func TestSessionPersistence(t *testing.T) {
	s := NewSession()

	r1, err := s.Run(t.Context(), "cd /tmp && export FOO=bar && pwd && echo hello")
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if r1.ExitCode != 0 {
		t.Fatalf("run1 exit=%d want 0", r1.ExitCode)
	}
	if !strings.Contains(r1.Stdout, "hello") {
		t.Fatalf("run1 stdout=%q missing hello", r1.Stdout)
	}
	if !strings.Contains(r1.Stdout, "/tmp") {
		t.Fatalf("run1 stdout=%q missing cwd", r1.Stdout)
	}

	r2, err := s.Run(t.Context(), "echo FOO=$FOO; pwd")
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if !strings.Contains(r2.Stdout, "FOO=bar") {
		t.Fatalf("run2 stdout=%q missing persisted FOO", r2.Stdout)
	}
	if !strings.Contains(r2.Stdout, "/tmp") {
		t.Fatalf("run2 stdout=%q missing persisted cwd", r2.Stdout)
	}
}

// TestSessionExitCode verifies the process exit code propagates, including
// for `exit N` (which terminates the script before markers run).
func TestSessionExitCode(t *testing.T) {
	s := NewSession()

	for _, tc := range []struct {
		cmd  string
		want int
	}{
		{"true", 0},
		{"false", 1},
		{"exit 7", 7},
		{"exit 42", 42},
		{"sh -c 'exit 3'", 3},
	} {
		r, err := s.Run(t.Context(), tc.cmd)
		if err != nil {
			t.Fatalf("%q: %v", tc.cmd, err)
		}
		if r.ExitCode != tc.want {
			t.Errorf("%q exit=%d want %d", tc.cmd, r.ExitCode, tc.want)
		}
	}
}

// TestSessionMarkersStripped verifies marker lines never leak to user stdout.
func TestSessionMarkersStripped(t *testing.T) {
	s := NewSession()
	r, err := s.Run(t.Context(), "echo visible")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(r.Stdout, "__LEAN_") {
		t.Fatalf("stdout leaked markers: %q", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "visible") {
		t.Fatalf("stdout=%q missing command output", r.Stdout)
	}
}

// TestSessionTimeout verifies a context deadline kills the whole process group
// (the client-side equivalent of a per-command timeout).
func TestSessionTimeout(t *testing.T) {
	s := NewSession()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Run(ctx, "sleep 30")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
}

// TestSessionDefaultCwd verifies the session lands in the process cwd, not a
// broken `cd ""`.
func TestSessionDefaultCwd(t *testing.T) {
	s := NewSession()
	r, err := s.Run(t.Context(), "pwd")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(r.Stdout, "/") {
		t.Fatal("stdout missing a real path")
	}
}

// TestSessionConcurrent verifies the session is safe for concurrent use.
func TestSessionConcurrent(t *testing.T) {
	s := NewSession()
	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := s.Run(t.Context(), "echo concurrent")
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent run: %v", err)
		}
	}
}
