package frontdoor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

func clipboardImageServer(t *testing.T) *Server {
	t.Helper()
	cfg := ergoFrontConfig(t)
	cfg.ImageUploadMaxBytes = proto.MaxImage
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.clipboardImageErr != nil {
		t.Fatal(s.clipboardImageErr)
	}
	t.Cleanup(s.clipboardImages.close)
	return s
}

func clipboardImageRequest(t *testing.T, s *Server, method, suffix, media string, body []byte, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	return ergoRequest(t, s, method, "http://localhost/api/clipboard/images"+suffix, media, bytes.NewReader(body), mutate)
}

func decodeClipboardImage(t *testing.T, w *httptest.ResponseRecorder) clipboardImageRecord {
	t.Helper()
	var record clipboardImageRecord
	if err := decodeStrict(w.Body.Bytes(), &record); err != nil || !validClipboardImageRecord(record) {
		t.Fatalf("invalid image response: %d %v", w.Code, err)
	}
	return record
}

func TestClipboardImageAPIContractAndIndependentTextStorage(t *testing.T) {
	s := clipboardImageServer(t)
	clock := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.clipboardImages.now = func() time.Time { return clock }
	before, err := os.ReadFile(s.cfg.SnippetStorePath)
	if err != nil {
		t.Fatal(err)
	}
	var records []clipboardImageRecord
	for _, fixture := range []struct {
		media string
		body  []byte
	}{
		{"image/png", oraclePNG(t, 1, 1)}, {"image/jpeg", oracleJPEG(t)}, {"image/gif", oracleGIF(t)}, {"image/webp", oracleWebP()},
	} {
		clock = clock.Add(time.Second)
		created := clipboardImageRequest(t, s, http.MethodPost, "", fixture.media, fixture.body, func(r *http.Request) { r.Header.Set(clipboardImageOriginHeader, " phone ") })
		if created.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", fixture.media, created.Code, created.Body.String())
		}
		record := decodeClipboardImage(t, created)
		if record.MediaType != fixture.media || record.ByteSize != len(fixture.body) || record.Origin != "phone" || !record.CreatedAt.Equal(clock) || !record.ExpiresAt.Equal(clock.Add(30*time.Minute)) {
			t.Fatalf("metadata: %+v", record)
		}
		fetched := clipboardImageRequest(t, s, http.MethodGet, "/"+record.ID, "", nil, nil)
		if fetched.Code != http.StatusOK || !bytes.Equal(fetched.Body.Bytes(), fixture.body) || fetched.Header().Get("Content-Type") != fixture.media || fetched.Header().Get("Cache-Control") != "no-store" || fetched.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("raw image response: %d %v", fetched.Code, fetched.Header())
		}
		records = append(records, record)
	}
	listed := clipboardImageRequest(t, s, http.MethodGet, "", "", nil, nil)
	var list struct {
		Items []clipboardImageRecord `json:"items"`
	}
	if listed.Code != http.StatusOK || decodeStrict(listed.Body.Bytes(), &list) != nil || len(list.Items) != 4 || list.Items[0].ID != records[3].ID || list.Items[3].ID != records[0].ID {
		t.Fatal("image listing order/shape")
	}
	clock = clock.Add(20 * time.Minute)
	for i := 0; i < 3; i++ {
		_ = clipboardImageRequest(t, s, http.MethodGet, "/"+records[0].ID, "", nil, nil)
	}
	if record := s.clipboardImages.records[records[0].ID]; !record.ExpiresAt.Equal(records[0].ExpiresAt) {
		t.Fatal("fetch renewed retention")
	}
	deleted := clipboardImageRequest(t, s, http.MethodDelete, "/"+records[0].ID, "application/json", []byte(`{}`), nil)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete: %d", deleted.Code)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(s.cfg.SnippetStorePath), clipboardImageDirectory, records[0].ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete retained image payload on disk")
	}
	assertBoundedRefusal(t, "deleted image", clipboardImageRequest(t, s, http.MethodGet, "/"+records[0].ID, "", nil, nil), http.StatusNotFound)
	clock = records[3].ExpiresAt
	for _, record := range records[1:] {
		assertBoundedRefusal(t, "expired image API", clipboardImageRequest(t, s, http.MethodGet, "/"+record.ID, "", nil, nil), http.StatusNotFound)
	}
	if w := clipboardImageRequest(t, s, http.MethodGet, "", "", nil, nil); w.Code != http.StatusOK || w.Body.String() != "{\"items\":[]}\n" {
		t.Fatal("expired images remained in API listing")
	}
	after, err := os.ReadFile(s.cfg.SnippetStorePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("image lifecycle changed text store bytes")
	}
}

