package frontdoor

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(data)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func contentVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func TestStaticAssetsAreCachedOnlyUnderTheirContentVersion(t *testing.T) {
	dir := t.TempDir()
	script := []byte("console.log('release');")
	style := []byte("body{color:red}")
	xterm := []byte(".xterm{}")
	files := map[string][]byte{
		"app.js": script, "app.js.gz": gzipped(t, script),
		"app.css":   style,
		"xterm.css": xterm, "xterm.css.gz": gzipped(t, []byte(".other{}")),
		"index.html": []byte(`<meta content="__PERSEA_STYLE_NONCE__"><link href="/xterm.css"><link href="/app.css"><script src="/app.js"></script>`),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, dir, cfg.Ingress.CanonicalHost)
	withoutCookie := func(r *http.Request) { r.Header.Del("Cookie"); r.Header.Set("Accept-Encoding", "gzip") }
	issuesCookie := func(w *http.Response) bool {
		for _, cookie := range w.Cookies() {
			if cookie.Name == csrfCookie {
				return true
			}
		}
		return false
	}

	document := ergoRequest(t, s, http.MethodGet, "http://localhost/terminal", "", nil, nil)
	html := document.Body.String()
	if document.Code != http.StatusOK || document.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("document status=%d headers=%v", document.Code, document.Header())
	}
	if !strings.Contains(html, `"/app.js?v=`+contentVersion(script)+`"`) || !strings.Contains(html, `"/app.css?v=`+contentVersion(style)+`"`) {
		t.Fatalf("document does not name its assets by content: %s", html)
	}
	if !strings.Contains(html, `href="/xterm.css"`) {
		t.Fatalf("an asset whose compressed copy differs was versioned: %s", html)
	}

	cached := ergoRequest(t, s, http.MethodGet, "http://localhost/app.js?v="+contentVersion(script), "", nil, withoutCookie)
	if cached.Code != http.StatusOK || cached.Header().Get("Cache-Control") != immutableAssetCacheControl || cached.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("versioned asset status=%d headers=%v", cached.Code, cached.Header())
	}
	if issuesCookie(cached.Result()) {
		t.Fatal("a response the browser keeps carried the CSRF cookie")
	}
	reader, err := gzip.NewReader(cached.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, script) {
		t.Fatalf("versioned asset body=%q err=%v", got, err)
	}

	for _, target := range []string{"/app.js", "/app.js?v=" + strings.Repeat("0", 32), "/app.js?v=" + contentVersion(script) + "&x=1", "/xterm.css?v=" + contentVersion(xterm)} {
		w := ergoRequest(t, s, http.MethodGet, "http://localhost"+target, "", nil, withoutCookie)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status=%d cache=%q", target, w.Code, w.Header().Get("Cache-Control"))
		}
		if !issuesCookie(w.Result()) {
			t.Fatalf("%s: an uncached read stopped minting the CSRF cookie", target)
		}
	}
}

func TestCSRFReadMintsOnlyAMissingCookie(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	missing := ergoRequest(t, s, http.MethodGet, "http://localhost/api/csrf", "", nil, func(r *http.Request) { r.Header.Del("Cookie") })
	if missing.Code != http.StatusNoContent || missing.Header().Get("Cache-Control") != "no-store" || missing.Body.Len() != 0 {
		t.Fatalf("csrf read status=%d headers=%v", missing.Code, missing.Header())
	}
	minted := false
	for _, cookie := range missing.Result().Cookies() {
		minted = minted || cookie.Name == csrfCookie && len(cookie.Value) == 43
	}
	if !minted {
		t.Fatal("a read without a cookie did not mint one")
	}
	present := ergoRequest(t, s, http.MethodGet, "http://localhost/api/csrf", "", nil, nil)
	if present.Code != http.StatusNoContent || len(present.Result().Cookies()) != 0 {
		t.Fatalf("a read with a valid cookie replaced it: %v", present.Header())
	}
}
