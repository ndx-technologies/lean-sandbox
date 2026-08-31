# lean-sandbox

Minimal Go sandbox on Kubernetes: claim a box, open a persistent bash session, run commands, get stdout/stderr/exit code.

Deploy control plane (single Pod/Service), it will maintain a warm pool of sandbox Pods. Then simply,

```go
import (
	"context"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

cp := &sdk.ControlPlane{
	BaseURL: "http://lean-sandbox-controlplane.opensandbox.svc:8080",
	APIKey:  "your-api-key",
}

sb, err := cp.NewSandbox(ctx, api.SandboxRequest{
	Image:          "ubuntu:22.04",
	TimeoutSeconds: 1800, // 30 min lifetime; renew with KeepAlive
})
defer cp.Delete(ctx, sb.Sandbox.ID)

res, err := sb.Run(ctx, "pwd && echo hi")
fmt.Print(res.Stdout, res.ExitCode)
```

## Benchmarks

```
=== RUN   TestTiming
    timing_test.go:26: claim (NewSandbox):      29.326873ms
    timing_test.go:34: first run (true):        16.347234ms
    timing_test.go:46: sequential echo x:       avg=22.312768ms (n=10)
    timing_test.go:53: run `sleep 1` (wall):    1.01384993s
--- PASS: TestTiming (1.50s)
```
