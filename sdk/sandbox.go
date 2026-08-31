package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Sandbox is a handle to a created sandbox, bound to the agent endpoint.
type Sandbox struct {
	Sandbox    api.Sandbox
	HTTPClient *http.Client
}

// NewSession opens a persistent bash session on the sandbox agent. It starts
// in the container's default working directory; use `cd`/`pwd` to navigate,
// and env/cwd persist across Run calls.
func (sb *Sandbox) NewSession(ctx context.Context) (*Session, error) {
	var out api.Session
	if err := sb.do(ctx, http.MethodPost, "/v1/session", nil, &out); err != nil {
		return nil, err
	}
	return &Session{Sandbox: sb, ID: out.ID}, nil
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
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("agent %s: %s", resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
