package frontdoor

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"persea-terminal/internal/config"
)

const (
	diagnosticTraceBodyLimit           = 16 << 20
	diagnosticTraceWritesPerCapture    = 512
	diagnosticTraceBytesPerCapture     = 1 << 30
	diagnosticTraceCapturesPerOperator = 4
	diagnosticTraceCapturesGlobal      = 8
	diagnosticTraceFilesLimit          = 4
	diagnosticTraceDiskBytesLimit      = 64 << 20
	diagnosticTraceEventCapacity       = 4096
	diagnosticTraceInactivity          = 30 * time.Minute
	diagnosticTraceFileMode            = 0640
	diagnosticTraceSchema              = "persea-sustained-backspace-v1"
	focusScrollDiagnosticTraceSchema   = "persea-focus-scroll-v1"
	diagnosticMaxSafeInteger           = uint64(1<<53 - 1)
)

var (
	errDiagnosticCaptureNotFound  = errors.New("diagnostic capture not found")
	errDiagnosticCaptureLimit     = errors.New("diagnostic capture limit")
	errDiagnosticDiskLimit        = errors.New("diagnostic disk limit")
	errDiagnosticStoreUnavailable = errors.New("diagnostic store unavailable")
	diagnosticCaptureIDRE         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	diagnosticFilenameRE          = regexp.MustCompile(`^trace-[0-9a-f]{32}\.json$`)
	diagnosticTemporaryFilenameRE = regexp.MustCompile(`^\.trace-[0-9a-f]{32}\.tmp$`)
	diagnosticBackupFilenameRE    = regexp.MustCompile(`^\.(trace-[0-9a-f]{32}\.json)\.[0-9a-f]{32}\.bak$`)
	diagnosticIdentifierRE        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)
	focusScrollEpochRE            = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)
)

type diagnosticTraceRequest struct {
	CaptureID *string             `json:"capture_id,omitempty"`
	Trace     diagnosticTraceWire `json:"trace"`
}

type diagnosticTraceWire struct {
	Schema         string               `json:"schema"`
	Capacity       int                  `json:"capacity"`
	RetainedEvents int                  `json:"retainedEvents"`
	DroppedEvents  uint64               `json:"droppedEvents"`
	Experiment     diagnosticExperiment `json:"experiment"`
	Events         []json.RawMessage    `json:"events"`
}

type validatedDiagnosticTrace struct {
	Schema         string               `json:"schema"`
	Capacity       int                  `json:"capacity"`
	RetainedEvents int                  `json:"retainedEvents"`
	DroppedEvents  uint64               `json:"droppedEvents"`
	Experiment     diagnosticExperiment `json:"experiment"`
	Events         []any                `json:"events"`
}

type validatedTrace struct {
	RetainedEvents int
	value          any
}

func (t validatedTrace) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.value)
}

type focusScrollDiagnosticTraceWire struct {
	Schema         string            `json:"schema"`
	Capacity       int               `json:"capacity"`
	RetainedEvents int               `json:"retainedEvents"`
	DroppedEvents  uint64            `json:"droppedEvents"`
	Enabled        bool              `json:"enabled"`
	Events         []json.RawMessage `json:"events"`
}

type validatedFocusScrollDiagnosticTrace struct {
	Schema         string                       `json:"schema"`
	Capacity       int                          `json:"capacity"`
	RetainedEvents int                          `json:"retainedEvents"`
	DroppedEvents  uint64                       `json:"droppedEvents"`
	Enabled        bool                         `json:"enabled"`
	Events         []focusScrollDiagnosticEvent `json:"events"`
}

type focusScrollDiagnosticRect struct {
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type focusScrollDiagnosticScrollGeometry struct {
	ScrollTop            float64 `json:"scrollTop"`
	ScrollHeight         float64 `json:"scrollHeight"`
	ClientHeight         float64 `json:"clientHeight"`
	MaxScrollTop         float64 `json:"maxScrollTop"`
	DistanceFromLiveEdge float64 `json:"distanceFromLiveEdge"`
	IsFollowingLive      bool    `json:"isFollowingLive"`
}

type focusScrollDiagnosticVisualViewport struct {
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
	OffsetLeft float64 `json:"offsetLeft"`
	OffsetTop  float64 `json:"offsetTop"`
	PageLeft   float64 `json:"pageLeft"`
	PageTop    float64 `json:"pageTop"`
	Scale      float64 `json:"scale"`
}

type focusScrollDiagnosticViewport struct {
	InnerHeight    float64                              `json:"innerHeight"`
	VisualViewport *focusScrollDiagnosticVisualViewport `json:"visualViewport"`
	VisibleTop     float64                              `json:"visibleTop"`
	VisibleBottom  float64                              `json:"visibleBottom"`
}

type focusScrollDiagnosticScroller struct {
	focusScrollDiagnosticScrollGeometry
	Rect focusScrollDiagnosticRect `json:"rect"`
}

type focusScrollDiagnosticTextarea struct {
	Rect           *focusScrollDiagnosticRect `json:"rect"`
	ScrollTop      float64                    `json:"scrollTop"`
	ScrollHeight   float64                    `json:"scrollHeight"`
	ClientHeight   float64                    `json:"clientHeight"`
	ValueLength    uint64                     `json:"valueLength"`
	SelectionStart *uint64                    `json:"selectionStart"`
	SelectionEnd   *uint64                    `json:"selectionEnd"`
}

type focusScrollDiagnosticComposer struct {
	Open      bool                          `json:"open"`
	Size      string                        `json:"size"`
	PanelRect *focusScrollDiagnosticRect    `json:"panelRect"`
	Textarea  focusScrollDiagnosticTextarea `json:"textarea"`
}

type focusScrollDiagnosticKeybar struct {
	Hidden          bool                       `json:"hidden"`
	Collapsed       bool                       `json:"collapsed"`
	OverflowVisible bool                       `json:"overflowVisible"`
	Rect            *focusScrollDiagnosticRect `json:"rect"`
}

type focusScrollDiagnosticOverlay struct {
	Composer *focusScrollDiagnosticComposer `json:"composer"`
	Keybar   focusScrollDiagnosticKeybar    `json:"keybar"`
	DockRect *focusScrollDiagnosticRect     `json:"dockRect"`
}

type focusScrollDiagnosticInset struct {
	MeasuredComposerHeight  float64 `json:"measuredComposerHeight"`
	PublishedInsetPx        float64 `json:"publishedInsetPx"`
	InsetBudgetPx           float64 `json:"insetBudgetPx"`
	CSSPublishedInsetPx     float64 `json:"cssPublishedInsetPx"`
	PublicationCount        uint64  `json:"publicationCount"`
	PublicationFramePending bool    `json:"publicationFramePending"`
}

type focusScrollDiagnosticOwnership struct {
	FollowLiveIntent                   *bool   `json:"followLiveIntent"`
	PendingKeyboardReveal              *bool   `json:"pendingKeyboardReveal"`
	PendingOverlayReveal               *bool   `json:"pendingOverlayReveal"`
	KeyboardOpen                       bool    `json:"keyboardOpen"`
	AttachmentGeneration               uint64  `json:"attachmentGeneration"`
	ProtocolEpoch                      *string `json:"protocolEpoch"`
	PresentedAggregateID               *uint64 `json:"presentedAggregateId"`
	PresentedAggregateEpoch            *string `json:"presentedAggregateEpoch"`
	SurfaceInputEpoch                  *uint64 `json:"surfaceInputEpoch"`
	QuiescenceEpoch                    *uint64 `json:"quiescenceEpoch"`
	OverlayInsetEpoch                  *uint64 `json:"overlayInsetEpoch"`
	ViewportEventEpoch                 uint64  `json:"viewportEventEpoch"`
	JournalSequence                    *uint64 `json:"journalSequence"`
	DisplayPreparationGeneration       *uint64 `json:"displayPreparationGeneration"`
	DisplayPreparationPublicationCount *uint64 `json:"displayPreparationPublicationCount"`
}

type focusScrollDiagnosticFocusTextarea struct {
	ValueLength    uint64  `json:"valueLength"`
	SelectionStart *uint64 `json:"selectionStart"`
	SelectionEnd   *uint64 `json:"selectionEnd"`
}

type focusScrollDiagnosticFocus struct {
	Role             string                              `json:"role"`
	DocumentHasFocus bool                                `json:"documentHasFocus"`
	FocusWithinRoot  bool                                `json:"focusWithinRoot"`
	Textarea         *focusScrollDiagnosticFocusTextarea `json:"textarea"`
}

type focusScrollDiagnosticCaret struct {
	TerminalCursorRect                          *focusScrollDiagnosticRect `json:"terminalCursorRect"`
	TerminalCaretDistanceToScrollerBottom       *float64                   `json:"terminalCaretDistanceToScrollerBottom"`
	TerminalCaretDistanceToVisualViewportBottom *float64                   `json:"terminalCaretDistanceToVisualViewportBottom"`
	TerminalCaretWithinScroller                 *bool                      `json:"terminalCaretWithinScroller"`
	TerminalCaretWithinVisualViewport           *bool                      `json:"terminalCaretWithinVisualViewport"`
}

type focusScrollDiagnosticSnapshot struct {
	Viewport  focusScrollDiagnosticViewport  `json:"viewport"`
	Scroller  *focusScrollDiagnosticScroller `json:"scroller"`
	Overlay   focusScrollDiagnosticOverlay   `json:"overlay"`
	Inset     focusScrollDiagnosticInset     `json:"inset"`
	Ownership focusScrollDiagnosticOwnership `json:"ownership"`
	Focus     focusScrollDiagnosticFocus     `json:"focus"`
	Caret     focusScrollDiagnosticCaret     `json:"caret"`
}

type focusScrollDiagnosticKeyboardState struct {
	PreviousOpen bool `json:"previousOpen"`
	CurrentOpen  bool `json:"currentOpen"`
}

type focusScrollDiagnosticReveal struct {
	RequestID uint64                              `json:"requestId"`
	Channel   string                              `json:"channel"`
	Trigger   string                              `json:"trigger"`
	Outcome   *string                             `json:"outcome"`
	Reasons   []string                            `json:"reasons"`
	Before    focusScrollDiagnosticScrollGeometry `json:"before"`
	After     focusScrollDiagnosticScrollGeometry `json:"after"`
}

type focusScrollDiagnosticScrollWrite struct {
	Reason             string                              `json:"reason"`
	RequestedScrollTop float64                             `json:"requestedScrollTop"`
	Before             focusScrollDiagnosticScrollGeometry `json:"before"`
	After              focusScrollDiagnosticScrollGeometry `json:"after"`
}

type focusScrollDiagnosticEvent struct {
	Ordinal          uint64                              `json:"ordinal"`
	MonotonicMS      float64                             `json:"monotonicMs"`
	Kind             string                              `json:"kind"`
	FocusTarget      *string                             `json:"focusTarget"`
	ActivationSource *string                             `json:"activationSource"`
	ActivationTarget *string                             `json:"activationTarget"`
	KeyboardState    *focusScrollDiagnosticKeyboardState `json:"keyboardState"`
	Reveal           *focusScrollDiagnosticReveal        `json:"reveal"`
	ScrollWrite      *focusScrollDiagnosticScrollWrite   `json:"scrollWrite"`
	Snapshot         focusScrollDiagnosticSnapshot       `json:"snapshot"`
}

type diagnosticExperiment struct {
	Enabled              bool                           `json:"enabled"`
	ComparisonInputTypes diagnosticComparisonInputTypes `json:"comparisonInputTypes"`
}

type diagnosticComparisonInputTypes struct {
	Terminal            []string `json:"terminal"`
	WebControlAllowed   []string `json:"web-control-allowed"`
	WebControlPrevented []string `json:"web-control-prevented"`
}

type diagnosticTarget struct {
	TargetIsTextarea bool   `json:"targetIsTextarea"`
	ValueLength      *int64 `json:"valueLength"`
	SelectionStart   *int64 `json:"selectionStart"`
	SelectionEnd     *int64 `json:"selectionEnd"`
}

type diagnosticVisualViewport struct {
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
	OffsetLeft float64 `json:"offsetLeft"`
	OffsetTop  float64 `json:"offsetTop"`
	Scale      float64 `json:"scale"`
}

type diagnosticFocus struct {
	DocumentHasFocus bool                      `json:"documentHasFocus"`
	FocusWithinRoot  bool                      `json:"focusWithinRoot"`
	ActiveIsTextarea bool                      `json:"activeIsTextarea"`
	VisualViewport   *diagnosticVisualViewport `json:"visualViewport"`
}

type diagnosticCommon struct {
	MonotonicMS       float64          `json:"monotonicMs"`
	CadenceMS         *float64         `json:"cadenceMs"`
	HoldElapsedMS     *float64         `json:"holdElapsedMs"`
	ExperimentEnabled bool             `json:"experimentEnabled"`
	Target            diagnosticTarget `json:"target"`
	Focus             diagnosticFocus  `json:"focus"`
}

type diagnosticReaderPaths struct {
	Reuse        uint64 `json:"reuse"`
	Rebuild      uint64 `json:"rebuild"`
	ReusePending bool   `json:"reusePending"`
}

type diagnosticSample struct {
	OngoingCommittedReaderPaths diagnosticReaderPaths `json:"ongoingCommittedReaderPaths"`
	InputFrames                 uint64                `json:"inputFrames"`
	InputBytes                  uint64                `json:"inputBytes"`
	InboxFrames                 uint64                `json:"inboxFrames"`
	InboxWeight                 uint64                `json:"inboxWeight"`
}

type diagnosticTargetRange struct {
	StartOffset int64 `json:"startOffset"`
	EndOffset   int64 `json:"endOffset"`
}

type diagnosticKeyEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	diagnosticCommon
	Key                       string `json:"key"`
	Code                      string `json:"code"`
	Repeat                    bool   `json:"repeat"`
	IsComposing               bool   `json:"isComposing"`
	Cancelable                bool   `json:"cancelable"`
	DefaultPreventedAtCapture bool   `json:"defaultPreventedAtCapture"`
	DiagnosticAction          string `json:"diagnosticAction"`
	BackspaceKeydownCount     uint64 `json:"backspaceKeydownCount"`
}

type diagnosticInputEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	diagnosticCommon
	InputType                 string                  `json:"inputType"`
	IsComposing               bool                    `json:"isComposing"`
	DataLength                *int64                  `json:"dataLength"`
	TargetRanges              []diagnosticTargetRange `json:"targetRanges"`
	Cancelable                bool                    `json:"cancelable"`
	DefaultPreventedAtCapture bool                    `json:"defaultPreventedAtCapture"`
	DiagnosticAction          string                  `json:"diagnosticAction"`
	Sample                    *diagnosticSample       `json:"sample"`
}

type diagnosticCompositionEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	diagnosticCommon
	DataLength int64 `json:"dataLength"`
}

type diagnosticFocusEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	diagnosticCommon
}

type diagnosticViewportEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	diagnosticCommon
}

type diagnosticPostInputSnapshotEvent struct {
	Ordinal      uint64            `json:"ordinal"`
	Kind         string            `json:"kind"`
	MonotonicMS  float64           `json:"monotonicMs"`
	EventOrdinal uint64            `json:"eventOrdinal"`
	Sample       *diagnosticSample `json:"sample"`
}

type diagnosticExperimentToggleEvent struct {
	Ordinal           uint64  `json:"ordinal"`
	Kind              string  `json:"kind"`
	Source            *string `json:"source"`
	MonotonicMS       float64 `json:"monotonicMs"`
	ExperimentEnabled bool    `json:"experimentEnabled"`
}

type diagnosticTerminalDataEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	diagnosticCommon
	ByteLength uint64  `json:"byteLength"`
	ControlHex *string `json:"controlHex"`
}

type diagnosticEventHeader struct {
	Ordinal uint64 `json:"ordinal"`
	Kind    string `json:"kind"`
}

func rejectDiagnosticDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make([]string, 0)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				for _, prior := range seen {
					if strings.EqualFold(prior, key) {
						return errors.New("duplicate object key")
					}
				}
				seen = append(seen, key)
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func strictDiagnosticJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func exactDiagnosticKeys(raw []byte, expected ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if len(object) != len(expected) {
		return errors.New("unexpected diagnostic object shape")
	}
	for _, key := range expected {
		if _, ok := object[key]; !ok {
			return errors.New("unexpected diagnostic object shape")
		}
	}
	return nil
}

func finiteDiagnosticNumber(value float64, nonnegative bool) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e12 {
		return false
	}
	return !nonnegative || value >= 0
}

func safeDiagnosticInteger(value uint64) bool {
	return value <= diagnosticMaxSafeInteger
}

func validateDiagnosticTarget(target diagnosticTarget) error {
	if target.TargetIsTextarea {
		if target.ValueLength == nil || target.SelectionStart == nil || target.SelectionEnd == nil ||
			*target.ValueLength < 0 || *target.SelectionStart < 0 || *target.SelectionEnd < *target.SelectionStart ||
			*target.SelectionEnd > *target.ValueLength ||
			uint64(*target.ValueLength) > diagnosticMaxSafeInteger ||
			uint64(*target.SelectionStart) > diagnosticMaxSafeInteger ||
			uint64(*target.SelectionEnd) > diagnosticMaxSafeInteger {
			return errors.New("invalid diagnostic textarea state")
		}
		return nil
	}
	if target.ValueLength != nil || target.SelectionStart != nil || target.SelectionEnd != nil {
		return errors.New("invalid non-textarea diagnostic state")
	}
	return nil
}

func validateDiagnosticSample(sample *diagnosticSample) error {
	if sample == nil {
		return nil
	}
	values := []uint64{
		sample.OngoingCommittedReaderPaths.Reuse,
		sample.OngoingCommittedReaderPaths.Rebuild,
		sample.InputFrames,
		sample.InputBytes,
		sample.InboxFrames,
		sample.InboxWeight,
	}
	for _, value := range values {
		if !safeDiagnosticInteger(value) {
			return errors.New("diagnostic sample counter is out of range")
		}
	}
	return nil
}

func validateDiagnosticSampleShape(raw json.RawMessage, sample *diagnosticSample) error {
	if sample == nil {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid null diagnostic sample")
		}
		return nil
	}
	if err := exactDiagnosticKeys(raw, "ongoingCommittedReaderPaths", "inputFrames", "inputBytes", "inboxFrames", "inboxWeight"); err != nil {
		return err
	}
	paths, err := nestedDiagnosticRaw(raw, "ongoingCommittedReaderPaths")
	if err != nil {
		return err
	}
	if err := exactDiagnosticKeys(paths, "reuse", "rebuild", "reusePending"); err != nil {
		return err
	}
	return validateDiagnosticSample(sample)
}

func validateDiagnosticCommon(raw map[string]json.RawMessage, common diagnosticCommon) error {
	if !finiteDiagnosticNumber(common.MonotonicMS, true) {
		return errors.New("invalid diagnostic monotonic time")
	}
	for _, value := range []*float64{common.CadenceMS, common.HoldElapsedMS} {
		if value != nil && !finiteDiagnosticNumber(*value, true) {
			return errors.New("invalid diagnostic cadence")
		}
	}
	if err := exactDiagnosticKeys(raw["target"], "targetIsTextarea", "valueLength", "selectionStart", "selectionEnd"); err != nil {
		return err
	}
	if err := validateDiagnosticTarget(common.Target); err != nil {
		return err
	}
	if err := exactDiagnosticKeys(raw["focus"], "documentHasFocus", "focusWithinRoot", "activeIsTextarea", "visualViewport"); err != nil {
		return err
	}
	if common.Focus.VisualViewport != nil {
		if err := exactDiagnosticKeys(raw["focus"], "documentHasFocus", "focusWithinRoot", "activeIsTextarea", "visualViewport"); err != nil {
			return err
		}
		var focus map[string]json.RawMessage
		if err := json.Unmarshal(raw["focus"], &focus); err != nil {
			return err
		}
		if err := exactDiagnosticKeys(focus["visualViewport"], "width", "height", "offsetLeft", "offsetTop", "scale"); err != nil {
			return err
		}
		viewport := common.Focus.VisualViewport
		if !finiteDiagnosticNumber(viewport.Width, true) || !finiteDiagnosticNumber(viewport.Height, true) ||
			!finiteDiagnosticNumber(viewport.OffsetLeft, false) || !finiteDiagnosticNumber(viewport.OffsetTop, false) ||
			!finiteDiagnosticNumber(viewport.Scale, true) || viewport.Scale == 0 {
			return errors.New("invalid visual viewport")
		}
	}
	return nil
}

