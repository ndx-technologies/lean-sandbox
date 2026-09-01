package sdk_test

import (
	"context"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/agent"
	"github.com/ndx-technologies/lean-sandbox/internal/jwt"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

// startAgent spins up the real agent HTTP server in-process on a random port,
// plus a fake control plane that answers keepalive (204), so Run/Stream's
// automatic lease renewal works without a real control plane.
func startAgent(t *testing.T) (*sdk.Sandbox, string) {
	t.Helper()
	sandboxID := api.NewSandboxID()
	agentSrv := httptest.NewServer(mustAgent(t, sandboxID, "").Handler())
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

// mustAgent builds an agent server, failing the test on invalid input.
func mustAgent(t *testing.T, sandboxID api.SandboxID, pubKeyB64 string) *agent.Server {
	t.Helper()
	srv, err := agent.NewServer(sandboxID, pubKeyB64)
	if err != nil {
		t.Fatalf("agent.NewServer: %v", err)
	}
	return srv
}

// mustSign signs an RS256 JWT with the shared jwt package, failing on error.
func mustSign(t *testing.T, key *rsa.PrivateKey, sub string, ttl time.Duration) string {
	t.Helper()
	tok, err := jwt.Sign(key, sub, ttl)
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return tok
}

func getStatus(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(api.AccessTokenHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
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
	priv, pubB64, err := jwt.GenerateKey()
	if err != nil {
		t.Fatalf("jwt.GenerateKey: %v", err)
	}
	sandboxID := api.NewSandboxID()
	srv := httptest.NewServer(mustAgent(t, sandboxID, pubB64).Handler())
	t.Cleanup(srv.Close)

	// /healthz is anonymous on purpose: it backs the k8s readiness probe, and
	// kubelet probes carry no app token.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d want 200 (anonymous)", resp.StatusCode)
	}

	// Everything else is gated by the per-sandbox token (test on /v1/file).
	const endpoint = "/v1/file"

	// No token -> 401.
	if got := getStatus(t, srv.URL+endpoint, ""); got != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d want 401", got)
	}

	// Wrong sandbox id -> 401.
	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, api.NewSandboxID().String(), time.Hour)); got != http.StatusUnauthorized {
		t.Fatalf("wrong-sub status=%d want 401", got)
	}

	// Expired -> 401.
	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, sandboxID.String(), -time.Minute)); got != http.StatusUnauthorized {
		t.Fatalf("expired status=%d want 401", got)
	}

	// Valid token passes auth; handler then rejects the missing path -> 400.
	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, sandboxID.String(), time.Hour)); got != http.StatusBadRequest {
		t.Fatalf("valid status=%d want 400", got)
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
