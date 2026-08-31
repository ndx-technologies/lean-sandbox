package agent

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Server is the in-pod agent HTTP server.
// Each pod hosts exactly one session, created lazily on the first run.
type Server struct {
	mu      sync.Mutex
	session *Session
	token   string
}

// NewServer returns an agent server. token empty disables auth.
func NewServer(token string) *Server { return &Server{token: token} }

// getOrCreate returns the pod's single session, creating it on first use.
func (s *Server) getOrCreate() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		s.session = NewSession()
	}
	return s.session
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /v1/run", s.handleRun)
	mux.HandleFunc("POST /v1/run-stream", s.handleRunStream)
	mux.HandleFunc("DELETE /v1/session", s.handleDeleteSession)
	mux.HandleFunc("GET /v1/file", s.handleReadFile)
	mux.HandleFunc("PUT /v1/file", s.handleWriteFile)

	return authMiddleware(s.token, mux)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	sess := s.getOrCreate()
	var req api.RunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	ctx := r.Context()

	res, err := sess.Run(ctx, req.Command)
	if err != nil {
		if ctx.Err() != nil {
			writeErr(w, http.StatusRequestTimeout, "request canceled")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.MarshalWrite(w, api.RunResponse{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}); err != nil {
		slog.ErrorContext(ctx, "cannot write response", "error", err)
	}
}

func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	sess := s.getOrCreate()
	var req api.RunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	events, err := sess.Stream(r.Context(), req.Command)
	if err != nil {
		if r.Context().Err() != nil {
			writeErr(w, http.StatusRequestTimeout, "request canceled")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// SSE: stream events; emit keepalive comments while idle so proxies and
	// conntrack never see a silent long-lived connection.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if ev.Type == "done" {
				return
			}
		}
	}
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.session = nil
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path query required")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "read file: "+err.Error())
		return
	}
	ctx := r.Context()

	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(http.StatusOK)

	if err := json.MarshalWrite(w, map[string]string{"content": string(data)}); err != nil {
		slog.ErrorContext(ctx, "cannot write response", "error", err)
	}
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	var req api.WriteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		writeErr(w, http.StatusBadRequest, "mkdir: "+err.Error())
		return
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
		writeErr(w, http.StatusBadRequest, "write: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	reader := io.LimitReader(r.Body, 1<<20)
	if err := json.UnmarshalRead(reader, v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return err
	}
	return nil
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.MarshalWrite(w, api.Error{Error: msg}); err != nil {
		slog.Error("cannot write error response", "error", err)
	}
}

func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(api.AccessTokenHeader) != token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
