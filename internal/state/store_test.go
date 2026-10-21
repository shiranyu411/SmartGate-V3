package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestStoreRejectsOversizedControlJSONBeforeDecode(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.SpecPath("spec"), bytes.Repeat([]byte("x"), MaxControlStateJSONBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(layout).LoadSpec()
	var limitErr *JSONResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized control state was decoded: %T %v", err, err)
	}
}

func TestStoreSeparatesSpecAndRuntime(t *testing.T) {
	t.Helper()
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewStore(layout)

	spec := DefaultSpecState()
	spec.Settings.ListenLAN = "192.168.8.1:3000"
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	runtime := RuntimeState{
		ConfigEpoch:  7,
		TopologyHash: "abc123",
		RescueActive: true,
	}
	if err := store.SaveRuntime(runtime); err != nil {
		t.Fatalf("save runtime: %v", err)
	}

	loadedSpec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if loadedSpec.Settings.ListenLAN != spec.Settings.ListenLAN {
		t.Fatalf("spec mismatch: got %q want %q", loadedSpec.Settings.ListenLAN, spec.Settings.ListenLAN)
	}

	loadedRuntime, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if loadedRuntime.ConfigEpoch != runtime.ConfigEpoch || loadedRuntime.TopologyHash != runtime.TopologyHash {
		t.Fatalf("runtime mismatch: got %+v want %+v", loadedRuntime, runtime)
	}
}

