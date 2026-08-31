package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Session is a persistent bash session inside a sandbox.
type Session struct {
	ID      api.SessionID
	Sandbox *Sandbox
}

// Run executes a command in the session, preserving cwd/env across calls.
// Bound the command with a context deadline (e.g. context.WithTimeout) to stop
// it; the agent kills the whole process group when the context is canceled.
func (s *Session) Run(ctx context.Context, command string) (*api.RunResponse, error) {
	var out api.RunResponse
	req := api.RunRequest{Command: command}
	if err := s.Sandbox.do(ctx, http.MethodPost, "/v1/session/"+s.ID.String()+"/run", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stream runs a command and returns an SSE stream of events. The caller must
// consume events until the channel closes; the final event is "done" with the
// exit code. Cancel ctx to stop the command (the agent kills the process group).
func (s *Session) Stream(ctx context.Context, command string) (<-chan api.StreamEvent, error) {
	req := api.RunRequest{Command: command}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.Sandbox.Sandbox.Endpoint+"/v1/session/"+s.ID.String()+"/run-stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if s.Sandbox.Sandbox.AccessToken != "" {
		httpReq.Header.Set(api.AccessTokenHeader, s.Sandbox.Sandbox.AccessToken)
	}
	resp, err := s.Sandbox.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e api.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("agent stream: %s", e.Error)
	}

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

func (s *Session) Close(ctx context.Context) error {
	return s.Sandbox.do(ctx, http.MethodDelete, "/v1/session/"+s.ID.String(), nil, nil)
}
