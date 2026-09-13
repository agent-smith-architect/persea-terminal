package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestImageFrameContractConstantsAndLimit(t *testing.T) {
	if FrameImage != FrameType(0x05) {
		t.Fatalf("FrameImage = 0x%02x, want 0x05", FrameImage)
	}
	if MaxImage != 10<<20 {
		t.Fatalf("MaxImage = %d, want %d", MaxImage, 10<<20)
	}
	limit, ok := limitFor(FrameImage)
	if !ok || limit != uint32(MaxImage) {
		t.Fatalf("limitFor(FrameImage) = (%d, %t), want (%d, true)", limit, ok, MaxImage)
	}
}

func TestImageFrameRoundTripAtBound(t *testing.T) {
	payload := bytes.Repeat([]byte{'i'}, MaxImage)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, FrameImage, payload); err != nil {
		t.Fatalf("WriteFrame(FrameImage): %v", err)
	}
	got, err := ReadFrame(&wire)
	if err != nil {
		t.Fatalf("ReadFrame(FrameImage): %v", err)
	}
	if got.Type != FrameImage || !bytes.Equal(got.Payload, payload) {
		t.Fatal("FrameImage round trip changed type or payload")
	}
}

func TestImageFrameRejectsOneByteOversizeOnReadAndWrite(t *testing.T) {
	const want = "invalid frame: type 0x05 length 10485761 exceeds 10485760"

	var header [5]byte
	header[0] = byte(FrameImage)
	binary.BigEndian.PutUint32(header[1:], uint32(MaxImage+1))
	if _, err := ReadFrame(bytes.NewReader(header[:])); !errors.Is(err, ErrInvalidFrame) || err.Error() != want {
		t.Fatalf("ReadFrame oversize error = %v, want %q wrapping ErrInvalidFrame", err, want)
	}

	var wire bytes.Buffer
	err := WriteFrame(&wire, FrameImage, make([]byte, MaxImage+1))
	if !errors.Is(err, ErrInvalidFrame) || err.Error() != want {
		t.Fatalf("WriteFrame oversize error = %v, want %q wrapping ErrInvalidFrame", err, want)
	}
	if wire.Len() != 0 {
		t.Fatalf("WriteFrame wrote %d bytes before rejecting oversize payload", wire.Len())
	}
}

func TestImageControlMessagesEncodeDecodeExactly(t *testing.T) {
	tests := []struct {
		name    string
		control Control
		wire    string
	}{
		{
			name: "stage",
			control: Control{
				Type:      ControlImageStage,
				MediaType: "image/png",
				Bytes:     421337,
			},
			wire: `{"type":"image_stage","media_type":"image/png","bytes":421337}`,
		},
		{
			name: "staged",
			control: Control{
				Type:      ControlImageStaged,
				Path:      "/var/lib/persea-terminal-staging/desk-a7/img-0123456789abcdef0123456789abcdef.png",
				ID:        "0123456789abcdef0123456789abcdef",
				ExpiresAt: "2026-07-30T18:04:11.2Z",
			},
			wire: `{"type":"image_staged","path":"/var/lib/persea-terminal-staging/desk-a7/img-0123456789abcdef0123456789abcdef.png","id":"0123456789abcdef0123456789abcdef","expires_at":"2026-07-30T18:04:11.2Z"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := MarshalControl(tc.control)
			if err != nil {
				t.Fatalf("MarshalControl: %v", err)
			}
			if string(wire) != tc.wire {
				t.Fatalf("encoding = %s, want %s", wire, tc.wire)
			}
			got, err := DecodeControl(wire)
			if err != nil {
				t.Fatalf("DecodeControl: %v", err)
			}
			if !reflect.DeepEqual(got, tc.control) {
				t.Fatalf("decoded = %#v, want %#v", got, tc.control)
			}
			if _, err := DecodeClientControl(wire); err == nil {
				t.Fatalf("DecodeClientControl accepted broker-only control %q", tc.control.Type)
			}
		})
	}
}

func TestImageRefusalCodesEncodeDecodeExactly(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{ImageRefusalImagesDisabled, `{"type":"image_refused","code":"images_disabled"}`},
		{ImageRefusalTooLarge, `{"type":"image_refused","code":"too_large"}`},
		{ImageRefusalUnsupportedType, `{"type":"image_refused","code":"unsupported_type"}`},
		{ImageRefusalCapacity, `{"type":"image_refused","code":"capacity"}`},
		{ImageRefusalIO, `{"type":"image_refused","code":"io"}`},
	} {
		control := Control{Type: ControlImageRefused, Code: tc.code}
		wire, err := MarshalControl(control)
		if err != nil {
			t.Fatalf("MarshalControl(%q): %v", tc.code, err)
		}
		if string(wire) != tc.want {
			t.Fatalf("encoding = %s, want %s", wire, tc.want)
		}
		got, err := DecodeControl(wire)
		if err != nil {
			t.Fatalf("DecodeControl(%q): %v", tc.code, err)
		}
		if !reflect.DeepEqual(got, control) {
			t.Fatalf("decoded = %#v, want %#v", got, control)
		}
		if _, err := DecodeClientControl(wire); err == nil {
			t.Fatal("DecodeClientControl accepted broker-only image_refused")
		}
	}
}

func TestImageControlHasNoFilenameSurface(t *testing.T) {
	const wantPrefix = `invalid control message: json: unknown field "filename"`
	if _, err := DecodeControl([]byte(`{"type":"image_stage","media_type":"image/png","bytes":1,"filename":"private.png"}`)); err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("filename error = %v, want prefix %q", err, wantPrefix)
	}
	typ := reflect.TypeOf(Control{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Name == "Filename" || strings.Split(field.Tag.Get("json"), ",")[0] == "filename" {
			t.Fatalf("Control exposes forbidden filename field: %s %q", field.Name, field.Tag.Get("json"))
		}
	}
}

func TestImageControlFieldTypesRemainStrict(t *testing.T) {
	for _, payload := range []string{
		`{"type":"image_stage","media_type":1,"bytes":1}`,
		`{"type":"image_stage","media_type":"image/png","bytes":"1"}`,
		`{"type":"image_staged","path":1,"id":"0123456789abcdef0123456789abcdef","expires_at":"2026-07-30T18:04:11.2Z"}`,
		`{"type":"image_staged","path":"/tmp/a.png","id":1,"expires_at":"2026-07-30T18:04:11.2Z"}`,
		`{"type":"image_staged","path":"/tmp/a.png","id":"0123456789abcdef0123456789abcdef","expires_at":1}`,
	} {
		if _, err := DecodeControl([]byte(payload)); err == nil || !strings.HasPrefix(err.Error(), "invalid control message: json: cannot unmarshal") {
			t.Fatalf("wrong-type payload error = %v, want invalid-control unmarshal prefix for %s", err, payload)
		}
	}
}