func TestClipboardImageAPIAuthCSRFAndClosedRequestGrammar(t *testing.T) {
	s := clipboardImageServer(t)
	png := oraclePNG(t, 1, 1)
	created := clipboardImageRequest(t, s, http.MethodPost, "", "image/png", png, nil)
	record := decodeClipboardImage(t, created)
	for _, tc := range ergoIdentityRedCases(s.cfg) {
		for _, suffix := range []string{"", "/" + record.ID} {
			assertBoundedRefusal(t, "GET "+tc.name, clipboardImageRequest(t, s, http.MethodGet, suffix, "", nil, tc.mutate), tc.status)
		}
	}
	for name, mutate := range map[string]func(*http.Request){
		"cross origin":             func(r *http.Request) { r.Header.Set("Origin", "https://other.example") },
		"cross site":               func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"unknown Tailscale header": func(r *http.Request) { r.Header.Set("Tailscale-Funnel-Request", "1") },
		"wrong forwarded host":     func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "other.example") },
	} {
		for _, suffix := range []string{"", "/" + record.ID} {
			assertBoundedRefusal(t, name, clipboardImageRequest(t, s, http.MethodGet, suffix, "", nil, mutate), http.StatusForbidden)
		}
	}
	for _, tc := range ergoMutationRedCases(s.cfg) {
		want := tc.status
		if tc.name == "wrong_content_type" || tc.name == "content_type_parameter" {
			want = http.StatusUnsupportedMediaType
		}
		assertBoundedRefusal(t, "POST "+tc.name, clipboardImageRequest(t, s, http.MethodPost, "", "image/png", png, tc.mutate), want)
		assertBoundedRefusal(t, "DELETE "+tc.name, clipboardImageRequest(t, s, http.MethodDelete, "/"+record.ID, "application/json", []byte(`{}`), tc.mutate), tc.status)
	}
	for _, suffix := range []string{"?x=1", "?", "/" + record.ID + "?x=1"} {
		assertBoundedRefusal(t, "query", clipboardImageRequest(t, s, http.MethodGet, suffix, "", nil, nil), http.StatusBadRequest)
	}
	for _, id := range []string{"invalid", strings.ToUpper(record.ID), strings.Repeat("a", 31), strings.Repeat("a", 33), "osc52", strings.Repeat("0", 32)} {
		assertBoundedRefusal(t, "invalid image id", clipboardImageRequest(t, s, http.MethodGet, "/"+id, "", nil, nil), http.StatusNotFound)
	}
	for _, tc := range []struct{ method, suffix string }{{"HEAD", ""}, {"HEAD", "/" + record.ID}, {"PUT", ""}, {"POST", "/" + record.ID}, {"DELETE", ""}} {
		assertBoundedRefusal(t, "method", clipboardImageRequest(t, s, tc.method, tc.suffix, "application/json", []byte(`{}`), nil), http.StatusMethodNotAllowed)
	}
	for _, body := range []string{"", "[]", "null", `{"id":"private.png"}`, `{} {}`, `{"PATH":"/private"}`} {
		assertBoundedRefusal(t, "delete grammar", clipboardImageRequest(t, s, http.MethodDelete, "/"+record.ID, "application/json", []byte(body), nil), http.StatusBadRequest, "private")
	}
	assertBoundedRefusal(t, "oversized delete", clipboardImageRequest(t, s, http.MethodDelete, "/"+record.ID, "application/json", bytes.Repeat([]byte(" "), 1025), nil), http.StatusRequestEntityTooLarge)
	for name, mutate := range map[string]func(*http.Request){
		"multiple types": func(r *http.Request) { r.Header.Add("Content-Type", "image/png") },
		"empty type":     func(r *http.Request) { r.Header.Del("Content-Type") },
	} {
		assertBoundedRefusal(t, name, clipboardImageRequest(t, s, http.MethodPost, "", "image/png", png, mutate), http.StatusUnsupportedMediaType)
	}
	for name, origin := range map[string]string{"control": "private\x00name", "long": strings.Repeat("x", 33), "invalid UTF8": string([]byte{0xff})} {
		assertBoundedRefusal(t, name, clipboardImageRequest(t, s, http.MethodPost, "", "image/png", png, func(r *http.Request) { r.Header.Set(clipboardImageOriginHeader, origin) }), http.StatusBadRequest, "private")
	}
	assertBoundedRefusal(t, "multiple origins", clipboardImageRequest(t, s, http.MethodPost, "", "image/png", png, func(r *http.Request) {
		r.Header.Add(clipboardImageOriginHeader, "phone")
		r.Header.Add(clipboardImageOriginHeader, "laptop")
	}), http.StatusBadRequest)
	if items, err := s.clipboardImages.list(); err != nil || len(items) != 1 {
		t.Fatal("refusals changed image store")
	}
}

