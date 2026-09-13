package broker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"persea-terminal/internal/proto"
)

const (
	imageFileLimit = 64
	imageByteLimit = int64(256 << 20)
	imageTTL       = 6 * time.Hour
)

var (
	imageFinalName = regexp.MustCompile(`^img-[0-9a-f]{32}\.(png|jpg|gif|webp)$`)
	imageTempName  = regexp.MustCompile(`^\.img-[0-9a-f]{32}\.tmp$`)
)

type imageStager struct {
	dir string
	mu  sync.Mutex

	now            func() time.Time
	randomRead     func([]byte) (int, error)
	openFile       func(string, int, os.FileMode) (*os.File, error)
	writeFile      func(*os.File, []byte) (int, error)
	fileSync       func(*os.File) error
	closeFile      func(*os.File) error
	renameFile     func(string, string) error
	openDirectory  func() (*os.File, error)
	dirSync        func(*os.File) error
	closeDirectory func(*os.File) error
	readDirectory  func() ([]os.DirEntry, error)
	lstat          func(string) (os.FileInfo, error)
	removeFile     func(string) error
}

type imageEntry struct {
	name     string
	path     string
	size     int64
	modified time.Time
	managed  bool
}

func validateImageStagingDirectory(info os.FileInfo, brokerUID uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != brokerUID {
		return fmt.Errorf("image staging directory must be broker-owned")
	}
	protected := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || protected != 0700 {
		return fmt.Errorf("image staging directory must have exact protected mode 0700")
	}
	return nil
}

func newImageStager(dir string) (*imageStager, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err := validateImageStagingDirectory(info, uint32(os.Geteuid())); err != nil {
		return nil, err
	}
	s := &imageStager{dir: dir}
	s.now = time.Now
	s.randomRead = rand.Read
	s.openFile = os.OpenFile
	s.writeFile = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	s.fileSync = (*os.File).Sync
	s.closeFile = (*os.File).Close
	s.renameFile = os.Rename
	s.openDirectory = func() (*os.File, error) { return os.Open(dir) }
	s.dirSync = (*os.File).Sync
	s.closeDirectory = (*os.File).Close
	s.readDirectory = func() ([]os.DirEntry, error) { return os.ReadDir(dir) }
	s.lstat = os.Lstat
	s.removeFile = os.Remove
	if _, _, err := s.reapAndAccount(0, false); err != nil {
		return nil, err
	}
	return s, nil
}

func validateImageStageControl(payload []byte, control proto.Control) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	for field := range fields {
		switch field {
		case "type", "media_type", "bytes":
		default:
			return fmt.Errorf("field %q is not allowed for image_stage", field)
		}
	}
	if len(fields) != 3 || control.MediaType == "" || control.Bytes < 0 {
		return fmt.Errorf("invalid image_stage")
	}
	return nil
}

func (s *imageStager) stage(declared string, data []byte) proto.Control {
	s.mu.Lock()
	defer s.mu.Unlock()

	ext, media := sniffImage(data)
	if media == "" || media != declared {
		return imageRefused(proto.ImageRefusalUnsupportedType)
	}
	if _, _, err := s.reapAndAccount(int64(len(data)), true); err != nil {
		if err == errImageCapacity {
			return imageRefused(proto.ImageRefusalCapacity)
		}
		return imageRefused(proto.ImageRefusalIO)
	}

	rawID := make([]byte, 16)
	if n, err := s.randomRead(rawID); err != nil || n != len(rawID) {
		return imageRefused(proto.ImageRefusalIO)
	}
	id := hex.EncodeToString(rawID)
	tempPath := filepath.Join(s.dir, ".img-"+id+".tmp")
	finalPath := filepath.Join(s.dir, "img-"+id+ext)
	flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY | syscall.O_NOFOLLOW
	file, err := s.openFile(tempPath, flags, 0600)
	if err != nil {
		return imageRefused(proto.ImageRefusalIO)
	}
	failed := true
	defer func() {
		if failed {
			_ = s.removeFile(tempPath)
			_ = s.removeFile(finalPath)
		}
	}()
	if n, err := s.writeFile(file, data); err != nil || n != len(data) {
		_ = s.closeFile(file)
		return imageRefused(proto.ImageRefusalIO)
	}
	if err := s.fileSync(file); err != nil {
		_ = s.closeFile(file)
		return imageRefused(proto.ImageRefusalIO)
	}
	if err := s.closeFile(file); err != nil {
		return imageRefused(proto.ImageRefusalIO)
	}
	if err := s.renameFile(tempPath, finalPath); err != nil {
		return imageRefused(proto.ImageRefusalIO)
	}
	directory, err := s.openDirectory()
	if err != nil {
		return imageRefused(proto.ImageRefusalIO)
	}
	if err := s.dirSync(directory); err != nil {
		_ = s.closeDirectory(directory)
		return imageRefused(proto.ImageRefusalIO)
	}
	if err := s.closeDirectory(directory); err != nil {
		return imageRefused(proto.ImageRefusalIO)
	}
	failed = false
	return proto.Control{
		Type: proto.ControlImageStaged, Path: finalPath, ID: id,
		ExpiresAt: s.now().Add(imageTTL).Format(time.RFC3339Nano),
	}
}

var errImageCapacity = fmt.Errorf("image staging capacity")

func (s *imageStager) reapAndAccount(incoming int64, enforceCapacity bool) (int, int64, error) {
	entries, err := s.readDirectory()
	if err != nil {
		return 0, 0, err
	}
	now := s.now()
	accounted := make([]imageEntry, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(s.dir, entry.Name())
		info, err := s.lstat(path)
		if err != nil {
			return 0, 0, err
		}
		regular := info.Mode().IsRegular()
		managed := regular && (imageFinalName.MatchString(entry.Name()) || imageTempName.MatchString(entry.Name()))
		if managed && info.ModTime().Before(now.Add(-imageTTL)) {
			if err := s.removeFile(path); err != nil {
				return 0, 0, err
			}
			continue
		}
		accounted = append(accounted, imageEntry{
			name: entry.Name(), path: path, size: info.Size(), modified: info.ModTime(),
			managed: managed,
		})
	}
	count := len(accounted)
	var bytes int64
	for _, entry := range accounted {
		bytes += entry.size
	}
	if !enforceCapacity {
		return count, bytes, nil
	}
	sort.Slice(accounted, func(i, j int) bool {
		if accounted[i].modified.Equal(accounted[j].modified) {
			return accounted[i].name < accounted[j].name
		}
		return accounted[i].modified.Before(accounted[j].modified)
	})
	for _, entry := range accounted {
		if count+1 <= imageFileLimit && bytes+incoming <= imageByteLimit {
			break
		}
		if !entry.managed {
			continue
		}
		if err := s.removeFile(entry.path); err != nil {
			return 0, 0, err
		}
		count--
		bytes -= entry.size
	}
	if count+1 > imageFileLimit || bytes+incoming > imageByteLimit {
		return count, bytes, errImageCapacity
	}
	return count, bytes, nil
}

func sniffImage(data []byte) (string, string) {
	media := http.DetectContentType(data)
	switch media {
	case "image/png":
		return ".png", media
	case "image/jpeg":
		return ".jpg", media
	case "image/gif":
		return ".gif", media
	case "image/webp":
		return ".webp", media
	default:
		return "", ""
	}
}

func imageRefused(code string) proto.Control {
	return proto.Control{Type: proto.ControlImageRefused, Code: code}
}
