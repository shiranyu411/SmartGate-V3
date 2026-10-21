package runtime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestOpenWrtDryRunHandlerReturnsOwnerTaggedPlan(t *testing.T) {
	server := newOpenWrtTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/runtime/openwrt/dryrun", bytes.NewReader(mustJSON(t, openWrtDryRunPayload{OpID: "openwrt-api-1"})))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", resp.Code, resp.Body.String())
	}
	var plan netgate.DryRunPlan
	data, err := json.Marshal(decodeEnvelopeData(t, resp.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if !plan.DryRunOnly || plan.OpID != "openwrt-api-1" || len(plan.Steps) == 0 {
		t.Fatalf("unexpected dryrun plan: %+v", plan)
	}
	for _, step := range plan.Steps {
		if !step.Resource.IsOwnerTagged() {
			t.Fatalf("step %q missing owner tag: %+v", step.Name, step.Resource)
		}
		for _, command := range step.Commands {
			if !runtimeCommandMentions(command, step.Resource.Comment) {
				t.Fatalf("command for %q missing owner comment %q: %+v", step.Name, step.Resource.Comment, command)
			}
		}
	}
}

func TestOpenWrtDryRunHandlerRejectsUnsafeOpID(t *testing.T) {
	server := newOpenWrtTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/runtime/openwrt/dryrun", bytes.NewReader(mustJSON(t, openWrtDryRunPayload{OpID: "../escape"})))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("dryrun status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOpenWrtRescueCleanupDryRunHandlerReturnsCleanupPlan(t *testing.T) {
	server := newOpenWrtTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/runtime/openwrt/rescue-cleanup/dryrun", bytes.NewReader(mustJSON(t, openWrtDryRunPayload{OpID: "cleanup-api-1"})))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("cleanup dryrun status = %d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Plan    netgate.DryRunPlan  `json:"plan"`
		Cleanup netgate.CleanupPlan `json:"cleanup"`
	}
	data, err := json.Marshal(decodeEnvelopeData(t, resp.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode cleanup payload: %v", err)
	}
	if !payload.Plan.DryRunOnly || len(payload.Cleanup.Actions) == 0 {
		t.Fatalf("unexpected cleanup payload: %+v", payload)
	}
	for _, action := range payload.Cleanup.Actions {
		if action.OwnerTag == "" || action.Comment == "" {
			t.Fatalf("cleanup action missing owner metadata: %+v", action)
		}
		for _, command := range action.Commands {
			if !runtimeCommandMentions(command, action.Comment) {
				t.Fatalf("cleanup command missing owner comment %q: %+v", action.Comment, command)
			}
		}
	}
}

func newOpenWrtTestServer() *Server {
	return NewServer(Config{
		ListenAddr:           "127.0.0.1:3000",
		Owners:               netgate.NewOwnerRegistry("smartgate"),
		WorkerCleanupTargets: testWorkerCleanupTargets(),
		Status:               NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
}

func runtimeCommandMentions(command netgate.PlannedCommand, needle string) bool {
	for _, arg := range command.Argv {
		if strings.Contains(arg, needle) {
			return true
		}
	}
	return false
}