func TestClipboardImageConcurrentUploadsKeepCapacityAtomic(t *testing.T) {
	s := clipboardImageServer(t)
	body := oraclePNG(t, 1, 1)
	var wg sync.WaitGroup
	results := make(chan error, 2*clipboardImageMaxCount)
	for i := 0; i < cap(results); i++ {
		unique := append(append([]byte(nil), body...), byte(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.clipboardImages.create("image/png", unique, "phone")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	accepted, full := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errClipboardImageFull):
			full++
		default:
			t.Fatal(err)
		}
	}
	if accepted != clipboardImageMaxCount || full != clipboardImageMaxCount {
		t.Fatalf("concurrent capacity: accepted=%d full=%d", accepted, full)
	}
	items, err := s.clipboardImages.list()
	if err != nil || len(items) != clipboardImageMaxCount {
		t.Fatalf("live images=%d: %v", len(items), err)
	}
	for _, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, got, err := s.clipboardImages.get(item.ID); err != nil || len(got) != len(body)+1 || !bytes.Equal(got[:len(body)], body) {
				t.Errorf("concurrent read: %v", err)
			}
			if err := s.clipboardImages.delete(item.ID); err != nil {
				t.Errorf("concurrent delete: %v", err)
			}
		}()
	}
	wg.Wait()
	if items, err := s.clipboardImages.list(); err != nil || len(items) != 0 {
		t.Fatalf("images after concurrent delete=%d: %v", len(items), err)
	}
}

func TestClipboardImageAPIValidationLimitsAndUnavailable(t *testing.T) {
	s := clipboardImageServer(t)
	for name, fixture := range map[string]struct {
		media string
		body  []byte
	}{
		"mismatch": {"image/jpeg", oraclePNG(t, 1, 1)}, "empty": {"image/png", nil},
		"text": {"image/png", []byte("private image data")}, "svg": {"image/svg+xml", []byte(`<svg/>`)},
		"truncated PNG": {"image/png", oraclePNG(t, 1, 1)[:12]}, "oversized dimensions": {"image/png", oraclePNG(t, maxImageDimension+1, 1)},
	} {
		assertBoundedRefusal(t, name, clipboardImageRequest(t, s, http.MethodPost, "", fixture.media, fixture.body, nil), http.StatusUnsupportedMediaType, "private")
	}
	assertBoundedRefusal(t, "per image cap", clipboardImageRequest(t, s, http.MethodPost, "", "image/png", bytes.Repeat([]byte{0}, proto.MaxImage+1), nil), http.StatusRequestEntityTooLarge)
	s.cfg.ImageUploadMaxBytes = 1 << 20
	assertBoundedRefusal(t, "configured lower image cap", clipboardImageRequest(t, s, http.MethodPost, "", "image/png", bytes.Repeat([]byte{0}, (1<<20)+1), nil), http.StatusRequestEntityTooLarge)
	s.cfg.ImageUploadMaxBytes = proto.MaxImage
	for i := 0; i < clipboardImageMaxCount; i++ {
		if w := clipboardImageRequest(t, s, http.MethodPost, "", "image/png", oraclePNG(t, i+1, 1), nil); w.Code != http.StatusCreated {
			t.Fatalf("image %d: %d", i, w.Code)
		}
	}
	assertBoundedRefusal(t, "image count cap", clipboardImageRequest(t, s, http.MethodPost, "", "image/png", oraclePNG(t, clipboardImageMaxCount+1, 1), nil), http.StatusInsufficientStorage)
	if items, err := s.clipboardImages.list(); err != nil || len(items) != clipboardImageMaxCount {
		t.Fatal("capacity deleted a live image")
	}
	cfg := ergoFrontConfig(t)
	disabled := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	assertBoundedRefusal(t, "disabled GET", clipboardImageRequest(t, disabled, http.MethodGet, "", "", nil, nil), http.StatusServiceUnavailable)
	assertBoundedRefusal(t, "disabled POST", clipboardImageRequest(t, disabled, http.MethodPost, "", "image/png", oraclePNG(t, 1, 1), nil), http.StatusServiceUnavailable)
}

