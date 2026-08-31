package sdk_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/agent"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

// startAgent spins up the real agent HTTP server in-process on a random port.
func startAgent(t *testing.T) (*sdk.Sandbox, string) {
	t.Helper()
	srv := httptest.NewServer(agent.NewServer("").Handler())
	t.Cleanup(srv.Close)
	return &sdk.Sandbox{Sandbox: api.Sandbox{Endpoint: srv.URL}, HTTPClient: srv.Client()}, srv.URL
}

func TestAgentHealthz(t *testing.T) {
	_, url := startAgent(t)
	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d want 200", resp.StatusCode)
	}
}

func TestSessionLifecycle(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := context.Background()

	sess, err := sb.NewSession(ctx)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if sess.ID.IsZero() {
		t.Fatal("empty session id")
	}

	// Persistence across runs.
	r1, err := sess.Run(ctx, "cd /tmp && export FOO=bar && pwd && echo hello")
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !strings.Contains(r1.Stdout, "hello") {
		t.Fatalf("run1 stdout=%q", r1.Stdout)
	}
	r2, err := sess.Run(ctx, "echo FOO=$FOO; pwd")
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if !strings.Contains(r2.Stdout, "FOO=bar") {
		t.Fatalf("run2 stdout=%q, env did not persist", r2.Stdout)
	}

	// Exit code propagation.
	r3, err := sess.Run(ctx, "exit 7")
	if err != nil {
		t.Fatalf("run3: %v", err)
	}
	if r3.ExitCode != 7 {
		t.Fatalf("run3 exit=%d want 7", r3.ExitCode)
	}

	// Cleanup.
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestFileReadWrite(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := context.Background()

	if err := sb.WriteFile(ctx, "/tmp/lean-sbx-test.txt", "content-123"); err != nil {
		t.Fatalf("write file: %v", err)
	}
	data, err := sb.ReadFile(ctx, "/tmp/lean-sbx-test.txt")
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if data != "content-123" {
		t.Fatalf("read=%q want content-123", data)
	}
}

func TestAgentAuth(t *testing.T) {
	// Token-auth server: requests without the token must be rejected.
	srv := httptest.NewServer(agent.NewServer("secret").Handler())
	t.Cleanup(srv.Close)

	// No token -> 401.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d want 401", resp.StatusCode)
	}

	// With token -> 200.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set(api.AccessTokenHeader, "secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("auth get: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("with-token status=%d want 200", resp2.StatusCode)
	}
}

// TestSingleSessionPerPod verifies the agent refuses a second session: one pod
// = one sandbox = one session, so sessions cannot touch each other's files.
func TestSingleSessionPerPod(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := context.Background()

	sess1, err := sb.NewSession(ctx)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	// A second session on the same sandbox must fail.
	_, err = sb.NewSession(ctx)
	if err == nil {
		t.Fatal("expected second session to be rejected")
	}

	// After closing, a new session is allowed.
	if err := sess1.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if _, err := sb.NewSession(ctx); err != nil {
		t.Fatalf("session after close: %v", err)
	}
}
