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

sb, err := cp.CreateSandbox(ctx, api.CreateSandboxRequest{
	Image:          "ubuntu:22.04",
	TimeoutSeconds: 1800, // 30 min lifetime; renew with KeepAlive
})
defer sb.Delete(ctx)

sess, err := sb.NewSession(ctx) 

res, err := sess.Run(ctx, "pwd && echo hi")
fmt.Print(res.Stdout, res.ExitCode)
```
