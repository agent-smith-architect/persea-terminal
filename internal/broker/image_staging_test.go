package broker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

var imagePNG = []byte("\x89PNG\r\n\x1a\n")
var imageJPEG = []byte("\xff\xd8\xff\xe0JFIF\x00")
var imageGIF = []byte("GIF89a\x01\x00\x01\x00")
var imageWebP = []byte("RIFF\x04\x00\x00\x00WEBPVP8 ")

type imageProbeListener struct {
	accepted bool
	err      error
}

func (l *imageProbeListener) Accept() (net.Conn, error) { l.accepted = true; return nil, l.err }
func (l *imageProbeListener) Close() error              { return nil }
func (l *imageProbeListener) Addr() net.Addr            { return imageProbeAddr("probe") }

type imageProbeAddr string

func (a imageProbeAddr) Network() string { return "test" }
func (a imageProbeAddr) String() string  { return string(a) }

type imageDirInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i imageDirInfo) Name() string       { return "images" }
func (i imageDirInfo) Size() int64        { return 0 }
func (i imageDirInfo) Mode() os.FileMode  { return i.mode | os.ModeDir }
func (i imageDirInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i imageDirInfo) IsDir() bool        { return true }
func (i imageDirInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func imageControl(t *testing.T, conn net.Conn, control proto.Control) {
	t.Helper()
	payload, err := proto.MarshalControl(control)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(conn, proto.FrameControl, payload); err != nil {
		t.Fatal(err)
	}
}

func readImageControl(t *testing.T, conn net.Conn) proto.Control {
	t.Helper()
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != proto.FrameControl {
		t.Fatalf("response frame type = 0x%02x, want control", frame.Type)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func startImageBroker(t *testing.T, stagingDir string) string {
	t.Helper()
	socketDir, err := os.MkdirTemp("/tmp", "ptb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(socketDir, "broker.sock")
	if len(path) >= 100 {
		t.Fatalf("broker socket path is %d bytes, want < 100: %q", len(path), path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	cfg := config.Broker{
		Realm: "r", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true,
		ImageStagingDir: stagingDir,
	}
	go func() { done <- Serve(listener, cfg) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("broker did not stop")
		}
	})
	return path
}

func dialImageBroker(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	imageControl(t, conn, proto.Control{Type: "hello", V: 1})
	if got := readImageControl(t, conn); got.Type != "hello_ok" {
		conn.Close()
		t.Fatalf("hello response = %+v", got)
	}
	return conn
}

func exchangeImage(t *testing.T, conn net.Conn, media string, declared int, frameType proto.FrameType, data []byte) proto.Control {
	t.Helper()
	imageControl(t, conn, proto.Control{Type: proto.ControlImageStage, MediaType: media, Bytes: declared})
	if err := proto.WriteFrame(conn, frameType, data); err != nil {
		t.Fatal(err)
	}
	return readImageControl(t, conn)
}

func deterministicImageStager(t *testing.T, dir string, now time.Time) *imageStager {
	t.Helper()
	stager, err := newImageStager(dir)
	if err != nil {
		t.Fatal(err)
	}
	stager.now = func() time.Time { return now }
	stager.randomRead = func(out []byte) (int, error) {
		for i := range out {
			out[i] = 0xab
		}
		return len(out), nil
	}
	return stager
}

func writeSizedImageEntry(t *testing.T, dir, name string, size int64, modified time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func imageNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestImageStageDisabledIsExactAndTouchesNoFilesystem(t *testing.T) {
	path := startImageBroker(t, "")
	conn := dialImageBroker(t, path)
	defer conn.Close()
	got := exchangeImage(t, conn, "image/png", len(imagePNG), proto.FrameImage, imagePNG)
	if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalImagesDisabled}) {
		t.Fatalf("disabled image response = %+v", got)
	}
}

func TestImageStagerStartupProbeRunsBeforeAccept(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	badMode := filepath.Join(base, "bad-mode")
	if err := os.Mkdir(badMode, 0750); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(base, "link")
	if err := os.Symlink(badMode, symlink); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{badMode, symlink, regular} {
		listener := &imageProbeListener{err: errors.New("accept sentinel")}
		err := Serve(listener, config.Broker{
			Realm: "r", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true, ImageStagingDir: path,
		})
		if err == nil || listener.accepted {
			t.Fatalf("startup path %q: err=%v acceptCalled=%t", path, err, listener.accepted)
		}
	}
	created := filepath.Join(base, "created")
	listener := &imageProbeListener{err: errors.New("accept sentinel")}
	err := Serve(listener, config.Broker{
		Realm: "r", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true, ImageStagingDir: created,
	})
	if err == nil || !listener.accepted {
		t.Fatalf("valid startup: err=%v acceptCalled=%t", err, listener.accepted)
	}
	info, statErr := os.Lstat(created)
	if statErr != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("created staging dir = %+v, %v", info, statErr)
	}
}

func TestImageStagingDirectoryOwnerAndModeAreExact(t *testing.T) {
	uid := uint32(os.Geteuid())
	for _, tc := range []struct {
		name string
		info os.FileInfo
		ok   bool
	}{
		{"exact", imageDirInfo{mode: 0700, uid: uid}, true},
		{"wrong owner", imageDirInfo{mode: 0700, uid: uid + 1}, false},
		{"group bit", imageDirInfo{mode: 0710, uid: uid}, false},
		{"setgid", imageDirInfo{mode: 0700 | os.ModeSetgid, uid: uid}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageStagingDirectory(tc.info, uid)
			if (err == nil) != tc.ok {
				t.Fatalf("validate = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}

func TestImageStageExchangeFailuresAreProtocolNotApplicationRefusals(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		declared  int
		frameType proto.FrameType
		data      []byte
		wantCode  string
	}{
		{"wrong frame", len(imagePNG), proto.FrameData, imagePNG, "protocol"},
		{"length mismatch", len(imagePNG) + 1, proto.FrameImage, imagePNG, "protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialImageBroker(t, startImageBroker(t, dir))
			got := exchangeImage(t, conn, "image/png", tc.declared, tc.frameType, tc.data)
			if got.Type != "error" || got.Code != tc.wantCode {
				t.Fatalf("malformed exchange = %+v, want error/%s", got, tc.wantCode)
			}
			if got.Type == proto.ControlImageRefused {
				t.Fatal("malformed exchange used application refusal vocabulary")
			}
			if _, err := proto.ReadFrame(conn); err == nil {
				t.Fatal("malformed exchange did not close")
			}
			conn.Close()
		})
	}
}

func TestImageStageOversizeDeclarationRefusesBeforeFrameRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	conn := dialImageBroker(t, startImageBroker(t, dir))
	defer conn.Close()
	imageControl(t, conn, proto.Control{Type: proto.ControlImageStage, MediaType: "image/png", Bytes: proto.MaxImage + 1})
	got := readImageControl(t, conn)
	if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalTooLarge}) {
		t.Fatalf("oversize declaration = %+v", got)
	}
	if err := proto.WriteFrame(conn, proto.FrameControl, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal("broker read or closed before early refusal: ", err)
	}
}

func TestImageStageInvalidFrameAndEOFResponses(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Run("invalid frame", func(t *testing.T) {
		conn := dialImageBroker(t, startImageBroker(t, dir))
		defer conn.Close()
		imageControl(t, conn, proto.Control{Type: proto.ControlImageStage, MediaType: "image/png", Bytes: proto.MaxImage})
		var header [5]byte
		header[0] = byte(proto.FrameImage)
		binary.BigEndian.PutUint32(header[1:], uint32(proto.MaxImage+1))
		if _, err := conn.Write(header[:]); err != nil {
			t.Fatal(err)
		}
		got := readImageControl(t, conn)
		if got.Type != "error" || got.Code != "bad_frame" {
			t.Fatalf("invalid frame = %+v", got)
		}
	})
	t.Run("EOF", func(t *testing.T) {
		conn := dialImageBroker(t, startImageBroker(t, dir))
		imageControl(t, conn, proto.Control{Type: proto.ControlImageStage, MediaType: "image/png", Bytes: len(imagePNG)})
		unix, ok := conn.(*net.UnixConn)
		if !ok {
			t.Fatal("connection is not UnixConn")
		}
		if err := unix.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := proto.ReadFrame(conn); err == nil {
			t.Fatal("EOF exchange produced a response")
		}
		conn.Close()
	})
}

func TestImageStageResniffsAndNamesFromBytes(t *testing.T) {
	for _, tc := range []struct {
		media, ext string
		data       []byte
	}{
		{"image/png", ".png", imagePNG},
		{"image/jpeg", ".jpg", imageJPEG},
		{"image/gif", ".gif", imageGIF},
		{"image/webp", ".webp", imageWebP},
	} {
		t.Run(tc.media, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			conn := dialImageBroker(t, startImageBroker(t, dir))
			defer conn.Close()
			got := exchangeImage(t, conn, tc.media, len(tc.data), proto.FrameImage, tc.data)
			if got.Type != proto.ControlImageStaged || !strings.HasSuffix(got.Path, tc.ext) || !filepath.IsAbs(got.Path) {
				t.Fatalf("staged response = %+v", got)
			}
			if filepath.Dir(got.Path) != dir || !strings.HasPrefix(filepath.Base(got.Path), "img-") || len(got.ID) != 32 {
				t.Fatalf("server-authored path/id = %+v", got)
			}
			info, err := os.Lstat(got.Path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				t.Fatalf("staged file = %+v, %v", info, err)
			}
			bytesOnDisk, err := os.ReadFile(got.Path)
			if err != nil || !bytes.Equal(bytesOnDisk, tc.data) {
				t.Fatalf("staged bytes mismatch: %v", err)
			}
		})
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	conn := dialImageBroker(t, startImageBroker(t, dir))
	defer conn.Close()
	got := exchangeImage(t, conn, "image/png", len(imageJPEG), proto.FrameImage, imageJPEG)
	if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalUnsupportedType}) {
		t.Fatalf("declared/sniff mismatch = %+v", got)
	}
}

