package sdk

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

type ControlPlane struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func (s *ControlPlane) CreateSandbox(ctx context.Context, req api.CreateSandboxRequest) (*Sandbox, error) {
	var out api.Sandbox
	if err := s.doJSON(ctx, http.MethodPost, "/v1/sandboxes", req, &out); err != nil {
		return nil, err
	}
	return &Sandbox{Sandbox: out, HTTPClient: s.HTTPClient}, nil
}

func (s *ControlPlane) GetSandbox(ctx context.Context, id api.SandboxID) (*Sandbox, error) {
	var out api.Sandbox
	if err := s.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+id.String(), nil, &out); err != nil {
		return nil, err
	}
	return &Sandbox{Sandbox: out, HTTPClient: s.HTTPClient}, nil
}

// KeepAlive renews the lease on a sandbox so the janitor does not reclaim it
// while a long-lived conversation (e.g. a chat thread) is still using it.
func (s *ControlPlane) KeepAlive(ctx context.Context, id api.SandboxID) error {
	return s.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+id.String()+"/keepalive", nil, nil)
}

// Delete tears a sandbox down (deletes its pod).
func (s *ControlPlane) Delete(ctx context.Context, id api.SandboxID) error {
	return s.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+id.String(), nil, nil)
}

func (s *ControlPlane) ListSandboxes(ctx context.Context) ([]api.Sandbox, error) {
	var out struct {
		Sandboxes []api.Sandbox `json:"sandboxes"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/v1/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

func (s *ControlPlane) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		req.Header.Set(api.APIKeyHeader, s.APIKey)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e api.Error
		_ = json.UnmarshalRead(resp.Body, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("control plane %s: %s", resp.Status, e.Error)
	}

	if out == nil {
		return nil
	}

	return json.UnmarshalRead(resp.Body, &out)
}
