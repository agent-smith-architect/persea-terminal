// Package attachmentwire is the single translation boundary between the
// browser-safe attachment JSON and terminal.Frame. The terminal package keeps
// its native Go JSON representation; only this package turns uint64 values into
// decimal strings and byte slices into canonical padded base64 for JavaScript.
package attachmentwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"persea-terminal/internal/terminal"
)

// MaxWireBytes is the smallest power-of-two envelope that carries a valid
// 2 MiB history even when every byte needs six-byte JSON escaping, plus the
// existing 256 KiB replay after base64 expansion and bounded frame metadata.
const MaxWireBytes = 16 << 20

type Direction uint8

const (
	ServerToBrowser Direction = iota + 1
	BrowserToServer
)

var serverTypes = map[terminal.FrameType]bool{
	terminal.FramePrepare: true,
	terminal.FrameCommit:  true,
	terminal.FrameLive:    true,
	terminal.FrameMode:    true,
	terminal.FrameEnd:     true,
}

var browserTypes = map[terminal.FrameType]bool{
	terminal.FrameReady:       true,
	terminal.FrameDefer:       true,
	terminal.FrameModeRequest: true,
	terminal.FrameInput:       true,
	terminal.FrameResize:      true,
	terminal.FrameHistory:     true,
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", terminal.ErrMalformed, fmt.Sprintf(format, args...))
}

func directionAllows(direction Direction, frameType terminal.FrameType) bool {
	switch direction {
	case ServerToBrowser:
		return serverTypes[frameType]
	case BrowserToServer:
		return browserTypes[frameType]
	default:
		return false
	}
}

// Encode emits the browser wire representation of one already-typed frame.
func Encode(frame terminal.Frame, direction Direction) ([]byte, error) {
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	if !directionAllows(direction, frame.Type) {
		return nil, malformed("frame %q is invalid for direction", frame.Type)
	}
	if frame.Type == terminal.FrameDefer && !validDeferReason(frame.Reason) {
		return nil, malformed("invalid DEFER reason %q", frame.Reason)
	}

	value := map[string]any{
		"version": frame.Version,
		"type":    frame.Type,
		"source":  frame.Source,
		"epoch":   strconv.FormatUint(frame.Epoch, 10),
	}
	if frame.Cut != 0 {
		value["cut"] = strconv.FormatUint(frame.Cut, 10)
	}
	switch frame.Type {
	case terminal.FramePrepare:
		value["kind"] = frame.Kind
		value["columns"] = frame.Columns
		value["rows"] = frame.Rows
		value["history"] = append([]string{}, frame.History...)
		value["truncated"] = frame.Truncated
		value["replay"] = base64.StdEncoding.EncodeToString(frame.Replay)
		if frame.Kind == terminal.CutHistory {
			value["request"] = strconv.FormatUint(frame.Request, 10)
			value["effective_history_rows"] = frame.EffectiveHistoryRows
		}
	case terminal.FrameResize:
		value["columns"] = frame.Columns
		value["rows"] = frame.Rows
	case terminal.FrameHistory:
		value["request"] = strconv.FormatUint(frame.Request, 10)
		value["history_rows"] = frame.HistoryRows
	case terminal.FrameLive, terminal.FrameInput:
		value["data"] = base64.StdEncoding.EncodeToString(frame.Data)
	case terminal.FrameMode, terminal.FrameModeRequest:
		value["mode"] = frame.Mode
		if frame.Reason != "" {
			value["reason"] = frame.Reason
		}
	case terminal.FrameDefer, terminal.FrameEnd:
		value["reason"] = frame.Reason
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return nil, malformed("encode: %v", err)
	}
	if len(raw) > MaxWireBytes {
		return nil, terminal.ErrSaturated
	}
	return raw, nil
}

// EncodeCoreJSON translates the exact JSON bytes emitted by terminal.Epoch.
func EncodeCoreJSON(raw []byte, direction Direction) ([]byte, error) {
	frame, err := terminal.DecodeFrame(raw)
	if err != nil {
		return nil, err
	}
	return Encode(frame, direction)
}