func TestImageStagerExtensionUsesIndependentlySniffedResult(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate image staging oracle source")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "image_staging.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, sourcePath, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stage *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "stage" || function.Recv == nil || len(function.Recv.List) != 1 {
			continue
		}
		pointer, ok := function.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		name, named := pointer.X.(*ast.Ident)
		if named && name.Name == "imageStager" {
			stage = function
			break
		}
	}
	if stage == nil {
		t.Fatal("image extension derivation stage function not found")
	}
	var binding, finalPath token.Pos
	var extWrites []token.Pos
	finalConsumesExt := false
	ast.Inspect(stage.Body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for _, left := range statement.Lhs {
				identifier, ok := left.(*ast.Ident)
				if !ok {
					continue
				}
				if identifier.Name == "ext" {
					isSniffBinding := statement.Tok == token.DEFINE && len(statement.Lhs) == 2 &&
						len(statement.Rhs) == 1 && callIsSniffImageData(statement.Rhs[0])
					if isSniffBinding && binding == token.NoPos {
						binding = statement.Pos()
					} else {
						extWrites = append(extWrites, statement.Pos())
					}
				}
				if identifier.Name == "finalPath" {
					finalPath = statement.Pos()
					for _, right := range statement.Rhs {
						ast.Inspect(right, func(expression ast.Node) bool {
							if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "ext" {
								finalConsumesExt = true
							}
							return true
						})
					}
				}
			}
		case *ast.DeclStmt:
			declaration, ok := statement.Decl.(*ast.GenDecl)
			if !ok {
				break
			}
			for _, specification := range declaration.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if name.Name == "ext" {
						extWrites = append(extWrites, value.Pos())
					}
				}
			}
		case *ast.IncDecStmt:
			if identifier, ok := statement.X.(*ast.Ident); ok && identifier.Name == "ext" {
				extWrites = append(extWrites, statement.Pos())
			}
		case *ast.RangeStmt:
			for _, expression := range []ast.Expr{statement.Key, statement.Value} {
				if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "ext" {
					extWrites = append(extWrites, statement.Pos())
				}
			}
		}
		return true
	})
	if binding == token.NoPos || finalPath == token.NoPos || binding >= finalPath || !finalConsumesExt {
		t.Fatalf("image extension derivation did not bind sniffImage(data) ext through finalPath: binding=%v final=%v consumes=%t", files.Position(binding), files.Position(finalPath), finalConsumesExt)
	}
	for _, write := range extWrites {
		if write > binding && write < finalPath {
			t.Fatalf("image extension derivation rewrote ext between sniff binding and finalPath: %s", files.Position(write))
		}
	}
}

