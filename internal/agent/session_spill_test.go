package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ndx-technologies/lean-sandbox/api"
)

func spillFileCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(spillDir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	return len(entries)
}

func TestRunOutputUnderCap(t *testing.T) {
	s := NewSession()

	before := spillFileCount(t)
	r, err := s.RunRequest(t.Context(), api.RunRequest{Command: "echo hello", MaxStdOut: 1024, MaxStdErr: 1024})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.Stdout != "hello\n" {
		t.Errorf("stdout=%q want %q", r.Stdout, "hello\n")
	}
	if r.StdoutPath != "" || r.StderrPath != "" {
		t.Errorf("unexpected spill paths: %q %q", r.StdoutPath, r.StderrPath)
	}
	if after := spillFileCount(t); after != before {
		t.Errorf("spill files created for output under the cap: %d -> %d", before, after)
	}
}

func TestRunSpillsStdout(t *testing.T) {
	s := NewSession()
	const cmd = "seq 1 1000"

	full, err := s.RunRequest(t.Context(), api.RunRequest{Command: cmd})
	if err != nil {
		t.Fatalf("uncapped run: %v", err)
	}

	r, err := s.RunRequest(t.Context(), api.RunRequest{Command: cmd, MaxStdOut: 100, MaxStdErr: 100})
	if err != nil {
		t.Fatalf("capped run: %v", err)
	}
	defer os.Remove(r.StdoutPath)

	if r.StdoutPath == "" {
		t.Fatal("expected stdout to spill")
	}
	if r.ExitCode != 0 {
		t.Errorf("exit=%d want 0", r.ExitCode)
	}
	if r.StderrPath != "" {
		t.Errorf("stderr spilled unexpectedly: %q", r.StderrPath)
	}
	if !strings.Contains(r.Stdout, r.StdoutPath) {
		t.Errorf("notice %q does not name the spill path", r.Stdout)
	}
	if len(r.Stdout) > 200 {
		t.Errorf("notice is %d bytes, want a small message", len(r.Stdout))
	}

	b, err := os.ReadFile(r.StdoutPath)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if string(b) != full.Stdout {
		t.Errorf("spill file holds %d bytes, want the full %d bytes of output", len(b), len(full.Stdout))
	}
}

func TestRunSpillsLargeOutput(t *testing.T) {
	s := NewSession()

	const (
		lines    = 200_000
		lineText = "ndx-lean-sandbox-large-output-line"
	)
	cmd := fmt.Sprintf("yes %s | head -n %d", lineText, lines)
	want := lines * (len(lineText) + 1)

	r, err := s.RunRequest(t.Context(), api.RunRequest{Command: cmd, MaxStdOut: 1024})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer os.Remove(r.StdoutPath)

	if r.StdoutPath == "" {
		t.Fatal("expected stdout to spill")
	}
	if r.ExitCode != 0 {
		t.Errorf("exit=%d want 0", r.ExitCode)
	}
	if len(r.Stdout) > 256 {
		t.Errorf("response is %d bytes for %d bytes of output", len(r.Stdout), want)
	}
	t.Logf("output=%d bytes, response=%d bytes, file=%s", want, len(r.Stdout), r.StdoutPath)
	t.Logf("notice: %s", strings.TrimSpace(r.Stdout))
	if !strings.Contains(r.Stdout, strconv.Itoa(want)) {
		t.Errorf("notice %q does not report the total size %d", r.Stdout, want)
	}

	fi, err := os.Stat(r.StdoutPath)
	if err != nil {
		t.Fatalf("stat spill file: %v", err)
	}
	if got := fi.Size(); got != int64(want) {
		t.Errorf("spill file holds %d bytes, want %d", got, want)
	}
}

func TestRunSpillsStderrOnly(t *testing.T) {
	s := NewSession()
	r, err := s.RunRequest(t.Context(), api.RunRequest{
		Command:   "for i in $(seq 1 500); do echo e$i >&2; done; echo ok",
		MaxStdOut: 1 << 20,
		MaxStdErr: 16,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer os.Remove(r.StderrPath)

	if r.StderrPath == "" {
		t.Fatal("expected stderr to spill")
	}
	if r.Stdout != "ok\n" {
		t.Errorf("stdout=%q want %q", r.Stdout, "ok\n")
	}
	if r.StdoutPath != "" {
		t.Errorf("stdout spilled unexpectedly: %q", r.StdoutPath)
	}

	b, err := os.ReadFile(r.StderrPath)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if !strings.Contains(string(b), "e500") {
		t.Errorf("spill file is missing the end of stderr (%d bytes)", len(b))
	}
}

func TestRunSpillKeepsExitCode(t *testing.T) {
	s := NewSession()
	r, err := s.RunRequest(t.Context(), api.RunRequest{Command: "seq 1 1000; exit 7", MaxStdOut: 64, MaxStdErr: 64})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer os.Remove(r.StdoutPath)

	if r.ExitCode != 7 {
		t.Errorf("exit=%d want 7", r.ExitCode)
	}
	if r.StdoutPath == "" {
		t.Fatal("expected stdout to spill")
	}
}

func TestRunStreamUncapped(t *testing.T) {
	s := NewSession()
	events, err := s.Stream(t.Context(), "seq 1 1000")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var stdout strings.Builder
	for ev := range events {
		if ev.Type == "stdout" {
			stdout.WriteString(ev.Data)
		}
	}
	if got := strings.Count(stdout.String(), "\n"); got != 1000 {
		t.Errorf("streamed %d lines, want 1000", got)
	}
}
