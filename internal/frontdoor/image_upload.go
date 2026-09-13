package frontdoor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

const maxImageDimension = 16384

var allowedImageMediaTypes = map[string]bool{
	"image/gif":  true,
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

var stagedImageID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type stagedImageResponse struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	MediaType string `json:"media_type"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Server) stageSessionImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	id, ok := identity(r)
	if !ok || id.uid != s.cfg.Ingress.PeerUID || id.operator != s.cfg.Ingress.OperatorLogin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	realm, ok := exactImageRealm(s.cfg.Realms, r.URL.Query())
	if !ok {
		http.Error(w, "invalid realm", http.StatusBadRequest)
		return
	}
	declared, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !allowedImageMediaTypes[declared] {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}

	limited := http.MaxBytesReader(w, r.Body, int64(s.cfg.ImageUploadMaxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid image body", http.StatusBadRequest)
		}
		return
	}
	if len(body) == 0 {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}
	sniffed := imageMediaType(body)
	if sniffed == "" || sniffed != declared || !validImageDimensions(sniffed, body) {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}

	result, err := requestRealmImageStage(r.Context(), realm, sniffed, body)
	if err != nil {
		http.Error(w, "image staging unavailable", http.StatusServiceUnavailable)
		return
	}
	if result.Type == proto.ControlImageRefused {
		http.Error(w, "image staging refused", imageRefusalStatus(result.Code))
		return
	}
	if result.Type != proto.ControlImageStaged || !validStagedImage(result, realm, sniffed, s.cfg.ImageStagingRoot) {
		http.Error(w, "image staging unavailable", http.StatusServiceUnavailable)
		return
	}

	response := stagedImageResponse{ID: result.ID, Path: result.Path, Bytes: len(body), MediaType: sniffed, ExpiresAt: result.ExpiresAt}
	hash := sha256.Sum256([]byte(result.ID))
	frontLogf("component=frontdoor event=image_staged realm=%q bytes=%d media_type=%q id_hash=%q", realm.Name, len(body), sniffed, hex.EncodeToString(hash[:6]))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

func exactImageRealm(realms []config.Realm, query map[string][]string) (config.Realm, bool) {
	if len(query) != 1 || len(query["realm"]) != 1 || query["realm"][0] == "" {
		return config.Realm{}, false
	}
	want := query["realm"][0]
	for _, realm := range realms {
		if realm.Name == want {
			return realm, true
		}
	}
	return config.Realm{}, false
}

func imageMediaType(body []byte) string {
	media := http.DetectContentType(body)
	if allowedImageMediaTypes[media] {
		return media
	}
	return ""
}

func validImageDimensions(media string, body []byte) bool {
	if media == "image/webp" {
		// WebP remains sniff-only because these bytes are never decoded or served
		// here. Adding a decoder dependency does not improve this inert handoff.
		return true
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	return err == nil && cfg.Width >= 1 && cfg.Width <= maxImageDimension && cfg.Height >= 1 && cfg.Height <= maxImageDimension
}

func requestRealmImageStage(ctx context.Context, realm config.Realm, media string, body []byte) (proto.Control, error) {
	conn, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return proto.Control{}, err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if err := ctx.Err(); err != nil {
		return proto.Control{}, err
	}
	if err := hello(conn); err != nil {
		return proto.Control{}, err
	}
	if err := writeControl(conn, proto.Control{Type: proto.ControlImageStage, MediaType: media, Bytes: len(body)}); err != nil {
		return proto.Control{}, err
	}
	if err := proto.WriteFrame(conn, proto.FrameImage, body); err != nil {
		return proto.Control{}, err
	}
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		return proto.Control{}, err
	}
	if frame.Type != proto.FrameControl {
		return proto.Control{}, fmt.Errorf("invalid image staging response frame")
	}
	result, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		return proto.Control{}, err
	}
	return result, nil
}

func imageRefusalStatus(code string) int {
	switch code {
	case proto.ImageRefusalTooLarge:
		return http.StatusRequestEntityTooLarge
	case proto.ImageRefusalUnsupportedType:
		return http.StatusUnsupportedMediaType
	case proto.ImageRefusalCapacity:
		return http.StatusInsufficientStorage
	case proto.ImageRefusalImagesDisabled, proto.ImageRefusalIO:
		return http.StatusServiceUnavailable
	default:
		return http.StatusServiceUnavailable
	}
}

func validStagedImage(result proto.Control, realm config.Realm, media, stagingRoot string) bool {
	extension := imageExtension(media)
	if !stagedImageID.MatchString(result.ID) || extension == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ExpiresAt); err != nil {
		return false
	}
	if stagingRoot == "" {
		// A loaded config always carries the normalized root; the empty value
		// exists only for direct construction and means the default.
		stagingRoot = config.DefaultImageStagingRoot
	}
	root := filepath.Join(stagingRoot, realm.Name)
	clean := filepath.Clean(result.Path)
	basename := "img-" + result.ID + extension
	return filepath.IsAbs(clean) && clean == result.Path &&
		strings.HasPrefix(clean, root+string(filepath.Separator)) && filepath.Base(clean) == basename
}

func imageExtension(media string) string {
	switch media {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}