func callIsSniffImageData(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	function, ok := call.Fun.(*ast.Ident)
	argument, argumentOK := call.Args[0].(*ast.Ident)
	return ok && argumentOK && function.Name == "sniffImage" && argument.Name == "data"
}

func TestImageStagerStartupReapsOnlyOwnedExpiredNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-7 * time.Hour)
	fresh := now.Add(-5 * time.Hour)
	writeSizedImageEntry(t, dir, "img-11111111111111111111111111111111.png", 1, stale)
	writeSizedImageEntry(t, dir, ".img-22222222222222222222222222222222.tmp", 1, stale)
	writeSizedImageEntry(t, dir, "img-33333333333333333333333333333333.jpg", 1, fresh)
	writeSizedImageEntry(t, dir, ".img-44444444444444444444444444444444.tmp", 1, fresh)
	writeSizedImageEntry(t, dir, "notes.txt", 1, stale)
	if err := os.Symlink("notes.txt", filepath.Join(dir, "img-55555555555555555555555555555555.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := newImageStager(dir); err != nil {
		t.Fatal(err)
	}
	want := []string{
		".img-44444444444444444444444444444444.tmp",
		"img-33333333333333333333333333333333.jpg",
		"img-55555555555555555555555555555555.png",
		"notes.txt",
	}
	if got := imageNames(t, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("startup reap names = %q, want %q", got, want)
	}
}

