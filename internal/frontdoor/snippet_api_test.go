package frontdoor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const snippetSecret = "SENTINEL-body-4e2a"

func decodeSnippet(t *testing.T, w *httptest.ResponseRecorder) SnippetRecord {
	t.Helper()
	var got SnippetRecord
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

func decodeSnippetList(t *testing.T, w *httptest.ResponseRecorder) []snippetView {
	t.Helper()
	var got struct {
		Items []snippetView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Items == nil {
		t.Fatalf("decode list %q: %v", w.Body.String(), err)
	}
	return got.Items
}

type snippetClient struct {
	t *testing.T
	s *Server
}

func (c snippetClient) get() *httptest.ResponseRecorder {
	return ergoRequest(c.t, c.s, http.MethodGet, "http://localhost/api/snippets", "", nil, nil)
}
func (c snippetClient) post(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	return ergoRequest(c.t, c.s, http.MethodPost, "http://localhost/api/snippets", "application/json", strings.NewReader(body), mutate)
}
func (c snippetClient) patch(id, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	return ergoRequest(c.t, c.s, http.MethodPatch, "http://localhost/api/snippets/"+id, "application/json", strings.NewReader(body), mutate)
}
func (c snippetClient) delete(id, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	return ergoRequest(c.t, c.s, http.MethodDelete, "http://localhost/api/snippets/"+id, "application/json", strings.NewReader(body), mutate)
}
func (c snippetClient) putOSC(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	return ergoRequest(c.t, c.s, http.MethodPut, "http://localhost/api/snippets/osc52", "application/json", strings.NewReader(body), mutate)
}

func TestSnippetAPIContract(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.snippetErr != nil {
		t.Fatal(s.snippetErr)
	}
	s.snippets.file.fileSync = func(*os.File) error { return nil }
	s.snippets.file.dirSync = func(*os.File) error { return nil }
	c := snippetClient{t, s}

	empty := c.get()
	if empty.Code != http.StatusOK || empty.Header().Get("Cache-Control") != "no-store" || len(decodeSnippetList(t, empty)) != 0 {
		t.Fatalf("empty list=%d %s", empty.Code, empty.Body.String())
	}
	created := c.post(`{"kind":"snippet","label":"Deploy","body":"make deploy\n","pinned":false,"origin":"laptop"}`, nil)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	snippet := decodeSnippet(t, created)
	if snippet.Kind != "snippet" || snippet.Label != "Deploy" || snippet.Revision != 1 || snippet.ExpiresAt != nil || !snippetIDRE.MatchString(snippet.ID) {
		t.Fatalf("snippet=%+v", snippet)
	}
	pinnedResponse := c.post(`{"kind":"snippet","label":"Logs","body":"tail -f log","pinned":true}`, nil)
	if pinnedResponse.Code != http.StatusCreated {
		t.Fatalf("pinned=%d %s", pinnedResponse.Code, pinnedResponse.Body.String())
	}
	pinned := decodeSnippet(t, pinnedResponse)
	clipResponse := c.post(`{"kind":"clip","body":"first line\nsecond line","origin":"iphone"}`, nil)
	if clipResponse.Code != http.StatusCreated {
		t.Fatalf("clip=%d %s", clipResponse.Code, clipResponse.Body.String())
	}
	clip := decodeSnippet(t, clipResponse)
	if clip.Kind != "clip" || clip.ExpiresAt == nil || clip.Origin != "iphone" || clip.Label != "" {
		t.Fatalf("clip=%+v", clip)
	}
	list := decodeSnippetList(t, c.get())
	if len(list) != 3 || list[1].ID != pinned.ID || list[1].Preview != "" || list[0].ID != clip.ID || list[0].Preview != "first line second line" || list[2].ID != snippet.ID {
		t.Fatalf("list=%+v", list)
	}

	// PATCH / DELETE carry the revision in the body (§3b); a stale revision
	// answers 409 with the current record.
	stale := c.patch(snippet.ID, `{"label":"Ship","revision":9}`, nil)
	if stale.Code != http.StatusConflict || stale.Header().Get("Cache-Control") != "no-store" || decodeSnippet(t, stale).Revision != 1 {
		t.Fatalf("stale patch=%d %s", stale.Code, stale.Body.String())
	}
	updated := c.patch(snippet.ID, `{"label":"Ship","pinned":true,"revision":1}`, nil)
	if updated.Code != http.StatusOK || decodeSnippet(t, updated).Revision != 2 || decodeSnippet(t, updated).Label != "Ship" || !decodeSnippet(t, updated).Pinned {
		t.Fatalf("patch=%d %s", updated.Code, updated.Body.String())
	}
	if w := c.patch(clip.ID, `{"label":"nope","revision":1}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("clip label patch=%d", w.Code)
	}
	if w := c.patch(clip.ID, `{"body":"edited","revision":1}`, nil); w.Code != http.StatusOK || decodeSnippet(t, w).Body != "edited" {
		t.Fatalf("clip body patch=%d %s", w.Code, w.Body.String())
	}
	staleDelete := c.delete(pinned.ID, `{"revision":5}`, nil)
	if staleDelete.Code != http.StatusConflict || decodeSnippet(t, staleDelete).ID != pinned.ID {
		t.Fatalf("stale delete=%d %s", staleDelete.Code, staleDelete.Body.String())
	}
	deleted := c.delete(pinned.ID, `{"revision":1}`, nil)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 || deleted.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("delete=%d %q", deleted.Code, deleted.Body.String())
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"deleted_patch":  c.patch(pinned.ID, `{"label":"x","revision":1}`, nil),
		"deleted_delete": c.delete(pinned.ID, `{"revision":1}`, nil),
		"unknown_id":     c.patch("0123456789abcdef0123456789abcdef", `{"label":"x","revision":1}`, nil),
		"malformed_id":   c.patch("not-an-id", `{"label":"x","revision":1}`, nil),
		"uppercase_id":   c.delete(strings.ToUpper(snippet.ID), `{"revision":2}`, nil),
	} {
		assertBoundedRefusal(t, name, w, http.StatusNotFound)
	}

	// OSC retains one internal publication/CAS record, while GET exposes only
	// editable canonical content in the bounded recent ring.
	assertBoundedRefusal(t, "POST reserved origin", c.post(`{"kind":"clip","body":"`+snippetSecret+`","origin":"osc52"}`, nil), http.StatusBadRequest, snippetSecret)
	assertBoundedRefusal(t, "POST padded reserved origin", c.post(`{"kind":"clip","body":"`+snippetSecret+`","origin":" osc52 "}`, nil), http.StatusBadRequest, snippetSecret)
	var osc SnippetRecord
	for i := 0; i < 30; i++ {
		origin := "iphone"
		if i%2 == 1 {
			origin = "laptop"
		}
		w := c.putOSC(`{"body":"osc `+strconv.Itoa(i)+`","origin":"`+origin+`"}`, nil)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("osc write %d: %d %s", i, w.Code, w.Body.String())
		}
		r := decodeSnippet(t, w)
		if i == 0 {
			if r.ID != snippetOSCID || r.Revision != 1 || r.Kind != snippetKindClip || r.Origin != "iphone" {
				t.Fatalf("first osc write=%+v", r)
			}
			osc = r
		} else if r.ID != osc.ID || r.Revision != uint64(i+1) || r.Origin != origin {
			t.Fatalf("osc write %d: record=%+v", i, r)
		}
	}
	list = decodeSnippetList(t, c.get())
	oscSeen := 0
	for _, v := range list {
		if isOSCSnippetID(v.ID) {
			oscSeen++
		}
	}
	if oscSeen != 0 || len(list) != snippetClipRing+2 || !snippetIDRE.MatchString(list[0].ID) || list[0].Preview != "osc 29" || list[len(list)-1].ID != snippet.ID {
		t.Fatalf("after osc flood list=%+v", list)
	}
	// Optional If-Match on the upsert: stale → 412 with revision metadata,
	// current → 200, malformed → 400; the record is server-owned, so PATCH
	// does not reach it and DELETE does, by its fixed id.
	stale412 := c.putOSC(`{"body":"late"}`, func(r *http.Request) { r.Header.Set("If-Match", `"3"`) })
	if stale412.Code != http.StatusPreconditionFailed || decodeSnippet(t, stale412).Revision != 30 || stale412.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("stale osc upsert=%d %s", stale412.Code, stale412.Body.String())
	}
	if w := c.putOSC(`{"body":"cas"}`, func(r *http.Request) { r.Header.Set("If-Match", `"30"`) }); w.Code != http.StatusOK || decodeSnippet(t, w).Revision != 31 || decodeSnippet(t, w).Origin != "" {
		t.Fatalf("cas osc upsert=%d %s", w.Code, w.Body.String())
	}
	assertBoundedRefusal(t, "osc malformed If-Match", c.putOSC(`{"body":"`+snippetSecret+`"}`, func(r *http.Request) { r.Header.Set("If-Match", `31`) }), http.StatusBadRequest, snippetSecret)
	assertBoundedRefusal(t, "PATCH osc record", c.patch(snippetOSCID, `{"body":"`+snippetSecret+`","revision":31}`, nil), http.StatusNotFound, snippetSecret)
	assertBoundedRefusal(t, "PUT other id", ergoRequest(t, s, http.MethodPut, "http://localhost/api/snippets/"+snippet.ID, "application/json", strings.NewReader(`{"body":"`+snippetSecret+`"}`), nil), http.StatusMethodNotAllowed, snippetSecret)
	for name, body := range map[string]string{
		"missing_body":    `{"origin":"iphone"}`,
		"unknown_field":   `{"body":"` + snippetSecret + `","kind":"clip"}`,
		"revision_field":  `{"body":"` + snippetSecret + `","revision":31}`,
		"id_field":        `{"body":"` + snippetSecret + `","id":"osc52"}`,
		"reserved_origin": `{"body":"` + snippetSecret + `","origin":"osc52"}`,
		"origin_too_long": `{"body":"` + snippetSecret + `","origin":"` + strings.Repeat("o", snippetOriginMaxRunes+1) + `"}`,
		"empty_body":      `{"body":""}`,
		"body_carriage":   `{"body":"` + snippetSecret + `\r"}`,
		"trailing":        `{"body":"` + snippetSecret + `"}{}`,
		"array":           `[]`,
	} {
		assertBoundedRefusal(t, "PUT osc "+name, c.putOSC(body, nil), http.StatusBadRequest, snippetSecret)
	}
	assertBoundedRefusal(t, "PUT osc body over 16 KiB", c.putOSC(`{"body":"`+strings.Repeat("s", snippetBodyMaxBytes+1)+`"}`, nil), http.StatusRequestEntityTooLarge, "ssssssss")
	if w := c.delete(snippetOSCID, `{"revision":31}`, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete osc record=%d %s", w.Code, w.Body.String())
	}
	if w := c.putOSC(`{"body":"again","origin":"laptop"}`, func(r *http.Request) { r.Header.Set("If-Match", `"31"`) }); w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), `"revision":0`) {
		t.Fatalf("stale upsert after delete=%d %s", w.Code, w.Body.String())
	}
	if w := c.putOSC(`{"body":"again","origin":"laptop"}`, func(r *http.Request) { r.Header.Set("If-Match", `"0"`) }); w.Code != http.StatusOK || decodeSnippet(t, w).Revision != 1 {
		t.Fatalf("recreate osc record=%d %s", w.Code, w.Body.String())
	}
	osc = decodeSnippet(t, c.putOSC(`{"body":"osc 29"}`, nil))
	beforeRefusals := len(decodeSnippetList(t, c.get()))

	// Route refusal cases for every mutation and every method.
	validSnippet := `{"kind":"snippet","label":"L","body":"` + snippetSecret + `"}`
	for _, tc := range ergoMutationRedCases(cfg) {
		assertBoundedRefusal(t, "POST "+tc.name, c.post(validSnippet, tc.mutate), tc.status, snippetSecret)
		assertBoundedRefusal(t, "PUT osc "+tc.name, c.putOSC(`{"body":"`+snippetSecret+`"}`, tc.mutate), tc.status, snippetSecret)
		assertBoundedRefusal(t, "PATCH "+tc.name, c.patch(snippet.ID, `{"body":"`+snippetSecret+`","revision":2}`, tc.mutate), tc.status, snippetSecret)
		assertBoundedRefusal(t, "DELETE "+tc.name, c.delete(snippet.ID, `{"revision":2}`, tc.mutate), tc.status)
	}
	for _, tc := range ergoIdentityRedCases(cfg) {
		assertBoundedRefusal(t, "GET "+tc.name, ergoRequest(t, s, http.MethodGet, "http://localhost/api/snippets", "", nil, tc.mutate), tc.status)
	}
	assertBoundedRefusal(t, "GET query", ergoRequest(t, s, http.MethodGet, "http://localhost/api/snippets?x=1", "", nil, nil), http.StatusBadRequest)
	assertBoundedRefusal(t, "PUT collection", ergoRequest(t, s, http.MethodPut, "http://localhost/api/snippets", "application/json", strings.NewReader(validSnippet), nil), http.StatusMethodNotAllowed, snippetSecret)
	assertBoundedRefusal(t, "PUT item", ergoRequest(t, s, http.MethodPut, "http://localhost/api/snippets/"+snippet.ID, "application/json", strings.NewReader(validSnippet), nil), http.StatusMethodNotAllowed, snippetSecret)
	assertBoundedRefusal(t, "POST item", ergoRequest(t, s, http.MethodPost, "http://localhost/api/snippets/"+snippet.ID, "application/json", strings.NewReader(validSnippet), nil), http.StatusMethodNotAllowed, snippetSecret)
	assertBoundedRefusal(t, "HEAD collection", ergoRequest(t, s, http.MethodHead, "http://localhost/api/snippets", "", nil, nil), http.StatusMethodNotAllowed)

	for name, body := range map[string]string{
		"unknown_field":       `{"kind":"snippet","label":"L","body":"` + snippetSecret + `","extra":1}`,
		"trailing_garbage":    validSnippet + ` {"body":"` + snippetSecret + `"}`,
		"trailing_scalar":     validSnippet + ` 0`,
		"duplicate_key":       `{"kind":"snippet","label":"L","body":"x","body":"` + snippetSecret + `"}`,
		"folded_key":          `{"kind":"snippet","label":"L","body":"x","BODY":"` + snippetSecret + `"}`,
		"array":               `[{"kind":"snippet","label":"L","body":"` + snippetSecret + `"}]`,
		"scalar":              `"` + snippetSecret + `"`,
		"null":                `null`,
		"empty":               ``,
		"not_json":            snippetSecret,
		"missing_kind":        `{"label":"L","body":"` + snippetSecret + `"}`,
		"missing_body":        `{"kind":"snippet","label":"L"}`,
		"unknown_kind":        `{"kind":"note","label":"L","body":"` + snippetSecret + `"}`,
		"snippet_no_label":    `{"kind":"snippet","body":"` + snippetSecret + `"}`,
		"snippet_blank_label": `{"kind":"snippet","label":"   ","body":"` + snippetSecret + `"}`,
		"label_too_long":      `{"kind":"snippet","label":"` + strings.Repeat("l", snippetLabelMaxRunes+1) + `","body":"` + snippetSecret + `"}`,
		"label_control":       `{"kind":"snippet","label":"a\tb","body":"` + snippetSecret + `"}`,
		"clip_with_label":     `{"kind":"clip","label":"L","body":"` + snippetSecret + `"}`,
		"clip_pinned":         `{"kind":"clip","pinned":true,"body":"` + snippetSecret + `"}`,
		"snippet_osc_origin":  `{"kind":"snippet","label":"L","body":"` + snippetSecret + `","origin":"osc52"}`,
		"origin_too_long":     `{"kind":"clip","body":"` + snippetSecret + `","origin":"` + strings.Repeat("o", snippetOriginMaxRunes+1) + `"}`,
		"body_carriage":       `{"kind":"snippet","label":"L","body":"` + snippetSecret + `\r\n"}`,
		"body_escape":         `{"kind":"snippet","label":"L","body":"\u001b[31m` + snippetSecret + `"}`,
		"body_empty":          `{"kind":"snippet","label":"L","body":""}`,
		"body_number":         `{"kind":"snippet","label":"L","body":1}`,
		"pinned_string":       `{"kind":"snippet","label":"L","body":"` + snippetSecret + `","pinned":"yes"}`,
		"invalid_utf8":        `{"kind":"snippet","label":"L","body":"` + string([]byte{0xc3, 0x28}) + snippetSecret + `"}`,
	} {
		assertBoundedRefusal(t, "POST "+name, c.post(body, nil), http.StatusBadRequest, snippetSecret)
	}
	for name, body := range map[string]string{
		"missing_revision":  `{"label":"` + snippetSecret + `"}`,
		"zero_revision":     `{"label":"` + snippetSecret + `","revision":0}`,
		"revision_string":   `{"label":"` + snippetSecret + `","revision":"2"}`,
		"revision_float":    `{"label":"` + snippetSecret + `","revision":2.0}`,
		"negative_revision": `{"label":"` + snippetSecret + `","revision":-1}`,
		"no_fields":         `{"revision":2}`,
		"unknown_field":     `{"label":"` + snippetSecret + `","revision":2,"kind":"clip"}`,
		"id_in_body":        `{"label":"` + snippetSecret + `","revision":2,"id":"x"}`,
		"trailing":          `{"label":"` + snippetSecret + `","revision":2}{}`,
	} {
		assertBoundedRefusal(t, "PATCH "+name, c.patch(snippet.ID, body, nil), http.StatusBadRequest, snippetSecret)
	}
	for name, body := range map[string]string{
		"missing_revision": `{}`,
		"zero_revision":    `{"revision":0}`,
		"unknown_field":    `{"revision":2,"body":"` + snippetSecret + `"}`,
		"empty":            ``,
		"array":            `[2]`,
	} {
		assertBoundedRefusal(t, "DELETE "+name, c.delete(snippet.ID, body, nil), http.StatusBadRequest, snippetSecret)
	}
	// 413: a decoded body over 16 KiB, and a raw request over the reader
	// bound, are each refused without echo; a maximal body is accepted.
	overBody := c.post(`{"kind":"snippet","label":"L","body":"`+strings.Repeat("s", snippetBodyMaxBytes+1)+`"}`, nil)
	assertBoundedRefusal(t, "body over 16 KiB", overBody, http.StatusRequestEntityTooLarge, "ssssssss")
	overRequest := c.post(`{"kind":"snippet","label":"L","body":"`+strings.Repeat("\\t", snippetRequestMaxBytes/2)+`"}`, nil)
	assertBoundedRefusal(t, "request over reader bound", overRequest, http.StatusRequestEntityTooLarge, "\\t\\t\\t\\t")
	escapedMax := c.post(`{"kind":"snippet","label":"Max","body":"`+strings.Repeat("\\n", snippetBodyMaxBytes)+`"}`, nil)
	if escapedMax.Code != http.StatusCreated || len(decodeSnippet(t, escapedMax).Body) != snippetBodyMaxBytes {
		t.Fatalf("maximal escaped body=%d %s", escapedMax.Code, escapedMax.Body.String()[:64])
	}
	overPatch := c.patch(snippet.ID, `{"body":"`+strings.Repeat("s", snippetBodyMaxBytes+1)+`","revision":2}`, nil)
	assertBoundedRefusal(t, "PATCH body over 16 KiB", overPatch, http.StatusRequestEntityTooLarge, "ssssssss")
	if got := decodeSnippetList(t, c.get()); len(got) != beforeRefusals+1 {
		t.Fatalf("refusals mutated the store: %d records", len(got))
	}

	// 507: the snippet cap, then the file cap, each with the fixed text and
	// no echo; clips still have their own ring afterwards.
	for i := 0; i < snippetStoreMaxSnippets; i++ {
		if _, err := s.snippets.create(snippetKindSnippet, "fill"+strconv.Itoa(i), "fill-body-"+strconv.Itoa(i), false, ""); err != nil && !errors.Is(err, errSnippetStoreFull) {
			t.Fatal(err)
		}
	}
	assertBoundedRefusal(t, "snippet cap", c.post(`{"kind":"snippet","label":"L","body":"`+snippetSecret+`"}`, nil), http.StatusInsufficientStorage, snippetSecret)
	if w := c.post(`{"kind":"clip","body":"still room","origin":"iphone"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("clip after snippet cap=%d %s", w.Code, w.Body.String())
	}
}

func TestSnippetAPIFileCapIsInsufficientStorage(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.snippetErr != nil {
		t.Fatal(s.snippetErr)
	}
	s.snippets.file.fileSync = func(*os.File) error { return nil }
	s.snippets.file.dirSync = func(*os.File) error { return nil }
	c := snippetClient{t, s}
	big := strings.Repeat("y", snippetBodyMaxBytes)
	var last *httptest.ResponseRecorder
	for i := 0; i < snippetStoreMaxSnippets; i++ {
		unique := strconv.Itoa(i) + big[len(strconv.Itoa(i)):]
		last = c.post(`{"kind":"snippet","label":"big`+strconv.Itoa(i)+`","body":"`+unique+`"}`, nil)
		if last.Code != http.StatusCreated {
			break
		}
	}
	assertBoundedRefusal(t, "file cap", last, http.StatusInsufficientStorage, "yyyyyyyy")
	if got := decodeSnippetList(t, c.get()); len(got) == 0 || len(got) >= snippetStoreMaxSnippets {
		t.Fatalf("file cap count=%d", len(got))
	}
	reopened, err := newSnippetStore(cfg.SnippetStorePath)
	if err != nil || len(reopened.list()) != len(decodeSnippetList(t, c.get())) {
		t.Fatalf("file-cap refusal changed the disk: %v", err)
	}
}

// SF1 (API half) and §3f: a store that cannot be trusted answers 503 on
// every route, never a partial list; a store that faulted after publishing
// still lists what is on disk and refuses further mutation.
func TestSnippetAPIStoreUnavailable(t *testing.T) {
	assertAllUnavailable := func(t *testing.T, s *Server, includeGet bool) {
		t.Helper()
		c := snippetClient{t, s}
		if includeGet {
			assertBoundedRefusal(t, "GET", c.get(), http.StatusServiceUnavailable)
		}
		assertBoundedRefusal(t, "POST", c.post(`{"kind":"snippet","label":"L","body":"`+snippetSecret+`"}`, nil), http.StatusServiceUnavailable, snippetSecret)
		assertBoundedRefusal(t, "PATCH", c.patch("0123456789abcdef0123456789abcdef", `{"body":"`+snippetSecret+`","revision":1}`, nil), http.StatusServiceUnavailable, snippetSecret)
		assertBoundedRefusal(t, "DELETE", c.delete("0123456789abcdef0123456789abcdef", `{"revision":1}`, nil), http.StatusServiceUnavailable)
		if !bytes.Equal(bytes.TrimSpace(c.post(`{"kind":"snippet","label":"L","body":"x"}`, nil).Body.Bytes()), []byte("snippet store unavailable")) {
			t.Fatal("unavailable text drifted")
		}
	}
	t.Run("unconfigured", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		cfg.SnippetStorePath = ""
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if !errors.Is(s.snippetErr, errSnippetStoreUnavailable) {
			t.Fatalf("unconfigured error=%v", s.snippetErr)
		}
		assertAllUnavailable(t, s, true)
	})
	for name, body := range map[string]string{
		"duplicate_ids":   `{"version":1,"items":[{"id":"0123456789abcdef0123456789abcdef","kind":"snippet","label":"a","body":"b","pinned":false,"origin":"","revision":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","expires_at":null},{"id":"0123456789abcdef0123456789abcdef","kind":"snippet","label":"c","body":"d","pinned":false,"origin":"","revision":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","expires_at":null}]}`,
		"unknown_version": `{"version":3,"items":[]}`,
		"duplicate_key":   `{"version":1,"items":[],"items":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ergoFrontConfig(t)
			if err := os.WriteFile(cfg.SnippetStorePath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
			if s.snippetErr == nil {
				t.Fatalf("%s accepted", name)
			}
			assertAllUnavailable(t, s, true)
		})
	}
	t.Run("faulted_after_publication", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if s.snippetErr != nil {
			t.Fatal(s.snippetErr)
		}
		c := snippetClient{t, s}
		s.snippets.file.dirSync = func(*os.File) error { return errors.New("injected directory sync") }
		w := c.post(`{"kind":"snippet","label":"L","body":"published"}`, nil)
		assertBoundedRefusal(t, "faulted post", w, http.StatusServiceUnavailable, "published")
		if got := decodeSnippetList(t, c.get()); len(got) != 1 || got[0].Body != "published" {
			t.Fatalf("published record hidden: %+v", got)
		}
		assertAllUnavailable(t, s, false)
	})
}

func TestSnippetAPISharesOperatorLimiter(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	limited := 0
	for i := 0; i < 60; i++ {
		if w := ergoRequestMetered(t, s, http.MethodGet, "http://localhost/api/snippets", "", nil, nil); w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("snippet route bypassed the shared operator limiter")
	}
}

// contract F1 (c1_osc_slot): the distinguished OSC 52 record has its own
// slot outside the 20-clip manual ring — creating it never evicts a manual
// clip, and manual clips never evict it.
func TestSnippetAPIOSCFirstWriteNeverEvictsManualClip(t *testing.T) {
	for _, year := range []int{2026, 2027} {
		t.Run(strconv.Itoa(year), func(t *testing.T) {
			cfg := ergoFrontConfig(t)
			s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
			if s.snippetErr != nil {
				t.Fatal(s.snippetErr)
			}
			s.snippets.file.fileSync = func(*os.File) error { return nil }
			s.snippets.file.dirSync = func(*os.File) error { return nil }
			base := time.Date(year, 8, 27, 12, 0, 0, 0, time.UTC)
			tick := 0
			now := func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }
			s.snippets.now = now
			c := snippetClient{t, s}
			for i := 0; i < snippetClipRing; i++ {
				if w := c.post(`{"kind":"clip","body":"manual-`+strconv.Itoa(i)+`","origin":"laptop"}`, nil); w.Code != http.StatusCreated {
					t.Fatalf("manual clip %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if w := c.putOSC(`{"body":"osc-value","origin":"laptop"}`, nil); w.Code != http.StatusOK {
				t.Fatalf("osc: %d %s", w.Code, w.Body.String())
			}
			manual, osc := 0, 0
			for _, v := range decodeSnippetList(t, c.get()) {
				if v.Kind != snippetKindClip {
					continue
				}
				if v.Body == "osc-value" {
					osc++
				} else {
					manual++
				}
			}
			if osc != 1 || manual != snippetClipRing {
				t.Fatalf("after the first OSC write: osc=%d manual=%d (want 1 and %d)", osc, manual, snippetClipRing)
			}
			// The file reloads with the full manual ring plus the OSC record.
			reopened, err := newSnippetStoreWithClock(cfg.SnippetStorePath, now)
			if err != nil || len(reopened.list()) != snippetClipRing+1 {
				t.Fatalf("reload=%v records=%d", err, len(reopened.list()))
			}
		})
	}
}

func TestSnippetAPIManualClipsNeverEvictOSCRecord(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.snippetErr != nil {
		t.Fatal(s.snippetErr)
	}
	s.snippets.file.fileSync = func(*os.File) error { return nil }
	s.snippets.file.dirSync = func(*os.File) error { return nil }
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tick := 0
	s.snippets.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }
	c := snippetClient{t, s}
	first := c.putOSC(`{"body":"osc-1","origin":"phone"}`, nil)
	if first.Code != http.StatusOK {
		t.Fatal(first.Code)
	}
	oscID := decodeSnippet(t, first).ID
	if oscID != snippetOSCID {
		t.Fatalf("osc id=%q", oscID)
	}
	for i := 0; i < 2*snippetClipRing; i++ {
		if w := c.post(`{"kind":"clip","body":"manual-`+strconv.Itoa(i)+`","origin":"phone"}`, nil); w.Code != http.StatusCreated {
			t.Fatalf("manual clip %d: %d", i, w.Code)
		}
	}
	found, manual := false, 0
	for _, v := range decodeSnippetList(t, c.get()) {
		if v.Body == "osc-1" {
			found = true
		} else if v.Kind == snippetKindClip {
			manual++
		}
	}
	if !found || manual != snippetClipRing {
		t.Fatalf("osc record present=%v manual=%d (want present and %d)", found, manual, snippetClipRing)
	}
	// The next OSC write is still the same record, updated in place.
	if w := c.putOSC(`{"body":"osc-2","origin":"phone"}`, nil); w.Code != http.StatusOK || decodeSnippet(t, w).ID != oscID || decodeSnippet(t, w).Revision != 2 {
		t.Fatalf("osc rewrite: %d %s", w.Code, w.Body.String())
	}
}

// contract F2 (c2_canonical_keys): encoding/json matches object keys
// case-insensitively, so a closed schema must refuse non-canonical spellings
// before decoding — on every route, including nested objects.
func TestSnippetAndPreferencesAPIRefuseCaseFoldedKeys(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.snippetErr != nil || s.preferencesErr != nil {
		t.Fatal(s.snippetErr, s.preferencesErr)
	}
	c := snippetClient{t, s}
	created := c.post(`{"kind":"snippet","label":"x","body":"y"}`, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("baseline create=%d %s", created.Code, created.Body.String())
	}
	id := decodeSnippet(t, created).ID
	put := func(body string) *httptest.ResponseRecorder {
		return ergoRequest(t, s, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(body), func(r *http.Request) { r.Header.Set("If-Match", `"0"`) })
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"PUT THEME":             put(`{"version":1,"THEME":"` + preferencesSecret + `","font_size":14,"default_session":null}`),
		"PUT Version":           put(`{"Version":1,"theme":"default","font_size":14,"default_session":null}`),
		"PUT nested Realm":      put(`{"version":1,"theme":"default","font_size":14,"default_session":{"Realm":"r","SERVER":"s","name":"` + preferencesSecret + `"}}`),
		"POST KIND/BODY":        c.post(`{"KIND":"snippet","Label":"x","BODY":"`+snippetSecret+`"}`, nil),
		"POST Origin":           c.post(`{"kind":"clip","body":"`+snippetSecret+`","Origin":"laptop"}`, nil),
		"PATCH Body/REVISION":   c.patch(id, `{"Body":"`+snippetSecret+`","REVISION":1}`, nil),
		"DELETE Revision":       c.delete(id, `{"Revision":1}`, nil),
		"POST hyphen key":       c.post(`{"kind":"snippet","label":"x","body":"`+snippetSecret+`","pin-ned":true}`, nil),
		"POST empty key":        c.post(`{"kind":"snippet","label":"x","body":"`+snippetSecret+`","":1}`, nil),
		"POST unicode fold key": c.post(`{"kind":"snippet","label":"x","body":"`+snippetSecret+`","pinneıd":true}`, nil),
	} {
		assertBoundedRefusal(t, name, w, http.StatusBadRequest, preferencesSecret, snippetSecret)
	}
	if got := decodeSnippetList(t, c.get()); len(got) != 1 || got[0].Revision != 1 || got[0].Body != "y" {
		t.Fatalf("case-folded refusals mutated the store: %+v", got)
	}
	if got := decodePreferences(t, ergoRequest(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)); got.Stored {
		t.Fatalf("case-folded PUT stored a record: %+v", got)
	}
	// Canonical spellings still land, so the walker refuses only the fold.
	if w := c.patch(id, `{"body":"z","revision":1}`, nil); w.Code != http.StatusOK {
		t.Fatalf("canonical patch=%d %s", w.Code, w.Body.String())
	}
}

// Two devices flood the idempotent upsert, each
// retrying against the current revision after a stale 412, and the store
// ends with exactly one OSC record, every manual clip intact, and no
// response other than 200 or 412 on the way.
func TestSnippetAPIOSCUpsertIsOneGlobalRecordAcrossDevices(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.snippetErr != nil {
		t.Fatal(s.snippetErr)
	}
	s.snippets.file.fileSync = func(*os.File) error { return nil }
	s.snippets.file.dirSync = func(*os.File) error { return nil }
	c := snippetClient{t, s}
	for i := 0; i < snippetClipRing; i++ {
		w := c.post(`{"kind":"clip","body":"manual-`+strconv.Itoa(i)+`","origin":"laptop"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("manual clip %d: %d", i, w.Code)
		}
	}
	const writesPerDevice = 25
	type outcome struct {
		codes   map[int]int
		retries int
		err     string
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for device, origin := range []string{"iphone", "laptop"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := outcome{codes: map[int]int{}}
			var known uint64
			for i := 0; i < writesPerDevice; i++ {
				for attempt := 0; ; attempt++ {
					if attempt > 200 {
						out.err = "retry budget exhausted"
						results[device] = out
						return
					}
					expect := known
					r := httptest.NewRequest(http.MethodPut, "http://localhost/api/snippets/osc52", strings.NewReader(`{"body":"`+origin+`-`+strconv.Itoa(i)+`","origin":"`+origin+`"}`))
					r.Host = "localhost"
					r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
					r.Header.Set("X-Forwarded-Proto", "http")
					r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
					r.Header.Set("Origin", externalOrigin(cfg.Ingress))
					r.Header.Set("Sec-Fetch-Site", "same-origin")
					r.Header.Set("Cookie", csrfCookie+"="+ergoCSRF)
					r.Header.Set("X-Persea-CSRF", ergoCSRF)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("If-Match", `"`+strconv.FormatUint(expect, 10)+`"`)
					r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
					s.limiter.mu.Lock()
					s.limiter.at = time.Time{}
					s.limiter.mu.Unlock()
					w := httptest.NewRecorder()
					s.handler().ServeHTTP(w, r)
					out.codes[w.Code]++
					var body struct {
						ID       string `json:"id"`
						Revision uint64 `json:"revision"`
					}
					_ = json.Unmarshal(w.Body.Bytes(), &body)
					switch w.Code {
					case http.StatusOK:
						if body.ID != snippetOSCID || body.Revision != expect+1 {
							out.err = "200 with unexpected record " + w.Body.String()
							results[device] = out
							return
						}
						known = body.Revision
					case http.StatusPreconditionFailed:
						out.retries++
						known = body.Revision
						continue
					default:
						out.err = "unexpected status " + strconv.Itoa(w.Code) + " " + w.Body.String()
						results[device] = out
						return
					}
					break
				}
			}
			results[device] = out
		}()
	}
	wg.Wait()
	for device, out := range results {
		if out.err != "" || out.codes[http.StatusOK] != writesPerDevice {
			t.Fatalf("device %d: %+v", device, out)
		}
	}
	list := decodeSnippetList(t, c.get())
	osc, manual := 0, 0
	record, publicationErr := s.snippets.precondition(snippetOSCID, 2*writesPerDevice)
	if publicationErr != nil {
		t.Fatal(publicationErr)
	}
	for _, v := range list {
		if isOSCSnippetID(v.ID) {
			t.Fatal("publication metadata leaked into visible list")
		}
		if v.Body == record.Body {
			osc++
		} else if v.Kind == snippetKindClip {
			manual++
		}
	}
	if osc != 1 || manual != snippetClipRing || len(list) != snippetClipRing+1 {
		t.Fatalf("after the two-device flood: osc=%d manual=%d total=%d", osc, manual, len(list))
	}
	if record.Revision != 2*writesPerDevice || (record.Origin != "iphone" && record.Origin != "laptop") {
		t.Fatalf("osc record after flood=%+v", record)
	}
	reopened, err := newSnippetStore(cfg.SnippetStorePath)
	if err != nil || len(reopened.list()) != snippetClipRing+1 {
		t.Fatalf("reload=%v records=%d", err, len(reopened.list()))
	}
}
