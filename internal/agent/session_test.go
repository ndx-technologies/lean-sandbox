package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
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

// TestSessionNoTrailingNewline verifies output that does not end with a newline
// keeps its state markers consumed. bash glues the start marker onto the last
// line of output, and a marker missed that way leaks the whole `export -p` block
// into the response and into any spill file.
func TestSessionNoTrailingNewline(t *testing.T) {
	s := NewSession()

	r1, err := s.Run(t.Context(), "cd /tmp && printf abc")
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if r1.Stdout != "abc\n" {
		t.Errorf("stdout=%q want %q", r1.Stdout, "abc\n")
	}
	for _, leak := range []string{"declare -x", "__LEAN_", "export -p"} {
		if strings.Contains(r1.Stdout, leak) {
			t.Errorf("stdout leaked %q: %q", leak, r1.Stdout)
		}
	}

	// The trailer must still be parsed after a glued marker: cwd persistence
	// proves the pwd marker was consumed.
	r2, err := s.Run(t.Context(), "pwd")
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if !strings.Contains(r2.Stdout, "/tmp") {
		t.Errorf("cwd not persisted: %q", r2.Stdout)
	}

	// The same filtering feeds the spill file, so the env dump must not land
	// there either. 5000 bytes of output must produce a 5000-byte file plus the
	// newline that forwarding adds.
	r3, err := s.RunRequest(t.Context(), api.RunRequest{Command: `printf "x%.0s" $(seq 1 5000)`, MaxStdOut: 64})
	if err != nil {
		t.Fatalf("run3: %v", err)
	}
	defer os.Remove(r3.StdoutPath)
	if r3.StdoutPath == "" {
		t.Fatal("expected stdout to spill")
	}

	b, err := os.ReadFile(r3.StdoutPath)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if strings.Contains(string(b), "declare -x") || strings.Contains(string(b), "__LEAN_") {
		t.Errorf("spill file leaked the state block (%d bytes)", len(b))
	}
	if len(b) != 5001 {
		t.Errorf("spill file holds %d bytes, want 5001", len(b))
	}
}