func TestClipboardImageAggregateLimitUsesActualBytes(t *testing.T) {
	s := clipboardImageServer(t)
	// Valid PNG metadata followed by inert padding is accepted by the existing
	// staging validator; use it to exercise the real byte budget, not a counter seam.
	body := append(oraclePNG(t, 1, 1), make([]byte, proto.MaxImage)...)
	body = body[:proto.MaxImage]
	for i := 0; i < 6; i++ {
		body[len(body)-1] = byte(i)
		if _, err := s.clipboardImages.create("image/png", body, ""); err != nil {
			t.Fatal(err)
		}
	}
	remaining := clipboardImageMaxTotalBytes - 6*proto.MaxImage
	if _, err := s.clipboardImages.create("image/png", body[:remaining], ""); err != nil {
		t.Fatalf("exact aggregate bound: %v", err)
	}
	if _, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), ""); !errors.Is(err, errClipboardImageFull) {
		t.Fatalf("over aggregate cap: %v", err)
	}
	items, err := s.clipboardImages.list()
	if err != nil || len(items) != 7 {
		t.Fatalf("capacity mutated store: %d %v", len(items), err)
	}
}

func TestClipboardImageRestartExpiryAndInterruptedPublication(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := base
	dir := filepath.Join(shortTestDir(t), clipboardImageDirectory)
	s, err := newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.create("image/png", oraclePNG(t, 1, 1), "phone")
	if err != nil {
		t.Fatal(err)
	}
	clock = base.Add(10 * time.Minute)
	second, err := s.create("image/jpeg", oracleJPEG(t), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	tmp := "." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 16) + ".tmp"
	if err := os.WriteFile(filepath.Join(dir, tmp), []byte("interrupted secret"), 0600); err != nil {
		t.Fatal(err)
	}
	clock = first.ExpiresAt
	s, err = newClipboardImageStore(dir, proto.MaxImage, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	items, err := s.list()
	if err != nil || len(items) != 1 || items[0] != second {
		t.Fatalf("restart: %+v %v", items, err)
	}
	for _, name := range []string{first.ID, tmp} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restart retained expired/interrupted payload %v", err)
		}
	}
	if _, _, err := s.get(first.ID); !errors.Is(err, errClipboardImageNotFound) {
		t.Fatalf("expired fetch: %v", err)
	}
	clock = second.ExpiresAt
	if _, _, err := s.get(second.ID); !errors.Is(err, errClipboardImageNotFound) {
		t.Fatalf("exact deadline fetch: %v", err)
	}
	if err := s.reap(); err != nil {
		t.Fatal(err)
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
		t.Fatal("reap retained expired image content")
	}
}