func diagnosticSource(value string) bool {
	return value == "terminal" || value == "web-control-allowed" || value == "web-control-prevented"
}

func diagnosticAction(value string) bool {
	return value == "observe-only" || value == "allow-default-await-beforeinput" || value == "prevent-default"
}

func validateDiagnosticInputType(value string) bool {
	// InputEvent.inputType is specified as a string and WebKit uses the empty
	// string when it has no more specific edit classification. Empty conveys no
	// operator content; every non-empty value must remain an identifier.
	return value == "" || diagnosticIdentifierRE.MatchString(value)
}

func decodeDiagnosticEvent(raw json.RawMessage) (any, uint64, error) {
	var header diagnosticEventHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, 0, err
	}
	if header.Ordinal == 0 || !safeDiagnosticInteger(header.Ordinal) {
		return nil, 0, errors.New("invalid diagnostic ordinal")
	}
	var rawObject map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawObject); err != nil {
		return nil, 0, err
	}
	commonKeys := []string{"ordinal", "kind", "source", "monotonicMs", "cadenceMs", "holdElapsedMs", "experimentEnabled", "target", "focus"}
	switch header.Kind {
	case "keydown", "keyup":
		expected := append(append([]string{}, commonKeys...), "key", "code", "repeat", "isComposing", "cancelable", "defaultPreventedAtCapture", "diagnosticAction", "backspaceKeydownCount")
		if err := exactDiagnosticKeys(raw, expected...); err != nil {
			return nil, 0, err
		}
		var event diagnosticKeyEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if !diagnosticSource(event.Source) || event.Key != "Backspace" || (event.Code != "" && event.Code != "Backspace") ||
			!diagnosticAction(event.DiagnosticAction) || !safeDiagnosticInteger(event.BackspaceKeydownCount) {
			return nil, 0, errors.New("invalid diagnostic key event")
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "beforeinput", "input":
		expected := append(append([]string{}, commonKeys...), "inputType", "isComposing", "dataLength", "targetRanges", "cancelable", "defaultPreventedAtCapture", "diagnosticAction", "sample")
		if err := exactDiagnosticKeys(raw, expected...); err != nil {
			return nil, 0, err
		}
		var event diagnosticInputEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if !diagnosticSource(event.Source) || !validateDiagnosticInputType(event.InputType) || !diagnosticAction(event.DiagnosticAction) ||
			(event.DataLength != nil && (*event.DataLength < 0 || uint64(*event.DataLength) > diagnosticMaxSafeInteger)) ||
			len(event.TargetRanges) > 32 {
			return nil, 0, errors.New("invalid diagnostic input event")
		}
		for index, targetRange := range event.TargetRanges {
			if targetRange.StartOffset < 0 || targetRange.EndOffset < targetRange.StartOffset ||
				uint64(targetRange.StartOffset) > diagnosticMaxSafeInteger ||
				uint64(targetRange.EndOffset) > diagnosticMaxSafeInteger {
				return nil, 0, errors.New("invalid diagnostic target range")
			}
			var ranges []json.RawMessage
			if err := json.Unmarshal(rawObject["targetRanges"], &ranges); err != nil || index >= len(ranges) ||
				exactDiagnosticKeys(ranges[index], "startOffset", "endOffset") != nil {
				return nil, 0, errors.New("invalid diagnostic target range shape")
			}
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		if err := validateDiagnosticSampleShape(rawObject["sample"], event.Sample); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "compositionstart", "compositionupdate", "compositionend":
		expected := append(append([]string{}, commonKeys...), "dataLength")
		if err := exactDiagnosticKeys(raw, expected...); err != nil {
			return nil, 0, err
		}
		var event diagnosticCompositionEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if !diagnosticSource(event.Source) || event.DataLength < 0 || uint64(event.DataLength) > diagnosticMaxSafeInteger {
			return nil, 0, errors.New("invalid diagnostic composition event")
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "focusin", "focusout":
		if err := exactDiagnosticKeys(raw, commonKeys...); err != nil {
			return nil, 0, err
		}
		var event diagnosticFocusEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if !diagnosticSource(event.Source) {
			return nil, 0, errors.New("invalid diagnostic focus event")
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "visual-viewport-resize", "visual-viewport-scroll":
		expected := []string{"ordinal", "kind", "monotonicMs", "cadenceMs", "holdElapsedMs", "experimentEnabled", "target", "focus"}
		if err := exactDiagnosticKeys(raw, expected...); err != nil {
			return nil, 0, err
		}
		var event diagnosticViewportEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "post-input-snapshot":
		if err := exactDiagnosticKeys(raw, "ordinal", "kind", "monotonicMs", "eventOrdinal", "sample"); err != nil {
			return nil, 0, err
		}
		var event diagnosticPostInputSnapshotEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if !finiteDiagnosticNumber(event.MonotonicMS, true) || event.EventOrdinal == 0 ||
			event.EventOrdinal >= event.Ordinal || !safeDiagnosticInteger(event.EventOrdinal) {
			return nil, 0, errors.New("invalid post-input snapshot")
		}
		if err := validateDiagnosticSampleShape(rawObject["sample"], event.Sample); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	case "experiment-toggle":
		if err := exactDiagnosticKeys(raw, "ordinal", "kind", "source", "monotonicMs", "experimentEnabled"); err != nil {
			return nil, 0, err
		}
		var event diagnosticExperimentToggleEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if event.Source != nil || !finiteDiagnosticNumber(event.MonotonicMS, true) {
			return nil, 0, errors.New("invalid experiment toggle")
		}
		return event, event.Ordinal, nil
	case "terminal-on-data":
		expected := append(append([]string{}, commonKeys...), "byteLength", "controlHex")
		if err := exactDiagnosticKeys(raw, expected...); err != nil {
			return nil, 0, err
		}
		var event diagnosticTerminalDataEvent
		if err := strictDiagnosticJSON(raw, &event); err != nil {
			return nil, 0, err
		}
		if event.Source != "terminal" || !safeDiagnosticInteger(event.ByteLength) {
			return nil, 0, errors.New("invalid terminal emission")
		}
		if event.ControlHex != nil {
			decoded, err := hex.DecodeString(*event.ControlHex)
			if err != nil || len(decoded) == 0 || uint64(len(decoded)) > event.ByteLength {
				return nil, 0, errors.New("invalid terminal control bytes")
			}
			for _, value := range decoded {
				if value >= 0x20 && value != 0x7f {
					return nil, 0, errors.New("printable terminal content is forbidden")
				}
			}
		}
		if err := validateDiagnosticCommon(rawObject, event.diagnosticCommon); err != nil {
			return nil, 0, err
		}
		return event, event.Ordinal, nil
	default:
		return nil, 0, errors.New("unknown diagnostic event kind")
	}
}

func focusScrollRawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func focusScrollFinite(value float64, nonnegative bool) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && (!nonnegative || value >= 0)
}

func focusScrollSafe(values ...uint64) bool {
	for _, value := range values {
		if !safeDiagnosticInteger(value) {
			return false
		}
	}
	return true
}

func validateFocusScrollRect(raw json.RawMessage, value *focusScrollDiagnosticRect) error {
	if value == nil {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid focus-scroll null rect")
		}
		return nil
	}
	if err := exactDiagnosticKeys(raw, "top", "right", "bottom", "left", "width", "height"); err != nil {
		return err
	}
	if !focusScrollFinite(value.Top, false) || !focusScrollFinite(value.Right, false) ||
		!focusScrollFinite(value.Bottom, false) || !focusScrollFinite(value.Left, false) ||
		!focusScrollFinite(value.Width, true) || !focusScrollFinite(value.Height, true) {
		return errors.New("invalid focus-scroll rect")
	}
	return nil
}

func validateFocusScrollGeometry(raw json.RawMessage, value focusScrollDiagnosticScrollGeometry) error {
	if err := exactDiagnosticKeys(raw, "scrollTop", "scrollHeight", "clientHeight", "maxScrollTop", "distanceFromLiveEdge", "isFollowingLive"); err != nil {
		return err
	}
	return validateFocusScrollGeometryValues(value)
}

func validateFocusScrollGeometryValues(value focusScrollDiagnosticScrollGeometry) error {
	if !focusScrollFinite(value.ScrollTop, false) || !focusScrollFinite(value.ScrollHeight, true) ||
		!focusScrollFinite(value.ClientHeight, true) || !focusScrollFinite(value.MaxScrollTop, true) ||
		!focusScrollFinite(value.DistanceFromLiveEdge, false) {
		return errors.New("invalid focus-scroll geometry")
	}
	return nil
}

func validateFocusScrollEpoch(value *string) error {
	if value == nil {
		return nil
	}
	if !focusScrollEpochRE.MatchString(*value) {
		return errors.New("invalid focus-scroll epoch")
	}
	if _, err := strconv.ParseUint(*value, 10, 64); err != nil {
		return errors.New("invalid focus-scroll epoch")
	}
	return nil
}

func validateFocusScrollSelection(valueLength uint64, start, end *uint64) error {
	if !focusScrollSafe(valueLength) || (start == nil) != (end == nil) {
		return errors.New("invalid focus-scroll selection")
	}
	if start != nil && (!focusScrollSafe(*start, *end) || *start > *end || *end > valueLength) {
		return errors.New("invalid focus-scroll selection")
	}
	return nil
}

func validateFocusScrollSnapshot(raw json.RawMessage, value focusScrollDiagnosticSnapshot) error {
	if err := exactDiagnosticKeys(raw, "viewport", "scroller", "overlay", "inset", "ownership", "focus", "caret"); err != nil {
		return err
	}
	object, err := focusScrollRawObject(raw)
	if err != nil {
		return err
	}
	if err := exactDiagnosticKeys(object["viewport"], "innerHeight", "visualViewport", "visibleTop", "visibleBottom"); err != nil {
		return err
	}
	if !focusScrollFinite(value.Viewport.InnerHeight, true) ||
		!focusScrollFinite(value.Viewport.VisibleTop, false) ||
		!focusScrollFinite(value.Viewport.VisibleBottom, false) ||
		value.Viewport.VisibleBottom < value.Viewport.VisibleTop {
		return errors.New("invalid focus-scroll viewport")
	}
	if value.Viewport.VisualViewport == nil {
		if !bytes.Equal(bytes.TrimSpace(mustDiagnosticRaw(object["viewport"], "visualViewport")), []byte("null")) ||
			value.Viewport.VisibleTop != 0 || value.Viewport.VisibleBottom != value.Viewport.InnerHeight {
			return errors.New("invalid focus-scroll layout viewport")
		}
	} else {
		rawViewport := mustDiagnosticRaw(object["viewport"], "visualViewport")
		if err := exactDiagnosticKeys(rawViewport, "width", "height", "offsetLeft", "offsetTop", "pageLeft", "pageTop", "scale"); err != nil {
			return err
		}
		viewport := value.Viewport.VisualViewport
		if !focusScrollFinite(viewport.Width, true) || !focusScrollFinite(viewport.Height, true) ||
			!focusScrollFinite(viewport.OffsetLeft, false) || !focusScrollFinite(viewport.OffsetTop, false) ||
			!focusScrollFinite(viewport.PageLeft, false) || !focusScrollFinite(viewport.PageTop, false) ||
			!focusScrollFinite(viewport.Scale, true) || viewport.Scale == 0 ||
			value.Viewport.VisibleTop != viewport.OffsetTop ||
			value.Viewport.VisibleBottom != viewport.OffsetTop+viewport.Height {
			return errors.New("invalid focus-scroll visual viewport")
		}
	}

	rawScroller := object["scroller"]
	if value.Scroller == nil {
		if !bytes.Equal(bytes.TrimSpace(rawScroller), []byte("null")) {
			return errors.New("invalid focus-scroll null scroller")
		}
	} else {
		if err := exactDiagnosticKeys(rawScroller, "scrollTop", "scrollHeight", "clientHeight", "maxScrollTop", "distanceFromLiveEdge", "rect", "isFollowingLive"); err != nil {
			return err
		}
		if err := validateFocusScrollGeometryValues(value.Scroller.focusScrollDiagnosticScrollGeometry); err != nil {
			return err
		}
		if err := validateFocusScrollRect(mustDiagnosticRaw(rawScroller, "rect"), &value.Scroller.Rect); err != nil {
			return err
		}
	}

	if err := exactDiagnosticKeys(object["overlay"], "composer", "keybar", "dockRect"); err != nil {
		return err
	}
	rawOverlay, err := focusScrollRawObject(object["overlay"])
	if err != nil {
		return err
	}
	if value.Overlay.Composer == nil {
		if !bytes.Equal(bytes.TrimSpace(rawOverlay["composer"]), []byte("null")) {
			return errors.New("invalid focus-scroll null composer")
		}
	} else {
		rawComposer := rawOverlay["composer"]
		if err := exactDiagnosticKeys(rawComposer, "open", "size", "panelRect", "textarea"); err != nil {
			return err
		}
		if value.Overlay.Composer.Size != "compact" && value.Overlay.Composer.Size != "expanded" {
			return errors.New("invalid focus-scroll composer size")
		}
		if err := validateFocusScrollRect(mustDiagnosticRaw(rawComposer, "panelRect"), value.Overlay.Composer.PanelRect); err != nil {
			return err
		}
		rawTextarea := mustDiagnosticRaw(rawComposer, "textarea")
		if err := exactDiagnosticKeys(rawTextarea, "rect", "scrollTop", "scrollHeight", "clientHeight", "valueLength", "selectionStart", "selectionEnd"); err != nil {
			return err
		}
		textarea := value.Overlay.Composer.Textarea
		if err := validateFocusScrollRect(mustDiagnosticRaw(rawTextarea, "rect"), textarea.Rect); err != nil {
			return err
		}
		if !focusScrollFinite(textarea.ScrollTop, false) || !focusScrollFinite(textarea.ScrollHeight, true) ||
			!focusScrollFinite(textarea.ClientHeight, true) ||
			validateFocusScrollSelection(textarea.ValueLength, textarea.SelectionStart, textarea.SelectionEnd) != nil {
			return errors.New("invalid focus-scroll composer textarea")
		}
	}
	if err := exactDiagnosticKeys(rawOverlay["keybar"], "hidden", "collapsed", "overflowVisible", "rect"); err != nil {
		return err
	}
	if err := validateFocusScrollRect(mustDiagnosticRaw(rawOverlay["keybar"], "rect"), value.Overlay.Keybar.Rect); err != nil {
		return err
	}
	if err := validateFocusScrollRect(rawOverlay["dockRect"], value.Overlay.DockRect); err != nil {
		return err
	}

	if err := exactDiagnosticKeys(object["inset"], "measuredComposerHeight", "publishedInsetPx", "insetBudgetPx", "cssPublishedInsetPx", "publicationCount", "publicationFramePending"); err != nil {
		return err
	}
	inset := value.Inset
	if !focusScrollFinite(inset.MeasuredComposerHeight, true) || !focusScrollFinite(inset.PublishedInsetPx, true) ||
		!focusScrollFinite(inset.InsetBudgetPx, true) || !focusScrollFinite(inset.CSSPublishedInsetPx, true) ||
		!focusScrollSafe(inset.PublicationCount) {
		return errors.New("invalid focus-scroll inset")
	}

	if err := exactDiagnosticKeys(object["ownership"],
		"followLiveIntent", "pendingKeyboardReveal", "pendingOverlayReveal", "keyboardOpen",
		"attachmentGeneration", "protocolEpoch", "presentedAggregateId", "presentedAggregateEpoch",
		"surfaceInputEpoch", "quiescenceEpoch", "overlayInsetEpoch", "viewportEventEpoch",
		"journalSequence", "displayPreparationGeneration", "displayPreparationPublicationCount"); err != nil {
		return err
	}
	ownership := value.Ownership
	if !focusScrollSafe(ownership.AttachmentGeneration, ownership.ViewportEventEpoch) ||
		validateFocusScrollEpoch(ownership.ProtocolEpoch) != nil ||
		validateFocusScrollEpoch(ownership.PresentedAggregateEpoch) != nil {
		return errors.New("invalid focus-scroll ownership")
	}
	for _, counter := range []*uint64{
		ownership.PresentedAggregateID, ownership.SurfaceInputEpoch, ownership.QuiescenceEpoch,
		ownership.OverlayInsetEpoch, ownership.JournalSequence, ownership.DisplayPreparationGeneration,
		ownership.DisplayPreparationPublicationCount,
	} {
		if counter != nil && !focusScrollSafe(*counter) {
			return errors.New("invalid focus-scroll ownership counter")
		}
	}
	surfaceAbsent := value.Scroller == nil
	for _, state := range []*bool{ownership.FollowLiveIntent, ownership.PendingKeyboardReveal, ownership.PendingOverlayReveal} {
		if surfaceAbsent != (state == nil) {
			return errors.New("inconsistent focus-scroll surface ownership")
		}
	}

	if err := exactDiagnosticKeys(object["focus"], "role", "documentHasFocus", "focusWithinRoot", "textarea"); err != nil {
		return err
	}
	if !slices.Contains([]string{"none", "terminal-helper", "composer", "diagnostic-control", "other-within-attachment", "other"}, value.Focus.Role) {
		return errors.New("invalid focus-scroll focus role")
	}
	rawFocusTextarea := mustDiagnosticRaw(object["focus"], "textarea")
	if value.Focus.Textarea == nil {
		if !bytes.Equal(bytes.TrimSpace(rawFocusTextarea), []byte("null")) {
			return errors.New("invalid focus-scroll null focus textarea")
		}
	} else {
		if err := exactDiagnosticKeys(rawFocusTextarea, "valueLength", "selectionStart", "selectionEnd"); err != nil {
			return err
		}
		if err := validateFocusScrollSelection(value.Focus.Textarea.ValueLength, value.Focus.Textarea.SelectionStart, value.Focus.Textarea.SelectionEnd); err != nil {
			return err
		}
	}

	if err := exactDiagnosticKeys(object["caret"], "terminalCursorRect", "terminalCaretDistanceToScrollerBottom", "terminalCaretDistanceToVisualViewportBottom", "terminalCaretWithinScroller", "terminalCaretWithinVisualViewport"); err != nil {
		return err
	}
	caret := value.Caret
	nullCaret := caret.TerminalCursorRect == nil
	if nullCaret != (caret.TerminalCaretDistanceToScrollerBottom == nil) ||
		nullCaret != (caret.TerminalCaretDistanceToVisualViewportBottom == nil) ||
		nullCaret != (caret.TerminalCaretWithinScroller == nil) ||
		nullCaret != (caret.TerminalCaretWithinVisualViewport == nil) {
		return errors.New("inconsistent focus-scroll caret")
	}
	if err := validateFocusScrollRect(mustDiagnosticRaw(object["caret"], "terminalCursorRect"), caret.TerminalCursorRect); err != nil {
		return err
	}
	if !nullCaret && (!focusScrollFinite(*caret.TerminalCaretDistanceToScrollerBottom, false) ||
		!focusScrollFinite(*caret.TerminalCaretDistanceToVisualViewportBottom, false)) {
		return errors.New("invalid focus-scroll caret distance")
	}
	return nil
}

func mustDiagnosticRaw(raw json.RawMessage, key string) json.RawMessage {
	object, _ := focusScrollRawObject(raw)
	return object[key]
}

func validateFocusScrollReveal(raw json.RawMessage, value *focusScrollDiagnosticReveal, kind string) error {
	if value == nil {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid focus-scroll null reveal")
		}
		return nil
	}
	if err := exactDiagnosticKeys(raw, "requestId", "channel", "trigger", "outcome", "reasons", "before", "after"); err != nil {
		return err
	}
	if !focusScrollSafe(value.RequestID) || value.RequestID == 0 ||
		!slices.Contains([]string{"keyboard", "overlay"}, value.Channel) ||
		!slices.Contains([]string{"keyboard-state-edge", "viewport-height-settle", "keybar-expand", "composer-inset-publish", "composer-focus", "pending-release"}, value.Trigger) {
		return errors.New("invalid focus-scroll reveal identity")
	}
	if err := validateFocusScrollGeometry(mustDiagnosticRaw(raw, "before"), value.Before); err != nil {
		return err
	}
	if err := validateFocusScrollGeometry(mustDiagnosticRaw(raw, "after"), value.After); err != nil {
		return err
	}
	order := []string{"destroyed", "unmounted", "follow-intent-false", "active-contact", "scroll-gesture", "dom-selection", "terminal-selection", "already-at-live-edge", "write-to-live-edge", "pending-cancelled"}
	last := -1
	for _, reason := range value.Reasons {
		index := slices.Index(order, reason)
		if index <= last {
			return errors.New("invalid focus-scroll reveal reasons")
		}
		last = index
	}
	if kind == "reveal-request" {
		if value.Outcome != nil || len(value.Reasons) != 0 {
			return errors.New("invalid focus-scroll reveal request")
		}
		return nil
	}
	if value.Outcome == nil {
		return errors.New("missing focus-scroll reveal outcome")
	}
	expected := map[string][]string{
		"already-visible": {"already-at-live-edge"},
		"revealed":        {"write-to-live-edge"},
		"cancelled":       {"pending-cancelled"},
	}
	switch *value.Outcome {
	case "blocked":
		if !slices.Contains(value.Reasons, "destroyed") && !slices.Contains(value.Reasons, "unmounted") && !slices.Contains(value.Reasons, "follow-intent-false") {
			return errors.New("invalid focus-scroll blocked reasons")
		}
	case "held":
		if !slices.Contains(value.Reasons, "active-contact") && !slices.Contains(value.Reasons, "scroll-gesture") &&
			!slices.Contains(value.Reasons, "dom-selection") && !slices.Contains(value.Reasons, "terminal-selection") {
			return errors.New("invalid focus-scroll held reasons")
		}
	case "already-visible", "revealed", "cancelled":
		if !slices.Equal(value.Reasons, expected[*value.Outcome]) {
			return errors.New("invalid focus-scroll outcome reasons")
		}
	default:
		return errors.New("invalid focus-scroll reveal outcome")
	}
	if kind == "reveal-cancellation" && *value.Outcome != "cancelled" {
		return errors.New("invalid focus-scroll cancellation")
	}
	if kind == "reveal-decision" && *value.Outcome == "cancelled" {
		return errors.New("invalid focus-scroll decision")
	}
	return nil
}

