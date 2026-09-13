package broker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"persea-terminal/internal/proto"
)

// Every `error` control the broker can write carries a code from the closed
// set proto classifies, and each site's own control flow agrees with its
// class: an operational code is followed by `continue` (the attachment loop
// goes on), a fatal code by `return` (the attachment ends). This is a
// structural fence over source, deliberately: the behaviour is proven by the
// e2e suites, and this keeps a NEW code from reaching the front door
// unclassified, where it would fail closed as a WebSocket close.
func TestEveryBrokerAttachmentErrorCodeIsClassified(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	site := regexp.MustCompile(`Type: "error", Code: "([a-z0-9_]+)"`)
	seen := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		for _, match := range site.FindAllStringSubmatchIndex(source, -1) {
			code := source[match[2]:match[3]]
			seen[code]++
			class := proto.ClassifyAttachmentError(code)
			if class == proto.AttachmentErrorUnknown {
				t.Errorf("%s: error code %q is not classified in proto (operational or fatal)", name, code)
				continue
			}
			// The statement that follows the write decides the attachment's
			// fate; it must agree with the class.
			rest := source[match[1]:]
			if index := strings.Index(rest, "\n"); index >= 0 {
				rest = rest[index+1:]
			}
			// Walk forward to the first statement that decides the loop's fate:
			// `continue` keeps the attachment, `return` ends it, and a closing
			// brace at column zero is the end of a helper whose callers return.
			verdict := ""
			for _, line := range strings.SplitN(rest, "\n", 8) {
				trimmed := strings.TrimSpace(line)
				switch {
				case trimmed == "continue":
					verdict = "continue"
				case trimmed == "return" || strings.HasPrefix(trimmed, "return "):
					verdict = "return"
				case line == "}":
					verdict = "return"
				}
				if verdict != "" {
					break
				}
			}
			switch class {
			case proto.AttachmentErrorOperational:
				if verdict != "continue" {
					t.Errorf("%s: operational code %q ends the loop (%q), want continue", name, code, verdict)
				}
			case proto.AttachmentErrorFatal:
				if verdict != "return" {
					t.Errorf("%s: fatal code %q keeps the loop (%q), want return", name, code, verdict)
				}
			}
		}
	}
	for _, sentinel := range []string{"resize_failed", "resize_rejected", "input_refused", "observe_mode", "attach_failed", "stale_target", "protocol"} {
		if seen[sentinel] == 0 {
			t.Fatalf("extraction lost a known emitted code: %s", sentinel)
		}
	}
	for _, code := range proto.OperationalAttachmentCodes() {
		if seen[code] == 0 {
			t.Errorf("operational code %q is classified but never emitted by the broker", code)
		}
	}
}
