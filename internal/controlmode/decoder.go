// Package controlmode contains the feature-off foundations for consuming a
// tmux control-mode observer stream.  It deliberately accepts only the tmux
// 3.4 records the broker has reviewed; ambiguity is a terminal parser fault.
package controlmode

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

var ErrDecoderFailed = errors.New("control-mode decoder failed")

const maxControlRecordBytes = 16 << 20

type EventKind uint8

const (
	EventOutput EventKind = iota + 1
	EventExtendedOutput
	EventPause
	EventContinue
	EventNotification
	EventCommandBegin
	EventCommandResponse
	EventCommandEnd
	EventCommandError
)

type Event struct {
	Kind   EventKind
	PaneID string
	Age    uint64
	Data   []byte
	Name   string
	Args   string
}

type commandWitness struct {
	time, number, flags string
}

type Decoder struct {
	buffer      []byte
	failed      bool
	command     *commandWitness
	memory      Memory
	bufferBytes int64
}

func NewDecoder() *Decoder { return &Decoder{} }

func (decoder *Decoder) Failed() bool { return decoder.failed }

func (decoder *Decoder) fail() error {
	decoder.failed = true
	decoder.Release()
	return ErrDecoderFailed
}

func (decoder *Decoder) Feed(input []byte) ([]Event, error) {
	// Unbudgeted compatibility API. Production consumers use EventBatch.Feed
	// so a returned batch has an explicit settlement owner.
	if decoder.memory != nil {
		return nil, ErrDecoderFailed
	}
	return decoder.feed(input, &EventBatch{})
}

func (decoder *Decoder) feed(input []byte, batch *EventBatch) ([]Event, error) {
	if decoder.failed {
		return nil, ErrDecoderFailed
	}
	if len(input) > int(^uint(0)>>1)-len(decoder.buffer) {
		return nil, decoder.fail()
	}
	want := len(decoder.buffer) + len(input)
	if want > cap(decoder.buffer) {
		capacity := roundedBytes(int64(want))
		growth := min(2*int64(cap(decoder.buffer)), int64(maxControlRecordBytes+(128<<10)))
		if growth > capacity {
			capacity = growth
		}
		if decoder.memory != nil && !decoder.memory.Reserve(capacity) {
			return nil, decoder.fail()
		}
		buffer := make([]byte, len(decoder.buffer), int(capacity))
		copy(buffer, decoder.buffer)
		old := decoder.bufferBytes
		decoder.buffer, decoder.bufferBytes = buffer, capacity
		if decoder.memory != nil {
			decoder.memory.Release(old)
		}
	}
	decoder.buffer = append(decoder.buffer, input...)
	count := bytes.Count(decoder.buffer, []byte{'\n'})
	// One exact event array plus payload/string copies and allocator rounding.
	// No unbounded Fields array is built for command witnesses. The buffer is
	// charged separately, including old/new capacity during growth.
	cost := 3*roundedBytes(int64(len(decoder.buffer))) + 256*int64(count)
	if count == 0 {
		cost = 0
	}
	if decoder.memory != nil && !decoder.memory.Reserve(cost) {
		return nil, decoder.fail()
	}
	batch.memory, batch.bytes = decoder.memory, cost
	events := make([]Event, 0, count)
	defer func() { batch.Events = events }()
	for {
		newline := bytes.IndexByte(decoder.buffer, '\n')
		if newline < 0 {
			if len(decoder.buffer) > maxControlRecordBytes {
				return nil, decoder.fail()
			}
			if len(decoder.buffer) == 0 {
				decoder.Release()
			}
			return events, nil
		}
		if newline > maxControlRecordBytes {
			return nil, decoder.fail()
		}
		line := decoder.buffer[:newline]
		decoder.buffer = decoder.buffer[newline+1:]
		event, err := decoder.decodeLine(line)
		if err != nil {
			return nil, decoder.fail()
		}
		events = append(events, event)
	}
}

func (decoder *Decoder) Close() error {
	if decoder.failed {
		return ErrDecoderFailed
	}
	if len(decoder.buffer) != 0 || decoder.command != nil {
		return decoder.fail()
	}
	return nil
}

