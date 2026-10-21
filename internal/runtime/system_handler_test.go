package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/coredesc"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type signalingSystemRunner struct{ called chan struct{} }

func (r signalingSystemRunner) Run(context.Context, []string) (string, error) {
	close(r.called)
	return "ok", nil
}

type fakeSystemRunner struct {
	responses map[string]fakeSystemResponse
	calls     [][]string
}

type fakeSystemResponse struct {
	output string
	err    error
}

func (r *fakeSystemRunner) Run(_ context.Context, argv []string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if resp, ok := r.responses[systemTestKey(argv)]; ok {
		return resp.output, resp.err
	}
	return "", nil
}

func TestSystemStatusReturnsObservabilitySnapshot(t *testing.T) {
	restore := stubMeminfo(t, "MemTotal: 1024 kB\nMemFree: 256 kB\nMemAvailable: 512 kB\n")
	defer restore()
	serviceName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ServiceUnit
	primaryProcess := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ProcessName
	secondaryProcess := runtimeTestWorkerDescriptors[runtimeCoreB].Process.ProcessName

	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/smartgate", "status"}):      {output: "running"},
		systemTestKey([]string{"/etc/init.d/" + serviceName, "status"}): {output: "running"},
		systemTestKey([]string{"/etc/init.d/dnsmasq", "status"}):        {output: "stopped", err: errors.New("not running")},
		systemTestKey([]string{"pidof", "smartgate"}):                   {output: "100"},
		systemTestKey([]string{"pidof", primaryProcess}):                {output: "101 102"},
		systemTestKey([]string{"pidof", "dnsmasq"}):                     {err: errors.New("no pid")},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/system/status", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	memory := data["memory"].(map[string]any)
	if memory["total_bytes"] != float64(1024*1024) || memory["used_bytes"] != float64(512*1024) {
		t.Fatalf("unexpected memory view: %+v", memory)
	}
	services := data["services"].([]any)
	if len(services) != 4 || !systemViewContainsName(services, "network") || !systemViewContainsName(services, serviceName) {
		t.Fatalf("expected registry/fabric service statuses, got %+v", services)
	}
	processes := data["processes"].([]any)
	if len(processes) != 4 || !systemViewContainsName(processes, secondaryProcess) || !systemViewContainsName(processes, primaryProcess) {
		t.Fatalf("expected registry/fabric process statuses, got %+v", processes)
	}
}

func systemViewContainsName(items []any, name string) bool {
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if ok && item["name"] == name {
			return true
		}
	}
	return false
}

func TestSystemLogsFilterAndCapBySource(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"logread", "-l", "2"}): {output: "smartgate booted\nsing-box ready\ndnsmasq ready\n"},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/system/logs?source=sing-box&limit=2", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("logs = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	lines := data["lines"].([]any)
	if len(lines) != 1 || !strings.Contains(lines[0].(string), legacyRuntimeFixture.LogSource) {
		t.Fatalf("unexpected filtered logs: %+v", lines)
	}
}

func TestSystemLogsSanitizeLegacySecrets(t *testing.T) {
	secretURL := "https://user:pass@example.com/sub?token=plain"
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"logread", "-l", "10"}): {output: "smartgate fetch " + secretURL + " password=hunter2"},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/system/logs?source=smartgate&limit=10", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("logs = %d body=%s", resp.Code, resp.Body.String())
	}
	for _, secret := range []string{secretURL, "user:pass", "/sub?token=plain", "hunter2"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("system logs leaked %q: %s", secret, resp.Body.String())
		}
	}
}

func TestSystemCommandErrorDetailsSanitizeOutput(t *testing.T) {
	secretURL := "https://user:pass@example.com/sub?token=plain"
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"logread", "-l", "100"}): {output: "failed " + secretURL, err: errors.New("token=abc123")},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/system/logs", nil)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("logs = %d body=%s", resp.Code, resp.Body.String())
	}
	for _, secret := range []string{secretURL, "user:pass", "/sub?token=plain", "abc123"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("system command error leaked %q: %s", secret, resp.Body.String())
		}
	}
}

