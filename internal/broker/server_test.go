package broker

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"persea-terminal/internal/proto"
)

func TestAttachmentRejectionLogIsBoundedAndOmitsPayload(t *testing.T) {
	var logs bytes.Buffer
	prior := brokerLogf
	brokerLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	t.Cleanup(func() { brokerLogf = prior })

	var response bytes.Buffer
	authority := &proto.Authority{Realm: "realm", Server: "server", SessionID: "$1"}
	(&Server{}).attach(nil, &lockedWriter{w: &response}, proto.Control{
		Type: "attach", Engine: "unified-dev", Mode: "invalid", Authority: authority, Msg: "terminal-input-secret",
	})
	got := logs.String()
	if !strings.Contains(got, `code="bad_mode"`) || !strings.Contains(got, `realm="realm"`) {
		t.Fatalf("bounded rejection fields missing: %q", got)
	}
	if strings.Contains(got, "terminal-input-secret") {
		t.Fatalf("attachment payload leaked into log: %q", got)
	}
}