func TestImageStagerTTLBoundaryAndExactSuccessResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 123456789, time.UTC)
	stager := deterministicImageStager(t, dir, now)
	writeSizedImageEntry(t, dir, "img-11111111111111111111111111111111.png", 1, now.Add(-6*time.Hour-time.Nanosecond))
	writeSizedImageEntry(t, dir, "img-22222222222222222222222222222222.png", 1, now.Add(-6*time.Hour))
	writeSizedImageEntry(t, dir, "img-33333333333333333333333333333333.png", 1, now.Add(-6*time.Hour+time.Nanosecond))
	got := stager.stage("image/png", imagePNG)
	wantID := strings.Repeat("ab", 16)
	wantPath := filepath.Join(dir, "img-"+wantID+".png")
	want := proto.Control{
		Type: proto.ControlImageStaged, Path: wantPath, ID: wantID,
		ExpiresAt: now.Add(6 * time.Hour).Format(time.RFC3339Nano),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged result = %+v, want %+v", got, want)
	}
	names := imageNames(t, dir)
	if containsImageName(names, "img-11111111111111111111111111111111.png") ||
		!containsImageName(names, "img-22222222222222222222222222222222.png") ||
		!containsImageName(names, "img-33333333333333333333333333333333.png") ||
		!containsImageName(names, filepath.Base(wantPath)) {
		t.Fatalf("TTL boundary survivors = %q", names)
	}
}

func TestImageStagerOldestFirstFileAndByteBudgets(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	t.Run("file count", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		stager := deterministicImageStager(t, dir, now)
		for i := 0; i < 64; i++ {
			name := fmt.Sprintf("img-%032x.png", i)
			writeSizedImageEntry(t, dir, name, 1, now.Add(time.Duration(i)*time.Second))
		}
		got := stager.stage("image/png", imagePNG)
		if got.Type != proto.ControlImageStaged {
			t.Fatalf("file-budget stage = %+v", got)
		}
		if names := imageNames(t, dir); len(names) != 64 || containsImageName(names, "img-"+strings.Repeat("0", 32)+".png") {
			t.Fatalf("file-budget survivors = %q", names)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		stager := deterministicImageStager(t, dir, now)
		for i := 0; i < 26; i++ {
			writeSizedImageEntry(t, dir, fmt.Sprintf("img-%032x.png", i), 10<<20, now.Add(time.Duration(i)*time.Second))
		}
		got := stager.stage("image/png", imagePNG)
		if got.Type != proto.ControlImageStaged {
			t.Fatalf("byte-budget stage = %+v", got)
		}
		var total int64
		for _, entry := range imageNames(t, dir) {
			info, err := os.Lstat(filepath.Join(dir, entry))
			if err != nil {
				t.Fatal(err)
			}
			total += info.Size()
		}
		if total > 256<<20 {
			t.Fatalf("byte budget = %d", total)
		}
	})
}