func TestSystemServiceActionRequiresConfirmAndRunsWhitelistedCommand(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server := newSystemTestServer(t, runner)
	serviceName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ServiceUnit

	resp := serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/restart", mustJSON(t, systemActionPayload{}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected confirm failure, got %d body=%s", resp.Code, resp.Body.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("service command should not run without confirm: %+v", runner.calls)
	}

	resp = serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/restart", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("restart = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := systemTestKey(runner.calls[len(runner.calls)-1]); got != systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}) {
		t.Fatalf("unexpected command: %s", got)
	}

	resp = serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/stop", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unsupported action should fail, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSystemServiceAndProcessRegistryIncludesThirdCoreAndAppliedWorker(t *testing.T) {
	thirdDescriptor := coredesc.Descriptor{
		ID:      smartnode.CoreID("core-c"),
		Control: coredesc.Control{Port: 11000},
		Ports:   coredesc.Ports{TProxyBase: 21000, DNSBase: 22000},
		Process: coredesc.Process{
			ServiceUnit: "core-c",
			ProcessName: "core-cd",
		},
	}
	thirdWorkerID := thirdDescriptor.WorkerID(1)
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/core-c", "status"}):               {output: "running"},
		systemTestKey([]string{systemWorkerControl, "status", thirdWorkerID}): {output: "running"},
		systemTestKey([]string{"pidof", thirdDescriptor.Process.ProcessName}): {output: "303"},
	}}
	server := newSystemTestServer(t, runner)
	if err := server.cfg.CoreRegistry.Register(thirdDescriptor); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedState{
		SchemaVersion: state.AppliedStateSchemaVersion,
		SpecCaptured:  true,
		ExecutionGraph: state.ExecutionGraph{
			Workers: []state.WorkerTarget{{ID: thirdWorkerID, Core: thirdDescriptor.ID}},
		},
	}
	if err := server.cfg.Store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := server.cfg.Store.SaveSelectionState(state.SelectionStateForApplied(applied, nil)); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/system/status", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	services := data["services"].([]any)
	if !systemViewContainsName(services, thirdDescriptor.Process.ServiceUnit) || !systemViewContainsName(services, thirdWorkerID) {
		t.Fatalf("registry/applied worker services missing: %+v", services)
	}
	processes := data["processes"].([]any)
	if !systemViewContainsName(processes, thirdDescriptor.Process.ProcessName) || !systemViewContainsName(processes, thirdWorkerID) {
		t.Fatalf("registry/applied worker processes missing: %+v", processes)
	}

	resp = serve(t, server, http.MethodPost, "/api/system/service/core-c/restart", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("core-c restart = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := systemTestKey(runner.calls[len(runner.calls)-1]); got != systemTestKey([]string{"/etc/init.d/core-c", "restart"}) {
		t.Fatalf("core-c restart command=%s", got)
	}
	resp = serve(t, server, http.MethodPost, "/api/system/service/"+thirdWorkerID+"/restart", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("worker restart = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := systemTestKey(runner.calls[len(runner.calls)-1]); got != systemTestKey([]string{systemWorkerControl, "restart", thirdWorkerID}) {
		t.Fatalf("worker restart command=%s", got)
	}
	resp = serve(t, server, http.MethodPost, "/api/system/service/"+thirdWorkerID+"/reload", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("worker reload should be rejected, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSystemDataPlaneRestartStopsForAppliedSafeAndOff(t *testing.T) {
	for _, mode := range []string{state.GatewayModeSafe, state.GatewayModeOff} {
		t.Run(mode, func(t *testing.T) {
			serviceName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ServiceUnit
			processName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ProcessName
			runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
				systemTestKey([]string{"/etc/init.d/" + serviceName, "stop"}): {output: "stopped"},
				systemTestKey([]string{"pidof", processName}):                 {err: errors.New("not running")},
			}}
			server := newSystemTestServer(t, runner)
			spec := state.DefaultSpecState()
			spec.Settings.GatewayMode = mode
			applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, TopologyHash: "mode", GatewayMode: mode}, spec, nil)
			if err := server.cfg.Store.SaveAppliedState(applied); err != nil {
				t.Fatal(err)
			}
			if err := server.cfg.Store.SaveRuntime(state.RuntimeState{GatewayMode: state.GatewayModeOn}); err != nil {
				t.Fatal(err)
			}
			resp := serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/restart", mustJSON(t, systemActionPayload{Confirm: true}))
			if resp.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			expectedStop := systemTestKey([]string{"/etc/init.d/" + serviceName, "stop"})
			expectedProbe := systemTestKey([]string{"pidof", processName})
			if len(runner.calls) != 2 || systemTestKey(runner.calls[0]) != expectedStop || systemTestKey(runner.calls[1]) != expectedProbe {
				t.Fatalf("commands=%+v", runner.calls)
			}
			runtimeState, err := server.cfg.Store.LoadRuntime()
			if err != nil {
				t.Fatal(err)
			}
			if runtimeState.GatewayMode != mode {
				t.Fatalf("running mode=%q want %q", runtimeState.GatewayMode, mode)
			}
		})
	}
}

