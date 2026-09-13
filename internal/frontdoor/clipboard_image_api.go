package frontdoor

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const clipboardImageOriginHeader = "X-Persea-Clipboard-Origin"
const clipboardImageRetentionHeader = "X-Persea-Clipboard-Retention"

func (s *Server) clipboardImageGate(w http.ResponseWriter, r *http.Request, method, allow string) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.snippetIdentity(w, r) {
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		http.Error(w, "invalid clipboard request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) clipboardImageReady(w http.ResponseWriter) bool {
	if s.cfg.ImageUploadMaxBytes <= 0 || s.clipboardImageErr != nil || s.clipboardImages == nil {
		http.Error(w, "image clipboard unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func writeClipboardImageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errClipboardImageConflict):
		http.Error(w, "clipboard image revision conflict", http.StatusPreconditionFailed)
	case errors.Is(err, errClipboardImageRetention):
		http.Error(w, "invalid clipboard retention", http.StatusBadRequest)
	case errors.Is(err, errClipboardImageNotFound):
		http.Error(w, "clipboard image not found", http.StatusNotFound)
	case errors.Is(err, errClipboardImageTooLarge):
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errClipboardImageInvalid):
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
	case errors.Is(err, errClipboardImageFull):
		http.Error(w, "image clipboard full", http.StatusInsufficientStorage)
	default:
		http.Error(w, "image clipboard unavailable", http.StatusServiceUnavailable)
	}
}

func (s *Server) listClipboardImages(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodGet, "GET, POST") || !s.clipboardImageReady(w) {
		return
	}
	items, err := s.clipboardImages.list()
	if err != nil {
		writeClipboardImageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (s *Server) createClipboardImage(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodPost, "GET, POST") {
		return
	}
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}
	media, _, err := mime.ParseMediaType(values[0])
	if err != nil || !allowedImageMediaTypes[media] {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}
	origins := r.Header.Values(clipboardImageOriginHeader)
	origin := strings.TrimSpace(r.Header.Get(clipboardImageOriginHeader))
	if len(origins) > 1 || !validClipboardImageOrigin(origin) {
		http.Error(w, "invalid clipboard origin", http.StatusBadRequest)
		return
	}
	if !s.clipboardImageReady(w) {
		return
	}
	retention, ready := s.clipboardDefault(w)
	if !ready {
		return
	}
	if values, present := r.Header[http.CanonicalHeaderKey(clipboardImageRetentionHeader)]; present {
		if len(values) != 1 {
			writeClipboardImageError(w, errClipboardImageRetention)
			return
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || strconv.Itoa(value) != values[0] || !validClipboardRetention(value) {
			writeClipboardImageError(w, errClipboardImageRetention)
			return
		}
		retention = value
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.cfg.ImageUploadMaxBytes)))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid image body", http.StatusBadRequest)
		}
		return
	}
	record, err := s.clipboardImages.create(media, body, origin, retention)
	if err != nil {
		writeClipboardImageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(record)
}

func (s *Server) getClipboardImage(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodGet, "GET, PATCH, DELETE") {
		return
	}
	id := r.PathValue("id")
	if !snippetIDRE.MatchString(id) {
		writeClipboardImageError(w, errClipboardImageNotFound)
		return
	}
	if !s.clipboardImageReady(w) {
		return
	}
	record, body, err := s.clipboardImages.get(id)
	if err != nil {
		writeClipboardImageError(w, err)
		return
	}
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func (s *Server) deleteClipboardImage(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodDelete, "GET, PATCH, DELETE") {
		return
	}
	if !jsonContentType(r) {
		http.Error(w, "invalid clipboard request", http.StatusBadRequest)
		return
	}
	var body struct {
		Revision clipboardRevisionInput `json:"revision"`
	}
	if err := decodeBoundedJSON(w, r, 1024, &body); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "clipboard request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid clipboard request", http.StatusBadRequest)
		}
		return
	}
	id := r.PathValue("id")
	if !snippetIDRE.MatchString(id) {
		writeClipboardImageError(w, errClipboardImageNotFound)
		return
	}
	if !s.clipboardImageReady(w) {
		return
	}
	var revision []uint64
	if body.Revision.Present {
		revision = []uint64{body.Revision.Value}
	}
	if err := s.clipboardImages.delete(id, revision...); err != nil {
		writeClipboardImageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateClipboardImage(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodPatch, "GET, PATCH, DELETE") {
		return
	}
	var wire struct {
		Retention clipboardRetentionInput `json:"retention_seconds"`
		Revision  clipboardRevisionInput  `json:"revision"`
	}
	if !jsonContentType(r) || decodeBoundedJSON(w, r, 1024, &wire) != nil || !wire.Retention.Present || !wire.Revision.Present {
		http.Error(w, "invalid clipboard request", http.StatusBadRequest)
		return
	}
	if !s.clipboardImageReady(w) {
		return
	}
	record, err := s.clipboardImages.updateRetention(r.PathValue("id"), wire.Retention.Value, wire.Revision.Value)
	if errors.Is(err, errClipboardImageConflict) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(record)
		return
	}
	if err != nil {
		writeClipboardImageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(record)
}