func containsImageName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func TestImageStagerProtectedEntriesCountButAreNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stager := deterministicImageStager(t, dir, time.Now())
	writeSizedImageEntry(t, dir, "private.bin", 256<<20, time.Now().Add(-24*time.Hour))
	if err := os.Symlink("private.bin", filepath.Join(dir, "img-11111111111111111111111111111111.png")); err != nil {
		t.Fatal(err)
	}
	got := stager.stage("image/png", imagePNG)
	if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalCapacity}) {
		t.Fatalf("protected pressure = %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "private.bin")); err != nil {
		t.Fatal("unknown file was deleted: ", err)
	}
	if info, err := os.Lstat(filepath.Join(dir, "img-11111111111111111111111111111111.png")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was deleted or followed: %+v %v", info, err)
	}
}

func TestImageStagerAtomicOperationOrderAndFailureSweep(t *testing.T) {
	phases := []string{"success", "open", "write", "short-write", "file-sync", "close", "rename", "directory-open", "directory-sync", "directory-close"}
	for _, failure := range phases {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			stager := deterministicImageStager(t, dir, time.Unix(123, 0).UTC())
			var order []string
			origOpen, origWrite := stager.openFile, stager.writeFile
			origFileSync, origClose := stager.fileSync, stager.closeFile
			origRename := stager.renameFile
			origDirOpen, origDirSync, origDirClose := stager.openDirectory, stager.dirSync, stager.closeDirectory
			stager.openFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				order = append(order, "open")
				wantFlags := os.O_CREATE | os.O_EXCL | os.O_WRONLY | syscall.O_NOFOLLOW
				if flags != wantFlags || mode != 0600 {
					t.Fatalf("open flags/mode = %#x/%#o, want %#x/0600", flags, mode, wantFlags)
				}
				if failure == "open" {
					return nil, errors.New("injected open")
				}
				return origOpen(name, flags, mode)
			}
			stager.writeFile = func(file *os.File, data []byte) (int, error) {
				order = append(order, "write")
				if failure == "write" {
					return 0, errors.New("injected write")
				}
				if failure == "short-write" {
					return len(data) - 1, nil
				}
				return origWrite(file, data)
			}
			stager.fileSync = func(file *os.File) error {
				order = append(order, "file-sync")
				if failure == "file-sync" {
					return errors.New("injected file sync")
				}
				return origFileSync(file)
			}
			stager.closeFile = func(file *os.File) error {
				order = append(order, "close")
				if failure == "close" {
					_ = origClose(file)
					return errors.New("injected close")
				}
				return origClose(file)
			}
			stager.renameFile = func(old, next string) error {
				order = append(order, "rename")
				if failure == "rename" {
					return errors.New("injected rename")
				}
				return origRename(old, next)
			}
			stager.openDirectory = func() (*os.File, error) {
				order = append(order, "directory-open")
				if failure == "directory-open" {
					return nil, errors.New("injected directory open")
				}
				return origDirOpen()
			}
			stager.dirSync = func(file *os.File) error {
				order = append(order, "directory-sync")
				if failure == "directory-sync" {
					return errors.New("injected directory sync")
				}
				return origDirSync(file)
			}
			stager.closeDirectory = func(file *os.File) error {
				order = append(order, "directory-close")
				if failure == "directory-close" {
					_ = origDirClose(file)
					return errors.New("injected directory close")
				}
				return origDirClose(file)
			}
			got := stager.stage("image/png", imagePNG)
			wantOrder := []string{"open", "write", "file-sync", "close", "rename", "directory-open", "directory-sync", "directory-close"}
			if failure == "success" {
				if got.Type != proto.ControlImageStaged || !reflect.DeepEqual(order, wantOrder) {
					t.Fatalf("success = %+v, operations=%q, want %q", got, order, wantOrder)
				}
				return
			}
			if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalIO}) {
				t.Fatalf("fault %s response = %+v, operations=%q", failure, got, order)
			}
			for _, name := range imageNames(t, dir) {
				if strings.HasPrefix(name, "img-") || strings.HasPrefix(name, ".img-") {
					t.Fatalf("fault %s left staged name %q; operations=%q", failure, name, order)
				}
			}
		})
	}
}

