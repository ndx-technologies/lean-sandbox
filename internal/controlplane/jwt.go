package controlplane

import (
	"log/slog"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/jwt"
)

// mintJWT signs an RS256 token for the sandbox with sub=sandbox id and the
// configured TTL. The agent verifies it against the control plane's public
// key, so a client (or a compromised agent) can never forge a token for
// another sandbox.
func (cp *ControlPlane) mintJWT(id api.SandboxID) string {
	tok, err := jwt.Sign(cp.signingKey, id.String(), cp.config.TokenTTL)
	if err != nil {
		slog.Error("mint jwt", "error", err)
		return ""
	}
	return tok
}
