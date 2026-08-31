package api

import (
	"uuid"
)

const (
	// AccessTokenHeader authenticates agent requests (optional; set via -access-token).
	AccessTokenHeader = "X-Access-Token"

	// APIKeyHeader authenticates control plane requests (required).
	APIKeyHeader = "X-Api-Key"
)

type SessionID struct{ uuid.UUID }

func (s SessionID) IsZero() bool { return s.UUID == uuid.Nil() }

func NewSessionID() SessionID { return SessionID{uuid.New()} }

func SessionIDFromString(s string) (SessionID, error) {
	u, err := uuid.Parse(s)
	return SessionID{u}, err
}

// Session identifies a persistent bash session. It starts in the container's
// default working directory (the image WORKDIR, or /); navigate with `cd` and
// check with `pwd`. Env/cwd persist server-side across Run calls.
type Session struct {
	ID SessionID `json:"id"`
}

// RunRequest executes a shell command in the session. There is no per-command
// timeout on the wire: bound the command with a context deadline and the agent
// kills the whole process group when the request context is canceled.
type RunRequest struct {
	Command string   `json:"command"`       // shell command line to execute (e.g. "pwd && echo hi")
	Env     []string `json:"env,omitempty"` // extra environment (KEY=VALUE pairs), applied for this run only
}

type RunResponse struct {
	SessionID SessionID `json:"session_id"`
	Stdout    string    `json:"stdout"`
	Stderr    string    `json:"stderr"`
	ExitCode  int       `json:"exit_code"`
}

// StreamEvent is one frame of the SSE stream from /v1/session/{id}/run-stream.
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

type CreateSandboxRequest struct {
	Image          string   `json:"image"`                     // container image
	Env            []string `json:"env,omitempty"`             // environment (KEY=VALUE pairs)
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"` // sandbox lifetime; the pod is deleted after this. 0 = default.
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
	ID       SandboxID `json:"id"`
	Image    string    `json:"image"`
	Status   string    `json:"status"`   // Pending | Running | Succeeded | Failed | Unknown
	Endpoint string    `json:"endpoint"` // host:port of the agent inside the pod
}

// Error returned by agent and control plane on failure.
type Error struct {
	Error string `json:"error"`
}
