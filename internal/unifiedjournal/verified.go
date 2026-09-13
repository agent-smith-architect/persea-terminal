package unifiedjournal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unsafe"
)

// A page is immutable once linked. Neither appending nor publishing a new
// suffix copies a historical page or grows an array of historical pointers.
// Small records allocate their exact payload; large records use bounded pages.
const projectionPayloadPageBytes = 32 << 10

type payloadPage struct {
	previous *payloadPage
	data     []byte
	start    int
}

type eventPage struct {
	previous                    *eventPage
	event                       Event
	payload                     *payloadPage
	payloadBytes, metadataBytes int64
}

type pendingRecord struct {
	next   *pendingRecord
	record Record
}

// verifiedCursor describes accepted physical bytes, not charged capacity or
// the append position. Work happens on a value copy and becomes authority only
// when the exact requested commit has been verified in full.
type verifiedCursor struct {
	physical, end, sequence      int64
	committed, committedSequence int64
	geometryCharge               int64
	last                         Record
	tail, view                   *eventPage
}

var errTornAppend = errors.New("torn uncommitted append")

// decodeHeader and verifiedCursor.next are shared by full recovery and live
// suffix verification. Recovery alone permits an incomplete final append.
func decodeHeader(reader io.Reader) (header journalHeader, size int64, digest [sha256.Size]byte, err error) {
	var prefix [8]byte
	if _, err = io.ReadFull(reader, prefix[:]); err != nil || string(prefix[:4]) != string(journalMagic[:]) {
		return header, 0, digest, ErrCorruptJournal
	}
	length := int(binary.BigEndian.Uint32(prefix[4:]))
	if length <= 0 || length > headerLimit {
		return header, 0, digest, ErrCorruptJournal
	}
	data := make([]byte, length)
	if _, err = io.ReadFull(reader, data); err != nil {
		return header, 0, digest, ErrCorruptJournal
	}
	if json.Unmarshal(data, &header) != nil || header.Version != journalVersion {
		return header, 0, digest, ErrCorruptJournal
	}
	hash := sha256.New()
	hash.Write(prefix[:])
	hash.Write(data)
	hash.Sum(digest[:0])
	size = int64(8 + length)
	return header, size, digest, nil
}

func acceptHeader(pane *paneJournal, header journalHeader) error {
	// Identity survives semantic failure so recovery can attribute the file.
	pane.key = header.Key
	switch header.Origin {
	case "", OriginBirth:
		pane.origin = OriginBirth
	case OriginReconstructed, OriginRotated:
		pane.origin = header.Origin
	default:
		return ErrCorruptJournal
	}
	if header.GeometryInitial != nil {
		if !header.GeometryInitial.valid() {
			return ErrCorruptJournal
		}
		pane.initial = *header.GeometryInitial
		pane.admitted = true
	}
	return nil
}

// next consumes one frame, checks all framing and typed semantic rules, and
// advances only private candidate state. Payload pages come from readback.
func (cursor *verifiedCursor) next(reader io.Reader, key PaneKey) (bool, error) {
	var frame [appendFixed]byte
	if _, err := io.ReadFull(reader, frame[:1]); err != nil {
		return false, err
	}
	marker := frame[0]
	size := commitFixed
	if marker == appendMarker {
		size = appendFixed
	} else if marker != commitMarker {
		return false, ErrCorruptJournal
	}
	if _, err := io.ReadFull(reader, frame[1:size]); err != nil {
		if marker == appendMarker && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return false, errTornAppend
		}
		return false, ErrCorruptJournal
	}
	record := Record{Key: key, Kind: RecordKind(frame[1]),
		Sequence: int64(binary.BigEndian.Uint64(frame[2:10])),
		Start:    int64(binary.BigEndian.Uint64(frame[10:18])),
		End:      int64(binary.BigEndian.Uint64(frame[18:26])),
	}
	if marker == commitMarker {
		copy(record.Hash[:], frame[26:commitFixed])
		last := cursor.last
		if record.Kind != last.Kind || record.Sequence != last.Sequence || record.Start != last.Start ||
			record.End != last.End || record.Hash != last.Hash || record.End > cursor.end || record.Sequence <= cursor.committedSequence {
			return false, ErrCorruptJournal
		}
		cursor.committed, cursor.committedSequence = record.End, record.Sequence
		cursor.view = cursor.tail
		cursor.physical += commitFixed
		return false, nil
	}
	length := int64(binary.BigEndian.Uint32(frame[26:30]))
	record.Geometry = Geometry{Columns: int(binary.BigEndian.Uint32(frame[30:34])), Rows: int(binary.BigEndian.Uint32(frame[34:38]))}
	copy(record.Hash[:], frame[38:appendFixed])
	var payload *payloadPage
	var metadata int64
	hash := sha256.New()
	for position := int64(0); position < length; {
		count := min(length-position, projectionPayloadPageBytes)
		data := make([]byte, int(count))
		if _, err := io.ReadFull(reader, data); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, errTornAppend
			}
			return false, err
		}
		hash.Write(data)
		payload = &payloadPage{previous: payload, data: data, start: int(position)}
		metadata += int64(unsafe.Sizeof(payloadPage{}))
		position += count
	}
	if record.Sequence != cursor.sequence+1 || record.Start != cursor.end {
		return false, ErrCorruptJournal
	}
	switch record.Kind {
	case RecordOutput:
		var actual [sha256.Size]byte
		hash.Sum(actual[:0])
		if record.End != record.Start+length || record.Geometry != (Geometry{}) || record.Hash != actual {
			return false, ErrCorruptJournal
		}
	case RecordGeometry:
		if length != 0 || record.End != record.Start || !record.Geometry.valid() || geometryHash(record.Sequence, record.Geometry) != record.Hash {
			return false, ErrCorruptJournal
		}
		cursor.geometryCharge += geometryRecordCost
	default:
		return false, ErrCorruptJournal
	}
	page := &eventPage{previous: cursor.tail, payload: payload,
		event:        Event{Kind: record.Kind, Sequence: record.Sequence, Start: record.Start, End: record.End, Geometry: record.Geometry},
		payloadBytes: length, metadataBytes: metadata + int64(unsafe.Sizeof(eventPage{})),
	}
	if cursor.tail != nil {
		page.payloadBytes += cursor.tail.payloadBytes
		page.metadataBytes += cursor.tail.metadataBytes
	}
	cursor.tail, cursor.last = page, record
	cursor.sequence, cursor.end = record.Sequence, record.End
	cursor.physical += int64(appendFixed) + length
	return true, nil
}