func TestImageStagerAccountingFailuresAreIONotCapacity(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"read-dir", "lstat", "remove"} {
		t.Run(failure, func(t *testing.T) {
			stager := deterministicImageStager(t, dir, time.Now())
			writeSizedImageEntry(t, dir, "img-11111111111111111111111111111111.png", 1, time.Now().Add(-7*time.Hour))
			switch failure {
			case "read-dir":
				stager.readDirectory = func() ([]os.DirEntry, error) { return nil, errors.New("injected read-dir") }
			case "lstat":
				stager.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("injected lstat") }
			case "remove":
				stager.removeFile = func(string) error { return errors.New("injected remove") }
			}
			got := stager.stage("image/png", imagePNG)
			if !reflect.DeepEqual(got, proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalIO}) {
				t.Fatalf("accounting failure %s = %+v", failure, got)
			}
			_ = os.Remove(filepath.Join(dir, "img-11111111111111111111111111111111.png"))
		})
	}
}

func TestImageStageRepeatedPairsAndRealmIsolation(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, dir := range []string{dirA, dirB} {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	connA := dialImageBroker(t, startImageBroker(t, dirA))
	defer connA.Close()
	for i := 0; i < 2; i++ {
		got := exchangeImage(t, connA, "image/png", len(imagePNG), proto.FrameImage, imagePNG)
		if got.Type != proto.ControlImageStaged || filepath.Dir(got.Path) != dirA {
			t.Fatalf("realm A pair %d = %+v", i, got)
		}
	}
	connB := dialImageBroker(t, startImageBroker(t, dirB))
	defer connB.Close()
	got := exchangeImage(t, connB, "image/jpeg", len(imageJPEG), proto.FrameImage, imageJPEG)
	if got.Type != proto.ControlImageStaged || filepath.Dir(got.Path) != dirB {
		t.Fatalf("realm B = %+v", got)
	}
	if len(imageNames(t, dirA)) != 2 || len(imageNames(t, dirB)) != 1 {
		t.Fatalf("cross-realm files: A=%q B=%q", imageNames(t, dirA), imageNames(t, dirB))
	}
}

func TestImageControlStillHasNoBrowserOrFilenameSurface(t *testing.T) {
	payload, err := proto.MarshalControl(proto.Control{Type: proto.ControlImageStage, MediaType: "image/png", Bytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.DecodeClientControl(payload); err == nil {
		t.Fatal("broker image control entered browser/client decoder")
	}
	if _, err := proto.DecodeControl([]byte(`{"type":"image_stage","media_type":"image/png","bytes":8,"filename":"x.png"}`)); err == nil {
		t.Fatal("image_stage accepted a client filename")
	}
}

func TestInventoryAdvertisesImageStagingCapability(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dir := shortTempDir(t)
		cfg := config.Broker{Realm: "local", Servers: []config.TmuxServer{{Label: "main", SocketPath: filepath.Join(dir, "no-server.sock")}}}
		if enabled {
			cfg.ImageStagingDir = filepath.Join(dir, "img")
		}
		listener, _ := startBrokerTest(t, cfg, filepath.Join(dir, "broker.sock"))
		conn, inventory := request(t, filepath.Join(dir, "broker.sock"), proto.Control{Type: "inventory"})
		conn.Close()
		listener.Close()
		if inventory.Type != "inventory_ok" || len(inventory.Servers) != 1 {
			t.Fatalf("enabled=%v inventory=%+v", enabled, inventory)
		}
		if inventory.Servers[0].CanStageImages != enabled {
			t.Fatalf("enabled=%v can_stage_images=%v", enabled, inventory.Servers[0].CanStageImages)
		}
	}
}