func decodeFocusScrollDiagnosticEvent(raw json.RawMessage) (focusScrollDiagnosticEvent, error) {
	var event focusScrollDiagnosticEvent
	if err := exactDiagnosticKeys(raw, "ordinal", "monotonicMs", "kind", "focusTarget", "activationSource", "activationTarget", "keyboardState", "reveal", "scrollWrite", "snapshot"); err != nil {
		return event, err
	}
	if err := strictDiagnosticJSON(raw, &event); err != nil {
		return event, err
	}
	if event.Ordinal == 0 || !focusScrollSafe(event.Ordinal) || !focusScrollFinite(event.MonotonicMS, true) {
		return event, errors.New("invalid focus-scroll event header")
	}
	kinds := []string{
		"enable-baseline", "activation", "focus-in", "focus-out", "visual-viewport-resize",
		"visual-viewport-scroll", "keyboard-state-edge", "composer-inset-publication", "reveal-request",
		"reveal-decision", "reveal-cancellation", "internal-scroll-write", "scroller-scroll",
		"settle-microtask", "settle-raf-1", "settle-raf-2", "settle-250ms",
	}
	if !slices.Contains(kinds, event.Kind) {
		return event, errors.New("invalid focus-scroll event kind")
	}
	object, err := focusScrollRawObject(raw)
	if err != nil {
		return event, err
	}
	focusKind := event.Kind == "focus-in" || event.Kind == "focus-out"
	if focusKind {
		if event.FocusTarget == nil || !slices.Contains([]string{"none", "terminal-helper", "composer", "diagnostic-control", "other-within-attachment", "other"}, *event.FocusTarget) {
			return event, errors.New("invalid focus-scroll focus target")
		}
	} else if event.FocusTarget != nil {
		return event, errors.New("focus target on wrong focus-scroll kind")
	}
	if event.Kind == "activation" {
		if event.ActivationSource == nil || !slices.Contains([]string{"pointerdown", "touchstart"}, *event.ActivationSource) ||
			event.ActivationTarget == nil || !slices.Contains([]string{"terminal-surface", "terminal-helper", "composer-open", "composer-textarea", "keybar", "diagnostic-control", "other-within-attachment", "other"}, *event.ActivationTarget) {
			return event, errors.New("invalid focus-scroll activation")
		}
	} else if event.ActivationSource != nil || event.ActivationTarget != nil {
		return event, errors.New("activation data on wrong focus-scroll kind")
	}
	if event.Kind == "keyboard-state-edge" {
		if event.KeyboardState == nil || event.KeyboardState.PreviousOpen == event.KeyboardState.CurrentOpen ||
			exactDiagnosticKeys(object["keyboardState"], "previousOpen", "currentOpen") != nil {
			return event, errors.New("invalid focus-scroll keyboard state")
		}
	} else if event.KeyboardState != nil {
		return event, errors.New("keyboard state on wrong focus-scroll kind")
	}
	revealKind := event.Kind == "reveal-request" || event.Kind == "reveal-decision" || event.Kind == "reveal-cancellation"
	if revealKind {
		if err := validateFocusScrollReveal(object["reveal"], event.Reveal, event.Kind); err != nil {
			return event, err
		}
	} else if event.Reveal != nil {
		return event, errors.New("reveal on wrong focus-scroll kind")
	}
	if event.Kind == "internal-scroll-write" {
		if event.ScrollWrite == nil {
			return event, errors.New("missing focus-scroll write")
		}
		if err := exactDiagnosticKeys(object["scrollWrite"], "reason", "requestedScrollTop", "before", "after"); err != nil {
			return event, err
		}
		write := event.ScrollWrite
		if !slices.Contains([]string{"live-edge-fit", "anchor-fit", "live-edge-display-adjustment", "anchor-display-adjustment", "live-edge-preparation", "live-edge-mount", "live-edge-keyboard", "live-edge-overlay", "head-removal", "tail-removal-residual", "head-removal-residual", "prepend-anchor", "request-live-edge"}, write.Reason) ||
			!focusScrollFinite(write.RequestedScrollTop, false) {
			return event, errors.New("invalid focus-scroll write")
		}
		if err := validateFocusScrollGeometry(mustDiagnosticRaw(object["scrollWrite"], "before"), write.Before); err != nil {
			return event, err
		}
		if err := validateFocusScrollGeometry(mustDiagnosticRaw(object["scrollWrite"], "after"), write.After); err != nil {
			return event, err
		}
	} else if event.ScrollWrite != nil {
		return event, errors.New("scroll write on wrong focus-scroll kind")
	}
	if err := validateFocusScrollSnapshot(object["snapshot"], event.Snapshot); err != nil {
		return event, err
	}
	return event, nil
}

