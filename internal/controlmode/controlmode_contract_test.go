package controlmode

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

const (
	decoderRed = "ISSUE25/PHASE6A1/CANDIDATE_RED/STRICT_CONTROL_DECODER"
	routerRed  = "ISSUE25/PHASE6A1/CANDIDATE_RED/SESSION_SCOPED_ROUTER"
)

func encodeControlPayload(payload []byte) []byte {
	encoded := make([]byte, 0, len(payload)*4)
	for _, value := range payload {
		encoded = append(encoded, '\\', '0'+((value>>6)&7), '0'+((value>>3)&7), '0'+(value&7))
	}
	return encoded
}

func decodeWithChunks(t *testing.T, stream []byte, cuts []int) []Event {
	t.Helper()
	decoder := NewDecoder()
	var events []Event
	start := 0
	for _, end := range cuts {
		if end < start || end > len(stream) {
			t.Fatalf("%s invalid test cut start=%d end=%d size=%d", decoderRed, start, end, len(stream))
		}
		got, err := decoder.Feed(stream[start:end])
		if err != nil {
			t.Fatalf("%s fragmented feed failed at [%d:%d]: %v", decoderRed, start, end, err)
		}
		events = append(events, got...)
		start = end
	}
	got, err := decoder.Feed(stream[start:])
	if err != nil {
		t.Fatalf("%s final feed failed at %d: %v", decoderRed, start, err)
	}
	events = append(events, got...)
	if err := decoder.Close(); err != nil {
		t.Fatalf("%s complete stream rejected: %v", decoderRed, err)
	}
	return events
}

func requireSingleOutput(t *testing.T, events []Event, kind EventKind, paneID string, age uint64, want []byte) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("%s event count=%d want=1 events=%+v", decoderRed, len(events), events)
	}
	got := events[0]
	if got.Kind != kind || got.PaneID != paneID || got.Age != age || !bytes.Equal(got.Data, want) {
		t.Fatalf("%s event=%+v bytes=%x want kind=%v pane=%q age=%d bytes=%x", decoderRed, got, got.Data, kind, paneID, age, want)
	}
}

func appendLiteralOutputRecord(stream []byte, paneID string, value byte, payloadBytes int, terminated bool) []byte {
	header := []byte("%output " + paneID + " ")
	recordBytes := len(header) + payloadBytes
	if terminated {
		recordBytes++
	}
	start := len(stream)
	needed := start + recordBytes
	if cap(stream) < needed {
		grown := make([]byte, len(stream), needed)
		copy(grown, stream)
		stream = grown
	}
	stream = stream[:needed]
	copy(stream[start:], header)
	payloadStart := start + len(header)
	for index := payloadStart; index < payloadStart+payloadBytes; index++ {
		stream[index] = value
	}
	if terminated {
		stream[needed-1] = '\n'
	}
	return stream
}

func requireUniformOutput(t *testing.T, event Event, paneID string, value byte, payloadBytes int) {
	t.Helper()
	if event.Kind != EventOutput || event.PaneID != paneID || event.Age != 0 || len(event.Data) != payloadBytes {
		t.Fatalf(
			"%s output metadata kind=%v pane=%q age=%d bytes=%d want pane=%q bytes=%d",
			decoderRed,
			event.Kind,
			event.PaneID,
			event.Age,
			len(event.Data),
			paneID,
			payloadBytes,
		)
	}
	for index, got := range event.Data {
		if got != value {
			t.Fatalf("%s output pane=%q first mismatch at byte=%d got=%02x want=%02x", decoderRed, paneID, index, got, value)
		}
	}
}

