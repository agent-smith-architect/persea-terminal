package frontdoor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persea-terminal/internal/config"
)

func writeBundle(t *testing.T, dir string) {
	t.Helper()
	for _, name := range requiredBundleFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateStaticDirAcceptsCompleteRelativeBundle(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir)
	parent, base := filepath.Split(dir)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := validateStaticDir(strings.TrimSuffix(base, string(filepath.Separator))); err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
}

func TestValidateStaticDirFailsClosed(t *testing.T) {
	for _, path := range []string{"", "static/../static", "/tmp/static"} {
		if err := validateStaticDir(path); err == nil {
			t.Errorf("path %q accepted", path)
		}
	}
	dir := t.TempDir()
	writeBundle(t, dir)
	if err := os.Remove(filepath.Join(dir, "app.js")); err != nil {
		t.Fatal(err)
	}
	parent, base := filepath.Split(dir)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := validateStaticDir(strings.TrimSuffix(base, string(filepath.Separator))); err == nil || !strings.Contains(err.Error(), "app.js") {
		t.Fatalf("incomplete bundle error = %v", err)
	}
}

func TestValidateStaticDirRejectsNonRegularBundleEntry(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir)
	if err := os.Remove(filepath.Join(dir, "app.js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html", filepath.Join(dir, "app.js")); err != nil {
		t.Fatal(err)
	}
	parent, base := filepath.Split(dir)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := validateStaticDir(strings.TrimSuffix(base, string(filepath.Separator))); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

// E-P6 — the installable shell is release payload, never a runtime download.
// The manifest and the three icons are ordinary static files that a release
// must contain as REGULAR files; a release missing or symlinking one does not
// start. A rollback runs the OLD release's binary against the OLD release's
// bundle, so this list only ever judges a release built by this tree.
func TestRequiredBundleCoversInstallableShell(t *testing.T) {
	for _, name := range []string{"manifest.webmanifest", "icon-192.png", "icon-512.png", "apple-touch-icon.png"} {
		found := false
		for _, required := range requiredBundleFiles {
			if required == name {
				found = true
			}
		}
		if !found {
			t.Errorf("installable-shell asset %s is not part of the required static bundle", name)
		}
	}
}

func TestValidateStaticDirRejectsMissingOrSymlinkedShellAssets(t *testing.T) {
	for _, name := range []string{"manifest.webmanifest", "icon-192.png", "icon-512.png", "apple-touch-icon.png"} {
		for _, mutation := range []string{"missing", "symlink"} {
			dir := t.TempDir()
			writeBundle(t, dir)
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			want := name
			if mutation == "symlink" {
				if err := os.Symlink("index.html", filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				want = "regular file"
			}
			parent, base := filepath.Split(dir)
			old, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(parent); err != nil {
				t.Fatal(err)
			}
			err = validateStaticDir(strings.TrimSuffix(base, string(filepath.Separator)))
			if chdirErr := os.Chdir(old); chdirErr != nil {
				t.Fatal(chdirErr)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s %s error = %v", name, mutation, err)
			}
		}
	}
}

// The manifest is served with its real media type rather than sniffed into
// text/plain, under the same headers as every other bundle file.
func TestStaticShellAssetsAreServedWithTheirMediaType(t *testing.T) {
	staticDir := writeTestStaticBundle(t, `<html><head><meta name="persea-style-nonce" content="`+styleNoncePlaceholder+`"></head><body></body></html>`)
	s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")
	handler := s.handler()
	for path, wantType := range map[string]string{
		"/manifest.webmanifest": "application/manifest+json",
		"/icon-192.png":         "image/png",
		"/icon-512.png":         "image/png",
		"/apple-touch-icon.png": "image/png",
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, wantType) {
			t.Errorf("%s content type = %q, want %q", path, got, wantType)
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s is missing the shared nosniff header", path)
		}
	}
	// The canonical-host guard applies to the shell assets exactly as it does to
	// the rest of the bundle.
	rr := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/manifest.webmanifest", nil)
	request.Host = "elsewhere.example"
	handler.ServeHTTP(rr, request)
	if rr.Code == http.StatusOK {
		t.Fatalf("manifest served to a non-canonical host: %d", rr.Code)
	}
}
