package frontdoor

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticAssetCompression(t *testing.T) {
	dir := t.TempDir()
	body := []byte("console.log('release asset');")
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(body)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.js", "app.js.gz", "index.html", "index.html.gz"} {
		data := body
		if filepath.Ext(name) == ".gz" {
			data = compressed.Bytes()
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, method, path, accept, rangeHeader string
		compressed                              bool
		status                                  int
	}{
		{"gzip", "GET", "/app.js", "br, gzip", "", true, 200},
		{"head", "HEAD", "/app.js", "gzip", "", true, 200},
		{"identity", "GET", "/app.js", "", "", false, 200},
		{"explicit refusal", "GET", "/app.js", "*;q=1, gzip;q=0", "", false, 200},
		{"wildcard", "GET", "/app.js", "br, *;q=0.5", "", true, 200},
		{"invalid quality", "GET", "/app.js", "gzip;q=NaN", "", false, 200},
		{"range stays identity", "GET", "/app.js", "gzip", "bytes=0-6", false, 206},
		{"HTML stays identity", "GET", "/index.html", "gzip", "", false, 301},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Accept-Encoding", tc.accept)
			r.Header.Set("Range", tc.rangeHeader)
			w := httptest.NewRecorder()
			staticAssets(dir).ServeHTTP(w, r)
			if w.Code != tc.status || (w.Header().Get("Content-Encoding") == "gzip") != tc.compressed {
				t.Fatalf("response: status=%d encoding=%q", w.Code, w.Header().Get("Content-Encoding"))
			}
			if tc.path == "/app.js" && w.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("missing representation negotiation: %v", w.Header())
			}
			if tc.method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
			if tc.compressed && tc.method == "GET" {
				reader, err := gzip.NewReader(w.Body)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				got, err := io.ReadAll(reader)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("decoded asset=%q, error=%v", got, err)
				}
				if w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
					t.Fatal("compressed asset lost its media type")
				}
			}
		})
	}
	if err := os.Remove(filepath.Join(dir, "app.js.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html.gz", filepath.Join(dir, "app.js.gz")); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/app.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	staticAssets(dir).ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatal("nonregular compressed asset did not fall back to the original")
	}
}