func requireStickyDecoderFailure(t *testing.T, stream []byte) {
	t.Helper()
	decoder := NewDecoder()
	events, err := decoder.Feed(stream)
	if !errors.Is(err, ErrDecoderFailed) || len(events) != 0 {
		t.Fatalf("%s oversized record result events=%d err=%v bytes=%d", decoderRed, len(events), err, len(stream))
	}
	if !decoder.Failed() {
		t.Fatalf("%s oversized record did not enter sticky failure bytes=%d", decoderRed, len(stream))
	}
	if events, err := decoder.Feed([]byte("%output %9 legal\n")); !errors.Is(err, ErrDecoderFailed) || len(events) != 0 {
		t.Fatalf("%s failed decoder healed events=%d err=%v", decoderRed, len(events), err)
	}
	if err := decoder.Close(); !errors.Is(err, ErrDecoderFailed) {
		t.Fatalf("%s failed decoder close err=%v", decoderRed, err)
	}
}

func TestStrictDecoderEveryByteAndEverySplit(t *testing.T) {
	payload := make([]byte, 256)
	for index := range payload {
		payload[index] = byte(index)
	}
	stream := append([]byte("%output %7 "), encodeControlPayload(payload)...)
	stream = append(stream, '\n')

	for split := 0; split <= len(stream); split++ {
		events := decodeWithChunks(t, stream, []int{split})
		requireSingleOutput(t, events, EventOutput, "%7", 0, payload)
	}

	random := rand.New(rand.NewSource(25))
	for run := 0; run < 100; run++ {
		var cuts []int
		for position := 0; position < len(stream); {
			position += 1 + random.Intn(17)
			if position < len(stream) {
				cuts = append(cuts, position)
			}
		}
		events := decodeWithChunks(t, stream, cuts)
		requireSingleOutput(t, events, EventOutput, "%7", 0, payload)
	}
}

func TestStrictDecoderLimitAppliesPerRecord(t *testing.T) {
	const decoderRecordLimitBytes = 16 << 20
	if maxControlRecordBytes != decoderRecordLimitBytes {
		t.Fatalf("%s record limit=%d want=%d", decoderRed, maxControlRecordBytes, decoderRecordLimitBytes)
	}

	t.Run("aggregate larger than one record limit", func(t *testing.T) {
		payloadBytes := decoderRecordLimitBytes / 2
		recordBytes := len("%output %1 ") + payloadBytes + 1
		if recordBytes > decoderRecordLimitBytes {
			t.Fatalf("%s fixture record bytes=%d exceeds limit=%d", decoderRed, recordBytes, decoderRecordLimitBytes)
		}
		stream := make([]byte, 0, 2*recordBytes)
		stream = appendLiteralOutputRecord(stream, "%1", 'A', payloadBytes, true)
		stream = appendLiteralOutputRecord(stream, "%2", 'B', payloadBytes, true)
		if len(stream) <= decoderRecordLimitBytes {
			t.Fatalf("%s aggregate bytes=%d did not cross record limit=%d", decoderRed, len(stream), decoderRecordLimitBytes)
		}

		decoder := NewDecoder()
		events, err := decoder.Feed(stream)
		if err != nil {
			t.Fatalf("%s two legal records in one feed failed aggregate=%d: %v", decoderRed, len(stream), err)
		}
		if err := decoder.Close(); err != nil {
			t.Fatalf("%s two legal records close failed: %v", decoderRed, err)
		}
		if len(events) != 2 {
			t.Fatalf("%s event count=%d want=2 aggregate=%d", decoderRed, len(events), len(stream))
		}
		requireUniformOutput(t, events[0], "%1", 'A', payloadBytes)
		requireUniformOutput(t, events[1], "%2", 'B', payloadBytes)
	})

	t.Run("oversized unterminated record", func(t *testing.T) {
		stream := make([]byte, 0, decoderRecordLimitBytes+len("%output %3 ")+1)
		stream = appendLiteralOutputRecord(stream, "%3", 'U', decoderRecordLimitBytes, false)
		requireStickyDecoderFailure(t, stream)
	})

	t.Run("oversized terminated record", func(t *testing.T) {
		stream := make([]byte, 0, decoderRecordLimitBytes+len("%output %4 ")+1)
		stream = appendLiteralOutputRecord(stream, "%4", 'T', decoderRecordLimitBytes, true)
		requireStickyDecoderFailure(t, stream)
	})
}

