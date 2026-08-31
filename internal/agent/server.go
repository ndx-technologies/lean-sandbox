package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Server is the in-pod agent HTTP server.
type Server struct {
	sessions *Registry
	token    string
}

// NewServer returns an agent server. token empty disables auth.
func NewServer(token string) *Server {
	return &Server{
		sessions: NewRegistry(),
		token:    token,
	}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /v1/session", s.handleCreateSession)
	mux.HandleFunc("POST /v1/session/{id}/run", s.handleRun)
	mux.HandleFunc("POST /v1/session/{id}/run-stream", s.handleRunStream)
	mux.HandleFunc("DELETE /v1/session/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /v1/file", s.handleReadFile)
	mux.HandleFunc("PUT /v1/file", s.handleWriteFile)

	return authMiddleware(s.token, mux)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	sess := NewSession()
	if err := s.sessions.Put(sess.ID, sess); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.Session{ID: sess.ID})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, err := api.SessionIDFromString(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var req api.RunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	res, err := sess.Run(r.Context(), req.Command, req.Env)
	if err != nil {
		if r.Context().Err() != nil {
			writeErr(w, http.StatusRequestTimeout, "request canceled")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.RunResponse{
		SessionID: sess.ID,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		ExitCode:  res.ExitCode,
	})
}

func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	id, err := api.SessionIDFromString(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var req api.RunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	events, err := sess.Stream(r.Context(), req.Command, req.Env)
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
	id, err := api.SessionIDFromString(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if sess, ok := s.sessions.Get(id); ok {
		sess.Close()
		s.sessions.Delete(id)
	}
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
	writeJSON(w, http.StatusOK, map[string]string{"content": string(data)})
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

// --- helpers ---

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
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
