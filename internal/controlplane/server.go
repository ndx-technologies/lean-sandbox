package controlplane

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Server is the control plane HTTP API.
type Server struct {
	cp     *ControlPlane
	apiKey string
}

// NewServer returns the HTTP API for a control plane. apiKey empty disables auth.
func NewServer(cp *ControlPlane, apiKey string) *Server {
	return &Server{cp: cp, apiKey: apiKey}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/sandboxes/{id}/keepalive", s.handleKeepAlive)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleDelete)
	mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	return apiKeyMiddleware(s.apiKey, mux)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeCPErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	sb, err := s.cp.Sandbox(r.Context(), req)
	if err != nil {
		writeCPErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeCPJSON(w, http.StatusOK, sb.toAPI())
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := api.SandboxIDFromString(r.PathValue("id"))
	if err != nil {
		writeCPErr(w, http.StatusBadRequest, "invalid sandbox id")
		return
	}
	sb, err := s.cp.GetSandbox(r.Context(), id)
	if err != nil {
		writeCPErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeCPJSON(w, http.StatusOK, sb.toAPI())
}

func (s *Server) handleKeepAlive(w http.ResponseWriter, r *http.Request) {
	id, err := api.SandboxIDFromString(r.PathValue("id"))
	if err != nil {
		writeCPErr(w, http.StatusBadRequest, "invalid sandbox id")
		return
	}
	sb, err := s.cp.KeepAlive(r.Context(), id)
	if err != nil {
		writeCPErr(w, http.StatusGone, err.Error())
		return
	}
	writeCPJSON(w, http.StatusOK, sb.toAPI())
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := api.SandboxIDFromString(r.PathValue("id"))
	if err != nil {
		writeCPErr(w, http.StatusBadRequest, "invalid sandbox id")
		return
	}
	if err := s.cp.DeleteSandbox(r.Context(), id); err != nil {
		writeCPErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	sbs := s.cp.ListSandboxes(r.Context())
	out := make([]api.Sandbox, 0, len(sbs))
	for _, sb := range sbs {
		out = append(out, sb.toAPI())
	}
	writeCPJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

func (sb *Sandbox) toAPI() api.Sandbox {
	return api.Sandbox{
		ID:          sb.ID,
		Image:       sb.Image,
		Status:      "Running",
		Endpoint:    sb.Endpoint,
		AccessToken: sb.AccessToken,
	}
}

func writeCPJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeCPErr(w http.ResponseWriter, code int, msg string) {
	writeCPJSON(w, code, api.Error{Error: msg})
}

func apiKeyMiddleware(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(api.APIKeyHeader) != key {
			writeCPErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