func TestStrictDecoderExtendedOutputPreservesBytesAndAge(t *testing.T) {
	payload := []byte{0, '\n', '\r', '\\', 0x1b, 0x7f, 0x80, 0xff}
	stream := append([]byte("%extended-output %3 987654 future-field another-field : "), encodeControlPayload(payload)...)
	stream = append(stream, '\n')
	events := decodeWithChunks(t, stream, []int{1, 2, 17, len(stream) - 1})
	requireSingleOutput(t, events, EventExtendedOutput, "%3", 987654, payload)
}

func TestStrictDecoderMixedLiteralAndOctalWirePayload(t *testing.T) {
	stream := []byte("%output %8 literal-\\000-escape-\\033-backslash-\\134-tail\n")
	want := append([]byte("literal-\x00-escape-\x1b-backslash-"), '\\')
	want = append(want, []byte("-tail")...)
	events := decodeWithChunks(t, stream, []int{1, 13, 22, 31, 44, len(stream) - 1})
	requireSingleOutput(t, events, EventOutput, "%8", 0, want)
}

func TestStrictDecoderCommandBlocksCannotBecomePaneOutput(t *testing.T) {
	stream := []byte("%begin 100 9 0\n%output %99 \\101\nfree text that starts %output %2 \\102\n%end 100 9 0\n%output %2 \\103\n")
	events := decodeWithChunks(t, stream, []int{7, 23, 44, 71, len(stream) - 2})
	if len(events) < 4 || events[0].Kind != EventCommandBegin {
		t.Fatalf("%s command block events=%+v", decoderRed, events)
	}
	var response []byte
	endIndex := -1
	for index := 1; index < len(events); index++ {
		switch events[index].Kind {
		case EventCommandResponse:
			response = append(response, events[index].Data...)
		case EventCommandEnd:
			endIndex = index
		case EventOutput, EventExtendedOutput:
			if endIndex < 0 {
				t.Fatalf("%s command response was confused with pane output: %+v", decoderRed, events[index])
			}
		}
	}
	if endIndex < 0 || !bytes.Equal(response, []byte("%output %99 \\101\nfree text that starts %output %2 \\102\n")) {
		t.Fatalf("%s end=%d command response=%q", decoderRed, endIndex, response)
	}
	last := events[len(events)-1]
	if last.Kind != EventOutput || last.PaneID != "%2" || !bytes.Equal(last.Data, []byte{'C'}) {
		t.Fatalf("%s post-command output=%+v", decoderRed, last)
	}
}

func TestStrictDecoderTmux34ObserverNotificationsAreTyped(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"client-detached", "client"},
		{"client-session-changed", "client $1 session"},
		{"config-error", "bad configuration"},
		{"continue", "%1"},
		{"exit", "reason"},
		{"layout-change", "@1 layout visible flags"},
		{"message", "message text"},
		{"pane-mode-changed", "%1"},
		{"paste-buffer-changed", "buffer"},
		{"paste-buffer-deleted", "buffer"},
		{"pause", "%1"},
		{"session-changed", "$1 session"},
		{"session-renamed", "renamed"},
		{"session-window-changed", "$1 @1"},
		{"sessions-changed", ""},
		{"subscription-changed", "name $1 @1 0 %1 future : value"},
		{"unlinked-window-add", "@1"},
		{"unlinked-window-close", "@1"},
		{"unlinked-window-renamed", "@1"},
		{"window-add", "@1"},
		{"window-close", "@1"},
		{"window-pane-changed", "@1 %1"},
		{"window-renamed", "@4 renamed window"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			line := "%" + item.name
			if item.args != "" {
				line += " " + item.args
			}
			line += "\n"
			events := decodeWithChunks(t, []byte(line), []int{1, len(line) / 2})
			wantKind := EventNotification
			switch item.name {
			case "pause":
				wantKind = EventPause
			case "continue":
				wantKind = EventContinue
			}
			if len(events) != 1 || events[0].Kind != wantKind || events[0].Name != item.name || events[0].Args != item.args {
				t.Fatalf("%s notification=%+v want=%s %q", decoderRed, events, item.name, item.args)
			}
		})
	}
}

