package frontdoor

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

// durableFile is the persisted-store substrate shared by the preferences and
// snippet stores. It is a structural copy of the alias store's contract
// (alias_store.go): a clean absolute path whose parent is a real directory
// chain without symlinks, mode exactly 0700, owned by the front's uid and
// pinned through os.Root; reads are bounded, symlink-refusing and fail closed;
// writes are an O_EXCL 0600 temp file, fsync, close, rename, then a directory
// fsync. A failure before the rename leaves the prior file; a failure after it
// leaves the complete new file and marks the store faulted, because the bytes
// are published but their durability is unproven.
//
// The alias store deliberately keeps its own copy: it is the reviewed
// reference for this contract and stays byte-identical.
type durableFile struct {
	label       string
	unavailable error
	maxBytes    int
	fs          aliasFS
	base        string
	fault       error
	writeFile   func(*os.File, []byte) (int, error)
	fileSync    func(*os.File) error
	closeFile   func(*os.File) error
	dirSync     func(*os.File) error
}

var storeLabelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func openDurableFile(path, label string, maxBytes int, unavailable error) (*durableFile, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s path must be clean and absolute", label)
	}
	parent := filepath.Dir(path)
	if err := validateStoreParent(parent, label, unavailable); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s parent: %v", unavailable, label, err)
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pin %s parent: %v", unavailable, label, err)
	}
	pinned, statErr := dir.Stat()
	_ = dir.Close()
	if statErr != nil || !pinned.IsDir() || pinned.Mode().Perm() != 0700 {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pinned %s parent failed validation", unavailable, label)
	}
	pinnedStat, ok := pinned.Sys().(*syscall.Stat_t)
	if !ok || pinnedStat.Uid != uint32(os.Getuid()) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pinned %s parent has wrong owner", unavailable, label)
	}
	d := &durableFile{label: label, unavailable: unavailable, maxBytes: maxBytes, fs: rootAliasFS{root: root}, base: filepath.Base(path)}
	d.writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	d.fileSync = func(f *os.File) error { return f.Sync() }
	d.closeFile = func(f *os.File) error { return f.Close() }
	d.dirSync = func(f *os.File) error { return f.Sync() }
	return d, nil
}

func validateStoreParent(parent, label string, unavailable error) error {
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: inspect %s parent: %v", unavailable, label, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s parent component is not a real directory", unavailable, label)
		}
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode().Perm() != 0700 {
		return fmt.Errorf("%w: %s parent must have mode 0700", unavailable, label)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%w: %s parent has wrong owner", unavailable, label)
	}
	return nil
}

// read returns the file bytes and whether the file exists. Every structural
// refusal (symlink, mode, owner, size, UTF-8, duplicate JSON keys) is an
// error, never a partial result.
func (d *durableFile) read() ([]byte, bool, error) {
	info, err := d.fs.Lstat(d.base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", d.unavailable, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
		return nil, false, fmt.Errorf("%s must be a regular owner-only file", d.label)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return nil, false, fmt.Errorf("%s has wrong owner", d.label)
	}
	if info.Size() > int64(d.maxBytes) {
		return nil, false, fmt.Errorf("%s is oversized", d.label)
	}
	fh, err := d.fs.OpenFile(d.base, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", d.unavailable, err)
	}
	defer fh.Close()
	opened, err := fh.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 || opened.Size() > int64(d.maxBytes) {
		return nil, false, fmt.Errorf("%w: opened %s failed validation", d.unavailable, d.label)
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || openedStat.Uid != uint32(os.Getuid()) {
		return nil, false, fmt.Errorf("%w: opened %s has wrong owner", d.unavailable, d.label)
	}
	b, err := io.ReadAll(io.LimitReader(fh, int64(d.maxBytes)+1))
	if err != nil || len(b) > d.maxBytes {
		return nil, false, fmt.Errorf("%w: bounded %s read failed", d.unavailable, d.label)
	}
	if !utf8.Valid(b) {
		return nil, false, fmt.Errorf("invalid %s: invalid UTF-8", d.label)
	}
	if err := rejectAliasDuplicateKeys(b); err != nil {
		return nil, false, fmt.Errorf("invalid %s: %w", d.label, err)
	}
	return b, true, nil
}

// decodeStrict decodes exactly one JSON object with no unknown fields and no
// trailing data.
func decodeStrict(b []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

var errStoreOversized = errors.New("store is oversized")

// write publishes b atomically. errStoreOversized is returned before any I/O
// when b exceeds the file cap, so the caller can roll back and report a full
// store rather than an unavailable one.
func (d *durableFile) write(b []byte) error {
	if d.fault != nil {
		return d.fault
	}
	if len(b) > d.maxBytes {
		return errStoreOversized
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := "." + d.base + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := d.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = d.fs.Remove(tmp)
		}
	}()
	n, err := d.writeFile(f, b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err = d.fileSync(f); err != nil {
		return err
	}
	if err = d.closeFile(f); err != nil {
		return err
	}
	if err = d.fs.Rename(tmp, d.base); err != nil {
		return err
	}
	ok = true
	dir, err := d.fs.Open(".")
	if err != nil {
		d.fault = fmt.Errorf("%w: published %s directory open failed: %v", d.unavailable, d.label, err)
		return d.fault
	}
	defer dir.Close()
	if err = d.dirSync(dir); err != nil {
		d.fault = fmt.Errorf("%w: published %s directory sync failed: %v", d.unavailable, d.label, err)
		return d.fault
	}
	return nil
}

var errRequestTooLarge = errors.New("request too large")
var errRequestInvalid = errors.New("invalid request")

// decodeBoundedJSON is the request half of the M4 contract: the body is
// bounded by http.MaxBytesReader BEFORE any decoding, must be valid UTF-8,
// exactly one JSON object with no duplicate keys (case-fold aware), no unknown
// fields and nothing after it. The caller maps the two errors to 413 and 400;
// neither carries any request bytes.
func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	limited := http.MaxBytesReader(w, r.Body, max)
	b, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errRequestInvalid
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(b) {
		return errRequestInvalid
	}
	if err := rejectAliasDuplicateKeys(b); err != nil {
		return errRequestInvalid
	}
	if err := rejectNonCanonicalKeys(b); err != nil {
		return errRequestInvalid
	}
	if err := decodeStrict(b, dst); err != nil {
		return errRequestInvalid
	}
	return nil
}

var canonicalKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// rejectNonCanonicalKeys closes the case-fold hole in encoding/json: the
// decoder matches "THEME" to the theme field, so DisallowUnknownFields alone
// does not make the request schema closed. Every schema key is lowercase
// snake_case; any other spelling is refused before decoding.
func rejectNonCanonicalKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				if key, _ := k.(string); !canonicalKeyRE.MatchString(key) {
					return errors.New("non-canonical object key")
				}
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	return walk()
}

// jsonContentType is the strict media type check for mutations (the takeover
// route's shape): the exact string, no parameters.
func jsonContentType(r *http.Request) bool {
	values, ok := r.Header[http.CanonicalHeaderKey("Content-Type")]
	return ok && len(values) == 1 && values[0] == "application/json"
}