func TestSystemSafeModeRestartRejectsStopWhenProcessRemains(t *testing.T) {
	serviceName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ServiceUnit
	processName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ProcessName
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "stop"}): {output: "stopped"},
		systemTestKey([]string{"pidof", processName}):                 {output: "321"},
	}}
	server := newSystemTestServer(t, runner)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, GatewayMode: state.GatewayModeSafe}, spec, nil)
	if err := server.cfg.Store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/restart", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("persistent process accepted: status=%d body=%s", resp.Code, resp.Body.String())
	}
	observation, err := server.cfg.Store.LoadDataPlaneObservation()
	if err != nil || !observation.Planes[state.DataPlaneGateway].Running || observation.RunningGatewayMode != "" {
		t.Fatalf("persistent process misreported: %+v err=%v", observation, err)
	}
}

func TestSystemDNSFlushAndRebootRequireConfirm(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodPost, "/api/system/dns/flush", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("flush dns = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := systemTestKey(runner.calls[len(runner.calls)-1]); got != systemTestKey([]string{"/etc/init.d/dnsmasq", "restart"}) {
		t.Fatalf("unexpected dns flush command: %s", got)
	}

	resp = serve(t, server, http.MethodPost, "/api/system/reboot", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("reboot = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := systemTestKey(runner.calls[len(runner.calls)-1]); got != systemTestKey([]string{"reboot"}) {
		t.Fatalf("unexpected reboot command: %s", got)
	}
}

func TestSystemMutationWaitsForConfigCommitLane(t *testing.T) {
	layout := testLayout(t)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := commitlane.New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = manager.WithCommit(context.Background(), commitlane.CommitRequest{OpID: "active-config", Kind: "config_apply"}, func(*lockset.Session) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	called := make(chan struct{})
	done := make(chan systemCommandResult, 1)
	api := systemAPI{runner: signalingSystemRunner{called: called}, commit: manager}
	go func() {
		done <- api.runMutation(context.Background(), "system-restart-test", "system_service_restart", []string{"restart"})
	}()
	select {
	case <-called:
		t.Fatal("system mutation ran concurrently with config transaction")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-done:
		if !result.OK {
			t.Fatalf("system mutation failed after commit released: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("system mutation did not resume")
	}
}

func newSystemTestServer(t *testing.T, runner *fakeSystemRunner) *Server {
	t.Helper()
	layout := testLayout(t)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	commit := commitlane.New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	return NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Layout:       layout,
		Locks:        locks,
		CommitLane:   commit,
		Store:        store,
		CoreRegistry: newRuntimeTestCoreRegistry(t),
		ProxyControl: &restartProxyControl{current: map[string]string{}},
		SystemRunner: runner,
		Status:       NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
}

func testLayout(t *testing.T) state.Layout {
	t.Helper()
	return state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
}

func systemTestKey(argv []string) string {
	return strings.Join(argv, "\x00")
}

func stubMeminfo(t *testing.T, body string) func() {
	t.Helper()
	old := syscallOpen
	syscallOpen = func(string) (closeReader, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	return func() { syscallOpen = old }
}

func TestSystemDataPlaneRestartPublishesRunningObservationForOnMode(t *testing.T) {
	serviceName := runtimeTestWorkerDescriptors[runtimeCoreA].Process.ServiceUnit
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "restarted"},
	}}
	server := newSystemTestServer(t, runner)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	// Empty selection keeps reconcile topology-free while still exercising the
	// on/global success path that publishes RunningGatewayMode.
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3, GatewayMode: state.GatewayModeOn}, spec, nil)
	if err := server.cfg.Store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := server.cfg.Store.SaveSelectionState(state.SelectionStateForApplied(applied, nil)); err != nil {
		t.Fatal(err)
	}
	if server.cfg.Recovery == nil {
		t.Fatal("test server missing recovery orchestrator")
	}
	resp := serve(t, server, http.MethodPost, "/api/system/service/"+serviceName+"/restart", mustJSON(t, systemActionPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("restart = %d body=%s", resp.Code, resp.Body.String())
	}
	observation, err := server.cfg.Store.LoadDataPlaneObservation()
	if err != nil || !observation.Planes[state.DataPlaneGateway].Running || observation.RunningGatewayMode != state.GatewayModeOn {
		t.Fatalf("on-mode restart did not publish observation: %+v err=%v", observation, err)
	}
}