type journalReadAt struct {
	realm *Realm
	file  *os.File
}

func (reader journalReadAt) ReadAt(data []byte, offset int64) (int, error) {
	n, err := reader.realm.ops.readAt(reader.file, data, offset)
	if n < 0 || n > len(data) {
		return 0, ErrStorage
	}
	reader.realm.verification.readBytes.Add(uint64(n))
	// ReadFull otherwise discards an error returned alongside a full buffer.
	// EOF with complete bytes is permitted by ReaderAt; an actual I/O failure
	// cannot become a successful commit merely because it also returned bytes.
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if n == 0 && err == nil && len(data) != 0 {
		err = io.ErrNoProgress
	}
	return n, err
}

func (realm *Realm) verifyCommitted(pane *paneJournal, expected Record) (verified verifiedCursor, resultErr error) {
	started := realm.verification.begin()
	defer func() { realm.verification.finish(started, resultErr != nil) }()
	verified = pane.verified
	reader := io.NewSectionReader(journalReadAt{realm: realm, file: pane.file}, verified.physical, pane.stored-verified.physical)
	if verified.physical == 0 {
		header, size, digest, err := decodeHeader(reader)
		var accepted paneJournal
		if err != nil || acceptHeader(&accepted, header) != nil || digest != pane.headerHash || size != pane.headerSize ||
			accepted.key != pane.key || accepted.initial != pane.initial || accepted.admitted != pane.admitted || accepted.origin != pane.origin ||
			header.BrokerIncarnation != realm.options.BrokerIncarnation {
			return verifiedCursor{}, ErrCorruptJournal
		}
		verified.physical = size
	}
	nextExpected := pane.pendingHead
	var decoded uint64
	for verified.physical < pane.stored {
		appended, err := verified.next(reader, pane.key)
		if err != nil {
			return verifiedCursor{}, ErrCorruptJournal
		}
		if appended {
			decoded++
			if nextExpected == nil || verified.last != nextExpected.record {
				return verifiedCursor{}, ErrCorruptJournal
			}
			nextExpected = nextExpected.next
		}
	}
	if verified.physical != pane.stored || nextExpected != nil || verified.last != expected ||
		verified.committed != expected.End || verified.committedSequence != expected.Sequence || verified.view != verified.tail {
		return verifiedCursor{}, ErrCorruptJournal
	}
	realm.verification.decodedRecords.Add(decoded)
	return verified, nil
}

// ProjectionUsage counts retained payload capacity and metadata structure
// bytes. Allocator rounding and snapshot copies are separate Phase 3 budget
// owners. Pending append metadata is included until verification releases it.
func (realm *Realm) ProjectionUsage(key PaneKey) (payload, metadata, records int64) {
	if pane := realm.panes[key]; pane != nil {
		if view := pane.verified.view; view != nil {
			payload, metadata, records = view.payloadBytes, view.metadataBytes, view.event.Sequence
		}
		metadata += pane.pendingCount * int64(unsafe.Sizeof(pendingRecord{}))
	}
	return
}

func copyEventPayload(destination []byte, page *eventPage) {
	for payload := page.payload; payload != nil; payload = payload.previous {
		copy(destination[payload.start:], payload.data)
	}
}

// A proven-unlinked tombstone still prevents late work from recreating its
// identity. It no longer owns payload/index pages or pending append metadata.
// The previous frontier numbers remain diagnostic history, not a readable view.
func (pane *paneJournal) releaseProjection() {
	pane.verified = verifiedCursor{}
	pane.pendingHead, pane.pendingTail, pane.pendingCount = nil, nil, 0
	pane.projectionReleased = true
}
