package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const workspaceRequestMaxBytes = 256 << 10

type workspaceMutationWire struct {
	Version *int           `json:"version"`
	Name    *string        `json:"name"`
	Tree    *WorkspaceNode `json:"tree"`
}

func (s *Server) workspaceIdentity(w http.ResponseWriter, r *http.Request) bool {
	id, ok := identity(r)
	if !ok || id.uid != s.cfg.Ingress.PeerUID || id.operator == "" || id.operator != s.cfg.Ingress.OperatorLogin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) workspaceGate(w http.ResponseWriter, r *http.Request, method, allow string, body bool) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !s.workspaceIdentity(w, r) {
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.URL.RawQuery != "" || (body && !jsonContentType(r)) {
		http.Error(w, "invalid workspace request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) workspaceStoreReady(w http.ResponseWriter) bool {
	if s.workspaceErr != nil || s.workspaces == nil {
		http.Error(w, "workspace store unavailable", http.StatusServiceUnavailable)
		return false
	}
	s.workspaces.mu.Lock()
	err := s.workspaces.availableLocked()
	s.workspaces.mu.Unlock()
	if err != nil {
		http.Error(w, "workspace store unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func writeWorkspace(w http.ResponseWriter, status int, record WorkspaceRecord) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Revision))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(record)
}

func writeWorkspaceError(w http.ResponseWriter, err error, current WorkspaceRecord) {
	switch {
	case errors.Is(err, errWorkspaceConflict):
		writeWorkspace(w, http.StatusConflict, current)
	case errors.Is(err, errWorkspaceNotFound):
		http.Error(w, "workspace not found", http.StatusNotFound)
	case errors.Is(err, errWorkspaceValidation):
		http.Error(w, "invalid workspace request", http.StatusBadRequest)
	case errors.Is(err, errWorkspaceStoreFull):
		http.Error(w, "workspace store full", http.StatusInsufficientStorage)
	default:
		http.Error(w, "workspace store unavailable", http.StatusServiceUnavailable)
	}
}

func decodeWorkspaceMutation(w http.ResponseWriter, r *http.Request) (string, WorkspaceNode, bool) {
	var wire workspaceMutationWire
	if err := decodeBoundedJSON(w, r, workspaceRequestMaxBytes, &wire); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "workspace request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid workspace request", http.StatusBadRequest)
		}
		return "", WorkspaceNode{}, false
	}
	if wire.Version == nil || *wire.Version != workspaceStoreVersion || wire.Name == nil || wire.Tree == nil {
		http.Error(w, "invalid workspace request", http.StatusBadRequest)
		return "", WorkspaceNode{}, false
	}
	return *wire.Name, *wire.Tree, true
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !s.workspaceGate(w, r, http.MethodGet, "GET, POST", false) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.workspaceErr != nil || s.workspaces == nil {
		http.Error(w, "workspace store unavailable", http.StatusServiceUnavailable)
		return
	}
	records, err := s.workspaces.list()
	if err != nil {
		http.Error(w, "workspace store unavailable", http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"version": workspaceStoreVersion, "items": records})
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.workspaceGate(w, r, http.MethodPost, "GET, POST", true) {
		return
	}
	name, tree, ok := decodeWorkspaceMutation(w, r)
	if !ok || !s.workspaceStoreReady(w) {
		return
	}
	record, err := s.workspaces.create(name, tree)
	if err != nil {
		writeWorkspaceError(w, err, record)
		return
	}
	writeWorkspace(w, http.StatusCreated, record)
}

func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.workspaceGate(w, r, http.MethodPut, "PUT, DELETE", true) {
		return
	}
	revision, ok := parsePreferencesIfMatch(r)
	if !ok {
		http.Error(w, "workspace precondition required", http.StatusPreconditionRequired)
		return
	}
	name, tree, ok := decodeWorkspaceMutation(w, r)
	if !ok || !s.workspaceStoreReady(w) {
		return
	}
	record, err := s.workspaces.update(r.PathValue("id"), name, tree, revision)
	if err != nil {
		writeWorkspaceError(w, err, record)
		return
	}
	writeWorkspace(w, http.StatusOK, record)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.workspaceGate(w, r, http.MethodDelete, "PUT, DELETE", false) {
		return
	}
	revision, ok := parsePreferencesIfMatch(r)
	if !ok {
		http.Error(w, "workspace precondition required", http.StatusPreconditionRequired)
		return
	}
	if !s.workspaceStoreReady(w) {
		return
	}
	record, err := s.workspaces.delete(r.PathValue("id"), revision)
	if err != nil {
		writeWorkspaceError(w, err, record)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
