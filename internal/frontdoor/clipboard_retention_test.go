package frontdoor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

func TestClipboardRetentionExactTextDedup(t *testing.T) {
	s := fastSnippetStore(t, shortTestDir(t))
	a, err := s.create(snippetKindClip, "", " exact bytes \n", false, "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.create(snippetKindClip, "", " exact bytes \n", false, "second")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(s.list()) != 1 || b.Revision <= a.Revision {
		t.Fatalf("duplicate did not renew one stable record: %q %q count=%d", a.ID, b.ID, len(s.list()))
	}
	c, err := s.create(snippetKindClip, "", "exact bytes \n", false, "third")
	if err != nil || c.ID == a.ID || len(s.list()) != 2 {
		t.Fatalf("identity must preserve leading space: %v", err)
	}
}

func TestClipboardRetentionEditMergesIntoExistingContent(t *testing.T) {
	s := fastSnippetStore(t, shortTestDir(t))
	a, err := s.create(snippetKindClip, "", "A", false, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.create(snippetKindClip, "", "B", false, "")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.update(a.ID, snippetUpdate{Body: strPtr("B")}, a.Revision)
	if err != nil || merged.ID != b.ID || len(s.list()) != 1 {
		t.Fatalf("edit did not merge into existing B: id=%q want=%q count=%d err=%v", merged.ID, b.ID, len(s.list()), err)
	}
}

func TestClipboardRetentionExactImageDedup(t *testing.T) {
	s := clipboardImageServer(t).clipboardImages
	body := oraclePNG(t, 3, 2)
	a, err := s.create("image/png", body, "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.create("image/png", body, "second")
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.list()
	if err != nil || a.ID != b.ID || len(items) != 1 {
		t.Fatalf("image duplicate did not preserve one stable ID: %q %q count=%d err=%v", a.ID, b.ID, len(items), err)
	}
}

func intPtr(v int) *int { return &v }

func TestClipboardRetentionMigrationRevisionOverflow(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ids := []string{strings.Repeat("1", 32), strings.Repeat("2", 32)}
	t.Run("text", func(t *testing.T) {
		path := filepath.Join(shortTestDir(t), "snippets.json")
		items := []SnippetRecord{}
		for _, id := range ids {
			items = append(items, SnippetRecord{ID: id, Kind: snippetKindClip, Body: "same", Revision: ^uint64(0), CreatedAt: clock, UpdatedAt: clock})
		}
		before, _ := json.Marshal(snippetStoreFile{Version: 2, Items: items})
		if err := os.WriteFile(path, before, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newSnippetStoreWithClock(path, func() time.Time { return clock }); !errors.Is(err, errSnippetStoreUnavailable) {
			t.Fatalf("overflow accepted: %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("overflow rewrote source")
		}
	})
	t.Run("image", func(t *testing.T) {
		dir := filepath.Join(shortTestDir(t), clipboardImageDirectory)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		payload := oraclePNG(t, 2, 2)
		before := map[string][]byte{}
		for _, id := range ids {
			record := clipboardImageRecord{ID: id, MediaType: "image/png", ByteSize: len(payload), CreatedAt: clock, UpdatedAt: clock, Revision: ^uint64(0)}
			header, _ := json.Marshal(record)
			raw := make([]byte, 8+len(header)+len(payload))
			copy(raw, "PCI2")
			binary.BigEndian.PutUint32(raw[4:8], uint32(len(header)))
			copy(raw[8:], header)
			copy(raw[8+len(header):], payload)
			before[id] = raw
			if err := os.WriteFile(filepath.Join(dir, id), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if s, err := newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock }); !errors.Is(err, errClipboardImageUnavailable) {
			if s != nil {
				s.close()
			}
			t.Fatalf("overflow accepted: %v", err)
		}
		for id, want := range before {
			got, err := os.ReadFile(filepath.Join(dir, id))
			if err != nil || !bytes.Equal(want, got) {
				t.Fatal("overflow changed source", err)
			}
		}
	})
}

func TestClipboardRetentionConcurrentDedupAndMaximumPolicy(t *testing.T) {
	s := fastSnippetStore(t, shortTestDir(t))
	image := clipboardImageServer(t).clipboardImages
	payload := oraclePNG(t, 2, 3)
	var wg sync.WaitGroup
	textIDs, imageIDs := make(chan string, 32), make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			policy := 1800
			if i%2 == 0 {
				policy = 604800
			}
			if i == 31 {
				policy = 0
			}
			r, err := s.create(snippetKindClip, "", "same exact body", false, "", policy)
			if err != nil {
				t.Error(err)
			} else {
				textIDs <- r.ID
			}
			im, err := image.create("image/png", payload, "", policy)
			if err != nil {
				t.Error(err)
			} else {
				imageIDs <- im.ID
			}
		}()
	}
	wg.Wait()
	close(textIDs)
	close(imageIDs)
	for _, ch := range []chan string{textIDs, imageIDs} {
		first := ""
		count := 0
		for id := range ch {
			count++
			if first == "" {
				first = id
			}
			if id != first {
				t.Error("concurrent duplicate changed ID")
			}
		}
		if count != 32 {
			t.Fatalf("accepted=%d", count)
		}
	}
	text := s.list()
	images, err := image.list()
	if len(text) != 1 || text[0].Revision != 32 || text[0].RetentionSeconds != 0 || text[0].ExpiresAt != nil {
		t.Fatalf("text=%+v", text)
	}
	if err != nil || len(images) != 1 || images[0].Revision != 32 || images[0].RetentionSeconds != 0 || !images[0].ExpiresAt.IsZero() {
		t.Fatalf("images=%+v %v", images, err)
	}
}

func TestClipboardRetentionSaveRenewMergeCASAndNoShortening(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	s, err := newSnippetStoreWithClock(filepath.Join(shortTestDir(t), "snippets.json"), func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.create(snippetKindClip, "", "A", false, "", 604800)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.create(snippetKindClip, "", "B", false, "", 1800)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if _, err := s.update(a.ID, snippetUpdate{Body: strPtr("B")}, a.Revision+1); !errors.Is(err, errSnippetConflict) || len(s.list()) != 2 {
		t.Fatal("stale edit mutated source or target", err)
	}
	merged, err := s.update(a.ID, snippetUpdate{Body: strPtr("B")}, a.Revision)
	if err != nil || merged.ID != b.ID || merged.Revision != b.Revision+1 || merged.RetentionSeconds != 604800 || !merged.ExpiresAt.Equal(clock.Add(7*24*time.Hour)) || len(s.list()) != 1 {
		t.Fatalf("merge=%+v err=%v", merged, err)
	}
	if _, err := s.update(b.ID, snippetUpdate{Body: strPtr("stale edit")}, b.Revision); !errors.Is(err, errSnippetConflict) {
		t.Fatal("merge lost target CAS", err)
	}
	clock = clock.Add(time.Minute)
	saved, err := s.update(b.ID, snippetUpdate{Body: strPtr("B")}, merged.Revision)
	if err != nil || !saved.UpdatedAt.After(merged.UpdatedAt) || !saved.ExpiresAt.After(*merged.ExpiresAt) {
		t.Fatal("unchanged explicit save did not renew", err)
	}
	clock = clock.Add(-time.Hour)
	extended, err := s.update(b.ID, snippetUpdate{Body: strPtr("B")}, saved.Revision)
	if err != nil || extended.RetentionSeconds != 604800 || extended.ExpiresAt.Before(*saved.ExpiresAt) || extended.UpdatedAt.Before(saved.UpdatedAt) {
		t.Fatal("clock rollback during automatic save shortened record", err)
	}
	forever, err := s.update(b.ID, snippetUpdate{RetentionSeconds: intPtr(0)}, extended.Revision)
	if err != nil || forever.ExpiresAt != nil {
		t.Fatal("no expiry not applied", err)
	}
	again, err := s.create(snippetKindClip, "", "B", false, "", 1800)
	if err != nil || again.ID != b.ID || again.RetentionSeconds != 0 || again.ExpiresAt != nil {
		t.Fatal("duplicate shortened no-expiry", err)
	}
}

func TestClipboardRetentionPreservesProtectedCapacity(t *testing.T) {
	for _, retention := range []int{0, 14400} {
		t.Run(fmt.Sprint(retention), func(t *testing.T) {
			s := fastSnippetStore(t, shortTestDir(t))
			for i := 0; i < snippetClipRing; i++ {
				if _, err := s.create(snippetKindClip, "", fmt.Sprint(i), false, "", retention); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.create(snippetKindClip, "", "overflow", false, "", 1800); !errors.Is(err, errSnippetStoreFull) || len(s.list()) != snippetClipRing {
				t.Fatal("protected entries evicted", err)
			}
			if _, err := s.create(snippetKindClip, "", "0", false, "", 1800); err != nil || len(s.list()) != snippetClipRing {
				t.Fatal("duplicate refused at capacity", err)
			}
		})
	}
}

func TestClipboardRetentionOSCAtomicCanonicalAndMigration(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(shortTestDir(t), "snippets.json")
	s, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.create(snippetKindSnippet, "Saved", "shared", true, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := s.upsertOSC("shared", "pane-a", uint64Ptr(0), 1800)
	if err != nil || publication.ID != snippetOSCID || publication.Revision != 1 {
		t.Fatal(err)
	}
	list := s.list()
	if len(list) != 1 || list[0].ID != saved.ID || list[0].ExpiresAt != nil {
		t.Fatalf("OSC duplicated existing saved text: %+v", list)
	}
	before, _ := os.ReadFile(path)
	s.file.fileSync = func(*os.File) error { return errors.New("injected") }
	if _, err := s.upsertOSC("new publication", "pane-b", uint64Ptr(1)); err == nil {
		t.Fatal("fault accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || s.records[snippetOSCID].Body != "shared" || len(s.list()) != 1 {
		t.Fatal("publication/canonical update not atomic")
	}
	// A legacy authority value migrates once, but deleting its canonical value
	// does not resurrect it on later version-2 restarts.
	expiry := clock.Add(30 * time.Minute)
	old := SnippetRecord{ID: snippetOSCID, Kind: snippetKindClip, Body: "legacy OSC", Revision: 7, CreatedAt: clock, UpdatedAt: clock, ExpiresAt: &expiry}
	wire, _ := json.Marshal(snippetStoreFile{Version: 1, Items: []SnippetRecord{old}})
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	list = reopened.list()
	if len(list) != 1 || !snippetIDRE.MatchString(list[0].ID) || list[0].Body != old.Body || list[0].RetentionSeconds != 1800 || !list[0].ExpiresAt.Equal(expiry) || reopened.records[snippetOSCID].Revision != 7 {
		t.Fatal("legacy OSC migration lost content/policy")
	}
	if _, err := reopened.delete(list[0].ID, list[0].Revision); err != nil {
		t.Fatal(err)
	}
	last, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
	if err != nil || len(last.list()) != 0 {
		t.Fatal("deleted canonical OSC resurrected", err)
	}
}

func TestClipboardRetentionImageRenewCASRestartAndLegacy(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(shortTestDir(t), clipboardImageDirectory)
	s, err := newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	payload := oraclePNG(t, 2, 2)
	first, err := s.create("image/png", payload, "", 14400)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	again, err := s.create("image/png", payload, "", 1800)
	if err != nil || again.ID != first.ID || again.RetentionSeconds != 14400 || again.Revision != 2 || !again.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("image renewal=%+v %v", again, err)
	}
	if _, err := s.updateRetention(first.ID, 0, first.Revision); !errors.Is(err, errClipboardImageConflict) {
		t.Fatal("stale image update accepted")
	}
	permanent, err := s.updateRetention(first.ID, 0, again.Revision)
	if err != nil || !permanent.ExpiresAt.IsZero() {
		t.Fatal(err)
	}
	body, _ := json.Marshal(permanent)
	if !bytes.Contains(body, []byte(`"expires_at":null`)) {
		t.Fatal("no-expiry wire is not null")
	}
	clock = clock.Add(31 * 24 * time.Hour)
	reopened, err := newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	read, got, err := reopened.get(first.ID)
	if err != nil || read != permanent || !bytes.Equal(got, payload) {
		t.Fatal("no-expiry image restart", err)
	}
	// Replace this test file with its legacy six-field PCI1 representation.
	legacyClock := clock
	legacy := map[string]any{"id": first.ID, "media_type": "image/png", "byte_size": len(payload), "created_at": legacyClock, "expires_at": legacyClock.Add(30 * time.Minute), "origin": ""}
	header, _ := json.Marshal(legacy)
	raw := make([]byte, 8+len(header)+len(payload))
	copy(raw, "PCI1")
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(header)))
	copy(raw[8:], header)
	copy(raw[8+len(header):], payload)
	if err := os.WriteFile(filepath.Join(dir, first.ID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	migrated, err := newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.close()
	item, _, err := migrated.get(first.ID)
	if err != nil || item.RetentionSeconds != 1800 || item.Revision != 1 || !item.UpdatedAt.Equal(legacyClock) {
		t.Fatal("legacy image migration", err)
	}
	clock = clock.Add(30 * time.Minute)
	if err := migrated.reap(); err != nil {
		t.Fatal(err)
	}
	if items, _ := migrated.list(); len(items) != 0 {
		t.Fatal("migrated image not reaped")
	}
}

func TestClipboardRetentionDefaultPersistenceAndWireGrammar(t *testing.T) {
	s := clipboardImageServer(t)
	get := ergoRequest(t, s, http.MethodGet, "http://localhost/api/clipboard/preferences", "", nil, nil)
	var initial clipboardPreferencesRecord
	if err := json.Unmarshal(get.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if get.Code != 200 || initial.Version != 1 || initial.DefaultRetentionSeconds != 1800 || initial.Revision != 1 || get.Header().Get("ETag") != `"1"` {
		t.Fatalf("preferences GET %d %s", get.Code, get.Body.String())
	}
	put := ergoRequest(t, s, http.MethodPut, "http://localhost/api/clipboard/preferences", "application/json", strings.NewReader(`{"default_retention_seconds":86400,"revision":1}`), nil)
	if put.Code != 200 || put.Header().Get("ETag") != `"2"` {
		t.Fatalf("preferences PUT %d %s", put.Code, put.Body.String())
	}
	stale := ergoRequest(t, s, http.MethodPut, "http://localhost/api/clipboard/preferences", "application/json", strings.NewReader(`{"default_retention_seconds":1800,"revision":1}`), nil)
	if stale.Code != 412 {
		t.Fatal("preferences CAS", stale.Code)
	}
	client := snippetClient{t, s}
	text := decodeSnippet(t, client.post(`{"kind":"clip","body":"default policy"}`, nil))
	if text.RetentionSeconds != 86400 {
		t.Fatal("text ignored shared default")
	}
	image := decodeClipboardImage(t, clipboardImageRequest(t, s, http.MethodPost, "", "image/png", oraclePNG(t, 1, 2), nil))
	if image.RetentionSeconds != 86400 {
		t.Fatal("image ignored shared default")
	}
	reopened, err := openClipboardPreferencesStore(s.cfg.SnippetStorePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.get()
	if err != nil || record.Revision != 2 || record.DefaultRetentionSeconds != 86400 {
		t.Fatal("default did not persist")
	}
	for _, body := range []string{`{"default_retention_seconds":1,"revision":2}`, `{"default_retention_seconds":null,"revision":2}`, `{"default_retention_seconds":1800.5,"revision":2}`, `{"default_retention_seconds":1800,"revision":2,"extra":1}`, `{"default_retention_seconds":1800,"revision":2,"revision":2}`, `{"DEFAULT_RETENTION_SECONDS":1800,"revision":2}`} {
		w := ergoRequest(t, s, http.MethodPut, "http://localhost/api/clipboard/preferences", "application/json", strings.NewReader(body), nil)
		if w.Code != 400 {
			t.Fatalf("prefs grammar status=%d body=%s", w.Code, body)
		}
	}
	for _, value := range []string{"null", "1", "-1", "1800.5", `"1800"`} {
		if w := client.post(`{"kind":"clip","body":"invalid","retention_seconds":`+value+`}`, nil); w.Code != 400 {
			t.Fatalf("text retention %s accepted: %d", value, w.Code)
		}
	}
	for _, value := range []string{"-1", "1", "1800.0", "01800", " 1800", "1800,1800"} {
		w := clipboardImageRequest(t, s, http.MethodPost, "", "image/png", oraclePNG(t, 1, 2), func(r *http.Request) { r.Header.Set(clipboardImageRetentionHeader, value) })
		if w.Code != 400 {
			t.Fatalf("image retention %q accepted: %d", value, w.Code)
		}
	}
	for _, tc := range ergoMutationRedCases(s.cfg) {
		w := ergoRequest(t, s, http.MethodPut, "http://localhost/api/clipboard/preferences", "application/json", strings.NewReader(`{"default_retention_seconds":0,"revision":2}`), tc.mutate)
		if w.Code != tc.status {
			t.Fatalf("preferences %s status=%d want=%d", tc.name, w.Code, tc.status)
		}
		w = clipboardImageRequest(t, s, http.MethodPatch, "/"+image.ID, "application/json", []byte(`{"retention_seconds":0,"revision":1}`), tc.mutate)
		if w.Code != tc.status {
			t.Fatalf("image PATCH %s status=%d want=%d", tc.name, w.Code, tc.status)
		}
	}
}

func TestClipboardRetentionImageAndDefaultFailureAtomicity(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprint(afterRename), func(t *testing.T) {
			s := clipboardImageServer(t)
			image, err := s.clipboardImages.create("image/png", oraclePNG(t, 2, 3), "", 1800)
			if err != nil {
				t.Fatal(err)
			}
			imagePath := filepath.Join(filepath.Dir(s.cfg.SnippetStorePath), clipboardImageDirectory, image.ID)
			old, _ := os.ReadFile(imagePath)
			fail := func(*os.File) error { return errors.New("injected") }
			if afterRename {
				s.clipboardImages.file.dirSync = fail
				s.clipboardPreferences.file.dirSync = fail
			} else {
				s.clipboardImages.file.fileSync = fail
				s.clipboardPreferences.file.fileSync = fail
			}
			if _, err := s.clipboardImages.updateRetention(image.ID, 0, image.Revision); err == nil {
				t.Fatal("failed image update accepted")
			}
			now, _ := os.ReadFile(imagePath)
			if afterRename {
				if bytes.Equal(old, now) || s.clipboardImages.file.fault == nil {
					t.Fatal("post-rename image state not whole/faulted")
				}
			} else if !bytes.Equal(old, now) || s.clipboardImages.records[image.ID] != image {
				t.Fatal("pre-rename image update changed state")
			}
			if _, err := s.clipboardPreferences.put(0, 1); err == nil {
				t.Fatal("failed preferences write accepted")
			}
			prefs, err := openClipboardPreferencesStore(s.cfg.SnippetStorePath)
			if err != nil {
				t.Fatal(err)
			}
			record, err := prefs.get()
			if err != nil {
				t.Fatal(err)
			}
			want := 1800
			if afterRename {
				want = 0
			}
			if record.DefaultRetentionSeconds != want {
				t.Fatal("preferences failure left partial state")
			}
		})
	}
}
