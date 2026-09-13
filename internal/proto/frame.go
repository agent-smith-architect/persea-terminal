package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type FrameType byte

const (
	FrameControl FrameType = 0x01
	FrameData    FrameType = 0x02
	FrameInput   FrameType = 0x03
	// FrameAttachment carries exactly one attachmentwire JSON object.  It is
	// deliberately the only broker framing class for the Epoch protocol.
	FrameAttachment FrameType = 0x04
	// FrameImage carries exactly one staged-image payload: raw bytes, with no
	// framing of its own.
	FrameImage FrameType = 0x05

	MaxControl    = 64 * 1024
	MaxData       = 32 * 1024
	MaxInput      = 8 * 1024
	MaxAttachment = 16 << 20
	MaxImage      = 10 << 20
)

var ErrInvalidFrame = errors.New("invalid frame")

type Frame struct {
	Type    FrameType
	Payload []byte
}

func limitFor(t FrameType) (uint32, bool) {
	switch t {
	case FrameControl:
		return MaxControl, true
	case FrameData:
		return MaxData, true
	case FrameInput:
		return MaxInput, true
	case FrameAttachment:
		return MaxAttachment, true
	case FrameImage:
		return MaxImage, true
	default:
		return 0, false
	}
}

func ReadFrame(r io.Reader) (Frame, error) {
	return ReadFrameWithAdmission(r, nil)
}

// ReadFrameWithAdmission lets the receiving owner fund the payload and its
// decoding copies after validating the header, before allocating or reading
// the body. The caller retains and releases that ownership after consumption.
func ReadFrameWithAdmission(r io.Reader, admit func(FrameType, uint32) error) (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	t := FrameType(header[0])
	limit, ok := limitFor(t)
	if !ok {
		return Frame{}, fmt.Errorf("%w: unknown type 0x%02x", ErrInvalidFrame, byte(t))
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > limit {
		return Frame{}, fmt.Errorf("%w: type 0x%02x length %d exceeds %d", ErrInvalidFrame, byte(t), n, limit)
	}
	if admit != nil {
		if err := admit(t, n); err != nil {
			return Frame{}, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: t, Payload: payload}, nil
}

func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	limit, ok := limitFor(t)
	if !ok {
		return fmt.Errorf("%w: unknown type 0x%02x", ErrInvalidFrame, byte(t))
	}
	if uint64(len(payload)) > uint64(limit) {
		return fmt.Errorf("%w: type 0x%02x length %d exceeds %d", ErrInvalidFrame, byte(t), len(payload), limit)
	}
	var header [5]byte
	header[0] = byte(t)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
