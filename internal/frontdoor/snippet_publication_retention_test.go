package frontdoor

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnippetPublicationFollowsCanonicalLifetime(t *testing.T) {
	for _, action := range []string{"delete", "delete-publication", "edit", "shorten", "load-orphan", "load-shorter"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(shortTestDir(t), "snippets.json")
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			s, err := newSnippetStoreWithClock(path, clock)
			if err != nil {
				t.Fatal(err)
			}
			publication, err := s.upsertOSC("retention-sentinel", "", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			canonical := s.list()[0].SnippetRecord
			switch action {
			case "delete":
				_, err = s.delete(canonical.ID, canonical.Revision)
			case "delete-publication":
				_, err = s.delete(publication.ID, publication.Revision)
			case "edit":
				_, err = s.update(canonical.ID, snippetUpdate{Body: strPtr("replacement")}, canonical.Revision)
			case "shorten", "load-shorter":
				seconds := 1800
				if action == "shorten" {
					canonical, err = s.update(canonical.ID, snippetUpdate{RetentionSeconds: &seconds}, canonical.Revision)
				} else {
					// A file produced by the old writer has mismatched deadlines.
					canonical.RetentionSeconds = seconds
					canonical.ExpiresAt = clipboardExpiry(now, seconds, nil)
					b, marshalErr := json.Marshal(snippetStoreFile{Version: snippetStoreVersion, Items: []SnippetRecord{publication, canonical}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					if err = os.WriteFile(path, b, 0600); err == nil {
						s, err = newSnippetStoreWithClock(path, clock)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				current, conflict := s.upsertOSC("stale", "", uint64Ptr(99))
				if !errors.Is(conflict, errSnippetConflict) || current.Revision != publication.Revision || current.ExpiresAt == nil || !current.ExpiresAt.Equal(*canonical.ExpiresAt) {
					t.Fatal("publication did not retain CAS revision with shortened deadline")
				}
				now = *canonical.ExpiresAt
				err = s.reap()
			case "load-orphan":
				b, marshalErr := json.Marshal(snippetStoreFile{Version: snippetStoreVersion, Items: []SnippetRecord{publication}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(path, b, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if action != "load-orphan" {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), "retention-sentinel") {
					t.Fatal("obsolete body remains on disk before restart")
				}
			}
			s, err = newSnippetStoreWithClock(path, clock)
			if err != nil {
				t.Fatal(err)
			}
			current, conflict := s.upsertOSC("stale", "", uint64Ptr(publication.Revision))
			if !errors.Is(conflict, errSnippetConflict) || current.ID != "" {
				t.Fatal("obsolete publication survived restart")
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "retention-sentinel") {
				t.Fatal("obsolete body remains on disk")
			}
			if action == "edit" && (len(s.list()) != 1 || s.list()[0].Body != "replacement") {
				t.Fatal("replacement was lost")
			}
		})
	}
}

func TestSnippetOSCConflictContainsOnlyRevisionMetadata(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	c := snippetClient{t, s}
	if w := c.putOSC(`{"body":"private-publication","origin":"private-origin"}`, nil); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	for _, revision := range []uint64{1, 0} {
		w := c.putOSC(`{"body":"stale"}`, func(r *http.Request) { r.Header.Set("If-Match", `"99"`) })
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusPreconditionFailed || w.Header().Get("Cache-Control") != "no-store" || len(metadata) != 2 || string(metadata["id"]) != `"osc52"` || decodeSnippet(t, w).Revision != revision {
			t.Fatal("conflict must contain only current id and revision")
		}
		if revision == 1 {
			item := decodeSnippetList(t, c.get())[0]
			if w := c.delete(item.ID, `{"revision":1}`, nil); w.Code != http.StatusNoContent {
				t.Fatal(w.Code)
			}
		}
	}
}