func decodeFocusScrollDiagnosticTraceRequest(data []byte) (diagnosticTraceRequest, validatedFocusScrollDiagnosticTrace, error) {
	var request diagnosticTraceRequest
	var rawRequest map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawRequest); err != nil {
		return request, validatedFocusScrollDiagnosticTrace{}, err
	}
	if rawCapture, ok := rawRequest["capture_id"]; ok {
		if bytes.Equal(bytes.TrimSpace(rawCapture), []byte("null")) {
			return request, validatedFocusScrollDiagnosticTrace{}, errors.New("capture ID must be omitted or a string")
		}
		var capture string
		if err := strictDiagnosticJSON(rawCapture, &capture); err != nil || !diagnosticCaptureIDRE.MatchString(capture) {
			return request, validatedFocusScrollDiagnosticTrace{}, errors.New("invalid capture ID")
		}
		request.CaptureID = &capture
	}
	rawTrace := rawRequest["trace"]
	if err := exactDiagnosticKeys(rawTrace, "schema", "capacity", "retainedEvents", "droppedEvents", "enabled", "events"); err != nil {
		return request, validatedFocusScrollDiagnosticTrace{}, err
	}
	var trace focusScrollDiagnosticTraceWire
	if err := strictDiagnosticJSON(rawTrace, &trace); err != nil {
		return request, validatedFocusScrollDiagnosticTrace{}, err
	}
	if trace.Schema != focusScrollDiagnosticTraceSchema || trace.Capacity != diagnosticTraceEventCapacity ||
		trace.RetainedEvents != len(trace.Events) || trace.RetainedEvents < 0 ||
		trace.RetainedEvents > diagnosticTraceEventCapacity || !focusScrollSafe(trace.DroppedEvents) {
		return request, validatedFocusScrollDiagnosticTrace{}, errors.New("invalid focus-scroll trace header")
	}
	validated := validatedFocusScrollDiagnosticTrace{
		Schema: trace.Schema, Capacity: trace.Capacity, RetainedEvents: trace.RetainedEvents,
		DroppedEvents: trace.DroppedEvents, Enabled: trace.Enabled,
		Events: make([]focusScrollDiagnosticEvent, 0, len(trace.Events)),
	}
	for index, raw := range trace.Events {
		event, err := decodeFocusScrollDiagnosticEvent(raw)
		if err != nil {
			return request, validatedFocusScrollDiagnosticTrace{}, fmt.Errorf("focus-scroll event %d: %w", index, err)
		}
		if event.Ordinal != uint64(index+1) {
			return request, validatedFocusScrollDiagnosticTrace{}, errors.New("focus-scroll ordinals are not contiguous from one")
		}
		validated.Events = append(validated.Events, event)
	}
	if len(trace.Events) == 0 && trace.DroppedEvents != 0 {
		return request, validatedFocusScrollDiagnosticTrace{}, errors.New("empty focus-scroll trace has dropped events")
	}
	return request, validated, nil
}