func TestSaveSpecIfChanged(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewStore(layout)
	spec := DefaultSpecState()

	wrote, err := store.SaveSpec(spec)
	if err != nil {
		t.Fatalf("first save spec: %v", err)
	}
	if !wrote {
		t.Fatal("expected first save to write")
	}

	wrote, err = store.SaveSpec(spec)
	if err != nil {
		t.Fatalf("second save spec: %v", err)
	}
	if wrote {
		t.Fatal("expected unchanged spec not to rewrite")
	}
	if err := os.Chmod(layout.SpecPath("spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(layout.SpecPath("spec"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("unchanged spec mode = %o, want 600", got)
	}
}

func TestDefaultSpecUsesParameterizedServiceRestartTask(t *testing.T) {
	spec := DefaultSpecState()
	idx := -1
	for i, task := range spec.Scheduler {
		if task.ID == "nightly-service-restart" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("default service restart task missing: %+v", spec.Scheduler)
	}
	task := spec.Scheduler[idx]
	if task.Type != SchedulerTaskRestartService || task.Spec[SchedulerTaskRestartServiceSpecKey] != SchedulerTaskRestartDefaultService() {
		t.Fatalf("service restart task not parameterized: %+v", task)
	}
}

func TestLoadSpecNormalizesLegacySchedulerAliases(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
  "schema_version": 3,
  "settings": {"gateway_mode":"on"},
  "scheduler": [
    {"id":"legacy-refresh","name":"Legacy refresh","type":"refresh_provider"},
    {"id":"legacy-restart","name":"Legacy restart","type":"restart_singbox"}
  ]
}`)
	if err := os.WriteFile(layout.SpecPath("spec"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := NewStore(layout).LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Scheduler[0].Type; got != SchedulerTaskProviderRefresh {
		t.Fatalf("refresh alias normalized to %q", got)
	}
	restart := spec.Scheduler[1]
	if restart.Type != SchedulerTaskRestartService || restart.Spec[SchedulerTaskRestartServiceSpecKey] != SchedulerTaskRestartDefaultService() {
		t.Fatalf("restart alias not normalized: %+v", restart)
	}
}

func TestLoadDataPlaneObservationAcceptsLegacyFixture(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"running_gateway_mode":"on","sing_box_running":true,"observed_at":"2026-06-25T00:00:00Z"}`)
	if err := os.WriteFile(layout.RuntimePath("data-plane-observation"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := NewStore(layout).LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Planes[DataPlaneGateway].Running {
		t.Fatalf("legacy observation did not load equivalently: %+v", observation)
	}
}

func TestLoadDataPlaneObservationAcceptsNewFixtureWithoutLegacyField(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"running_gateway_mode":"on","planes":{"gateway":{"running":true}},"observed_at":"2026-06-25T00:00:00Z"}`)
	if err := os.WriteFile(layout.RuntimePath("data-plane-observation"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := NewStore(layout).LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Planes[DataPlaneGateway].Running {
		t.Fatalf("new observation did not drive legacy view: %+v", observation)
	}
}

func TestSaveDataPlaneObservationWritesNeutralGatewayPlane(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	if err := store.SaveDataPlaneObservation(DataPlaneObservationForMode(GatewayModeOn)); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	body, err := os.ReadFile(layout.RuntimePath("data-plane-observation"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw[legacyDataPlaneRunningField()]; exists {
		t.Fatalf("data-plane observation retained legacy field: %s", body)
	}
	var planes map[string]DataPlaneComponentObservation
	if err := json.Unmarshal(raw["planes"], &planes); err != nil {
		t.Fatal(err)
	}
	if !planes[DataPlaneGateway].Running {
		t.Fatalf("data-plane observation omitted gateway plane: %s", body)
	}
}

func TestChmodFileIfNeededSkipsMatchingModeAndRepairsMismatch(t *testing.T) {
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := chmodFileIfNeeded(path, 0o600)
	if err != nil || changed {
		t.Fatalf("matching mode changed=%t err=%v", changed, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = chmodFileIfNeeded(path, 0o600)
	if err != nil || !changed {
		t.Fatalf("mismatched mode changed=%t err=%v", changed, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("repaired mode = %o, want 600", got)
	}
}

func TestLayoutEnsureTightensExistingDirectories(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := os.Chmod(layout.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(layout.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{layout.DataDir, layout.RuntimeDir, layout.SnapshotsDir(), layout.BackupsDir(), layout.LastGoodDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory %s mode = %o, want 700", dir, got)
		}
	}
}

func TestMergeNodeHealthKeepsOtherRuntimeFields(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewStore(layout)
	nextRun := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	if err := store.SaveRuntime(RuntimeState{
		ProxySelection: map[string]string{"general": "node-old"},
		Scheduler: map[string]SchedulerTaskRuntime{
			"failover-watchdog": {NextRun: &nextRun},
		},
		NodeHealth: map[string]NodeHealth{"node-old": {Status: "failed"}},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}

	if err := store.MergeNodeHealth(map[string]NodeHealth{"node-new": {Status: "ok", DelayMS: 88}}); err != nil {
		t.Fatalf("merge node health: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != "node-old" || runtimeState.Scheduler["failover-watchdog"].NextRun == nil {
		t.Fatalf("merge should preserve other runtime fields: %+v", runtimeState)
	}
	if runtimeState.NodeHealth["node-old"].Status != "failed" || runtimeState.NodeHealth["node-new"].Status != "ok" {
		t.Fatalf("node health not merged: %+v", runtimeState.NodeHealth)
	}
}

func TestAppendProviderLogsKeepsOtherRuntimeFieldsAndTrims(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewStore(layout)
	if err := store.SaveRuntime(RuntimeState{
		ProxySelection: map[string]string{"general": "node-1"},
		NodeHealth:     map[string]NodeHealth{"node-1": {Status: "ok"}},
		ProviderLogs:   []ProviderRefreshLog{{Time: time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC), Scope: "old", Status: "ok"}},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	base := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	var logs []ProviderRefreshLog
	for i := 0; i < 5; i++ {
		logs = append(logs, ProviderRefreshLog{Time: base.Add(time.Duration(i) * time.Minute), Scope: "all", Status: "ok"})
	}
	if err := store.AppendProviderLogs(logs, 3); err != nil {
		t.Fatalf("append provider logs: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != "node-1" || runtimeState.NodeHealth["node-1"].Status != "ok" {
		t.Fatalf("append logs should preserve other runtime fields: %+v", runtimeState)
	}
	if len(runtimeState.ProviderLogs) != 3 || !runtimeState.ProviderLogs[0].Time.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("provider logs should trim newest entries: %+v", runtimeState.ProviderLogs)
	}
}

func TestFailoverEventStoreAppendsAndTrims(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewFailoverEventStore(layout)
	base := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	for i := 0; i < MaxFailoverEvents+5; i++ {
		if _, err := store.Append([]FailoverEvent{{
			Time:   base.Add(time.Duration(i) * time.Minute),
			PoolID: "general",
			Kind:   "switch",
			Reason: "test",
			Status: "applied",
		}}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	events, err := store.List()
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != MaxFailoverEvents {
		t.Fatalf("event store should trim to %d, got %d", MaxFailoverEvents, len(events))
	}
	if !events[0].Time.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("event store should keep newest events, first=%s", events[0].Time)
	}
}

func TestFailoverStateStoreDefaultsAndPersists(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewFailoverStateStore(layout)
	failoverState, err := store.Load()
	if err != nil {
		t.Fatalf("load default failover state: %v", err)
	}
	if !failoverState.Enabled || failoverState.CooldownSeconds != DefaultFailoverCooldownSeconds || failoverState.Locks == nil || failoverState.Pools == nil {
		t.Fatalf("unexpected default failover state: %+v", failoverState)
	}
	failoverState.Enabled = false
	failoverState.Locks["ai"] = FailoverLock{Reason: "manual", LockedAt: time.Now().UTC()}
	if err := store.Save(failoverState); err != nil {
		t.Fatalf("save failover state: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failover state: %v", err)
	}
	if loaded.Enabled || loaded.Locks["ai"].Reason != "manual" {
		t.Fatalf("failover state not persisted: %+v", loaded)
	}
}

func TestRulesFromLegacyOverridesKeepsUnmentionedDefaults(t *testing.T) {
	defaults := RuleCatalogToLegacyGroups(DefaultRules(), DefaultBusinessPools())
	if len(defaults["block"].DomainSuffix) != 0 || len(defaults["ai"].DomainSuffix) == 0 {
		t.Fatalf("expected empty local block rules and default AI rules: %+v", defaults)
	}

	overridden := RuleCatalogToLegacyGroups(RulesFromLegacyOverrides(map[string]RuleSpec{
		"block": {DomainSuffix: []string{"ads.example"}},
	}, DefaultBusinessPools()), DefaultBusinessPools())
	if len(overridden["block"].DomainSuffix) != 1 || overridden["block"].DomainSuffix[0] != "ads.example" {
		t.Fatalf("explicit block rules should override defaults: %+v", overridden["block"])
	}
	if len(overridden["ai"].DomainSuffix) == 0 {
		t.Fatalf("unmentioned groups should keep defaults: %+v", overridden["ai"])
	}
}

func TestRuleCatalogLegacyJSONUsesGroupOverrideSemantics(t *testing.T) {
	var catalog RuleCatalog
	body := []byte(`{"block":{"domain_suffix":["ads.example"]}}`)
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	groups := RuleCatalogToLegacyGroups(catalog, DefaultBusinessPools())
	if got := groups["block"].DomainSuffix; len(got) != 1 || got[0] != "ads.example" {
		t.Fatalf("legacy block override was lost: %+v", groups["block"])
	}
	if len(groups["ai"].DomainSuffix) == 0 {
		t.Fatalf("legacy omitted groups must retain defaults: %+v", groups["ai"])
	}
}

func TestRuleCatalogLegacyJSONKeepsFirstDuplicateRuleOwner(t *testing.T) {
	var catalog RuleCatalog
	body := []byte(`{
  "ai": {"domain_suffix": ["agentrouter.org"]},
  "banking": {"domain_suffix": ["agentrouter.org"]}
}`)
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	groups := RuleCatalogToLegacyGroups(catalog, DefaultBusinessPools())
	if got := groups["ai"].DomainSuffix; len(got) != 1 || got[0] != "agentrouter.org" {
		t.Fatalf("first legacy duplicate owner was not kept: %+v", groups)
	}
	if got := groups["banking"].DomainSuffix; len(got) != 0 {
		t.Fatalf("later legacy duplicate owner was not dropped: %+v", groups["banking"])
	}
}

func TestBulkNodeHealthPreservesWatchdogEvidence(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	now := time.Now().UTC()
	if err := store.SaveRuntime(RuntimeState{NodeHealth: map[string]NodeHealth{
		"node": {Status: "failed", WatchdogStatus: "failed", WatchdogUpdatedAt: &now, WatchdogContext: NodeHealthContext{Revision: 2}, ConsecutiveFailures: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeNodeHealth(map[string]NodeHealth{"node": {Status: "unknown"}}); err != nil {
		t.Fatal(err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	health := runtimeState.NodeHealth["node"]
	if health.Status != "unknown" || health.WatchdogStatus != "failed" || health.WatchdogUpdatedAt == nil || health.ConsecutiveFailures != 1 || health.WatchdogContext.Revision != 2 {
		t.Fatalf("bulk probe changed watchdog evidence: %+v", health)
	}
}

func TestLegacyPoolsPreserveImmediateConnectionInterruption(t *testing.T) {
	legacy := SpecState{SchemaVersion: 1, BusinessPools: []BusinessPoolSpec{{ID: "general", Role: "general"}, {ID: "streaming", Role: "streaming"}, {ID: "ai", Role: "ai"}}}
	normalized := NormalizeSpecState(legacy)
	for _, pool := range normalized.BusinessPools {
		if pool.Policy.Cutover != CutoverStrict {
			t.Fatalf("V2 pool gained delayed draining: %+v", pool)
		}
	}
	explicit := DefaultSpecState()
	if got := NormalizeSpecState(explicit).BusinessPools[0].Policy.Cutover; got != CutoverGraceful {
		t.Fatal("new V3 policy changed")
	}
	legacy.BusinessPools[0].Policy = TemplatePoolPolicy("general")
	if got := NormalizeSpecState(legacy).BusinessPools[0].Policy.Cutover; got != CutoverGraceful {
		t.Fatal("explicit policy was overwritten")
	}
}
