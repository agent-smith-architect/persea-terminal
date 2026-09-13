package broker

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func TestUnifiedAdmissionKeepsPreControlOutput(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			conn, err := listener.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			read := func(kind proto.FrameType) error {
				frame, err := proto.ReadFrame(conn)
				if err == nil && frame.Type != kind {
					return fmt.Errorf("request frame type %d, want %d", frame.Type, kind)
				}
				return err
			}
			control := func(kind string) error {
				b, err := proto.MarshalControl(proto.Control{Type: kind, V: 1})
				if err != nil {
					return err
				}
				return proto.WriteFrame(conn, proto.FrameControl, b)
			}
			write := func(frame terminal.Frame) error {
				frame.Version, frame.Source, frame.Epoch = terminal.ProtocolVersion, "source", 1
				b, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
				if err != nil {
					return err
				}
				return proto.WriteFrame(conn, proto.FrameAttachment, b)
			}
			if err := read(proto.FrameControl); err != nil {
				return err
			}
			if err := control("hello_ok"); err != nil {
				return err
			}
			if err := read(proto.FrameControl); err != nil {
				return err
			}
			if err := control("attach_ok"); err != nil {
				return err
			}
			if err := write(terminal.Frame{Type: terminal.FramePrepare, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24, History: []string{}, Replay: []byte("snapshot|")}); err != nil {
				return err
			}
			if err := read(proto.FrameAttachment); err != nil {
				return err
			}
			if err := write(terminal.Frame{Type: terminal.FrameCommit, Cut: 1}); err != nil {
				return err
			}
			if err := read(proto.FrameAttachment); err != nil {
				return err
			}
			// Journal output may arrive between COMMIT and Control admission.
			if err := write(terminal.Frame{Type: terminal.FrameLive, Cut: 1, Data: []byte("early|")}); err != nil {
				return err
			}
			if err := write(terminal.Frame{Type: terminal.FrameMode, Mode: terminal.ModeControl}); err != nil {
				return err
			}
			return write(terminal.Frame{Type: terminal.FrameLive, Cut: 1, Data: []byte("later")})
		}()
	}()
	conn, prepared, stream := unifiedE2E1OpenStream(t, listener.Addr().String(), proto.Authority{})
	defer conn.Close()
	if string(prepared.Replay) != "snapshot|" || string(stream.text) != "snapshot|early|" {
		t.Fatalf("admission snapshot=%q visible=%q", prepared.Replay, stream.text)
	}
	stream.readUntil(func() bool { return string(stream.text) == "snapshot|early|later" }, "live output after Control")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
