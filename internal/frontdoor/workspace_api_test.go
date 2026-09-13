package frontdoor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type workspaceListInterleavingRecorder struct {
	*httptest.ResponseRecorder
	headerCalls int
	afterReady  chan struct{}
	resume      chan struct{}
}

func (w *workspaceListInterleavingRecorder) Header() http.Header {
	w.headerCalls++
	if w.headerCalls == 2 {
		close(w.afterReady)
		<-w.resume
	}
	return w.ResponseRecorder.Header()
}

// The first W2 integration falsifier is deliberately existing-seam only: the
// secured front handler currently has no durable-workspace route, so the
// natural baseline is 404. The product contract makes an unconfigured store a
// typed, no-store 503 instead of omitting the route or serving an ephemeral
// fallback.
func TestWorkspaceAPIUnavailableStoreIsTyped(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	w := ergoRequest(t, s, http.MethodGet, "http://localhost/api/workspaces", "", nil, nil)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" || w.Body.String() != "workspace store unavailable\n" {
		t.Fatalf("unconfigured workspace store=%d cache=%q body=%q", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
}

func workspaceAPIServer(t *testing.T) *Server {
	t.Helper()
	cfg := ergoFrontConfig(t)
	cfg.WorkspaceStorePath = filepath.Join(filepath.Dir(cfg.PreferencesStorePath), "workspaces.json")
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.workspaceErr != nil {
		t.Fatal(s.workspaceErr)
	}
	next := 0
	s.workspaces.newID = func() (string, error) {
		next++
		return strings.Repeat("0", 31) + string(rune('0'+next)), nil
	}
	return s
}

func workspaceMutationBody(t *testing.T, name string, tree WorkspaceNode) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": workspaceStoreVersion, "name": name, "tree": tree})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeWorkspaceRecord(t *testing.T, body string) WorkspaceRecord {
	t.Helper()
	var record WorkspaceRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatalf("decode workspace %q: %v", body, err)
	}
	return record
}

func TestWorkspaceAPICRUDAndConflict(t *testing.T) {
	s := workspaceAPIServer(t)
	request := func(method, target, body, match string) *httptest.ResponseRecorder {
		media := ""
		if body != "" {
			media = "application/json"
		}
		return ergoRequest(t, s, method, target, media, strings.NewReader(body), func(r *http.Request) {
			if match != "" {
				r.Header.Set("If-Match", match)
			}
		})
	}

	listed := request(http.MethodGet, "http://localhost/api/workspaces", "", "")
	if listed.Code != http.StatusOK || listed.Header().Get("Cache-Control") != "no-store" || listed.Body.String() != "{\"items\":[],\"version\":1}\n" {
		t.Fatalf("empty list=%d body=%q", listed.Code, listed.Body.String())
	}
	created := request(http.MethodPost, "http://localhost/api/workspaces", workspaceMutationBody(t, "Ops", workspaceSixTree()), "")
	if created.Code != http.StatusCreated || created.Header().Get("ETag") != `"1"` {
		t.Fatalf("create=%d etag=%q body=%s", created.Code, created.Header().Get("ETag"), created.Body.String())
	}
	record := decodeWorkspaceRecord(t, created.Body.String())
	if record.Name != "Ops" || record.NormalizedName != "ops" || record.Revision != 1 {
		t.Fatalf("created=%+v", record)
	}
	collision := request(http.MethodPost, "http://localhost/api/workspaces", workspaceMutationBody(t, "ops", workspaceLeaf("other")), "")
	if collision.Code != http.StatusConflict || decodeWorkspaceRecord(t, collision.Body.String()).WorkspaceID != record.WorkspaceID {
		t.Fatalf("collision=%d body=%s", collision.Code, collision.Body.String())
	}
	missingMatch := request(http.MethodPut, "http://localhost/api/workspaces/"+record.WorkspaceID, workspaceMutationBody(t, "Renamed", workspaceSixTree()), "")
	if missingMatch.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match=%d", missingMatch.Code)
	}
	updated := request(http.MethodPut, "http://localhost/api/workspaces/"+record.WorkspaceID, workspaceMutationBody(t, "Renamed", workspaceSixTree()), `"1"`)
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") != `"2"` {
		t.Fatalf("update=%d etag=%q body=%s", updated.Code, updated.Header().Get("ETag"), updated.Body.String())
	}
	stale := request(http.MethodPut, "http://localhost/api/workspaces/"+record.WorkspaceID, workspaceMutationBody(t, "Overwrite", workspaceSixTree()), `"1"`)
	if stale.Code != http.StatusConflict || decodeWorkspaceRecord(t, stale.Body.String()).Name != "Renamed" {
		t.Fatalf("stale=%d body=%s", stale.Code, stale.Body.String())
	}
	staleDelete := request(http.MethodDelete, "http://localhost/api/workspaces/"+record.WorkspaceID, "", `"1"`)
	if staleDelete.Code != http.StatusConflict || decodeWorkspaceRecord(t, staleDelete.Body.String()).Revision != 2 {
		t.Fatalf("stale delete=%d body=%s", staleDelete.Code, staleDelete.Body.String())
	}
	deleted := request(http.MethodDelete, "http://localhost/api/workspaces/"+record.WorkspaceID, "", `"2"`)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete=%d body=%q", deleted.Code, deleted.Body.String())
	}
}