func decodeDiagnosticTraceRequest(data []byte) (diagnosticTraceRequest, validatedTrace, error) {
	var request diagnosticTraceRequest
	if !utf8.Valid(data) {
		return request, validatedTrace{}, errors.New("invalid UTF-8")
	}
	if err := rejectDiagnosticDuplicateKeys(data); err != nil {
		return request, validatedTrace{}, err
	}
	if err := exactDiagnosticKeys(data, requestKeys(data)...); err != nil {
		return request, validatedTrace{}, err
	}
	rawTrace, err := nestedDiagnosticRaw(data, "trace")
	if err != nil {
		return request, validatedTrace{}, err
	}
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(rawTrace, &header); err != nil {
		return request, validatedTrace{}, err
	}
	switch header.Schema {
	case diagnosticTraceSchema:
		decoded, trace, err := decodeSustainedDiagnosticTraceRequest(data)
		return decoded, validatedTrace{RetainedEvents: trace.RetainedEvents, value: trace}, err
	case focusScrollDiagnosticTraceSchema:
		decoded, trace, err := decodeFocusScrollDiagnosticTraceRequest(data)
		return decoded, validatedTrace{RetainedEvents: trace.RetainedEvents, value: trace}, err
	default:
		return request, validatedTrace{}, errors.New("invalid diagnostic schema")
	}
}

func decodeSustainedDiagnosticTraceRequest(data []byte) (diagnosticTraceRequest, validatedDiagnosticTrace, error) {
	var request diagnosticTraceRequest
	if !utf8.Valid(data) {
		return request, validatedDiagnosticTrace{}, errors.New("invalid UTF-8")
	}
	if err := rejectDiagnosticDuplicateKeys(data); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if err := exactDiagnosticKeys(data, requestKeys(data)...); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if err := strictDiagnosticJSON(data, &request); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	var rawRequest map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawRequest); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if rawCaptureID, present := rawRequest["capture_id"]; present && bytes.Equal(bytes.TrimSpace(rawCaptureID), []byte("null")) {
		return request, validatedDiagnosticTrace{}, errors.New("capture ID must be omitted or a string")
	}
	if request.CaptureID != nil && !diagnosticCaptureIDRE.MatchString(*request.CaptureID) {
		return request, validatedDiagnosticTrace{}, errors.New("invalid capture ID")
	}
	rawTrace, err := nestedDiagnosticRaw(data, "trace")
	if err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if err := exactDiagnosticKeys(rawTrace, "schema", "capacity", "retainedEvents", "droppedEvents", "experiment", "events"); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	rawExperiment, err := nestedDiagnosticRaw(rawTrace, "experiment")
	if err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if err := exactDiagnosticKeys(rawExperiment, "enabled", "comparisonInputTypes"); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	rawComparison, err := nestedDiagnosticRaw(rawExperiment, "comparisonInputTypes")
	if err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	if err := exactDiagnosticKeys(rawComparison, "terminal", "web-control-allowed", "web-control-prevented"); err != nil {
		return request, validatedDiagnosticTrace{}, err
	}
	trace := request.Trace
	if trace.Schema != diagnosticTraceSchema || trace.Capacity != diagnosticTraceEventCapacity ||
		trace.RetainedEvents != len(trace.Events) || trace.RetainedEvents < 0 ||
		trace.RetainedEvents > diagnosticTraceEventCapacity || trace.DroppedEvents > diagnosticMaxSafeInteger {
		return request, validatedDiagnosticTrace{}, errors.New("invalid diagnostic trace header")
	}
	for _, values := range [][]string{
		trace.Experiment.ComparisonInputTypes.Terminal,
		trace.Experiment.ComparisonInputTypes.WebControlAllowed,
		trace.Experiment.ComparisonInputTypes.WebControlPrevented,
	} {
		if len(values) > diagnosticTraceEventCapacity {
			return request, validatedDiagnosticTrace{}, errors.New("invalid comparison input types")
		}
		for _, value := range values {
			if !strings.HasPrefix(value, "delete") || !validateDiagnosticInputType(value) {
				return request, validatedDiagnosticTrace{}, errors.New("invalid comparison input type")
			}
		}
	}
	validated := validatedDiagnosticTrace{
		Schema: trace.Schema, Capacity: trace.Capacity, RetainedEvents: trace.RetainedEvents,
		DroppedEvents: trace.DroppedEvents, Experiment: trace.Experiment,
		Events: make([]any, 0, len(trace.Events)),
	}
	var prior uint64
	var first uint64
	expectedComparison := diagnosticComparisonInputTypes{}
	var lastToggle *bool
	for index, raw := range trace.Events {
		event, ordinal, err := decodeDiagnosticEvent(raw)
		if err != nil {
			return request, validatedDiagnosticTrace{}, fmt.Errorf("event %d: %w", index, err)
		}
		if prior != 0 && ordinal != prior+1 {
			return request, validatedDiagnosticTrace{}, errors.New("diagnostic ordinals are not contiguous")
		}
		if first == 0 {
			first = ordinal
		}
		prior = ordinal
		validated.Events = append(validated.Events, event)
		switch typed := event.(type) {
		case diagnosticInputEvent:
			if typed.Kind == "beforeinput" && typed.ExperimentEnabled && strings.HasPrefix(typed.InputType, "delete") {
				switch typed.Source {
				case "terminal":
					expectedComparison.Terminal = append(expectedComparison.Terminal, typed.InputType)
				case "web-control-allowed":
					expectedComparison.WebControlAllowed = append(expectedComparison.WebControlAllowed, typed.InputType)
				case "web-control-prevented":
					expectedComparison.WebControlPrevented = append(expectedComparison.WebControlPrevented, typed.InputType)
				}
			}
		case diagnosticExperimentToggleEvent:
			value := typed.ExperimentEnabled
			lastToggle = &value
		}
	}
	if len(trace.Events) == 0 {
		if trace.DroppedEvents != 0 {
			return request, validatedDiagnosticTrace{}, errors.New("empty diagnostic trace has dropped events")
		}
	} else if trace.DroppedEvents == 0 {
		if first != 1 {
			return request, validatedDiagnosticTrace{}, errors.New("diagnostic trace does not begin at ordinal one")
		}
	} else if len(trace.Events) != diagnosticTraceEventCapacity || first != trace.DroppedEvents+1 {
		return request, validatedDiagnosticTrace{}, errors.New("diagnostic ring accounting is inconsistent")
	}
	if !slices.Equal(trace.Experiment.ComparisonInputTypes.Terminal, expectedComparison.Terminal) ||
		!slices.Equal(trace.Experiment.ComparisonInputTypes.WebControlAllowed, expectedComparison.WebControlAllowed) ||
		!slices.Equal(trace.Experiment.ComparisonInputTypes.WebControlPrevented, expectedComparison.WebControlPrevented) {
		return request, validatedDiagnosticTrace{}, errors.New("comparison input types do not match retained events")
	}
	if lastToggle != nil && trace.Experiment.Enabled != *lastToggle {
		return request, validatedDiagnosticTrace{}, errors.New("experiment state does not match retained toggle")
	}
	return request, validated, nil
}

func requestKeys(data []byte) []string {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return []string{"trace"}
	}
	if _, ok := object["capture_id"]; ok {
		return []string{"capture_id", "trace"}
	}
	return []string{"trace"}
}

func nestedDiagnosticRaw(data []byte, key string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	raw, ok := object[key]
	if !ok {
		return nil, errors.New("missing diagnostic field")
	}
	return raw, nil
}

type diagnosticTraceLimits struct {
	writesPerCapture    uint64
	bytesPerCapture     uint64
	capturesPerOperator int
	capturesGlobal      int
	files               int
	diskBytes           int64
	inactivity          time.Duration
}

func productionDiagnosticTraceLimits() diagnosticTraceLimits {
	return diagnosticTraceLimits{
		writesPerCapture: diagnosticTraceWritesPerCapture, bytesPerCapture: diagnosticTraceBytesPerCapture,
		capturesPerOperator: diagnosticTraceCapturesPerOperator, capturesGlobal: diagnosticTraceCapturesGlobal,
		files: diagnosticTraceFilesLimit, diskBytes: diagnosticTraceDiskBytesLimit, inactivity: diagnosticTraceInactivity,
	}
}

type diagnosticRoot interface {
	Lstat(string) (os.FileInfo, error)
	Open(string) (*os.File, error)
	OpenFile(string, int, os.FileMode) (*os.File, error)
	Link(string, string) error
	Remove(string) error
	Rename(string, string) error
	Close() error
}

type diagnosticCapture struct {
	id, operator, filename string
	writes, requestBytes   uint64
	canonicalBytes         int64
	lastActive             time.Time
}

