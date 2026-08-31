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

// startAgent spins up the real agent HTTP server in-process on a random port,
// plus a fake control plane that answers keepalive (204), so Run/Stream's
// automatic lease renewal works without a real control plane.
func startAgent(t *testing.T) (*sdk.Sandbox, string) {
	t.Helper()
	agentSrv := httptest.NewServer(agent.NewServer("").Handler())
	t.Cleanup(agentSrv.Close)

	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cpSrv.Close)

	return &sdk.Sandbox{
		Sandbox:      api.Sandbox{Endpoint: agentSrv.URL},
		HTTPClient:   agentSrv.Client(),
		ControlPlane: &sdk.ControlPlane{BaseURL: cpSrv.URL, HTTPClient: cpSrv.Client()},
	}, agentSrv.URL
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

	// Persistence across runs.
	r1, err := sb.Run(ctx, "cd /tmp && export FOO=bar && pwd && echo hello")
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !strings.Contains(r1.Stdout, "hello") {
		t.Fatalf("run1 stdout=%q", r1.Stdout)
	}
	r2, err := sb.Run(ctx, "echo FOO=$FOO; pwd")
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if !strings.Contains(r2.Stdout, "FOO=bar") {
		t.Fatalf("run2 stdout=%q, env did not persist", r2.Stdout)
	}

	// Exit code propagation.
	r3, err := sb.Run(ctx, "exit 7")
	if err != nil {
		t.Fatalf("run3: %v", err)
	}
	if r3.ExitCode != 7 {
		t.Fatalf("run3 exit=%d want 7", r3.ExitCode)
	}

	// Cleanup.
	if err := sb.Close(ctx); err != nil {
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

// TestSessionReopen verifies the sandbox's single session is reused across Run
// calls and a fresh one can be started after Close.
func TestSessionReopen(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := context.Background()

	if _, err := sb.Run(ctx, "echo first"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := sb.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}
	// After closing, a fresh session can be started on the same sandbox.
	if _, err := sb.Run(ctx, "echo second"); err != nil {
		t.Fatalf("run after close: %v", err)
	}
}