func TestWorkspaceAPIListRefusesPostRenameFaultAtomically(t *testing.T) {
	s := workspaceAPIServer(t)
	before, err := s.workspaces.create("Before", workspaceLeaf("one"))
	if err != nil {
		t.Fatal(err)
	}
	s.workspaces.file.dirSync = func(*os.File) error { return errors.New("dir sync fault") }

	r := httptest.NewRequest(http.MethodGet, "http://localhost/api/workspaces", nil)
	r.Header.Set("Tailscale-User-Login", s.cfg.Ingress.OperatorLogin)
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: s.cfg.Ingress.PeerUID, operator: s.cfg.Ingress.OperatorLogin}))
	w := &workspaceListInterleavingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		afterReady:       make(chan struct{}),
		resume:           make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		s.listWorkspaces(w, r)
		close(done)
	}()
	<-w.afterReady

	if _, err := s.workspaces.update(before.WorkspaceID, "After", workspaceLeaf("one"), before.Revision); err == nil {
		t.Fatal("post-rename fault unexpectedly committed")
	}
	close(w.resume)
	<-done
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "workspace store unavailable\n" {
		t.Fatalf("faulted list=%d body=%q", w.Code, w.Body.String())
	}
}

func TestWorkspaceAPISecurityAndClosedSchema(t *testing.T) {
	s := workspaceAPIServer(t)
	valid := workspaceMutationBody(t, "Secure", workspaceLeaf("one"))
	for name, mutate := range map[string]func(*http.Request){
		"csrf_absent":    func(r *http.Request) { r.Header.Del("X-Persea-CSRF") },
		"wrong_origin":   func(r *http.Request) { r.Header.Set("Origin", "https://attacker.invalid") },
		"wrong_operator": func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.com") },
	} {
		t.Run(name, func(t *testing.T) {
			w := ergoRequest(t, s, http.MethodPost, "http://localhost/api/workspaces", "application/json", strings.NewReader(valid), mutate)
			if w.Code < 400 || len(workspaceStoreRecords(t, s.workspaces)) != 0 {
				t.Fatalf("mutation=%d records=%d", w.Code, len(workspaceStoreRecords(t, s.workspaces)))
			}
		})
	}
	for name, body := range map[string]string{
		"unknown_root": strings.TrimSuffix(valid, "}") + `,"handle":"secret"}`,
		"unknown_leaf": `{"version":1,"name":"bad","tree":{"kind":"leaf","session":{"realm":"r","server":"s","name":"n"},"on_missing":"offer","source":"secret"}}`,
		"authority":    `{"version":1,"name":"bad","tree":{"kind":"leaf","session":{"realm":"r","server":"s","name":"n","generation":1},"on_missing":"offer"}}`,
		"duplicate":    `{"version":1,"version":1,"name":"bad","tree":{"kind":"leaf","session":{"realm":"r","server":"s","name":"n"},"on_missing":"offer"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := ergoRequest(t, s, http.MethodPost, "http://localhost/api/workspaces", "application/json", strings.NewReader(body), nil)
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "secret") || len(workspaceStoreRecords(t, s.workspaces)) != 0 {
				t.Fatalf("closed schema=%d body=%q records=%d", w.Code, w.Body.String(), len(workspaceStoreRecords(t, s.workspaces)))
			}
		})
	}
	tooLarge := `{"version":1,"name":"` + strings.Repeat("x", workspaceRequestMaxBytes) + `"}`
	if w := ergoRequest(t, s, http.MethodPost, "http://localhost/api/workspaces", "application/json", strings.NewReader(tooLarge), nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize=%d", w.Code)
	}
}
