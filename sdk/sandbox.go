package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Sandbox is a handle to a created sandbox.
// Each sandbox has exactly one persistent bash session, created lazily on first use.
type Sandbox struct {
	Sandbox      api.Sandbox
	HTTPClient   *http.Client
	ControlPlane *ControlPlane
}

// Run executes a command in the sandbox's persistent session, preserving
// cwd/env across calls. Bound the command with a context deadline (e.g.
// context.WithTimeout) to stop it; the agent kills the whole process group
// when the context is canceled.
func (sb *Sandbox) Run(ctx context.Context, command string) (*api.RunResponse, error) {
	var out api.RunResponse
	req := api.RunRequest{Command: command}
	if err := sb.do(ctx, http.MethodPost, "/v1/run", req, &out); err != nil {
		return nil, err
	}

	go func() {
		if err := sb.ControlPlane.KeepAlive(ctx, sb.Sandbox.ID); err != nil {
			slog.ErrorContext(ctx, "cannot keep alive sandbox", "sandbox_id", sb.Sandbox.ID, "error", err)
		}
	}()

	return &out, nil
}

// Stream runs a command and returns an SSE stream of events. The caller must
// consume events until the channel closes; the final event is "done" with the
// exit code. Cancel ctx to stop the command (the agent kills the process group).
func (sb *Sandbox) Stream(ctx context.Context, command string) (<-chan api.StreamEvent, error) {
	req := api.RunRequest{Command: command}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, sb.Sandbox.Endpoint+"/v1/run-stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if sb.Sandbox.AccessToken != "" {
		httpReq.Header.Set(api.AccessTokenHeader, sb.Sandbox.AccessToken)
	}
	resp, err := sb.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e api.Error
		if err := json.UnmarshalRead(resp.Body, &e); err != nil {
			slog.ErrorContext(ctx, "cannot decode respose error", "error", err)
		}

		resp.Body.Close()
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("agent stream: %s", e.Error)
	}

	go func() {
		if err := sb.ControlPlane.KeepAlive(ctx, sb.Sandbox.ID); err != nil {
			slog.ErrorContext(ctx, "cannot keep alive sandbox", "sandbox_id", sb.Sandbox.ID, "error", err)
		}
	}()

	events := make(chan api.StreamEvent, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

		for sc.Scan() {
			line := sc.Text()
			if len(line) < 6 || line[:6] != "data: " {
				continue // skip : ping comments and empty frames
			}

			var ev api.StreamEvent
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				continue
			}

			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}

		if err := sc.Err(); err != nil {
			// Stream ended abnormally: emit a done event with the error so
			// the caller sees a terminal state instead of a silent hang.
			select {
			case events <- api.StreamEvent{Type: "done", ExitCode: -1, Error: err.Error()}:
			case <-ctx.Done():
			}
		}
	}()

	return events, nil
}

// Close closes the sandbox's session so a fresh one can be started.
func (sb *Sandbox) Close(ctx context.Context) error {
	return sb.do(ctx, http.MethodDelete, "/v1/session", nil, nil)
}

func (sb *Sandbox) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, sb.Sandbox.Endpoint+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if sb.Sandbox.AccessToken != "" {
		req.Header.Set(api.AccessTokenHeader, sb.Sandbox.AccessToken)
	}
	resp, err := sb.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e api.Error
		if err := json.UnmarshalRead(resp.Body, &e); err != nil {
			slog.ErrorContext(ctx, "cannot decode error", "error", err)
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("agent %s: %s", resp.Status, e.Error)
	}

	if out == nil {
		return nil
	}

	return json.UnmarshalRead(resp.Body, &out)
}

// ReadFile reads a file from the sandbox filesystem.
func (sb *Sandbox) ReadFile(ctx context.Context, path string) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	if err := sb.do(ctx, http.MethodGet, "/v1/file?path="+path, nil, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// WriteFile writes a file into the sandbox filesystem.
func (sb *Sandbox) WriteFile(ctx context.Context, path, content string) error {
	return sb.do(ctx, http.MethodPut, "/v1/file", api.WriteRequest{Path: path, Content: content}, nil)
}