type diagnosticPersistResult struct {
	housekeepingFailures []string
}

type diagnosticTraceStore struct {
	mu                      sync.Mutex
	root                    diagnosticRoot
	dirGID                  uint32
	limits                  diagnosticTraceLimits
	captures                map[string]*diagnosticCapture
	now                     func() time.Time
	randomRead              func([]byte) (int, error)
	writeFile               func(*os.File, []byte) (int, error)
	fileSync                func(*os.File) error
	closeFile               func(*os.File) error
	renameFile              func(string, string) error
	dirSync                 func(*os.File) error
	publishedLstat          func(string) (os.FileInfo, error)
	openPublishedDirectory  func() (*os.File, error)
	closePublishedDirectory func(*os.File) error
	removePublishedBackup   func(string) error
}

type diagnosticSaveResult struct {
	captureID, outcome   string
	eventCount           int
	writeOrdinal         uint64
	acceptedAt           time.Time
	canonicalBytes       int
	housekeepingFailures []string
}

func diagnosticPathBelow(root, target string) bool {
	if root == "" || target == "" || !filepath.IsAbs(root) || !filepath.IsAbs(target) ||
		filepath.Clean(root) != root || filepath.Clean(target) != target {
		return false
	}
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != "." && relative != "" && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func openDiagnosticTraceRoot(path, allowedRoot string) (*os.Root, os.FileInfo, error) {
	if !diagnosticPathBelow(allowedRoot, path) {
		return nil, nil, errors.New("diagnostic trace directory is outside the evidence root")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect diagnostic trace directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("diagnostic trace directory contains a non-directory or symlink")
		}
	}
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("diagnostic trace directory must be pre-existing and real")
	}
	if before.Mode()&os.ModeSetgid == 0 {
		return nil, nil, errors.New("diagnostic trace directory must preserve its analyst group")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open diagnostic trace directory: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("pin diagnostic trace directory: %w", err)
	}
	pinned, statErr := dir.Stat()
	_ = dir.Close()
	if statErr != nil || !pinned.IsDir() || !os.SameFile(before, pinned) {
		_ = root.Close()
		return nil, nil, errors.New("pinned diagnostic trace directory changed during startup")
	}
	return root, pinned, nil
}

func newDiagnosticTraceStore(path string) (*diagnosticTraceStore, error) {
	root, info, err := openDiagnosticTraceRoot(path, config.DiagnosticTraceRoot)
	if err != nil {
		return nil, err
	}
	store, err := newDiagnosticTraceStoreFromRoot(root, info, productionDiagnosticTraceLimits())
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return store, nil
}

func newDiagnosticTraceStoreFromRoot(root diagnosticRoot, directory os.FileInfo, limits diagnosticTraceLimits) (*diagnosticTraceStore, error) {
	stat, ok := directory.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("diagnostic trace directory metadata unavailable")
	}
	store := &diagnosticTraceStore{
		root: root, dirGID: stat.Gid, limits: limits,
		captures: make(map[string]*diagnosticCapture), now: time.Now, randomRead: rand.Read,
	}
	store.writeFile = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	store.fileSync = func(file *os.File) error { return file.Sync() }
	store.closeFile = func(file *os.File) error { return file.Close() }
	store.renameFile = root.Rename
	store.dirSync = func(file *os.File) error { return file.Sync() }
	store.publishedLstat = root.Lstat
	store.openPublishedDirectory = func() (*os.File, error) { return root.Open(".") }
	store.closePublishedDirectory = func(file *os.File) error { return file.Close() }
	store.removePublishedBackup = root.Remove
	if _, _, err := store.scanDiskLocked(); err != nil {
		return nil, err
	}
	if err := store.probeWritable(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *diagnosticTraceStore) randomHex(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	n, err := s.randomRead(value)
	if err != nil {
		return "", err
	}
	if n != len(value) {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(value), nil
}

func (s *diagnosticTraceStore) probeWritable() error {
	nonce, err := s.randomHex(8)
	if err != nil {
		return err
	}
	name := ".diagnostic-startup-" + nonce + ".tmp"
	file, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, diagnosticTraceFileMode)
	if err != nil {
		return fmt.Errorf("diagnostic trace directory is not writable: %w", err)
	}
	if err := file.Chmod(diagnosticTraceFileMode); err != nil {
		_ = file.Close()
		_ = s.root.Remove(name)
		return fmt.Errorf("diagnostic trace file mode unavailable: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		_ = s.root.Remove(name)
		return errors.New("diagnostic trace probe metadata unavailable")
	}
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !statOK || !info.Mode().IsRegular() || info.Mode().Perm() != diagnosticTraceFileMode ||
		stat.Uid != uint32(os.Getuid()) || stat.Gid != s.dirGID {
		_ = file.Close()
		_ = s.root.Remove(name)
		return errors.New("diagnostic trace probe ownership or mode mismatch")
	}
	if err := file.Close(); err != nil {
		_ = s.root.Remove(name)
		return fmt.Errorf("diagnostic trace probe close: %w", err)
	}
	if err := s.root.Remove(name); err != nil {
		return fmt.Errorf("diagnostic trace probe cleanup: %w", err)
	}
	return nil
}

func (s *diagnosticTraceStore) validateEvidenceFile(name string) (os.FileInfo, error) {
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != diagnosticTraceFileMode {
		return nil, fmt.Errorf("%w: unsafe evidence file", errDiagnosticStoreUnavailable)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Gid != s.dirGID ||
		info.Size() < 0 || info.Size() > diagnosticTraceBodyLimit {
		return nil, fmt.Errorf("%w: invalid evidence ownership or size", errDiagnosticStoreUnavailable)
	}
	return info, nil
}

// reapPublishedBackupLocked finishes housekeeping from a transaction whose
// rename made the new final name visible but whose backup removal did not
// complete. The first sync makes the visible final durable before its rollback
// link is retired. A persistent cleanup failure stops later writes, bounding the
// residue at one backup instead of allowing hidden files to accumulate.
func (s *diagnosticTraceStore) reapPublishedBackupLocked(backup, final string) error {
	if _, err := s.validateEvidenceFile(backup); err != nil {
		return err
	}
	if _, err := s.validateEvidenceFile(final); err != nil {
		return err
	}
	directory, err := s.root.Open(".")
	if err != nil {
		return fmt.Errorf("%w: open evidence directory for backup recovery", errDiagnosticStoreUnavailable)
	}
	if err := s.dirSync(directory); err != nil {
		_ = directory.Close()
		return fmt.Errorf("%w: sync evidence directory for backup recovery", errDiagnosticStoreUnavailable)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("%w: close evidence directory for backup recovery", errDiagnosticStoreUnavailable)
	}
	if err := s.root.Remove(backup); err != nil {
		return fmt.Errorf("%w: remove recovered diagnostic backup", errDiagnosticStoreUnavailable)
	}
	directory, err = s.root.Open(".")
	if err != nil {
		return fmt.Errorf("%w: reopen evidence directory after backup recovery", errDiagnosticStoreUnavailable)
	}
	syncErr := s.dirSync(directory)
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("%w: persist diagnostic backup recovery", errDiagnosticStoreUnavailable)
	}
	return nil
}

func (s *diagnosticTraceStore) scanDiskLocked() (int, int64, error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return 0, 0, fmt.Errorf("%w: open evidence directory", errDiagnosticStoreUnavailable)
	}
	entries, readErr := directory.Readdir(-1)
	_ = directory.Close()
	if readErr != nil {
		return 0, 0, fmt.Errorf("%w: read evidence directory", errDiagnosticStoreUnavailable)
	}
	count, total := 0, int64(0)
	for _, entry := range entries {
		if match := diagnosticBackupFilenameRE.FindStringSubmatch(entry.Name()); match != nil {
			if err := s.reapPublishedBackupLocked(entry.Name(), match[1]); err != nil {
				return 0, 0, err
			}
			continue
		}
		if diagnosticTemporaryFilenameRE.MatchString(entry.Name()) {
			return 0, 0, fmt.Errorf("%w: stale diagnostic work file", errDiagnosticStoreUnavailable)
		}
		if !diagnosticFilenameRE.MatchString(entry.Name()) {
			continue
		}
		info, err := s.validateEvidenceFile(entry.Name())
		if err != nil {
			return 0, 0, err
		}
		count++
		total += info.Size()
	}
	return count, total, nil
}

func (s *diagnosticTraceStore) cleanupExpiredLocked(now time.Time) {
	for id, capture := range s.captures {
		if now.Sub(capture.lastActive) >= s.limits.inactivity {
			delete(s.captures, id)
		}
	}
}

func (s *diagnosticTraceStore) activeForOperatorLocked(operator string) int {
	count := 0
	for _, capture := range s.captures {
		if capture.operator == operator {
			count++
		}
	}
	return count
}

func (s *diagnosticTraceStore) uniqueCaptureIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		id, err := s.randomHex(16)
		if err != nil {
			return "", err
		}
		if _, exists := s.captures[id]; !exists {
			return id, nil
		}
	}
	return "", errors.New("diagnostic capture ID collision")
}

func (s *diagnosticTraceStore) uniqueFilenameLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		nonce, err := s.randomHex(16)
		if err != nil {
			return "", err
		}
		name := "trace-" + nonce + ".json"
		if _, err := s.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("diagnostic filename collision")
}

