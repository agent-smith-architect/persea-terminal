package broker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
)

func TestRecordingAttachmentLeaseSurvivesConsumerAndCallerRemoval(t *testing.T) {
	var budget recordingReaderBudget
	lease, err := budget.acquireAttachment()
	if err != nil {
		t.Fatal(err)
	}
	// The actual production edges hold separately for Epoch, PTY and Wait.
	for i := 0; i < 3; i++ {
		lease.hold()
	}
	if err := lease.reserveSnapshot(2 << 20); err != nil {
		t.Fatal(err)
	}
	if !lease.reserveTransport(4 << 20) {
		t.Fatal("transport reserve")
	}
	before := budget.snapshot()
	budget.limit = before.Bytes
	lease.detach()
	lease.done()
	lease.releaseSnapshot()
	for i := 0; i < 3; i++ {
		lease.done()
	}
	if got := budget.snapshot(); got.Readers != 1 || got.Bytes == 0 {
		t.Fatal("transport outlived refunded reader", got)
	}
	if _, err := budget.acquireAttachment(); !errors.Is(err, errRecordingReaders) {
		t.Fatal(err)
	}
	lease.releaseTransport(4 << 20)
	if got := budget.snapshot(); got.Readers != 0 || got.Bytes != 0 {
		t.Fatal(got)
	}
}

func TestRecordingUnifiedCutKeepsMarkerAndOmitsUnusedCapture(t *testing.T) {
	for _, ownsOutput := range []bool{false, true} {
		name := "classic"
		if ownsOutput {
			name = "journal"
		}
		t.Run(name, func(t *testing.T) {
			d := newDisposable(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			authority, err := incarnation(d.tmux)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := details(d.tmux, "$0")
			if err != nil {
				t.Fatal(err)
			}
			authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
			witness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, detail.ID)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var observed bytes.Buffer
			delayed := newDelayedPTY()
			delayed.setConsumer(func(data []byte) error { mu.Lock(); defer mu.Unlock(); _, _ = observed.Write(data); return nil }, func(error) {})
			txn := &tmuxPinnedTransaction{server: d.tmux, bootID: authority.BootID, authority: authority, pty: delayed, writerOwnsOutput: ownsOutput}
			bound, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = txn.RunPinned(context.Background(), terminal.TransactionRequest{Action: terminal.ActionCleanup, Witness: witness, Attachment: bound.Attachment})
			}()
			buffer := "persea_cut_" + strings.TrimPrefix(bound.Attachment.ClientID, "client-")
			d.run("set-buffer", "-b", buffer, "existing-buffer-proves-no-capture")
			marker := []byte("recording-marker-boundary")
			cut, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionCut, Witness: witness, Attachment: bound.Attachment, Cut: 1, Kind: terminal.CutInitial, Marker: marker, HistoryRows: 1000})
			if err != nil {
				t.Fatal(err)
			}
			out, bufferErr := tmuxOutput(d.tmux, "show-buffer", "-b", buffer)
			if ownsOutput {
				if len(cut.Capture.Capture) != 0 || bufferErr != nil || strings.TrimSpace(out) != "existing-buffer-proves-no-capture" {
					t.Fatal("unified cut ran discarded capture", bufferErr, len(cut.Capture.Capture))
				}
			} else if bufferErr == nil {
				t.Fatal("classic cut did not consume its capture buffer")
			}
			for {
				mu.Lock()
				found := bytes.Contains(observed.Bytes(), marker)
				mu.Unlock()
				if found {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("guarded marker was lost")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestRecordingAttachmentAndSnapshotUseOneAdmission(t *testing.T) {
	effects, key := recordingReaderFixture(t, 2<<20)
	lease, err := effects.readers.acquireAttachment()
	if err != nil {
		t.Fatal(err)
	}
	events, _, tail, cancel, err := effects.openSnapshotTailWithLease(key.Session, lease)
	if err != nil {
		t.Fatal(err)
	}
	if effects.readers.snapshot().Readers != 1 || tail.lease != lease || len(events) == 0 {
		t.Fatal("snapshot created an independent reader")
	}
	cancel()
	if got := effects.readers.snapshot(); got.Readers != 1 || got.Snapshots != 1 {
		t.Fatal(got)
	}
	events = nil
	tail.releaseSnapshot()
	if got := effects.readers.snapshot(); got.Readers != 1 {
		t.Fatal("Epoch refunded with snapshot", got)
	}
	lease.done()
	if got := effects.readers.snapshot(); got.Readers != 0 || got.Bytes != 0 {
		t.Fatal(got)
	}
}
