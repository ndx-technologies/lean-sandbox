package sdk_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

func TestRealSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping live-sandbox integration test")
	}
	url := os.Getenv("CONTROL_PLANE_URL")
	apiKey := os.Getenv("CONTROL_PLANE_API_KEY")

	ctx := context.Background()
	cp := &sdk.ControlPlane{BaseURL: url, APIKey: apiKey, HTTPClient: http.DefaultClient}

	sb, err := cp.NewSandbox(ctx, api.SandboxRequest{Image: "ubuntu:22.04"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = cp.Delete(context.Background(), sb.Sandbox.ID) })

	// Sequential run: stdout + exit code round-trip.
	res, err := sb.Run(ctx, "echo hello-lean-sandbox && pwd")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-lean-sandbox") {
		t.Fatalf("stdout=%q missing marker", res.Stdout)
	}

	// Parallel runs on the SAME sandbox/session: the agent must serialize
	// session state safely while running each command in its own process.
	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			defer wg.Done()
			r, err := sb.Run(ctx, fmt.Sprintf("echo p%d && sleep 0.2", i))
			if err != nil {
				errs <- fmt.Errorf("parallel run %d: %w", i, err)
				return
			}
			if r.ExitCode != 0 {
				errs <- fmt.Errorf("parallel run %d exit=%d", i, r.ExitCode)
				return
			}
			if !strings.Contains(r.Stdout, fmt.Sprintf("p%d", i)) {
				errs <- fmt.Errorf("parallel run %d stdout=%q", i, r.Stdout)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
