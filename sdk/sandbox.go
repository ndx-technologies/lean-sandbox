package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/tarx"
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

	go func(ctx context.Context) {
		if err := sb.ControlPlane.KeepAlive(ctx, sb.Sandbox.ID); err != nil {
			slog.ErrorContext(ctx, "cannot keep alive sandbox", "sandbox_id", sb.Sandbox.ID, "error", err)
		}
	}(context.Background())

	return &out, nil
}

func (sb *Sandbox) RunRequest(ctx context.Context, req api.RunRequest) (*api.RunResponse, error) {
	var out api.RunResponse
	if err := sb.do(ctx, http.MethodPost, "/v1/run", req, &out); err != nil {
		return nil, err
	}

	go func(ctx context.Context) {
		if err := sb.ControlPlane.KeepAlive(ctx, sb.Sandbox.ID); err != nil {
			slog.ErrorContext(ctx, "cannot keep alive sandbox", "sandbox_id", sb.Sandbox.ID, "error", err)
		}
	}(context.Background())

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
		defer resp.Body.Close()
		return nil, httpError(ctx, resp)
	}

	go func(ctx context.Context) {
		if err := sb.ControlPlane.KeepAlive(ctx, sb.Sandbox.ID); err != nil {
			slog.ErrorContext(ctx, "cannot keep alive sandbox", "sandbox_id", sb.Sandbox.ID, "error", err)
		}
	}(context.Background())

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
		return httpError(ctx, resp)
	}

	if out == nil {
		return nil
	}

	return json.UnmarshalRead(resp.Body, &out)
}

func (sb *Sandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	return sb.doRaw(ctx, http.MethodPut, "/v1/file?path="+url.QueryEscape(path), bytes.NewReader(content), nil)
}

func (sb *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := sb.doRaw(ctx, http.MethodGet, "/v1/file?path="+url.QueryEscape(path), nil, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (sb *Sandbox) UploadDir(ctx context.Context, from, to string) error {
	to = filepath.Clean(to)
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() { pw.CloseWithError(tarx.Pack(pw, to, filepath.Base(to))) }()

	return sb.doRaw(ctx, http.MethodPut, "/v1/files?root="+url.QueryEscape(from), pr, nil)
}

func (sb *Sandbox) DownloadDir(ctx context.Context, from, to string) error {
	resp, err := sb.doRawResponse(ctx, http.MethodGet, "/v1/files?root="+url.QueryEscape(from), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return tarx.Unpack(resp.Body, to)
}

func (sb *Sandbox) doRaw(ctx context.Context, method, path string, body io.Reader, out io.Writer) error {
	resp, err := sb.doRawResponse(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func (sb *Sandbox) doRawResponse(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, sb.Sandbox.Endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if sb.Sandbox.AccessToken != "" {
		req.Header.Set(api.AccessTokenHeader, sb.Sandbox.AccessToken)
	}
	resp, err := sb.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, httpError(ctx, resp)
	}
	return resp, nil
}