func TestClipboardImageDisablingUploadsDoesNotStrandExpiredContent(t *testing.T) {
	s := clipboardImageServer(t)
	record, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), "phone")
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.ImageUploadMaxBytes = 0
	disabled := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if disabled.clipboardImageErr != nil || disabled.clipboardImages == nil {
		t.Fatal("disabled images lost their maintenance owner")
	}
	defer disabled.clipboardImages.close()
	assertBoundedRefusal(t, "disabled existing image GET", clipboardImageRequest(t, disabled, http.MethodGet, "/"+record.ID, "", nil, nil), http.StatusServiceUnavailable)
	assertBoundedRefusal(t, "disabled existing image POST", clipboardImageRequest(t, disabled, http.MethodPost, "", "image/png", oraclePNG(t, 1, 1), nil), http.StatusServiceUnavailable)
	disabled.clipboardImages.now = func() time.Time { return record.ExpiresAt }
	if err := disabled.clipboardImages.reap(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(cfg.SnippetStorePath), clipboardImageDirectory, record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled uploads stranded expired image file")
	}
}

func TestClipboardMaintenanceReclaimsTextAndImagesWithoutRequests(t *testing.T) {
	s := clipboardImageServer(t)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := base
	s.snippets.now = func() time.Time { return clock }
	s.clipboardImages.now = func() time.Time { return clock }
	if _, err := s.snippets.create(snippetKindClip, "", "expired secret", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.snippets.create(snippetKindSnippet, "Saved", "permanent secret", true, ""); err != nil {
		t.Fatal(err)
	}
	image, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	clock = base.Add(30 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.maintainClipboard(ctx, ticks) }()
	// Receiving the second tick proves the first maintenance transaction has
	// completed, without sleeps or browser/API activity that could do the work.
	ticks <- clock
	ticks <- clock
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop with server context")
	}
	text, err := os.ReadFile(s.cfg.SnippetStorePath)
	if err != nil || bytes.Contains(text, []byte("expired secret")) || !bytes.Contains(text, []byte("permanent secret")) {
		t.Fatal("maintenance failed to reclaim expired text or deleted saved text")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(s.cfg.SnippetStorePath), clipboardImageDirectory, image.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("maintenance retained expired image file")
	}
}

func TestClipboardImageRefusesUnsafeFilesystemState(t *testing.T) {
	for _, mutation := range []string{"symlink", "hardlink", "mode", "corrupt", "unknown file", "directory mode"} {
		t.Run(mutation, func(t *testing.T) {
			parent := shortTestDir(t)
			dir := filepath.Join(parent, clipboardImageDirectory)
			s, err := newClipboardImageStore(dir, proto.MaxImage, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			record, err := s.create("image/png", oraclePNG(t, 1, 1), "")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, record.ID)
			outside := filepath.Join(parent, "private")
			switch mutation {
			case "symlink":
				if err := os.Rename(file, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, file); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(file, outside); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(file, []byte("private corrupt content"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown file":
				if err := os.WriteFile(filepath.Join(dir, "private-name.png"), []byte("private"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory mode":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.get(record.ID); !errors.Is(err, errClipboardImageUnavailable) {
				t.Fatalf("runtime accepted %s: %v", mutation, err)
			}
			s.close()
			if reopened, err := newClipboardImageStore(dir, proto.MaxImage, time.Now); err == nil {
				reopened.close()
				t.Fatalf("restart accepted %s", mutation)
			}
		})
	}
	parent := shortTestDir(t)
	outside := shortTestDir(t)
	link := filepath.Join(parent, clipboardImageDirectory)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if s, err := newClipboardImageStore(link, proto.MaxImage, time.Now); err == nil {
		s.close()
		t.Fatal("symlink directory accepted")
	}
	if files, err := os.ReadDir(outside); err != nil || len(files) != 0 {
		t.Fatal("symlink target was modified")
	}
}

func TestClipboardImagePublicationFailuresAndRetry(t *testing.T) {
	for _, phase := range []string{"write", "file sync", "directory sync"} {
		t.Run(phase, func(t *testing.T) {
			s := clipboardImageServer(t)
			originalWrite, originalSync, originalDirSync := s.clipboardImages.file.writeFile, s.clipboardImages.file.fileSync, s.clipboardImages.file.dirSync
			switch phase {
			case "write":
				s.clipboardImages.file.writeFile = func(*os.File, []byte) (int, error) { return 0, io.ErrShortWrite }
			case "file sync":
				s.clipboardImages.file.fileSync = func(*os.File) error { return errors.New("injected") }
			case "directory sync":
				s.clipboardImages.file.dirSync = func(*os.File) error { return errors.New("injected") }
			}
			if _, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), ""); !errors.Is(err, errClipboardImageUnavailable) {
				t.Fatalf("publication result: %v", err)
			}
			s.clipboardImages.file.writeFile, s.clipboardImages.file.fileSync, s.clipboardImages.file.dirSync = originalWrite, originalSync, originalDirSync
			dir := filepath.Join(filepath.Dir(s.cfg.SnippetStorePath), clipboardImageDirectory)
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "directory sync" {
				if len(files) != 1 {
					t.Fatal("published image was not retained after uncertain durability")
				}
				if _, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), ""); !errors.Is(err, errClipboardImageUnavailable) {
					t.Fatal("faulted store accepted mutation")
				}
			} else {
				if len(files) != 0 {
					t.Fatal("failed publication retained secret temp file")
				}
				if _, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), ""); err != nil {
					t.Fatalf("retry: %v", err)
				}
			}
			reopened, err := newClipboardImageStore(dir, proto.MaxImage, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			if list, err := reopened.list(); err != nil || len(list) != 1 {
				t.Fatalf("recovered publication: %d %v", len(list), err)
			}
		})
	}
}

