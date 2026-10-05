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
	"strconv"
	"strings"
)

// The release assets the document references by content. Each has a version:
// the first 128 bits of the SHA-256 of its bytes. The document asks for
// /app.js?v=<version>; a request carrying the current version may be kept by
// the browser for a year, because those bytes can never change under that
// URL. The document itself is never cached, so the next page load after a
// release asks for the new versions. Any other request is served as before.
var versionedAssets = []string{"app.js", "app.css", "xterm.css"}

const immutableAssetCacheControl = "private, max-age=31536000, immutable"

// assetVersions maps a request path ("/app.js") to its current version.
type assetVersions map[string]string

// loadAssetVersions reads the release's assets once. An asset that is missing,
// or whose compressed copy does not decode to the same bytes, gets no version
// and so is never cached: a browser must not keep content its URL does not name.
func loadAssetVersions(directory string) assetVersions {
	versions := assetVersions{}
	for _, name := range versionedAssets {
		plain, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		if compressed, err := os.ReadFile(filepath.Join(directory, name+".gz")); err == nil && !gzipDecodesTo(compressed, plain) {
			frontLogf("component=frontdoor event=static_asset_unversioned asset=%q reason=%q", name, "compressed copy differs")
			continue
		}
		sum := sha256.Sum256(plain)
		versions["/"+name] = hex.EncodeToString(sum[:16])
	}
	return versions
}

func gzipDecodesTo(compressed, plain []byte) bool {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return false
	}
	decoded, err := io.ReadAll(reader)
	return err == nil && bytes.Equal(decoded, plain)
}

// versionedDocument points the document's asset references at their versions.
func (versions assetVersions) versionedDocument(document []byte) []byte {
	for path, version := range versions {
		document = bytes.Replace(document, []byte(`"`+path+`"`), []byte(`"`+path+`?v=`+version+`"`), 1)
	}
	return document
}

// Static compression never wraps HTML or authenticated terminal/API payloads.
// The representations are generated together by the release's UI build.
func staticAssets(directory string, versions assetVersions) http.Handler {
	plain := http.FileServer(http.Dir(directory))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var mediaType string
		switch r.URL.Path {
		case "/app.js":
			mediaType = "text/javascript; charset=utf-8"
		case "/app.css", "/xterm.css":
			mediaType = "text/css; charset=utf-8"
		default:
			plain.ServeHTTP(w, r)
			return
		}
		if version, ok := versions[r.URL.Path]; ok && r.URL.RawQuery == "v="+version {
			w.Header().Set("Cache-Control", immutableAssetCacheControl)
			// A response the browser keeps must not carry a per-visitor cookie.
			// The CSRF cookie is minted again on the document or any API read.
			w.Header().Del("Set-Cookie")
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Header.Get("Range") == "" && acceptsGzip(r.Header.Values("Accept-Encoding")) {
			name := filepath.Join(directory, strings.TrimPrefix(r.URL.Path, "/")+".gz")
			if info, err := os.Lstat(name); err == nil && info.Mode().IsRegular() {
				if file, err := os.Open(name); err == nil {
					defer file.Close()
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Content-Type", mediaType)
					http.ServeContent(w, r, r.URL.Path, info.ModTime(), file)
					return
				}
			}
		}
		plain.ServeHTTP(w, r)
	})
}

func acceptsGzip(values []string) bool {
	wildcard := false
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			parts := strings.Split(entry, ";")
			coding := strings.ToLower(strings.TrimSpace(parts[0]))
			if coding != "gzip" && coding != "*" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				key, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if !ok || !strings.EqualFold(key, "q") {
					quality = 0
					break
				}
				parsed, err := strconv.ParseFloat(raw, 64)
				if err != nil || !(parsed >= 0 && parsed <= 1) {
					quality = 0
					break
				}
				quality = parsed
			}
			if coding == "gzip" {
				return quality > 0
			}
			wildcard = quality > 0
		}
	}
	return wildcard
}
