package frontdoor

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Static compression never wraps HTML or authenticated terminal/API payloads.
// The representations are generated together by the release's UI build.
func staticAssets(directory string) http.Handler {
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
