package frontdoor

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestAliasAPIJSONETagAndCAS(t *testing.T) {
	dir := shortTestDir(t)
	socket := filepath.Join(dir, "alias-api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a := auth(7, "$3")
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = proto.ReadFrame(c)
				_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
				_, _ = proto.ReadFrame(c)
				_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{{Label: "s", Status: "ok", Sessions: []proto.Session{{Authority: a, Name: "live", Width: 80, Height: 24}}}}})
			}()
		}
	}()
	cfg := config.Front{Realms: []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}}, HandleTTLSeconds: 60, HandleCapacity: 8}
	s := newServer(cfg, ".", "127.0.0.1:8080")
	handle, err := s.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}
	h := s.handler()
	request := func(method, path, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewBufferString(body))
		r.Host = "127.0.0.1:8080"
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	requestBytes := func(method, path string, body []byte, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewReader(body))
		r.Host = "127.0.0.1:8080"
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	created := request(http.MethodPost, "/api/aliases", `{"display_alias":"Work","handle":"`+handle+`"}`, "")
	if created.Code != http.StatusCreated || created.Header().Get("ETag") != "\"1\"" {
		t.Fatalf("create=%d etag=%q body=%s", created.Code, created.Header().Get("ETag"), created.Body.String())
	}
	var record AliasRecord
	if err = json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	missing := request(http.MethodPatch, "/api/aliases/"+record.AliasID, `{"display_alias":"New"}`, "")
	if missing.Code != http.StatusPreconditionFailed {
		t.Fatalf("missing If-Match=%d", missing.Code)
	}
	invalidPrecondition := request(http.MethodPatch, "/api/aliases/"+record.AliasID, `{"display_alias":"New"}`, `"invalid"`)
	if invalidPrecondition.Code != http.StatusPreconditionFailed {
		t.Fatalf("invalid If-Match=%d", invalidPrecondition.Code)
	}
	responses := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, alias := range []string{"New", "Loser"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = request(http.MethodPatch, "/api/aliases/"+record.AliasID, `{"display_alias":"`+alias+`"}`, `"1"`)
		}()
	}
	wg.Wait()
	statuses := map[int]int{}
	for _, response := range responses {
		statuses[response.Code]++
		if response.Header().Get("ETag") != "\"2\"" {
			t.Fatalf("concurrent etag=%q", response.Header().Get("ETag"))
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent statuses=%v", statuses)
	}
	staleWithBadHandle := request(http.MethodPatch, "/api/aliases/"+record.AliasID, `{"display_alias":"Rebound","handle":"not-a-handle"}`, `"1"`)
	if staleWithBadHandle.Code != http.StatusConflict || staleWithBadHandle.Header().Get("ETag") != `"2"` {
		t.Fatalf("stale revision attribution=%d etag=%q body=%s", staleWithBadHandle.Code, staleWithBadHandle.Header().Get("ETag"), staleWithBadHandle.Body.String())
	}
	var current AliasRecord
	if err = json.Unmarshal(staleWithBadHandle.Body.Bytes(), &current); err != nil || current.Revision != 2 || current.AliasID != record.AliasID {
		t.Fatalf("stale current record=%+v err=%v", current, err)
	}
	for name, body := range map[string][]byte{
		"value": append([]byte(`{"display_alias":"`), append([]byte{0xff}, []byte(`","handle":"`+handle+`"}`)...)...),
		"key":   append([]byte(`{"display_ali`), append([]byte{0xff}, []byte(`s":"x","handle":"`+handle+`"}`)...)...),
	} {
		if got := requestBytes(http.MethodPost, "/api/aliases", body, ""); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid UTF-8 %s=%d body=%s", name, got.Code, got.Body.String())
		}
	}
	bad := request(http.MethodPost, "/api/aliases", `{"display_alias":"x","display_alias":"y","handle":"z"}`, "")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("duplicate body=%d", bad.Code)
	}
	unsupported := request(http.MethodPut, "/api/aliases", `{}`, "")
	if unsupported.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported=%d", unsupported.Code)
	}
	deleted := request(http.MethodDelete, "/api/aliases/"+record.AliasID, "", `"2"`)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
}