func TestStrictDecoderMalformedOrAmbiguousInputFailsSticky(t *testing.T) {
	cases := map[string][]byte{
		"short octal":                   []byte("%output %1 \\12\n"),
		"nonoctal":                      []byte("%output %1 \\8aa\n"),
		"trailing escape":               []byte("%output %1 payload\\"),
		"extended output missing colon": []byte("%extended-output %1 10 future \\101\n"),
		"unknown notification":          []byte("%not-a-real-event %1\n"),
		"free text":                     []byte("%output %1 \\101\nordinary text\n"),
		"unmatched command end":         []byte("%end 100 9 0\n"),
		"mismatched command end":        []byte("%begin 100 9 0\nreply\n%end 100 8 0\n"),
		"mismatched command error":      []byte("%begin 100 9 0\nreply\n%error 101 9 0\n"),
		"nested command begin":          []byte("%begin 100 9 0\n%begin 101 10 0\n"),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			decoder := NewDecoder()
			_, feedErr := decoder.Feed(stream)
			closeErr := decoder.Close()
			if feedErr == nil && closeErr == nil {
				t.Fatalf("%s malformed stream accepted: %q", decoderRed, stream)
			}
			if !decoder.Failed() {
				t.Fatalf("%s decoder did not enter sticky failure", decoderRed)
			}
			if _, err := decoder.Feed([]byte("%output %1 \\102\n")); err == nil {
				t.Fatalf("%s failed decoder accepted later output", decoderRed)
			}
		})
	}
}

func pane(server, session string, generation uint64, window, paneID, incarnation string) PaneWitness {
	return PaneWitness{
		Session: SessionWitness{Server: server, Session: session, ControlGeneration: generation},
		Window:  window, Pane: paneID, Incarnation: incarnation,
	}
}

func requireRoute(t *testing.T, result RouteResult, disposition RouteDisposition, reason RouteReason) {
	t.Helper()
	if result.Disposition != disposition || result.Reason != reason {
		t.Fatalf("%s route=%+v want disposition=%v reason=%v", routerRed, result, disposition, reason)
	}
}

func TestSessionRouterIsolatesSessionsWindowsAndPanes(t *testing.T) {
	sessionA := SessionWitness{Server: "server-a", Session: "$1", ControlGeneration: 4}
	sessionB := SessionWitness{Server: "server-a", Session: "$2", ControlGeneration: 9}
	routerA := NewSessionRouter(sessionA)
	routerB := NewSessionRouter(sessionB)
	a := pane("server-a", "$1", 4, "@1", "%1", "inc-overlap")
	b := pane("server-a", "$2", 9, "@1", "%1", "inc-overlap")
	aSecondWindow := pane("server-a", "$1", 4, "@2", "%2", "inc-2")
	if err := routerA.Admit(a); err != nil {
		t.Fatalf("%s admit a: %v", routerRed, err)
	}
	if err := routerA.Admit(aSecondWindow); err != nil {
		t.Fatalf("%s admit second window: %v", routerRed, err)
	}
	if err := routerB.Admit(b); err != nil {
		t.Fatalf("%s admit b: %v", routerRed, err)
	}
	if err := routerA.Admit(b); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("%s router A foreign admission err=%v want=%v", routerRed, err, ErrForeignSession)
	}
	if err := routerB.Admit(a); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("%s router B foreign admission err=%v want=%v", routerRed, err, ErrForeignSession)
	}

	first := routerA.Observe(Observation{Kind: ObservationOutput, Witness: a, Data: []byte("a")})
	requireRoute(t, first, RouteDeliver, RouteExactWitness)
	if first.Delivery == nil || first.Delivery.Witness != a || !bytes.Equal(first.Delivery.Data, []byte("a")) {
		t.Fatalf("%s first delivery=%+v", routerRed, first.Delivery)
	}
	secondWindow := routerA.Observe(Observation{Kind: ObservationOutput, Witness: aSecondWindow, Data: []byte("window-two")})
	requireRoute(t, secondWindow, RouteDeliver, RouteExactWitness)
	second := routerB.Observe(Observation{Kind: ObservationOutput, Witness: b, Data: []byte("b")})
	requireRoute(t, second, RouteDeliver, RouteExactWitness)
	if second.Delivery == nil || second.Delivery.Witness != b {
		t.Fatalf("%s second delivery=%+v", routerRed, second.Delivery)
	}

	requireRoute(t, routerA.Observe(Observation{Kind: ObservationOutput, Witness: b, Data: []byte("foreign-b")}), RouteIgnore, RouteOtherSession)
	requireRoute(t, routerB.Observe(Observation{Kind: ObservationOutput, Witness: a, Data: []byte("foreign-a")}), RouteIgnore, RouteOtherSession)
	if !routerA.UnifiedEligible(a) || !routerA.UnifiedEligible(aSecondWindow) || !routerB.UnifiedEligible(b) {
		t.Fatalf("%s cross-session traffic invalidated admitted panes", routerRed)
	}
}

