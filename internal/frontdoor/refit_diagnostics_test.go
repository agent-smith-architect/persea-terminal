package frontdoor

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func refit_failureRefitServer(t *testing.T, reply func(proto.Control) proto.Control) (*Server, string, string) {
	t.Helper()
	operation := strings.Repeat("O", 43)
	source := strings.Repeat("S", 43)
	authority := auth(77, "$17")
	socket, _ := stubCreateBroker(t, reply)
	server := newServer(config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60, HandleCapacity: 8,
	}, ".", "127.0.0.1:8080")
	release, err := server.bindings.bind("", source, authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return server, source, operation
}

func refit_failurePostRefit(t *testing.T, server *Server, source, operation string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session-refits", bytes.NewBufferString(
		`{"source":"`+source+`","columns":97,"operation":"`+operation+`"}`,
	))
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, req)
	return rr
}

func TestRefitFaultLogsOnlyClosedStageAndClass(t *testing.T) {
	const private = "PRIVATE /var/lib/token capability-secret"
	server, source, operation := refit_failureRefitServer(t, func(request proto.Control) proto.Control {
		return proto.Control{
			Type: "refit_refused", ID: request.ID, Code: "refit_faulted",
			RefitStage: proto.RefitFailureQueueBootstrap, RefitClass: proto.RefitFailureStorage,
			Msg: private,
		}
	})
	var logs strings.Builder
	oldLogf := frontLogf
	frontLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	rr := refit_failurePostRefit(t, server, source, operation)
	frontLogf = oldLogf
	if rr.Code != http.StatusBadGateway || strings.TrimSpace(rr.Body.String()) != "refit_faulted" {
		t.Fatalf("refit response status=%d body=%q", rr.Code, rr.Body.String())
	}
	got := logs.String()
	if !strings.Contains(got, `event=refit_failure`) || !strings.Contains(got, `stage="queue_bootstrap"`) || !strings.Contains(got, `class="storage"`) {
		t.Fatalf("front log omitted closed metadata: %q", got)
	}
	if strings.Contains(got, private) || strings.Contains(got, source) || strings.Contains(got, operation) || strings.Contains(rr.Body.String(), private) {
		t.Fatalf("front exposed private refit data: log=%q body=%q", got, rr.Body.String())
	}
}

func TestRefitFaultRejectsUnclosedMetadata(t *testing.T) {
	for name, response := range map[string]proto.Control{
		"missing":         {Type: "refit_refused", Code: "refit_faulted"},
		"arbitrary_stage": {Type: "refit_refused", Code: "refit_faulted", RefitStage: "private/path", RefitClass: proto.RefitFailureStorage},
		"arbitrary_class": {Type: "refit_refused", Code: "refit_faulted", RefitStage: proto.RefitFailureBeginRegistry, RefitClass: "secret_text"},
		"partial":         {Type: "refit_refused", Code: "refit_faulted", RefitStage: proto.RefitFailureBeginRegistry},
		"wrong_code":      {Type: "refit_refused", Code: "bad_refit", RefitStage: proto.RefitFailureBeginRegistry, RefitClass: proto.RefitFailureInternal},
	} {
		t.Run(name, func(t *testing.T) {
			server, source, operation := refit_failureRefitServer(t, func(request proto.Control) proto.Control {
				response.ID = request.ID
				return response
			})
			if rr := refit_failurePostRefit(t, server, source, operation); rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("unclosed metadata status=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}
