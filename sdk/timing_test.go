package sdk_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

func TestTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping live-sandbox timing test")
	}
	cp := &sdk.ControlPlane{
		BaseURL:    os.Getenv("CONTROL_PLANE_URL"),
		APIKey:     os.Getenv("CONTROL_PLANE_API_KEY"),
		HTTPClient: http.DefaultClient,
	}
	ctx := context.Background()

	t0 := time.Now()
	sb, err := cp.NewSandbox(ctx, api.SandboxRequest{Image: "ubuntu:22.04"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Logf("claim (NewSandbox):      %s", time.Since(t0))
	t.Cleanup(func() { _ = cp.Delete(context.Background(), sb.Sandbox.ID) })

	// First run on the session (includes any lazy setup).
	t1 := time.Now()
	if _, err := sb.Run(ctx, "true"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	t.Logf("first run (true):        %s", time.Since(t1))

	// Steady-state sequential command latency.
	const n = 10
	var seq time.Duration
	for i := range n {
		t2 := time.Now()
		if _, err := sb.Run(ctx, "echo x"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		seq += time.Since(t2)
	}
	t.Logf("sequential echo x:       avg=%s (n=%d)", seq/time.Duration(n), n)

	// Wall-clock of a real workload command (agent must wait for completion).
	t3 := time.Now()
	if _, err := sb.Run(ctx, "sleep 1"); err != nil {
		t.Fatalf("sleep run: %v", err)
	}
	t.Logf("run `sleep 1` (wall):    %s", time.Since(t3))
}
