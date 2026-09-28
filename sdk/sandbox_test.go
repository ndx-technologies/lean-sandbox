package sdk_test

import (
	"context"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/agent"
	"github.com/ndx-technologies/lean-sandbox/internal/jwt"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

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

func mustAgent(t *testing.T, sandboxID api.SandboxID, pubKeyB64 string) *agent.Server {
	t.Helper()
	srv, err := agent.NewServer(sandboxID, pubKeyB64)
	if err != nil {
		t.Fatalf("agent.NewServer: %v", err)
	}
	return srv
}

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

	r3, err := sb.Run(ctx, "exit 7")
	if err != nil {
		t.Fatalf("run3: %v", err)
	}
	if r3.ExitCode != 7 {
		t.Fatalf("run3 exit=%d want 7", r3.ExitCode)
	}

	if err := sb.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestFileReadWrite(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := t.Context()

	path := filepath.Join(t.TempDir(), "lean-sbx-test.txt")
	if err := sb.WriteFile(ctx, path, []byte("content-123")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	data, err := sb.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "content-123" {
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

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d want 200 (anonymous)", resp.StatusCode)
	}

	const endpoint = "/v1/file"

	if got := getStatus(t, srv.URL+endpoint, ""); got != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d want 401", got)
	}

	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, api.NewSandboxID().String(), time.Hour)); got != http.StatusUnauthorized {
		t.Fatalf("wrong-sub status=%d want 401", got)
	}

	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, sandboxID.String(), -time.Minute)); got != http.StatusUnauthorized {
		t.Fatalf("expired status=%d want 401", got)
	}

	if got := getStatus(t, srv.URL+endpoint, mustSign(t, priv, sandboxID.String(), time.Hour)); got != http.StatusBadRequest {
		t.Fatalf("valid status=%d want 400", got)
	}
}

func TestSessionReopen(t *testing.T) {
	sb, _ := startAgent(t)
	ctx := context.Background()

	if _, err := sb.Run(ctx, "echo first"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := sb.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if _, err := sb.Run(ctx, "echo second"); err != nil {
		t.Fatalf("run after close: %v", err)
	}
}
