package runtime

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/selection"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type schedulerFetcher struct {
	calls  int
	header http.Header
}

func (f *schedulerFetcher) Fetch(_ context.Context, req provider.FetchRequest) (provider.FetchResponse, error) {
	f.calls++
	return provider.FetchResponse{StatusCode: http.StatusOK, Header: f.header, Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`)}, nil
}

func TestSchedulerRunNowProviderRefresh(t *testing.T) {
	fetcher := &schedulerFetcher{header: http.Header{
		"Subscription-Userinfo": []string{"upload=1024; download=2048; total=8192; expire=1799999999"},
	}}
	server, store := newSchedulerTestServer(t, fetcher)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	body := mustJSON(t, schedulerRunNowPayload{Task: scheduler.TaskProviderRefresh})
	req := httptest.NewRequest(http.MethodPost, "/api/scheduler/run-now", bytes.NewReader(body))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run-now status = %d body=%s", resp.Code, resp.Body.String())
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected one refresh fetch, got %d", fetcher.calls)
	}
	if !provider.NewLastGoodStore(server.cfg.Layout).Exists("p1") {
		t.Fatal("scheduled refresh should persist last-good")
	}
	assertSchedulerRunResponseSafe(t, resp.Body.String())
}

func TestSchedulerRunNowGatewayOffReturnsLocked(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = "off"
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	body := mustJSON(t, schedulerRunNowPayload{Task: scheduler.TaskProviderRefresh})
	req := httptest.NewRequest(http.MethodPost, "/api/scheduler/run-now", bytes.NewReader(body))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked {
		t.Fatalf("run-now gateway off status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSchedulerRunNowRebootRouterRequiresConfirm(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server, _ := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/run-now", mustJSON(t, schedulerRunNowPayload{Task: scheduler.TaskRebootRouter}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("legacy run-now reboot without confirm status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 0 {
		t.Fatalf("legacy run-now reboot should not run without confirm: %+v", systemRunner.calls)
	}

	resp = serve(t, server, http.MethodPost, "/api/scheduler/run-now", mustJSON(t, schedulerRunNowPayload{Task: scheduler.TaskRebootRouter, Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("legacy run-now reboot with confirm status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"reboot"}) {
		t.Fatalf("unexpected legacy run-now reboot calls: %+v", systemRunner.calls)
	}
}

func TestSchedulerCRUDPersistsTasks(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	createResp := serve(t, server, http.MethodPost, "/api/scheduler", mustJSON(t, state.SchedulerTaskSpec{
		ID:       "custom-refresh",
		Name:     "Custom Refresh",
		Type:     "refresh_provider",
		Enabled:  true,
		Schedule: "@every 2h",
	}))
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", createResp.Code, createResp.Body.String())
	}
	createData := decodeEnvelopeData(t, createResp.Body.Bytes())
	if createData["type"] != scheduler.TaskProviderRefresh || createData["next_run"] == nil {
		t.Fatalf("unexpected create data: %+v", createData)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	idx := schedulerTaskIndex(spec.Scheduler, "custom-refresh")
	if idx < 0 || spec.Scheduler[idx].Type != scheduler.TaskProviderRefresh {
		t.Fatalf("task not persisted with normalized type: %+v", spec.Scheduler)
	}

	updateResp := serve(t, server, http.MethodPut, "/api/scheduler/custom-refresh", mustJSON(t, state.SchedulerTaskSpec{
		Name:     "Custom Backup",
		Type:     scheduler.TaskBackup,
		Enabled:  false,
		Schedule: "0 5 * * *",
	}))
	if updateResp.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", updateResp.Code, updateResp.Body.String())
	}
	updateData := decodeEnvelopeData(t, updateResp.Body.Bytes())
	if updateData["type"] != scheduler.TaskBackup || updateData["next_run"] != nil {
		t.Fatalf("unexpected update data: %+v", updateData)
	}

	toggleResp := serve(t, server, http.MethodPut, "/api/scheduler/custom-refresh", mustJSON(t, map[string]bool{"enabled": true}))
	if toggleResp.Code != http.StatusOK {
		t.Fatalf("toggle patch status = %d body=%s", toggleResp.Code, toggleResp.Body.String())
	}
	toggleData := decodeEnvelopeData(t, toggleResp.Body.Bytes())
	if toggleData["type"] != scheduler.TaskBackup || toggleData["enabled"] != true {
		t.Fatalf("partial update should preserve task fields: %+v", toggleData)
	}

	listResp := serve(t, server, http.MethodGet, "/api/scheduler", nil)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResp.Code, listResp.Body.String())
	}
	listData := decodeEnvelopeData(t, listResp.Body.Bytes())
	if _, ok := listData["tasks"].([]any); !ok {
		t.Fatalf("tasks list missing: %+v", listData)
	}

	deleteResp := serve(t, server, http.MethodDelete, "/api/scheduler/custom-refresh", nil)
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after delete: %v", err)
	}
	if schedulerTaskIndex(spec.Scheduler, "custom-refresh") >= 0 {
		t.Fatalf("task should be deleted: %+v", spec.Scheduler)
	}
}

func TestSchedulerViewsSanitizeLegacyOutputAndSpec(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	spec.Scheduler = []state.SchedulerTaskSpec{{
		ID: "legacy-sensitive", Name: "Legacy", Type: scheduler.TaskCustomShell,
		Spec: map[string]any{
			"token":  "plain-token",
			"url":    "https://user:pass@example.com/sub?token=plain",
			"nested": map[string]any{"password": "plain-password"},
		},
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{Scheduler: map[string]state.SchedulerTaskRuntime{
		"legacy-sensitive": {LastStatus: "failed", LastOutput: "fetch https://user:pass@example.com/sub?token=plain password=hunter2"},
	}}); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/scheduler", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("scheduler status=%d body=%s", resp.Code, resp.Body.String())
	}
	for _, secret := range []string{"plain-token", "user:pass", "/sub?token=plain", "plain-password", "hunter2"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("scheduler response leaked %q: %s", secret, resp.Body.String())
		}
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	tasks := data["tasks"].([]any)
	view := tasks[0].(map[string]any)
	viewSpec := view["spec"].(map[string]any)
	if viewSpec["token"] != "[REDACTED]" || viewSpec["nested"] != "[REDACTED]" {
		t.Fatalf("scheduler redaction markers missing: %+v", viewSpec)
	}
	if view["last_output"] != "自定义脚本执行失败" {
		t.Fatalf("legacy scheduler output was not localized: %+v", view)
	}
}

func TestSchedulerCompletionUsesCurrentSpec(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	spec := state.DefaultSpecState()
	spec.Scheduler = append(spec.Scheduler, state.SchedulerTaskSpec{
		ID: "changing-task", Name: "Changing task", Type: scheduler.TaskBackup,
		Enabled: true, Schedule: "@every 24h",
	})
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save current task definition: %v", err)
	}
	at := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	api := schedulerAPI{store: store, locks: server.cfg.Locks}
	if _, err := api.recordTaskRun("changing-task", "ok", "completed", at); err != nil {
		t.Fatalf("record completion: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["changing-task"]
	if record.NextRun == nil || !record.NextRun.Equal(at.Add(24*time.Hour)) {
		t.Fatalf("completion used stale schedule: next=%v want=%v", record.NextRun, at.Add(24*time.Hour))
	}

	spec.Scheduler = spec.Scheduler[:len(spec.Scheduler)-1]
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := api.recordTaskRun("changing-task", "ok", "late completion", at); err != nil {
		t.Fatalf("record completion for deleted task: %v", err)
	}
	runtimeState, _ = store.LoadRuntime()
	if _, exists := runtimeState.Scheduler["changing-task"]; exists {
		t.Fatal("late completion resurrected a deleted task runtime record")
	}
}

func TestSchedulerRunTaskByIDProviderRefresh(t *testing.T) {
	fetcher := &schedulerFetcher{header: http.Header{
		"Subscription-Userinfo": []string{"upload=1024; download=2048; total=8192; expire=1799999999"},
	}}
	server, store := newSchedulerTestServer(t, fetcher)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	resp := serve(t, server, http.MethodPost, "/api/scheduler/provider-auto-refresh/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run task status = %d body=%s", resp.Code, resp.Body.String())
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected one refresh fetch, got %d", fetcher.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["provider-auto-refresh"]
	if record.LastStatus != "ok" || record.LastRun == nil || !strings.Contains(record.LastOutput, "订阅刷新完成") {
		t.Fatalf("run record not saved: %+v", record)
	}
	assertSchedulerRunResponseSafe(t, resp.Body.String())
}

func TestSafeSchedulerResultSanitizesFailoverError(t *testing.T) {
	safe := safeSchedulerResult(scheduler.Result{Failover: &scheduler.FailoverResult{
		Error: "request https://user:pass@example.com/private?token=plain password=hunter2",
	}})
	encoded := string(mustJSON(t, safe))
	for _, secret := range []string{"user:pass", "/private?token=plain", "hunter2"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("scheduler result leaked %q: %s", secret, encoded)
		}
	}
}

func assertSchedulerRunResponseSafe(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"upload=1024", `"raw"`} {
		if strings.Contains(body, secret) {
			t.Fatalf("scheduler run response leaked %q: %s", secret, body)
		}
	}
}

func TestSchedulerRunTaskByIDBackupCreatesArchive(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	resp := serve(t, server, http.MethodPost, "/api/scheduler/daily-backup/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run backup status = %d body=%s", resp.Code, resp.Body.String())
	}
	archives, err := filepath.Glob(filepath.Join(server.cfg.Layout.BackupsDir(), "scheduled-*.tar.gz"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected one scheduled backup archive, got %+v", archives)
	}
	info, err := os.Stat(archives[0])
	if err != nil || info.Size() == 0 {
		t.Fatalf("backup archive not written: info=%+v err=%v", info, err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["daily-backup"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.LastOutput == "" {
		t.Fatalf("backup run should be recorded: %+v", record)
	}
}

func TestSchedulerRunUnsupportedTaskReturnsNotImplemented(t *testing.T) {
	server, store := newSchedulerTestServer(t, &schedulerFetcher{})
	createResp := serve(t, server, http.MethodPost, "/api/scheduler", mustJSON(t, state.SchedulerTaskSpec{
		ID:       "custom-shell",
		Name:     "Custom shell",
		Type:     scheduler.TaskCustomShell,
		Enabled:  false,
		Schedule: "",
	}))
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create custom shell status = %d body=%s", createResp.Code, createResp.Body.String())
	}
	resp := serve(t, server, http.MethodPost, "/api/scheduler/custom-shell/run", nil)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("run unsupported status = %d body=%s", resp.Code, resp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["custom-shell"]
	if record.LastStatus != "unsupported" {
		t.Fatalf("unsupported run should be recorded: %+v", record)
	}
}

func TestSchedulerRunDelayTestPersistsNodeHealth(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newSchedulerTestServerWithControl(t, &schedulerFetcher{}, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil || len(nodes) != 1 {
		t.Fatalf("build scheduler delay nodes: nodes=%+v err=%v", nodes, err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save scheduler delay pool: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/node-delay-test/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run delay test status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.delayOutbound == "" {
		t.Fatalf("delay test should call worker route control")
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if len(runtimeState.NodeHealth) != 1 {
		t.Fatalf("node health not persisted: %+v", runtimeState.NodeHealth)
	}
	for _, health := range runtimeState.NodeHealth {
		if health.Status != "ok" || health.DelayMS != 123 || health.Context.Method != "worker_route_http_204" || health.Context.Worker == "" {
			t.Fatalf("unexpected health: %+v", health)
		}
	}
	record := runtimeState.Scheduler["node-delay-test"]
	if record.LastStatus != "ok" || record.LastOutput == "" {
		t.Fatalf("scheduler record not saved: %+v", record)
	}
}

func TestSchedulerRunTaskByIDApplyConfig(t *testing.T) {
	server, store, netGateRunner := newSchedulerConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Scheduler = append(spec.Scheduler, state.SchedulerTaskSpec{
		ID:       "apply-now",
		Name:     "Apply now",
		Type:     scheduler.TaskApplyConfig,
		Enabled:  true,
		Schedule: "@every 1h",
	})
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	resp := serve(t, server, http.MethodPost, "/api/scheduler/apply-now/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run apply_config status = %d body=%s", resp.Code, resp.Body.String())
	}
	if netGateRunner.calls == 0 {
		t.Fatalf("expected graph/Fabric apply, openwrt=%d", netGateRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ConfigEpoch != 1 || runtimeState.TopologyHash == "" {
		t.Fatalf("config runtime not advanced: %+v", runtimeState)
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		t.Fatalf("load applied state: %v", err)
	}
	if len(appliedState.ExecutionGraph.Workers) == 0 || !server.cfg.Workers.IsRunning(appliedState.ExecutionGraph.Workers[0].ID) {
		t.Fatalf("scheduler run did not activate graph worker: %+v", appliedState.ExecutionGraph.Workers)
	}
	record := runtimeState.Scheduler["apply-now"]
	if record.LastStatus != "ok" || !strings.Contains(record.LastOutput, "OpenWrt 步骤") {
		t.Fatalf("apply run should be recorded: %+v", record)
	}
}

func TestSchedulerRunTaskByIDApplyConfigRejectsLowOverlaySpace(t *testing.T) {
	server, store, netGateRunner := newSchedulerConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Scheduler = append(spec.Scheduler, state.SchedulerTaskSpec{
		ID:       "apply-now",
		Name:     "Apply now",
		Type:     scheduler.TaskApplyConfig,
		Enabled:  true,
		Schedule: "@every 1h",
	})
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	oldDiskUsage := configDiskUsage
	configDiskUsage = func(string) (systemDiskView, error) {
		return systemDiskView{Path: "/overlay", FreeBytes: 1024 * 1024}, nil
	}
	defer func() { configDiskUsage = oldDiskUsage }()

	resp := serve(t, server, http.MethodPost, "/api/scheduler/apply-now/run", nil)
	if resp.Code != http.StatusInsufficientStorage {
		t.Fatalf("run apply_config low space status = %d body=%s", resp.Code, resp.Body.String())
	}
	if netGateRunner.calls != 0 {
		t.Fatalf("apply should not run commands after guard rejection, openwrt=%d", netGateRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["apply-now"]
	if record.LastStatus != "failed" || record.LastOutput != "配置应用执行失败" {
		t.Fatalf("low-space apply should be recorded as failed: %+v", record)
	}
}

func TestSchedulerRunTaskByIDRestartsConfiguredService(t *testing.T) {
	serviceName := state.SchedulerTaskRestartDefaultService()
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "restarted"},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/nightly-service-restart/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run restart_singbox status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}) {
		t.Fatalf("unexpected system runner calls: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["nightly-service-restart"]
	if record.LastStatus != "ok" || record.LastOutput != serviceName+" 服务重启完成" {
		t.Fatalf("restart run should be recorded: %+v", record)
	}
}

func TestSchedulerRunTaskByIDLegacyServiceRestartAlias(t *testing.T) {
	serviceName := state.SchedulerTaskRestartDefaultService()
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "restarted"},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	spec.Scheduler = append(spec.Scheduler, state.SchedulerTaskSpec{
		ID:   "legacy-restart",
		Name: "Legacy restart",
		Type: "restart_singbox",
	})
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodPost, "/api/scheduler/legacy-restart/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run legacy restart status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}) {
		t.Fatalf("unexpected system runner calls: %+v", systemRunner.calls)
	}
	saved, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	idx := schedulerTaskIndex(saved.Scheduler, "legacy-restart")
	if idx < 0 || saved.Scheduler[idx].Type != scheduler.TaskRestartService || saved.Scheduler[idx].Spec[state.SchedulerTaskRestartServiceSpecKey] != state.SchedulerTaskRestartDefaultService() {
		t.Fatalf("legacy restart was not normalized in spec: %+v", saved.Scheduler)
	}
}

func TestSchedulerServiceRestartRestoresDurableNonDefaultSelection(t *testing.T) {
	serviceName := state.SchedulerTaskRestartDefaultService()
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "restarted"},
	}}
	control := &restartProxyControl{current: map[string]string{}}
	server, store := newSchedulerSystemTestServerWithControl(t, systemRunner, control)
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	spec.Settings.GatewayMode = state.GatewayModeOn
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider", Type: provider.TypeManual, Enabled: true}}
	raw := []map[string]any{
		{"type": "vmess", "tag": "first", "server": "first.example"},
		{"type": "vmess", "tag": "second", "server": "second.example"},
	}
	nodes := provider.NormalizeProviderOutbounds(spec.Providers[0], raw)
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{nodes[0].ID, nodes[1].ID}}}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 4, TopologyHash: "restart-topology", GatewayMode: state.GatewayModeOn,
		ProxySelection: map[string]string{"general": nodes[0].ID},
	}, spec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: raw}})
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": nodes[1].ID})); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 4, TopologyHash: "restart-topology", GatewayMode: state.GatewayModeOn, ProxySelection: map[string]string{"general": nodes[1].ID}}); err != nil {
		t.Fatal(err)
	}
	control.current["Pool_general"] = nodes[0].ID

	resp := serve(t, server, http.MethodPost, "/api/scheduler/nightly-service-restart/run", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("restart with selector reconcile status = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := control.current["Pool_general"]; got != nodes[1].ID {
		t.Fatalf("restart did not restore durable selection: got %s want %s", got, nodes[1].ID)
	}
	if len(control.selectCalls) != 1 || control.selectCalls[0] != "Pool_general="+nodes[1].ID {
		t.Fatalf("unexpected selector reconcile calls: %+v", control.selectCalls)
	}
}

func TestSchedulerRunTaskByIDServiceRestartFailureReturnsBadGateway(t *testing.T) {
	serviceName := state.SchedulerTaskRestartDefaultService()
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "boom", err: os.ErrPermission},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/nightly-service-restart/run", nil)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("run restart_singbox failure status = %d body=%s", resp.Code, resp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["nightly-service-restart"]
	if record.LastStatus != "failed" || record.LastOutput != "服务重启执行失败" {
		t.Fatalf("restart failure should be recorded: %+v", record)
	}
}

func TestSchedulerRunTaskByIDRebootRouterRequiresConfirm(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/weekly-router-reboot/run", nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("run reboot without confirm status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 0 {
		t.Fatalf("reboot should not run without confirm: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.LastRun != nil || record.LastStatus != "" {
		t.Fatalf("reboot without confirm should not be recorded as a run: %+v", record)
	}
}

func TestSchedulerRunTaskByIDRebootRouter(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"reboot"}): {output: "rebooting"},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/weekly-router-reboot/run", mustJSON(t, schedulerRunTaskPayload{Confirm: true}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("run reboot_router status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"reboot"}) {
		t.Fatalf("unexpected system runner calls: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.LastStatus != "ok" || record.LastOutput != "路由器重启命令已执行" {
		t.Fatalf("reboot run should be recorded: %+v", record)
	}
}

func TestSchedulerRunTaskByIDRebootRouterFailureReturnsBadGateway(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"reboot"}): {output: "boom", err: os.ErrPermission},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)

	resp := serve(t, server, http.MethodPost, "/api/scheduler/weekly-router-reboot/run", mustJSON(t, schedulerRunTaskPayload{Confirm: true}))
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("run reboot_router failure status = %d body=%s", resp.Code, resp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.LastStatus != "failed" || record.LastOutput != "路由器重启执行失败" {
		t.Fatalf("reboot failure should be recorded: %+v", record)
	}
}

func newSchedulerTestServer(t *testing.T, fetcher provider.Fetcher) (*Server, *state.Store) {
	return newSchedulerTestServerWithControl(t, fetcher, nil)
}

func newSchedulerTestServerWithControl(t *testing.T, fetcher provider.Fetcher, control *fakeProxyControl) (*Server, *state.Store) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	if control != nil {
		attachFakeSelectorLookup(control, store)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	workers, err := worker.NewManager(
		runtimeTestWorkerAdapter{core: runtimeCoreA, control: control},
		runtimeTestWorkerAdapter{core: runtimeCoreB, control: control},
	)
	if err != nil {
		t.Fatalf("new scheduler worker manager: %v", err)
	}
	fabricManager := fabric.NewManager(owners, runtimeTestFabricRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)), fetcher)
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, state.NewSnapshotStore(layout), layout)
	runner := scheduler.NewRunner(store, refresher).WithBackupCreator(testSchedulerBackupCreator{txn: backupTxn})
	if control != nil {
		runner.WithDelayTester(layout, NewSchedulerRuntimeStore(store, locks), NewSchedulerNodeDelayTester(Config{Store: store, Layout: layout, Locks: locks, Workers: workers}))
	}
	cfg := Config{
		ListenAddr:  "127.0.0.1:3000",
		Layout:      layout,
		Locks:       locks,
		CommitLane:  commitMgr,
		Store:       store,
		BackupTxn:   backupTxn,
		ProviderRef: refresher,
		Scheduler:   runner,
		Owners:      owners,
		Workers:     workers,
		Fabric:      fabricManager,
		Status:      NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	}
	if control != nil {
		cfg.ProxyControl = control
	}
	server := NewServer(cfg)
	t.Cleanup(func() {
		_ = fabricManager.Stop(context.Background())
		_ = workers.StopAll(context.Background())
	})
	return server, store
}

func newSchedulerSystemTestServer(t *testing.T, systemRunner *fakeSystemRunner) (*Server, *state.Store) {
	return newSchedulerSystemTestServerWithControl(t, systemRunner, nil)
}

func newSchedulerSystemTestServerWithControl(t *testing.T, systemRunner *fakeSystemRunner, control selection.ProxyController) (*Server, *state.Store) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	if control == nil {
		control = &restartProxyControl{current: map[string]string{}}
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)), &schedulerFetcher{})
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, state.NewSnapshotStore(layout), layout)
	runner := scheduler.NewRunner(store, refresher).WithBackupCreator(testSchedulerBackupCreator{txn: backupTxn})
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Layout:       layout,
		Locks:        locks,
		CommitLane:   commitMgr,
		Store:        store,
		BackupTxn:    backupTxn,
		ProviderRef:  refresher,
		Scheduler:    runner,
		Owners:       owners,
		SystemRunner: systemRunner,
		CoreRegistry: newRuntimeTestCoreRegistry(t),
		ProxyControl: control,
		Status:       NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}

type restartProxyControl struct {
	current     map[string]string
	selectCalls []string
}

func (c *restartProxyControl) Select(_ context.Context, selector, target string) error {
	c.current[selector] = target
	c.selectCalls = append(c.selectCalls, selector+"="+target)
	return nil
}

func (c *restartProxyControl) Selected(_ context.Context, selector string) (string, error) {
	value := c.current[selector]
	if value == "" {
		return "", fmt.Errorf("selector %s has no current target", selector)
	}
	return value, nil
}

func (c *restartProxyControl) Delay(context.Context, string, string, time.Duration) (selection.DelayResult, error) {
	return selection.DelayResult{}, nil
}

func (c *restartProxyControl) Connections(context.Context) (map[string]any, error) { return nil, nil }
func (c *restartProxyControl) CloseConnections(context.Context) error              { return nil }
func (c *restartProxyControl) CloseConnection(context.Context, string) error       { return nil }

func newSchedulerConfigTestServer(t *testing.T) (*Server, *state.Store, *configTestRunner) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)), &schedulerFetcher{})
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, state.NewSnapshotStore(layout), layout)
	runner := scheduler.NewRunner(store, refresher).WithBackupCreator(testSchedulerBackupCreator{txn: backupTxn})
	netGateRunner := &configTestRunner{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, nil, netGateRunner)
	server := NewServer(Config{
		ListenAddr:    "127.0.0.1:3000",
		Layout:        layout,
		Locks:         locks,
		CommitLane:    commitMgr,
		Store:         store,
		BackupTxn:     backupTxn,
		ProviderRef:   refresher,
		Scheduler:     runner,
		Owners:        owners,
		NetGateRunner: netGateRunner,
		Workers:       workers,
		Fabric:        fabricManager,
		CoreRegistry:  newRuntimeTestCoreRegistry(t),
		Status:        NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store, netGateRunner
}

type testSchedulerBackupCreator struct {
	txn *commitlane.BackupTxn
}

func (c testSchedulerBackupCreator) Create(ctx context.Context, opID, kind string) (scheduler.BackupResult, error) {
	record, err := c.txn.Create(ctx, opID, kind)
	if err != nil {
		return scheduler.BackupResult{}, err
	}
	return scheduler.BackupResult{
		ID:        record.ID,
		Path:      record.Path,
		Size:      record.Size,
		SHA256:    record.SHA256,
		CreatedAt: record.CreatedAt,
	}, nil
}