func (s *diagnosticTraceStore) persistLocked(filename string, canonical []byte) (diagnosticPersistResult, error) {
	var temporary string
	var file *os.File
	for attempt := 0; attempt < 8; attempt++ {
		nonce, err := s.randomHex(16)
		if err != nil {
			return diagnosticPersistResult{}, err
		}
		temporary = ".trace-" + nonce + ".tmp"
		file, err = s.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, diagnosticTraceFileMode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return diagnosticPersistResult{}, err
		}
		break
	}
	if file == nil {
		return diagnosticPersistResult{}, errors.New("diagnostic temporary filename collision")
	}
	published := false
	defer func() {
		_ = file.Close()
		if !published {
			_ = s.root.Remove(temporary)
		}
	}()
	if err := file.Chmod(diagnosticTraceFileMode); err != nil {
		return diagnosticPersistResult{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != diagnosticTraceFileMode {
		return diagnosticPersistResult{}, errors.New("diagnostic temporary file failed validation")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Gid != s.dirGID {
		return diagnosticPersistResult{}, errors.New("diagnostic temporary file ownership mismatch")
	}
	n, err := s.writeFile(file, canonical)
	if err == nil && n != len(canonical) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return diagnosticPersistResult{}, err
	}
	if err := s.fileSync(file); err != nil {
		return diagnosticPersistResult{}, err
	}
	if err := s.closeFile(file); err != nil {
		return diagnosticPersistResult{}, err
	}
	backup := ""
	if _, err := s.root.Lstat(filename); err == nil {
		for attempt := 0; attempt < 8; attempt++ {
			nonce, randomErr := s.randomHex(16)
			if randomErr != nil {
				return diagnosticPersistResult{}, randomErr
			}
			backup = "." + filename + "." + nonce + ".bak"
			if linkErr := s.root.Link(filename, backup); errors.Is(linkErr, os.ErrExist) {
				continue
			} else if linkErr != nil {
				return diagnosticPersistResult{}, linkErr
			}
			break
		}
		if backup == "" {
			return diagnosticPersistResult{}, errors.New("diagnostic backup filename collision")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return diagnosticPersistResult{}, err
	}
	if err := s.renameFile(temporary, filename); err != nil {
		if backup != "" {
			_ = s.root.Remove(backup)
		}
		return diagnosticPersistResult{}, err
	}
	published = true
	result := diagnosticPersistResult{}
	recordFailure := func(stage string) {
		result.housekeepingFailures = append(result.housekeepingFailures, stage)
	}
	final, err := s.publishedLstat(filename)
	finalValid := true
	if err != nil || !final.Mode().IsRegular() || final.Mode().Perm() != diagnosticTraceFileMode || final.Size() != int64(len(canonical)) {
		recordFailure("published_validation")
		finalValid = false
	}
	if finalValid {
		finalStat, ok := final.Sys().(*syscall.Stat_t)
		if !ok || finalStat.Uid != uint32(os.Getuid()) || finalStat.Gid != s.dirGID {
			recordFailure("published_ownership")
			finalValid = false
		}
	}
	directoryDurable := false
	directory, err := s.openPublishedDirectory()
	if err != nil {
		recordFailure("published_directory_open")
	} else {
		if err := s.dirSync(directory); err != nil {
			recordFailure("published_directory_sync")
		} else {
			directoryDurable = true
		}
		if err := s.closePublishedDirectory(directory); err != nil {
			recordFailure("published_directory_close")
		}
	}
	// Rename is the client-visible commit point. Everything after it is
	// housekeeping: never report a failure while the new bytes are visible.
	// Keep the rollback link if validation or durability could not be confirmed;
	// scanDiskLocked will validate, sync, and retire it before a later write.
	if backup != "" && finalValid && directoryDurable {
		if err := s.removePublishedBackup(backup); err != nil {
			recordFailure("published_backup_remove")
		} else {
			// The published target was already made durable above. This second
			// sync only retires the rollback link. Failure can resurrect at most
			// one validated backup after a crash; startup reaps it before use.
			directory, err := s.openPublishedDirectory()
			if err != nil {
				recordFailure("published_cleanup_directory_open")
			} else {
				if err := s.dirSync(directory); err != nil {
					recordFailure("published_cleanup_directory_sync")
				}
				if err := s.closePublishedDirectory(directory); err != nil {
					recordFailure("published_cleanup_directory_close")
				}
			}
		}
	}
	return result, nil
}

func (s *diagnosticTraceStore) save(operator string, captureID *string, requestBytes int, eventCount int, canonical []byte) (diagnosticSaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.cleanupExpiredLocked(now)
	files, diskBytes, err := s.scanDiskLocked()
	if err != nil {
		return diagnosticSaveResult{}, err
	}
	if requestBytes < 0 || uint64(requestBytes) > s.limits.bytesPerCapture {
		return diagnosticSaveResult{}, errDiagnosticCaptureLimit
	}
	if captureID == nil {
		if len(s.captures) >= s.limits.capturesGlobal || s.activeForOperatorLocked(operator) >= s.limits.capturesPerOperator {
			return diagnosticSaveResult{}, errDiagnosticCaptureLimit
		}
		if files+1 > s.limits.files || diskBytes+int64(len(canonical)) > s.limits.diskBytes {
			return diagnosticSaveResult{}, errDiagnosticDiskLimit
		}
		id, err := s.uniqueCaptureIDLocked()
		if err != nil {
			return diagnosticSaveResult{}, err
		}
		filename, err := s.uniqueFilenameLocked()
		if err != nil {
			return diagnosticSaveResult{}, err
		}
		persisted, err := s.persistLocked(filename, canonical)
		if err != nil {
			return diagnosticSaveResult{}, fmt.Errorf("%w: %v", errDiagnosticStoreUnavailable, err)
		}
		capture := &diagnosticCapture{
			id: id, operator: operator, filename: filename, writes: 1,
			requestBytes: uint64(requestBytes), canonicalBytes: int64(len(canonical)), lastActive: now,
		}
		s.captures[id] = capture
		return diagnosticSaveResult{
			captureID: id, outcome: "created", eventCount: eventCount, writeOrdinal: 1,
			acceptedAt: now, canonicalBytes: len(canonical),
			housekeepingFailures: persisted.housekeepingFailures,
		}, nil
	}
	capture, ok := s.captures[*captureID]
	if !ok || capture.operator != operator {
		return diagnosticSaveResult{}, errDiagnosticCaptureNotFound
	}
	if capture.writes >= s.limits.writesPerCapture ||
		capture.requestBytes > s.limits.bytesPerCapture-uint64(requestBytes) {
		return diagnosticSaveResult{}, errDiagnosticCaptureLimit
	}
	current, err := s.root.Lstat(capture.filename)
	if err != nil || !current.Mode().IsRegular() || current.Size() != capture.canonicalBytes {
		return diagnosticSaveResult{}, fmt.Errorf("%w: capture file changed", errDiagnosticStoreUnavailable)
	}
	nextDiskBytes := diskBytes - capture.canonicalBytes + int64(len(canonical))
	if files > s.limits.files || nextDiskBytes > s.limits.diskBytes {
		return diagnosticSaveResult{}, errDiagnosticDiskLimit
	}
	persisted, err := s.persistLocked(capture.filename, canonical)
	if err != nil {
		return diagnosticSaveResult{}, fmt.Errorf("%w: %v", errDiagnosticStoreUnavailable, err)
	}
	capture.writes++
	capture.requestBytes += uint64(requestBytes)
	capture.canonicalBytes = int64(len(canonical))
	capture.lastActive = now
	return diagnosticSaveResult{
		captureID: capture.id, outcome: "replaced", eventCount: eventCount,
		writeOrdinal: capture.writes, acceptedAt: now, canonicalBytes: len(canonical),
		housekeepingFailures: persisted.housekeepingFailures,
	}, nil
}

func (s *diagnosticTraceStore) close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

type diagnosticTraceResponse struct {
	CaptureID    string `json:"capture_id"`
	EventCount   int    `json:"event_count"`
	WriteOrdinal uint64 `json:"write_ordinal"`
	AcceptedAt   string `json:"accepted_at"`
}

func (s *Server) diagnosticTrace(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := identity(request)
	if !ok || id.uid != s.cfg.Ingress.PeerUID || id.operator == "" || id.operator != s.cfg.Ingress.OperatorLogin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "invalid diagnostic trace", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, diagnosticTraceBodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "diagnostic trace too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid diagnostic trace", http.StatusBadRequest)
		return
	}
	decoded, trace, err := decodeDiagnosticTraceRequest(body)
	if err != nil {
		http.Error(w, "invalid diagnostic trace", http.StatusBadRequest)
		return
	}
	canonical, err := json.Marshal(trace)
	if err != nil {
		http.Error(w, "diagnostic trace unavailable", http.StatusServiceUnavailable)
		return
	}
	canonical = append(canonical, '\n')
	result, err := s.diagnostic.save(id.operator, decoded.CaptureID, len(body), trace.RetainedEvents, canonical)
	if err != nil {
		switch {
		case errors.Is(err, errDiagnosticCaptureNotFound):
			http.Error(w, "diagnostic capture unavailable", http.StatusNotFound)
		case errors.Is(err, errDiagnosticCaptureLimit):
			http.Error(w, "diagnostic capture limit reached", http.StatusTooManyRequests)
		case errors.Is(err, errDiagnosticDiskLimit):
			http.Error(w, "diagnostic evidence capacity reached", http.StatusInsufficientStorage)
		default:
			http.Error(w, "diagnostic trace unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	hash := sha256.Sum256([]byte(result.captureID))
	if len(result.housekeepingFailures) != 0 {
		frontLogf(
			"component=frontdoor event=diagnostic_trace_housekeeping_failure operator=%q capture_hash=%q stages=%q",
			id.operator, hex.EncodeToString(hash[:6]), strings.Join(result.housekeepingFailures, ","),
		)
	}
	frontLogf(
		"component=frontdoor event=diagnostic_trace_write operator=%q capture_hash=%q request_bytes=%d canonical_bytes=%d event_count=%d write_ordinal=%d outcome=%q",
		id.operator, hex.EncodeToString(hash[:6]), len(body), result.canonicalBytes, result.eventCount, result.writeOrdinal, result.outcome,
	)
	response := diagnosticTraceResponse{
		CaptureID: result.captureID, EventCount: result.eventCount,
		WriteOrdinal: result.writeOrdinal, AcceptedAt: result.acceptedAt.Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "diagnostic trace unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if result.outcome == "created" {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.Write(append(payload, '\n'))
}