// Decode validates one browser wire frame and returns the canonical typed
// representation consumed by terminal.Epoch.HandleFrame.
func Decode(raw []byte, direction Direction) (terminal.Frame, error) {
	if len(raw) == 0 || len(raw) > MaxWireBytes || !utf8.Valid(raw) {
		return terminal.Frame{}, malformed("wire payload is empty, oversized, or invalid UTF-8")
	}
	object, err := decodeObject(raw)
	if err != nil {
		return terminal.Frame{}, err
	}

	frameTypeText, err := requiredString(object, "type")
	if err != nil {
		return terminal.Frame{}, err
	}
	frameType := terminal.FrameType(frameTypeText)
	if !directionAllows(direction, frameType) {
		return terminal.Frame{}, malformed("frame %q is invalid for direction", frameType)
	}
	if err := validateKeys(object, frameType); err != nil {
		return terminal.Frame{}, err
	}

	version, err := requiredInt(object, "version")
	if err != nil {
		return terminal.Frame{}, err
	}
	source, err := requiredString(object, "source")
	if err != nil {
		return terminal.Frame{}, err
	}
	epochText, err := requiredString(object, "epoch")
	if err != nil {
		return terminal.Frame{}, err
	}
	epoch, err := parseUint64(epochText, "epoch")
	if err != nil {
		return terminal.Frame{}, err
	}
	frame := terminal.Frame{Version: version, Type: frameType, Source: source, Epoch: epoch}

	if _, ok := object["cut"]; ok {
		cutText, err := requiredString(object, "cut")
		if err != nil {
			return terminal.Frame{}, err
		}
		frame.Cut, err = parseUint64(cutText, "cut")
		if err != nil {
			return terminal.Frame{}, err
		}
	}
	if _, ok := object["request"]; ok {
		requestText, err := requiredString(object, "request")
		if err != nil {
			return terminal.Frame{}, err
		}
		frame.Request, err = parseUint64(requestText, "request")
		if err != nil {
			return terminal.Frame{}, err
		}
	}
	switch frameType {
	case terminal.FramePrepare:
		kind, err := requiredString(object, "kind")
		if err != nil {
			return terminal.Frame{}, err
		}
		frame.Kind = terminal.CutKind(kind)
		if frame.Columns, err = requiredInt(object, "columns"); err != nil {
			return terminal.Frame{}, err
		}
		if frame.Rows, err = requiredInt(object, "rows"); err != nil {
			return terminal.Frame{}, err
		}
		if err = decodeField(object, "history", &frame.History); err != nil || frame.History == nil {
			return terminal.Frame{}, malformed("history must be an array")
		}
		if err = decodeField(object, "truncated", &frame.Truncated); err != nil {
			return terminal.Frame{}, err
		}
		encoded, err := requiredString(object, "replay")
		if err != nil {
			return terminal.Frame{}, err
		}
		if frame.Replay, err = decodeBase64(encoded, "replay"); err != nil {
			return terminal.Frame{}, err
		}
		if frame.Kind == terminal.CutHistory {
			if frame.EffectiveHistoryRows, err = requiredInt(object, "effective_history_rows"); err != nil {
				return terminal.Frame{}, err
			}
		}
	case terminal.FrameResize:
		if frame.Columns, err = requiredInt(object, "columns"); err != nil {
			return terminal.Frame{}, err
		}
		if frame.Rows, err = requiredInt(object, "rows"); err != nil {
			return terminal.Frame{}, err
		}
	case terminal.FrameHistory:
		if frame.HistoryRows, err = requiredInt(object, "history_rows"); err != nil {
			return terminal.Frame{}, err
		}
	case terminal.FrameLive, terminal.FrameInput:
		encoded, err := requiredString(object, "data")
		if err != nil {
			return terminal.Frame{}, err
		}
		if frame.Data, err = decodeBase64(encoded, "data"); err != nil {
			return terminal.Frame{}, err
		}
	case terminal.FrameMode, terminal.FrameModeRequest:
		mode, err := requiredString(object, "mode")
		if err != nil {
			return terminal.Frame{}, err
		}
		frame.Mode = terminal.Mode(mode)
		if _, ok := object["reason"]; ok {
			frame.Reason, err = requiredString(object, "reason")
			if err != nil {
				return terminal.Frame{}, err
			}
		}
	case terminal.FrameDefer, terminal.FrameEnd:
		frame.Reason, err = requiredString(object, "reason")
		if err != nil {
			return terminal.Frame{}, err
		}
		if frameType == terminal.FrameDefer && !validDeferReason(frame.Reason) {
			return terminal.Frame{}, malformed("invalid DEFER reason %q", frame.Reason)
		}
	}
	if err := frame.Validate(); err != nil {
		return terminal.Frame{}, err
	}
	return frame, nil
}