func TestClipboardImageAPILogsNoBodiesOrFilenames(t *testing.T) {
	s := clipboardImageServer(t)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	body := append(oraclePNG(t, 1, 1), []byte("PRIVATE-BODY-SENTINEL")...)
	w := clipboardImageRequest(t, s, http.MethodPost, "", "image/png", body, func(r *http.Request) { r.Header.Set(clipboardImageOriginHeader, "PRIVATE-FILENAME.png") })
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	record := decodeClipboardImage(t, w)
	_ = clipboardImageRequest(t, s, http.MethodGet, "/"+record.ID, "", nil, nil)
	_ = clipboardImageRequest(t, s, http.MethodDelete, "/"+record.ID, "application/json", []byte(`{}`), nil)
	if strings.Contains(output.String(), "PRIVATE") || strings.Contains(output.String(), record.ID) {
		t.Fatal("clipboard body, label, or ID logged")
	}
}

func TestClipboardImageAPIUsesSharedOperatorLimiter(t *testing.T) {
	s := clipboardImageServer(t)
	limited := false
	for i := 0; i < 60; i++ {
		if w := ergoRequestMetered(t, s, http.MethodGet, "http://localhost/api/clipboard/images", "", nil, nil); w.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("clipboard images bypass operator limiter")
	}
}

// Keep the metadata grammar explicit: no transport path or operator identity
// can be smuggled into a record, and the data cannot be mistaken for text JSON.
func TestClipboardImageStoredMetadataIsClosed(t *testing.T) {
	s := clipboardImageServer(t)
	r, err := s.clipboardImages.create("image/png", oraclePNG(t, 1, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 9 {
		t.Fatalf("metadata field count %d", len(fields))
	}
	for _, key := range []string{"id", "media_type", "byte_size", "created_at", "expires_at", "origin", "updated_at", "revision", "retention_seconds"} {
		if _, ok := fields[key]; !ok {
			t.Fatal("metadata field missing", key)
		}
	}
	// JSON decoding alone replaces invalid UTF-8. Persistent metadata must
	// instead be refused, just like the existing secret-bearing text stores.
	dir := filepath.Join(filepath.Dir(s.cfg.SnippetStorePath), clipboardImageDirectory)
	path := filepath.Join(dir, r.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.BigEndian.Uint32(data[4:8]))
	header := bytes.Replace(data[8:8+n], []byte(`"origin":""`), []byte{'"', 'o', 'r', 'i', 'g', 'i', 'n', '"', ':', '"', 0xff, '"'}, 1)
	corrupt := make([]byte, 8+len(header)+len(data[8+n:]))
	copy(corrupt, "PCI1")
	binary.BigEndian.PutUint32(corrupt[4:8], uint32(len(header)))
	copy(corrupt[8:], header)
	copy(corrupt[8+len(header):], data[8+n:])
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := newClipboardImageStore(dir, proto.MaxImage, time.Now); err == nil {
		reopened.close()
		t.Fatal("invalid UTF-8 image metadata accepted on restart")
	}
}
