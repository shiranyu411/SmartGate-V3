package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"smartgate/internal/netgate"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
	"strings"
	"testing"
)

func TestNetworkRepairRequiresPostAndConfirmation(t *testing.T) {
	called := false
	api := systemAPI{networkRepair: func(context.Context) error { called = true; return nil }}
	for _, tc := range []struct {
		method, body string
		code         int
	}{{"GET", "", 405}, {"POST", `{}`, 400}, {"POST", `not json`, 400}} {
		w := httptest.NewRecorder()
		api.repairNetwork(w, httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body)))
		if w.Code != tc.code || called {
			t.Fatalf("status=%d called=%v", w.Code, called)
		}
	}
}

func TestNetworkDiagnosticsDoesNotReportOffAsHealthy(t *testing.T) {
	server := newSystemTestServer(t, &fakeSystemRunner{responses: map[string]fakeSystemResponse{}})
	if err := server.cfg.Store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{GatewayMode: state.GatewayModeOff}, state.DefaultSpecState(), nil)); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodGet, "/api/system/network/diagnostics", nil)
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), `"status":"disabled"`) {
		t.Fatal(resp.Body.String())
	}
}

func TestNetworkDiagnosticsReadsDurableRescueAfterStartup(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server := newSystemTestServer(t, runner)
	server.cfg.Rescue = rescue.New(server.cfg.Layout.RescueStatusPath(), server.cfg.CommitLane, netgate.NewOwnerRegistry("smartgate"))
	if err := server.cfg.Store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{GatewayMode: state.GatewayModeOn}, state.DefaultSpecState(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := server.cfg.Store.SaveRuntime(state.RuntimeState{GatewayMode: state.GatewayModeOn}); err != nil {
		t.Fatal(err)
	}
	if err := rescue.NewStore(server.cfg.Layout.RescueStatusPath()).Save(rescue.ReasonManual, netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodGet, "/api/system/network/diagnostics", nil)
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), `"status":"disabled"`) {
		t.Fatal(resp.Body.String())
	}
	if len(runner.calls) != 0 {
		t.Fatal("diagnostics ran network probes while rescue is active")
	}
}