func (decoder *Decoder) decodeLine(line []byte) (Event, error) {
	if decoder.command != nil {
		if bytes.HasPrefix(line, []byte("%begin ")) {
			return Event{}, ErrDecoderFailed
		}
		if bytes.HasPrefix(line, []byte("%end ")) || bytes.HasPrefix(line, []byte("%error ")) {
			fields := commandFields(line)
			if len(fields) != 4 || fields[1] != decoder.command.time || fields[2] != decoder.command.number || fields[3] != decoder.command.flags {
				return Event{}, ErrDecoderFailed
			}
			kind := EventCommandEnd
			if fields[0] == "%error" {
				kind = EventCommandError
			}
			decoder.command = nil
			return Event{Kind: kind}, nil
		}
		data := make([]byte, len(line)+1)
		copy(data, line)
		data[len(line)] = '\n'
		return Event{Kind: EventCommandResponse, Data: data}, nil
	}

	if bytes.HasPrefix(line, []byte("%begin ")) {
		fields := commandFields(line)
		if len(fields) != 4 {
			return Event{}, ErrDecoderFailed
		}
		decoder.command = &commandWitness{time: fields[1], number: fields[2], flags: fields[3]}
		return Event{Kind: EventCommandBegin}, nil
	}
	if bytes.HasPrefix(line, []byte("%output ")) {
		return decodeOutput(line, false)
	}
	if bytes.HasPrefix(line, []byte("%extended-output ")) {
		return decodeOutput(line, true)
	}
	if len(line) == 0 || line[0] != '%' {
		return Event{}, ErrDecoderFailed
	}
	space := bytes.IndexByte(line, ' ')
	name := string(line[1:])
	args := ""
	if space >= 0 {
		name = string(line[1:space])
		args = string(line[space+1:])
	}
	if !knownNotification(name) {
		return Event{}, ErrDecoderFailed
	}
	switch name {
	case "pause":
		return Event{Kind: EventPause, Name: name, Args: args}, nil
	case "continue":
		return Event{Kind: EventContinue, Name: name, Args: args}, nil
	}
	return Event{Kind: EventNotification, Name: name, Args: args}, nil
}

// A malformed witness with millions of fields must not allocate a field slice
// proportional to its bytes before the four-field grammar rejects it.
func commandFields(line []byte) []string {
	fields := make([]string, 0, 4)
	for field := range strings.FieldsSeq(string(line)) {
		if len(fields) == 4 {
			return nil
		}
		fields = append(fields, field)
	}
	return fields
}

func decodeOutput(line []byte, extended bool) (Event, error) {
	prefix := []byte("%output ")
	kind := EventOutput
	if extended {
		prefix = []byte("%extended-output ")
		kind = EventExtendedOutput
	}
	rest := line[len(prefix):]
	firstSpace := bytes.IndexByte(rest, ' ')
	if firstSpace <= 0 {
		return Event{}, ErrDecoderFailed
	}
	paneID := string(rest[:firstSpace])
	rest = rest[firstSpace+1:]
	var age uint64
	if extended {
		ageEnd := bytes.IndexByte(rest, ' ')
		if ageEnd <= 0 {
			return Event{}, ErrDecoderFailed
		}
		parsed, err := strconv.ParseUint(string(rest[:ageEnd]), 10, 64)
		if err != nil {
			return Event{}, ErrDecoderFailed
		}
		age = parsed
		rest = rest[ageEnd+1:]
		if bytes.HasPrefix(rest, []byte(": ")) {
			rest = rest[2:]
		} else {
			colon := bytes.Index(rest, []byte(" : "))
			if colon < 0 {
				return Event{}, ErrDecoderFailed
			}
			rest = rest[colon+3:]
		}
	}
	data, err := decodePayload(rest)
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: kind, PaneID: paneID, Age: age, Data: data}, nil
}

func decodePayload(encoded []byte) ([]byte, error) {
	decoded := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			decoded = append(decoded, encoded[index])
			index++
			continue
		}
		if index+3 >= len(encoded) {
			return nil, ErrDecoderFailed
		}
		var value byte
		for offset := 1; offset <= 3; offset++ {
			digit := encoded[index+offset]
			if digit < '0' || digit > '7' {
				return nil, ErrDecoderFailed
			}
			value = value*8 + digit - '0'
		}
		decoded = append(decoded, value)
		index += 4
	}
	return decoded, nil
}

func knownNotification(name string) bool {
	switch name {
	case "client-detached", "client-session-changed", "config-error", "continue", "exit",
		"layout-change", "message", "pane-mode-changed", "paste-buffer-changed",
		"paste-buffer-deleted", "pause", "session-changed", "session-renamed",
		"session-window-changed", "sessions-changed", "subscription-changed",
		"unlinked-window-add", "unlinked-window-close", "unlinked-window-renamed",
		"window-add", "window-close", "window-pane-changed", "window-renamed":
		return true
	default:
		return false
	}
}
