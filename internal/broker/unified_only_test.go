package broker

import (
	"bytes"
	"encoding/json"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestAttachRejectsRetiredEngineBeforeEffects(t *testing.T) {
	for _, raw := range []string{`{"type":"attach","mode":"observe"}`, `{"type":"attach","mode":"control","engine":""}`, `{"type":"attach","mode":"control","engine":"other"}`} {
		for _, validated := range []bool{false, true} {
			var ctrl proto.Control
			if err := json.Unmarshal([]byte(raw), &ctrl); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			// No authority, configured server, connection or reader exists: any
			// attempt to progress into attachment work cannot satisfy this test.
			s := &Server{}
			if validated {
				s.attachValidated(nil, &lockedWriter{w: &out}, config.TmuxServer{}, sessionDetails{}, ctrl)
			} else {
				s.attach(nil, &lockedWriter{w: &out}, ctrl)
			}
			frame, err := proto.ReadFrame(&out)
			if err != nil {
				t.Fatal(err)
			}
			got, err := proto.DecodeControl(frame.Payload)
			if err != nil || got.Type != "error" || got.Code != "protocol" || out.Len() != 0 {
				t.Fatalf("validated=%v request=%s got=%+v err=%v", validated, raw, got, err)
			}
		}
	}
}