func validDeferReason(reason string) bool {
	switch reason {
	case "ACTIVE_CONTACT_OR_SCROLL_GESTURE", "INTERSECTING_DOM_SELECTION", "AFFECTED_XTERM_SELECTION", "VISIBLE_ANCHOR_WOULD_BE_EVICTED":
		return true
	default:
		return false
	}
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, malformed("wire frame must be a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		// PREPARE with a history request is the largest schema (13 fields).
		// Reject excess fields before allocating an unbounded malformed index.
		if len(object) == 13 {
			return nil, malformed("too many wire fields")
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, malformed("invalid object key: %v", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, malformed("object key is not a string")
		}
		if _, duplicate := object[key]; duplicate {
			return nil, malformed("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, malformed("invalid field %q: %v", key, err)
		}
		object[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, malformed("unterminated wire object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, malformed("trailing JSON")
	}
	return object, nil
}

func validateKeys(object map[string]json.RawMessage, frameType terminal.FrameType) error {
	required := []string{"version", "type", "source", "epoch"}
	optional := []string{}
	switch frameType {
	case terminal.FramePrepare:
		required = append(required, "cut", "kind", "columns", "rows", "history", "truncated", "replay")
		kind, err := requiredString(object, "kind")
		if err != nil {
			return err
		}
		if terminal.CutKind(kind) == terminal.CutHistory {
			required = append(required, "request", "effective_history_rows")
		}
	case terminal.FrameReady, terminal.FrameCommit:
		required = append(required, "cut")
	case terminal.FrameDefer:
		required = append(required, "cut", "reason")
	case terminal.FrameLive:
		required = append(required, "cut", "data")
	case terminal.FrameModeRequest:
		required = append(required, "mode")
	case terminal.FrameMode:
		required = append(required, "mode")
		optional = append(optional, "reason")
	case terminal.FrameInput:
		required = append(required, "data")
	case terminal.FrameResize:
		required = append(required, "columns", "rows")
	case terminal.FrameHistory:
		required = append(required, "request", "history_rows")
	case terminal.FrameEnd:
		required = append(required, "reason")
	default:
		return malformed("unknown frame type %q", frameType)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range append(required, optional...) {
		allowed[key] = true
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return malformed("missing field %q", key)
		}
	}
	for key := range object {
		if !allowed[key] {
			return malformed("unexpected field %q", key)
		}
	}
	return nil
}

func decodeField(object map[string]json.RawMessage, key string, target any) error {
	if err := json.Unmarshal(object[key], target); err != nil {
		return malformed("invalid field %q: %v", key, err)
	}
	return nil
}

func requiredString(object map[string]json.RawMessage, key string) (string, error) {
	var value string
	if err := decodeField(object, key, &value); err != nil {
		return "", err
	}
	return value, nil
}

func requiredInt(object map[string]json.RawMessage, key string) (int, error) {
	var value int
	if err := decodeField(object, key, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func parseUint64(value, name string) (uint64, error) {
	if value == "" || value[0] == '0' || len(value) > 20 {
		return 0, malformed("%s is not a canonical positive uint64", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return 0, malformed("%s is not a canonical positive uint64", name)
	}
	return parsed, nil
}

func decodeBase64(value, name string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, malformed("%s is not canonical padded base64", name)
	}
	return decoded, nil
}
