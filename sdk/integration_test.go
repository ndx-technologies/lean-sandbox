package sdk_test

import (
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

	cp := &sdk.ControlPlane{
		BaseURL:    os.Getenv("CONTROL_PLANE_URL"),
		APIKey:     os.Getenv("CONTROL_PLANE_API_KEY"),
		HTTPClient: http.DefaultClient,
	}

	ctx := t.Context()

	sb, err := cp.NewSandbox(ctx, api.SandboxRequest{Image: "ubuntu:22.04"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = cp.Delete(ctx, sb.Sandbox.ID) })

	// Sequential run: stdout + exit code round-trip.
	res, err := sb.Run(ctx, "echo hello-lean-sandbox && pwd")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Error(err)
	}
	if !strings.Contains(res.Stdout, "hello-lean-sandbox") {
		t.Errorf("stdout=%q missing marker", res.Stdout)
	}

	// Parallel runs on the SAME sandbox/session: the agent must serialize
	// session state safely while running each command in its own process.
	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Go(func() {
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