func TestSessionRouterBirthAdmissionMustPrecedeFirstOutput(t *testing.T) {
	session := SessionWitness{Server: "server", Session: "$1", ControlGeneration: 1}

	t.Run("admitted before first byte", func(t *testing.T) {
		router := NewSessionRouter(session)
		born := pane("server", "$1", 1, "@1", "%2", "born")
		if err := router.Admit(born); err != nil {
			t.Fatalf("%s birth admission: %v", routerRed, err)
		}
		requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: born, Data: []byte("first")}), RouteDeliver, RouteExactWitness)
	})

	t.Run("missed first byte cannot be healed", func(t *testing.T) {
		router := NewSessionRouter(session)
		missed := pane("server", "$1", 1, "@1", "%3", "missed")
		requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: missed, Data: []byte("already missed")}), RouteIgnore, RouteMissedFirstByte)
		if err := router.Admit(missed); !errors.Is(err, ErrMissedFirstByte) {
			t.Fatalf("%s post-output admission err=%v want=%v", routerRed, err, ErrMissedFirstByte)
		}
		if router.UnifiedEligible(missed) || router.CanInjectFocus(missed) {
			t.Fatalf("%s missed pane healed eligible=%v focus=%v", routerRed, router.UnifiedEligible(missed), router.CanInjectFocus(missed))
		}
		requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: missed, Data: []byte("later")}), RouteIgnore, RouteAlreadyInvalid)
	})
}

func TestSessionRouterRenameRetainsButReplacementAndPaneReuseInvalidate(t *testing.T) {
	session := SessionWitness{Server: "server", Session: "$1", ControlGeneration: 7}

	t.Run("rename retains identity", func(t *testing.T) {
		router := NewSessionRouter(session)
		original := pane("server", "$1", 7, "@1", "%1", "inc-a")
		if err := router.Admit(original); err != nil {
			t.Fatalf("%s admit: %v", routerRed, err)
		}
		requireRoute(t, router.Observe(Observation{Kind: ObservationRename, Witness: original, Label: "new-name"}), RouteRetain, RouteIdentityUnchanged)
		if !router.UnifiedEligible(original) {
			t.Fatalf("%s rename invalidated identity", routerRed)
		}
	})

	t.Run("replacement invalidates", func(t *testing.T) {
		router := NewSessionRouter(session)
		original := pane("server", "$1", 7, "@1", "%1", "inc-a")
		replacement := pane("server", "$1", 7, "@1", "%2", "inc-b")
		if err := router.Admit(original); err != nil {
			t.Fatalf("%s admit: %v", routerRed, err)
		}
		requireRoute(t, router.Observe(Observation{Kind: ObservationReplacement, Witness: original, Replacement: replacement}), RouteInvalidate, RouteSourceReplacement)
		if router.UnifiedEligible(original) || router.UnifiedEligible(replacement) || router.CanInjectFocus(original) || router.CanInjectFocus(replacement) {
			t.Fatalf("%s replacement retained authority eligible=%v/%v focus=%v/%v", routerRed, router.UnifiedEligible(original), router.UnifiedEligible(replacement), router.CanInjectFocus(original), router.CanInjectFocus(replacement))
		}
	})

	t.Run("pane id reuse invalidates", func(t *testing.T) {
		router := NewSessionRouter(session)
		original := pane("server", "$1", 7, "@1", "%1", "inc-a")
		reused := pane("server", "$1", 7, "@1", "%1", "inc-b")
		if err := router.Admit(original); err != nil {
			t.Fatalf("%s admit: %v", routerRed, err)
		}
		requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: reused, Data: []byte("wrong source")}), RouteInvalidate, RoutePaneIncarnationChanged)
		if router.UnifiedEligible(original) || router.CanInjectFocus(original) || router.CanInjectFocus(reused) {
			t.Fatalf("%s pane id reuse retained authority", routerRed)
		}
	})
}

