package api

import "uuid"

const (
	// AccessTokenHeader carries the control-plane-signed JWT that authenticates
	// agent requests. The agent verifies it with -controlplane-public-key and
	// requires sub to equal its -sandbox-id.
	AccessTokenHeader = "X-Access-Token"

	// APIKeyHeader authenticates control plane requests (required).
	APIKeyHeader = "X-Api-Key"
)

// RunRequest executes a shell command in the sandbox's single session. There is
// no per-command timeout on the wire: bound the command with a context deadline
// and the agent kills the whole process group when the request context is
// canceled.
type RunRequest struct {
	Command string `json:"command"`
}

type RunResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// StreamEvent is one frame of the SSE stream from /v1/run-stream.
// Type is one of: "stdout", "stderr", "done". The "done" event carries the
// final exit code. Error is set only on abnormal termination (e.g. client
// disconnect / context cancellation killing the process). The session's
// cwd/env are persisted server-side but not exposed here; read `pwd`/`env`
// from stdout if needed.
type StreamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ReadRequest reads a file inside the sandbox filesystem.
type ReadRequest struct {
	Path string `json:"path"`
}

// WriteRequest writes a file inside the sandbox filesystem (creates parent dirs).
type WriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type SandboxRequest struct {
	Image          string `json:"image"`                     // container image
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // sandbox lifetime; the pod is deleted after this. 0 = default.
}

type SandboxID struct{ uuid.UUID }

func (s SandboxID) IsZero() bool { return s.UUID == uuid.Nil() }

func NewSandboxID() SandboxID { return SandboxID{uuid.New()} }

func SandboxIDFromString(s string) (SandboxID, error) {
	u, err := uuid.Parse(s)
	return SandboxID{u}, err
}

// Sandbox is the control plane's view of a created sandbox.
type Sandbox struct {
	ID          SandboxID `json:"id"`
	Image       string    `json:"image"`
	Status      string    `json:"status"` // Pending | Running | Succeeded | Failed | Unknown
	Endpoint    string    `json:"endpoint"`
	AccessToken string    `json:"access_token,omitempty"`
}

// Error returned by agent and control plane on failure.
type Error struct {
	Error string `json:"error"`
}