func TestSessionRouterControlFaultsInvalidateBeforeDelivery(t *testing.T) {
	cases := []struct {
		name   string
		kind   ObservationKind
		reason RouteReason
	}{
		{"disconnect", ObservationDisconnect, RouteControlDisconnected},
		{"pause", ObservationPause, RouteControlPaused},
		{"decoder ambiguity", ObservationDecoderAmbiguity, RouteDecoderAmbiguous},
		{"server restart", ObservationServerRestart, RouteServerRestarted},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			witness := pane("server", "$1", 3, "@1", "%1", "inc")
			router := NewSessionRouter(witness.Session)
			if err := router.Admit(witness); err != nil {
				t.Fatalf("%s admit: %v", routerRed, err)
			}
			requireRoute(t, router.Observe(Observation{Kind: item.kind, Witness: witness}), RouteInvalidate, item.reason)
			requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: witness, Data: []byte("after")}), RouteIgnore, RouteAlreadyInvalid)
			if router.UnifiedEligible(witness) || router.CanInjectFocus(witness) {
				t.Fatalf("%s %s retained authority eligible=%v focus=%v", routerRed, item.name, router.UnifiedEligible(witness), router.CanInjectFocus(witness))
			}
		})
	}
}

func TestSessionRouterGenerationFenceAndForeignPipeUnitClassification(t *testing.T) {
	witness := pane("server", "$1", 11, "@1", "%1", "inc")
	router := NewSessionRouter(witness.Session)
	if err := router.Admit(witness); err != nil {
		t.Fatalf("%s admit: %v", routerRed, err)
	}
	requireRoute(t, router.Observe(Observation{Kind: ObservationForeignPipe, Witness: witness}), RouteRetain, RouteForeignObserverCoexists)
	if !router.CanInjectFocus(witness) {
		t.Fatalf("%s exact generation rejected focus injection", routerRed)
	}
	stale := witness
	stale.Session.ControlGeneration--
	if router.CanInjectFocus(stale) {
		t.Fatalf("%s stale control generation authorized focus injection", routerRed)
	}
	wrongServer := witness
	wrongServer.Session.Server = "server-after-restart"
	requireRoute(t, router.Observe(Observation{Kind: ObservationOutput, Witness: wrongServer, Data: []byte("new server")}), RouteInvalidate, RouteServerRestarted)
	if router.CanInjectFocus(witness) || router.CanInjectFocus(wrongServer) {
		t.Fatalf("%s source change retained focus authority", routerRed)
	}

	generationRouter := NewSessionRouter(witness.Session)
	if err := generationRouter.Admit(witness); err != nil {
		t.Fatalf("%s generation admit: %v", routerRed, err)
	}
	changedGeneration := witness
	changedGeneration.Session.ControlGeneration++
	requireRoute(t, generationRouter.Observe(Observation{Kind: ObservationOutput, Witness: changedGeneration, Data: []byte("new generation")}), RouteInvalidate, RouteControlGenerationChanged)
	if generationRouter.CanInjectFocus(witness) || generationRouter.CanInjectFocus(changedGeneration) {
		t.Fatalf("%s generation change retained focus authority", routerRed)
	}
}
