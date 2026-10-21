package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
)

type configTestRunner struct {
	calls    int
	failAt   int
	failErr  error
	failOnce bool
}

func (r *configTestRunner) Run(_ context.Context, _ []string) (string, error) {
	r.calls++
	if r.failAt > 0 && r.calls == r.failAt {
		err := r.failErr
		if err == nil {
			err = fmt.Errorf("runner failed at call %d", r.calls)
		}
		if r.failOnce {
			r.failAt = 0
		}
		return "failed", err
	}
	return "restarted", nil
}

func TestConfigGatewayOnOverviewAndApply(t *testing.T) {
	server, store, netGateRunner := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	stateResp := serve(t, server, http.MethodGet, "/api/config/state", nil)
	if stateResp.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", stateResp.Code, stateResp.Body.String())
	}
	stateData := decodeEnvelopeData(t, stateResp.Body.Bytes())
	if stateData["dirty"] != true {
		t.Fatalf("expected dirty state, got %+v", stateData)
	}

	modeResp := serve(t, server, http.MethodPost, "/api/gateway/mode", mustJSON(t, map[string]string{"mode": "on"}))
	if modeResp.Code != http.StatusOK {
		t.Fatalf("gateway mode status = %d body=%s", modeResp.Code, modeResp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if spec.Settings.GatewayMode != "on" {
		t.Fatalf("gateway mode not saved: %+v", spec.Settings)
	}

	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "config-test"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	dryRunData := decodeEnvelopeData(t, dryRunResp.Body.Bytes())
	dryRunID, _ := dryRunData["id"].(string)
	if dryRunID != "config-test" {
		t.Fatalf("unexpected dryrun id: %+v", dryRunData)
	}

	overviewResp := serve(t, server, http.MethodGet, "/api/overview", nil)
	if overviewResp.Code != http.StatusOK {
		t.Fatalf("overview status = %d body=%s", overviewResp.Code, overviewResp.Body.String())
	}
	overviewData := decodeEnvelopeData(t, overviewResp.Body.Bytes())
	if overviewData["gateway"] == nil || overviewData["config"] == nil {
		t.Fatalf("overview missing gateway/config: %+v", overviewData)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "config-test"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	applyData := decodeEnvelopeData(t, applyResp.Body.Bytes())
	if applyData["resource_guard"] == nil {
		t.Fatalf("apply response missing resource guard: %+v", applyData)
	}
	if _, exists := applyData[legacyRuntimeFixture.ApplyField]; exists {
		t.Fatalf("apply response contains retired implementation field: %+v", applyData)
	}
	if netGateRunner.calls == 0 {
		t.Fatal("expected OpenWrt apply commands to run")
	}
	workers, _ := applyData["workers"].([]any)
	if len(workers) == 0 {
		t.Fatalf("apply response missing worker activations: %+v", applyData)
	}
	coreResults, _ := applyData["core_results"].([]any)
	if len(coreResults) == 0 || coreResults[0].(map[string]any)["core"] == "" {
		t.Fatalf("apply response missing neutral core results: %+v", applyData)
	}
	stateResp = serve(t, server, http.MethodGet, "/api/config/state", nil)
	stateData = decodeEnvelopeData(t, stateResp.Body.Bytes())
	if stateData["dirty"] != false {
		t.Fatalf("expected clean state after apply, got %+v", stateData)
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		t.Fatalf("load applied state: %v", err)
	}
	if appliedState.ConfigEpoch == 0 || appliedState.TopologyHash == "" || appliedState.ConfigSHA256 == "" || appliedState.LastAppliedAt == nil {
		t.Fatalf("applied state was not persisted: %+v", appliedState)
	}
	if len(appliedState.ExecutionGraph.Workers) == 0 || !server.cfg.Workers.IsRunning(appliedState.ExecutionGraph.Workers[0].ID) {
		t.Fatalf("worker graph was not activated: %+v", appliedState.ExecutionGraph.Workers)
	}
}

func TestRuleConflictDryRunBlocksAndApplyRejectsDirectCall(t *testing.T) {
	conflictingSpec := func() state.SpecState {
		spec := state.DefaultSpecState()
		spec.Rules = append(spec.Rules,
			state.Rule{ID: "user-conflict-ai", Source: state.RuleSourceUser, Kind: state.RuleKindDomainSuffix, Value: "conflict.example", PoolID: "ai", Enabled: true},
			state.Rule{ID: "user-conflict-streaming", Source: state.RuleSourceUser, Kind: state.RuleKindDomainSuffix, Value: "conflict.example", PoolID: "streaming", Enabled: true},
		)
		return spec
	}

	t.Run("dry-run exposes one blocking conflict", func(t *testing.T) {
		server, store, _ := newConfigTestServer(t)
		if _, err := store.SaveSpec(conflictingSpec()); err != nil {
			t.Fatal(err)
		}
		dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "rule-conflict-dryrun"}))
		if dryRunResp.Code != http.StatusUnprocessableEntity {
			t.Fatalf("dry-run status=%d body=%s", dryRunResp.Code, dryRunResp.Body.String())
		}
		code, details := decodeEnvelopeError(t, dryRunResp.Body.Bytes())
		if code != "execution_graph_invalid" {
			t.Fatalf("dry-run error code=%s details=%+v", code, details)
		}
		if aliases := details["legacy_error_codes"].([]any); len(aliases) != 1 || aliases[0] != "validation_failed" {
			t.Fatalf("dry-run legacy aliases mismatch: %+v", details)
		}
		dryRun, ok := details["dryrun"].(map[string]any)
		if !ok {
			t.Fatalf("dry-run error did not include preview: %+v", details)
		}
		changes, ok := dryRun["changes"].([]any)
		if !ok {
			t.Fatalf("dry-run preview missing changes: %+v", dryRun)
		}
		blocking := 0
		for _, raw := range changes {
			change, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("dry-run change is %T: %+v", raw, raw)
			}
			if change["category"] == "rule-conflict" && change["blocking"] == true {
				blocking++
			}
		}
		if blocking != 1 {
			t.Fatalf("dry-run should expose one blocking conflict, got %d changes=%+v", blocking, changes)
		}
	})

	t.Run("apply independently revalidates conflicts", func(t *testing.T) {
		server, store, _ := newConfigTestServer(t)
		baselineResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "rule-conflict-baseline"}))
		if baselineResp.Code != http.StatusOK {
			t.Fatalf("baseline dry-run status=%d body=%s", baselineResp.Code, baselineResp.Body.String())
		}
		baseline := decodeEnvelopeAs[configDryRunView](t, baselineResp.Body.Bytes())
		if _, err := store.SaveSpec(conflictingSpec()); err != nil {
			t.Fatal(err)
		}
		applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": baseline.ID}))
		if applyResp.Code != http.StatusUnprocessableEntity {
			t.Fatalf("apply status=%d body=%s", applyResp.Code, applyResp.Body.String())
		}
		var envelope struct {
			OK    bool            `json:"ok"`
			Error apiErrorPayload `json:"error"`
		}
		if err := json.Unmarshal(applyResp.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.OK || envelope.Error.Code != "rule_conflict_unresolved" {
			t.Fatalf("apply did not return stable conflict code: %+v", envelope)
		}
	})
}

func TestConfigStateSeparatesSavedAppliedAndRunningGatewayModes(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	appliedSpec := state.DefaultSpecState()
	appliedSpec.Settings.GatewayMode = state.GatewayModeOn
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(appliedSpec, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 7, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOn,
	}, appliedSpec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 7, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOn}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDataPlaneObservation(state.DataPlaneObservationForGateway(state.GatewayModeOn, true)); err != nil {
		t.Fatal(err)
	}
	savedSpec := appliedSpec
	savedSpec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(savedSpec); err != nil {
		t.Fatal(err)
	}

	stateResp := serve(t, server, http.MethodGet, "/api/config/state", nil)
	stateData := decodeEnvelopeData(t, stateResp.Body.Bytes())
	if stateData["saved_gateway_mode"] != state.GatewayModeOff || stateData["gateway_mode"] != state.GatewayModeOff || stateData["applied_gateway_mode"] != state.GatewayModeOn || stateData["running_gateway_mode"] != state.GatewayModeOn {
		t.Fatalf("gateway modes were conflated: %+v", stateData)
	}
	overviewResp := serve(t, server, http.MethodGet, "/api/overview", nil)
	overview := decodeEnvelopeData(t, overviewResp.Body.Bytes())
	gateway, _ := overview["gateway"].(map[string]any)
	if gateway["mode"] != state.GatewayModeOn || gateway["saved_mode"] != state.GatewayModeOff || gateway["applied_mode"] != state.GatewayModeOn || gateway["running_mode"] != state.GatewayModeOn {
		t.Fatalf("overview misreported saved mode as running: %+v", gateway)
	}
}

func TestOverviewKeepsSavedRefreshInPendingMetrics(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	appliedSpec := state.DefaultSpecState()
	appliedSpec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider", Type: provider.TypeManual, Enabled: true}}
	appliedSpec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	oldRaw := []map[string]any{{"type": "vmess", "tag": "old", "server": "old.example"}}
	oldHash, err := provider.HashEffectiveTopologyFromSnapshots(appliedSpec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: oldRaw}})
	if err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3, TopologyHash: oldHash, GatewayMode: state.GatewayModeOn}, appliedSpec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: oldRaw}})
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 3, TopologyHash: oldHash, GatewayMode: state.GatewayModeOn}); err != nil {
		t.Fatal(err)
	}
	saved := appliedSpec
	saved.BusinessPools = append(saved.BusinessPools, state.BusinessPoolSpec{ID: "ai", Name: "AI", Role: "ai"})
	if _, err := store.SaveSpec(saved); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{{"type": "vmess", "tag": "new-1", "server": "one.example"}, {"type": "vmess", "tag": "new-2", "server": "two.example"}}); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/overview", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("overview=%d %s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if nodes := data["nodes"].(map[string]any); nodes["total"] != float64(1) {
		t.Fatalf("running nodes polluted: %+v", nodes)
	}
	if pools := data["business_pools"].(map[string]any); pools["total"] != float64(1) {
		t.Fatalf("running pools polluted: %+v", pools)
	}
	pending := data["pending"].(map[string]any)
	if pending["dirty"] != true || pending["nodes"].(map[string]any)["total"] != float64(2) || pending["business_pools"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("unexpected pending metrics: %+v", pending)
	}
}

func TestConfigApplyRestoresProxySelections(t *testing.T) {
	server, store, netGateRunner, _ := newConfigTestServerWithControl(t)
	spec := state.DefaultSpecState()
	nodeID := configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	setRuntimeTestPoolNodeIDs(t, &spec, "general", nodeID)
	setRuntimeTestPoolNodeIDs(t, &spec, "ai", nodeID)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"ai": nodeID}}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := state.NewFailoverStateStore(server.cfg.Layout).Save(state.FailoverState{
		Enabled:         true,
		CooldownSeconds: state.DefaultFailoverCooldownSeconds,
		Locks: map[string]state.FailoverLock{
			"proxy": {Reason: "legacy", LockedAt: now},
		},
		Pools: map[string]state.FailoverPoolState{
			"ai": {
				PoolID:        "ai",
				Role:          "ai",
				Current:       "Block",
				Previous:      nodeID,
				LastKind:      "fail_closed",
				LastReason:    "no_healthy_sensitive_pool_candidate",
				LastChangedAt: &now,
				Terminal:      true,
			},
			"proxy": {
				PoolID:        "proxy",
				Role:          "general",
				Current:       "Direct",
				LastChangedAt: &now,
			},
		},
	}); err != nil {
		t.Fatalf("save failover state: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "restore-selection"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "restore-selection"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	if netGateRunner.calls == 0 {
		t.Fatal("expected Gateway Fabric network apply commands to run")
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatalf("load failover state: %v", err)
	}
	poolState := failoverState.Pools["ai"]
	if poolState.Current != nodeID || poolState.Previous != "" || poolState.Terminal {
		t.Fatalf("expected failover state to reconcile with restored selector, got %+v", poolState)
	}
	if poolState.LastKind != "restore" || poolState.LastReason != "config_apply_restore_proxy_selection" {
		t.Fatalf("unexpected reconciled failover metadata: %+v", poolState)
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		t.Fatalf("load applied state: %v", err)
	}
	if appliedState.ProxySelection["ai"] != nodeID {
		t.Fatalf("restored selector was not persisted durably: %+v", appliedState.ProxySelection)
	}
}

func TestConfigApplyRejectsSelectionChangedAfterDryRun(t *testing.T) {
	server, store, _, control := newConfigTestServerWithControl(t)
	raw := []map[string]any{
		{"type": "vmess", "tag": "node-a", "server": "a.example"},
		{"type": "vmess", "tag": "node-b", "server": "b.example"},
	}
	spec := state.DefaultSpecState()
	nodes := configureRuntimeManualProviderNodes(t, server.cfg.Layout, &spec, "manual", raw)
	if len(nodes) != 2 {
		t.Fatalf("expected two graph-ready nodes, got %+v", nodes)
	}
	setRuntimeTestPoolNodeIDs(t, &spec, "general", nodes[0].ID, nodes[1].ID)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	dry := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "latest-selector"}))
	if dry.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d body=%s", dry.Code, dry.Body.String())
	}
	lastGood, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]string{"general": nodes[1].ID}
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{ProxySelection: latest, GatewayMode: state.GatewayModeOn}, spec, lastGood)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: latest}); err != nil {
		t.Fatal(err)
	}
	control.selectCalls = nil
	resp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "latest-selector"}))
	if resp.Code != http.StatusConflict {
		t.Fatalf("apply selection drift status=%d body=%s", resp.Code, resp.Body.String())
	}
	if len(control.selectCalls) != 0 {
		t.Fatalf("selection drift touched data plane: %+v", control.selectCalls)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.ProxySelection["general"] != nodes[1].ID {
		t.Fatalf("latest selector was not persisted: %+v", applied.ProxySelection)
	}
}

func TestConfigApplySynchronizesStaleFailoverStateWithoutProxySelection(t *testing.T) {
	server, store, _, _ := newConfigTestServerWithControl(t)
	spec := state.DefaultSpecState()
	nodeID := configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	setRuntimeTestPoolNodeIDs(t, &spec, "ai", nodeID)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := state.NewFailoverStateStore(server.cfg.Layout).Save(state.FailoverState{
		Enabled:         true,
		CooldownSeconds: state.DefaultFailoverCooldownSeconds,
		Pools: map[string]state.FailoverPoolState{
			"ai": {
				PoolID:        "ai",
				Role:          "ai",
				Current:       "Block",
				Previous:      nodeID,
				LastKind:      "fail_closed",
				LastReason:    "no_healthy_sensitive_pool_candidate",
				LastChangedAt: &now,
				Terminal:      true,
			},
		},
	}); err != nil {
		t.Fatalf("save failover state: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "sync-stale-failover"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "sync-stale-failover"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatalf("load failover state: %v", err)
	}
	poolState := failoverState.Pools["ai"]
	if poolState.Current != nodeID || poolState.Previous != "" || poolState.Terminal {
		t.Fatalf("expected stale failover state to sync to active selector, got %+v", poolState)
	}
	if poolState.LastKind != "sync" || poolState.LastReason != "config_apply_sync_selector_state" {
		t.Fatalf("unexpected synced failover metadata: %+v", poolState)
	}
	if _, ok := failoverState.Pools["proxy"]; ok {
		t.Fatalf("orphan legacy proxy pool should be removed: %+v", failoverState.Pools)
	}
	if _, ok := failoverState.Locks["proxy"]; ok {
		t.Fatalf("orphan legacy proxy lock should be removed: %+v", failoverState.Locks)
	}
}

func TestConfigGatewaySafeModeAppliesCleanupWithoutCoreCandidate(t *testing.T) {
	server, store, netGateRunner := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "safe-mode"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	record, err := configAPI{store: store, layout: server.cfg.Layout}.loadDryRun()
	if err != nil {
		t.Fatalf("load dry-run: %v", err)
	}
	if record.GatewayMode != state.GatewayModeSafe || record.RuntimeAction != "cleanup_safe_direct" {
		t.Fatalf("dry-run missing safe cleanup action: %+v", record)
	}
	if record.CoreCandidate != nil {
		t.Fatalf("safe mode should not build a core candidate: %+v", record)
	}
	if record.OpenWrtPlan == nil || len(record.OpenWrtPlan.Steps) == 0 || !record.OpenWrtPlan.Steps[0].Destructive {
		t.Fatalf("safe mode should preview cleanup plan: %+v", record.OpenWrtPlan)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "safe-mode"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	applyData := decodeEnvelopeData(t, applyResp.Body.Bytes())
	if applyData["runtime_action"] != "cleanup_safe_direct" {
		t.Fatalf("apply response missing safe cleanup action: %+v", applyData)
	}
	if netGateRunner.calls == 0 {
		t.Fatal("safe mode should execute cleanup OpenWrt commands")
	}
	stateResp := serve(t, server, http.MethodGet, "/api/config/state", nil)
	stateData := decodeEnvelopeData(t, stateResp.Body.Bytes())
	if stateData["dirty"] != false {
		t.Fatalf("expected clean state after safe apply, got %+v", stateData)
	}
}

func TestConfigGatewayOffModeAppliesCleanupWithoutCoreCandidate(t *testing.T) {
	server, store, netGateRunner := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	resp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "off-mode"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", resp.Code, resp.Body.String())
	}
	record, err := configAPI{store: store, layout: server.cfg.Layout}.loadDryRun()
	if err != nil {
		t.Fatalf("load dry-run: %v", err)
	}
	if record.RuntimeAction != "cleanup_off" || record.CoreCandidate != nil {
		t.Fatalf("off mode should only build cleanup plan: %+v", record)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "off-mode"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	if netGateRunner.calls == 0 {
		t.Fatal("off mode should execute cleanup OpenWrt commands")
	}
}

func TestConfigGatewayGlobalModeBuildsGlobalGraph(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	nodeID := configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	setRuntimeTestPoolNodeIDs(t, &spec, "general", nodeID)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	modeResp := serve(t, server, http.MethodPost, "/api/gateway/mode", mustJSON(t, map[string]string{"mode": state.GatewayModeGlobal}))
	if modeResp.Code != http.StatusOK {
		t.Fatalf("gateway global mode status = %d body=%s", modeResp.Code, modeResp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if spec.Settings.GatewayMode != state.GatewayModeGlobal {
		t.Fatalf("global mode not saved: %+v", spec.Settings)
	}

	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "global-mode"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	record, err := configAPI{store: store, layout: server.cfg.Layout}.loadDryRun()
	if err != nil {
		t.Fatalf("load dry-run: %v", err)
	}
	if record.GatewayMode != state.GatewayModeGlobal || record.GraphHash == "" || record.WorkerPrepared == nil || record.FabricPrepared == nil {
		t.Fatalf("dry-run missing global graph artifacts: %+v", record)
	}
	if record.CoreCandidate != nil {
		t.Fatalf("config dry-run should not build legacy core candidate: %+v", record)
	}
	if len(record.ExecutionGraph.Workers) == 0 || len(record.ExecutionGraph.Fabric.Dispatch) == 0 {
		t.Fatalf("global graph missing workers or fabric dispatch: %+v", record.ExecutionGraph)
	}
	if record.ExecutionGraph.Fabric.FallbackPool != "general" {
		t.Fatalf("global graph fallback pool = %q, want general", record.ExecutionGraph.Fabric.FallbackPool)
	}
}

func TestConfigGatewayModeRejectsUnknown(t *testing.T) {
	server, _, _ := newConfigTestServer(t)
	resp := serve(t, server, http.MethodPost, "/api/gateway/mode", mustJSON(t, map[string]string{"mode": "mystery"}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unknown gateway mode status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConfigGatewayModeDefaultsLegacyEmptyModeToOn(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = ""
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	resp := serve(t, server, http.MethodGet, "/api/gateway/mode", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("gateway mode status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["mode"] != state.GatewayModeOn {
		t.Fatalf("legacy empty gateway mode should read as on, got %+v", data)
	}
}

func TestConfigDryRunCheckFailureReturnsBoundedRedactedView(t *testing.T) {
	server, store, _, control := newConfigTestServerWithControl(t)
	secret := "cookie-secret-value"
	path := "/private/tmp/smartgate/runtime/candidates/secret/config.json"
	control.workerPrepareErr = fmt.Errorf("candidate invalid: token=%s path=%s", secret, path)
	spec := state.DefaultSpecState()
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	resp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "bad-candidate"}))
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dryrun failure status = %d body=%s", resp.Code, resp.Body.String())
	}
	var env apiEnvelope
	if err := json.Unmarshal(resp.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got %+v", env)
	}
	if resp.Body.Len() >= dryRunResponseHardLimit {
		t.Fatalf("error response exceeded hard limit: %d", resp.Body.Len())
	}
	responseText := resp.Body.String()
	for _, forbidden := range []string{secret, path, "check_log", "core_candidate", "singbox_candidate", "\"path\"", "config_sha256"} {
		if strings.Contains(responseText, forbidden) {
			t.Fatalf("failure response leaked %q: %s", forbidden, responseText)
		}
	}
	errorPayload, ok := env.Error.(map[string]any)
	if !ok {
		t.Fatalf("expected structured error payload, got %+v", env.Error)
	}
	details, ok := errorPayload["details"].(map[string]any)
	if !ok || details["dryrun"] == nil {
		t.Fatalf("expected bounded dryrun details, got %+v", errorPayload["details"])
	}
	dryrun, ok := details["dryrun"].(map[string]any)
	if !ok || dryrun["id"] != "bad-candidate" || dryrun["changes_complete"] == nil {
		t.Fatalf("unexpected dryrun view: %+v", details["dryrun"])
	}
	for _, forbiddenField := range []string{"core_candidate", "singbox_candidate", "openwrt_plan", "proxy_selection"} {
		if _, exists := dryrun[forbiddenField]; exists {
			t.Fatalf("failure response serialized internal field %q: %+v", forbiddenField, dryrun)
		}
	}
}

func TestConfigDryRunErrorResponseHardLimit(t *testing.T) {
	resp := httptest.NewRecorder()
	writeDryRunAPIError(resp, http.StatusBadRequest, strings.Repeat("e", dryRunResponseHardLimit*2), nil)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized error response status=%d body-bytes=%d", resp.Code, resp.Body.Len())
	}
	if resp.Body.Len() >= dryRunResponseHardLimit {
		t.Fatalf("oversized error fallback exceeded hard limit: %d", resp.Body.Len())
	}
}

func TestConfigApplyRejectsLowOverlaySpace(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "low-space"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}

	oldDiskUsage := configDiskUsage
	configDiskUsage = func(string) (systemDiskView, error) {
		return systemDiskView{Path: "/overlay", FreeBytes: 1024 * 1024}, nil
	}
	defer func() { configDiskUsage = oldDiskUsage }()

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "low-space"}))
	if applyResp.Code != http.StatusInsufficientStorage {
		t.Fatalf("apply low space status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
}

func TestConfigApplyRejectsRescueActive(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	if _, err := store.SaveSpec(state.DefaultSpecState()); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "rescue-active"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	if err := os.WriteFile(server.cfg.Layout.RescueStatusPath(), []byte(`{"active":true,"reason":"manual"}`), 0o644); err != nil {
		t.Fatalf("write rescue status: %v", err)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "rescue-active"}))
	if applyResp.Code != http.StatusLocked {
		t.Fatalf("apply during rescue status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
}

func TestSummarizeConfigChangesDescribesBusinessImpact(t *testing.T) {
	before := state.DefaultSpecState()
	after := before
	after.Settings.GatewayMode = state.GatewayModeOff
	after.BusinessPools = append(after.BusinessPools, state.BusinessPoolSpec{ID: "video", Name: "Video", Role: "streaming", NodeIDs: []string{"node-a"}})
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3}, before, nil)
	changes := summarizeConfigChanges(applied, after, applied.TopologyHash, nil)
	if len(changes) < 2 {
		t.Fatalf("expected readable changes, got %+v", changes)
	}
	if changes[0].Before != state.GatewayModeOn || changes[0].After != state.GatewayModeOff || changes[0].Impact == "" {
		t.Fatalf("gateway change is not user-readable: %+v", changes[0])
	}
	foundVideo := false
	for _, change := range changes {
		if change.Title == "新增 video" && strings.Contains(change.After, "node-a") {
			foundVideo = true
		}
	}
	if !foundVideo {
		t.Fatalf("pool change is not user-readable: %+v", changes)
	}
}

func TestSummarizeConfigChangesShowsOrderAndDeepNodeChanges(t *testing.T) {
	before := state.DefaultSpecState()
	before.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Enabled: true}, {ID: "p2", Name: "P2", Enabled: true}}
	after := before
	after.Providers = []state.ProviderSpec{before.Providers[1], before.Providers[0]}
	oldSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"tag": "node-a", "type": "trojan", "server": "example.com", "server_port": 443, "password": "old"}}}}
	newSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"tag": "node-a", "type": "trojan", "server": "example.com", "server_port": 443, "password": "new"}}}}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1}, before, oldSnapshots)
	changes := summarizeConfigChanges(applied, after, "new-topology", newSnapshots)
	foundOrder, foundOldSecret, foundNewSecret := false, false, false
	for _, change := range changes {
		if change.Title == "Provider 顺序" && strings.Contains(change.Before, "p1 → p2") && strings.Contains(change.After, "p2 → p1") {
			foundOrder = true
		}
		if change.Category == "实际节点" && strings.Contains(change.Before, "secret_ref=[REDACTED]") {
			foundOldSecret = true
		}
		if change.Category == "实际节点" && strings.Contains(change.After, "secret_ref=[REDACTED]") {
			foundNewSecret = true
		}
	}
	if !foundOrder || !foundOldSecret || !foundNewSecret {
		t.Fatalf("order or deep node change missing: %+v", changes)
	}
}

func TestSummarizeConfigChangesIgnoresDisabledProviderLastGood(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "disabled", Name: "Disabled", Enabled: false}}
	oldSnapshots := []state.LastGoodSnapshot{{ProviderID: "disabled", Outbounds: []map[string]any{{"tag": "node-old", "server": "old"}}}}
	newSnapshots := []state.LastGoodSnapshot{{ProviderID: "disabled", Outbounds: []map[string]any{{"tag": "node-new", "server": "new"}}}}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: "same"}, spec, oldSnapshots)
	changes := summarizeConfigChanges(applied, spec, "same", newSnapshots)
	for _, change := range changes {
		if change.Category == "实际节点" {
			t.Fatalf("disabled provider node was misreported: %+v", changes)
		}
	}
}

func TestSummarizeConfigChangesFirstApplyShowsEffectiveDefaults(t *testing.T) {
	changes := summarizeConfigChanges(state.AppliedState{}, state.DefaultSpecState(), "topology", nil)
	foundPool, foundRule := false, false
	for _, change := range changes {
		if change.Title == "新增 general" {
			foundPool = true
		}
		if change.Category == "rule" {
			foundRule = true
		}
	}
	if !foundPool || !foundRule {
		t.Fatalf("first apply hid effective defaults: %+v", changes)
	}
}

func TestSummarizeConfigChangesMarksRulesSavedOnlyWhenGatewayOff(t *testing.T) {
	before := state.DefaultSpecState()
	after := before
	after.Settings.GatewayMode = state.GatewayModeOff
	after.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"direct": {DomainSuffix: []string{"example.com"}}}, after.BusinessPools)
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1}, before, nil)
	changes := summarizeConfigChanges(applied, after, applied.TopologyHash, nil)
	found := false
	for _, change := range changes {
		if change.Category == "rule" && strings.Contains(change.Impact, "本次仅保存") {
			found = true
		}
	}
	if !found {
		t.Fatalf("off-mode rule changes were described as active: %+v", changes)
	}
}

func TestGatewayModeDerivedChangesOnlyReportsNewGlobalSemantics(t *testing.T) {
	global := state.DefaultSpecState()
	global.Settings.GatewayMode = state.GatewayModeGlobal
	global.DNS.DirectServers = []string{"dhcp://auto"}
	appliedGlobal := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1}, global, nil)
	if got := gatewayModeDerivedChanges(appliedGlobal, global); len(got) != 0 {
		t.Fatalf("stable global semantics were misreported: %+v", got)
	}
	on := state.DefaultSpecState()
	appliedOn := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1}, on, nil)
	got := gatewayModeDerivedChanges(appliedOn, global)
	if len(got) != 2 || !strings.Contains(got[0].After, "final=proxy") || !strings.Contains(got[0].After, "直连DNS=1.1.1.1, 1.0.0.1") || !strings.Contains(got[0].After, "原始本地DNS仍保留为 dns-cn/节点域名解析=dhcp://auto") {
		t.Fatalf("global derived behavior missing: %+v", got)
	}
	backToOn := gatewayModeDerivedChanges(appliedGlobal, on)
	if len(backToOn) != 2 || !strings.Contains(backToOn[0].Before, "final=proxy") || !strings.Contains(backToOn[0].After, "final=direct") {
		t.Fatalf("leaving global behavior missing: %+v", backToOn)
	}
}

func TestGatewayDerivedSemanticsSanitizesProxyFinalDNSInOnMode(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.DNS.Final = "proxy"
	spec.DNS.DirectServers = []string{"192.168.1.1"}
	dns, _ := gatewayDerivedSemantics(spec)
	if !strings.Contains(dns, "直连DNS=1.1.1.1, 1.0.0.1") || !strings.Contains(dns, "原始本地DNS仍保留为 dns-cn/节点域名解析=192.168.1.1") {
		t.Fatalf("proxy-final DNS safety rewrite hidden: %s", dns)
	}
}

func TestOperationalChangeExplainsReconcileEvenWithoutSpecDiff(t *testing.T) {
	plan := netgate.DryRunPlan{Steps: []netgate.DryRunStep{{Name: "restart-sing-box"}, {Name: "replace-firewall", Destructive: true}}}
	change := operationalChange("apply_transparent_proxy", plan)
	if change.Category != "实际执行" || !strings.Contains(change.After, "准备核心候选") || !strings.Contains(change.After, "破坏性=1") {
		t.Fatalf("operational impact is hidden: %+v", change)
	}
}

func TestGlobalModeDoesNotMislabelAllRemoteRulesAsSavedOnly(t *testing.T) {
	changes := []configChangeView{
		{Category: "广告拦截", Title: "广告拦截设置", Impact: "会改变广告规则集及拦截模式"},
		{Category: "远程规则集", Title: "修改 custom-block", Impact: "会改变该远程规则集的来源、匹配类型或路由目标"},
		{Category: "远程规则集", Title: "修改 geosite-cn", Impact: "会改变该远程规则集的来源、匹配类型或路由目标"},
	}
	got := applyGatewayModeSemantics(changes, state.GatewayModeGlobal)
	if strings.Contains(got[0].Impact, "仅保存") || strings.Contains(got[1].Impact, "仅保存") {
		t.Fatalf("active global rules were mislabeled: %+v", got)
	}
	if !strings.Contains(got[2].Impact, "目标为 direct") {
		t.Fatalf("global CN direct exception was not explained: %+v", got[2])
	}
}

func TestSelectorChangesShowsFallbackWhenSelectedNodeDisappears(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Enabled: true}}
	oldSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"tag": "old", "type": "trojan", "server": "old.example", "server_port": 443, "password": "x"}}}}
	newSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"tag": "new", "type": "trojan", "server": "new.example", "server_port": 443, "password": "y"}}}}
	oldNodes, err := provider.BuildNodeViewsFromSnapshots(spec, oldSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, GatewayMode: state.GatewayModeOn, ProxySelection: map[string]string{"general": oldNodes[0].ID}}, spec, oldSnapshots)
	changes := selectorChanges(applied, spec, newSnapshots, applied.ProxySelection)
	found := false
	for _, change := range changes {
		if change.Title == "业务池 general 将切换节点" && strings.Contains(change.Before, oldNodes[0].ID) && !strings.Contains(change.After, oldNodes[0].ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("selector fallback was hidden: %+v", changes)
	}
}

func TestResolvedRouteTargetChangesShowsPoolRoleFallback(t *testing.T) {
	before := state.DefaultSpecState()
	after := before
	after.BusinessPools = []state.BusinessPoolSpec{}
	for _, pool := range state.DefaultBusinessPools() {
		if pool.ID != "ai" {
			after.BusinessPools = append(after.BusinessPools, pool)
		}
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, GatewayMode: state.GatewayModeOn}, before, nil)
	changes := resolvedRouteTargetChanges(applied, after, nil, applied.ProxySelection)
	found := false
	for _, change := range changes {
		if change.Title == "本地规则 ai" && strings.Contains(change.Before, "Pool_ai") && change.After == "Proxy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved route fallback was hidden: %+v", changes)
	}
}

func TestFinalizeConfigChangesFailsClosedWhenSummaryExceedsBudget(t *testing.T) {
	record := configDryRunRecord{}
	large := strings.Repeat("x", 3*1024*1024)
	record.Changes = []configChangeView{{Category: "实际执行", Title: "critical", After: "restart"}, {Category: "实际节点", Title: "one", After: large}, {Category: "实际节点", Title: "two", After: large}}
	finalizeConfigChanges(&record)
	if record.ChangesComplete || record.ChangesOmitted == 0 {
		t.Fatalf("oversized summary did not fail closed: %+v", record)
	}
	if len(record.Changes) == 0 || record.Changes[0].Category != "实际执行" {
		t.Fatalf("critical operational change was not prioritized: %+v", record.Changes)
	}
}

func TestDryRunFailsClosedBeforeOM(t *testing.T) {
	large := strings.Repeat("x", dryRunResponseHardLimit)
	resp := httptest.NewRecorder()
	writeDryRunAPIData(resp, http.StatusOK, map[string]any{"changes_complete": true, "payload": large})
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized response status=%d body-bytes=%d", resp.Code, resp.Body.Len())
	}
	if resp.Body.Len() >= dryRunResponseHardLimit {
		t.Fatalf("fail-closed response exceeded hard limit: %d", resp.Body.Len())
	}

	outbound := map[string]any{"tag": "node", "type": "trojan", "server": strings.Repeat("s", dryRunResponseHardLimit)}
	summary := flattenOutbound(outbound)
	if len(summary) > dryRunFieldLimit+128 || !strings.Contains(summary, "[TRUNCATED]") {
		t.Fatalf("outbound summary was not stopped incrementally: bytes=%d", len(summary))
	}
	record := configDryRunRecord{ID: "known-id", ChangesComplete: true, Changes: []configChangeView{{Category: "x", After: large}}}
	enforceConfigDryRunResponseLimit(&record)
	if record.ChangesComplete {
		t.Fatal("oversized server-side dry-run remained applicable by its known id")
	}
	if _, err := (configAPI{}).applyDryRunRecord(context.Background(), record, false, false); configDryRunErrorStatus(err) != http.StatusPreconditionRequired {
		t.Fatalf("known oversized dry-run id was not rejected: %v", err)
	}
}

func TestSensitiveFieldsRedactedInDryRun(t *testing.T) {
	outbound := map[string]any{
		"type": "trojan", "tag": "safe", "server": "example.com",
		"password": "plain-password", "passwd": "plain-passwd", "psk": "plain-psk",
		"headers":   map[string]any{"Cookie": "session=plain", "X-Token": "plain-token"},
		"transport": map[string]any{"type": "ws", "path": "/secret-token"},
	}
	text := flattenOutbound(outbound) + " " + redactSource("https://user:pass@example.com/path-token?token=plain#fragment")
	for _, secret := range []string{"plain-password", "plain-passwd", "plain-psk", "session=plain", "plain-token", "secret-token", "path-token", "user:pass"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sensitive value leaked in dry-run display: %q in %q", secret, text)
		}
	}
	if !strings.Contains(text, "https://example.com") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("safe URL identity or redaction marker missing: %s", text)
	}

	schedulerSpec := state.DefaultSpecState()
	schedulerSpec.Scheduler = append(schedulerSpec.Scheduler, state.SchedulerTaskSpec{
		ID: "custom-sensitive", Name: "Custom", Type: scheduler.TaskCustomShell, Enabled: true, Schedule: "@every 1h",
		Spec: map[string]any{
			"provider_id": "provider-safe", "action": "refresh",
			"nested":  map[string]any{"token": "scheduler-token", "password": "scheduler-password", "headers": map[string]any{"Authorization": "Bearer plain"}},
			"command": "/bin/sh", "args": []any{"-c", "curl https://secret.example/token"},
			"unknown": "unknown-plain-value",
		},
	})
	schedulerDisplay := ""
	for _, change := range schedulerChanges(nil, schedulerSpec.Scheduler, false) {
		schedulerDisplay += change.After
	}
	for _, secret := range []string{"scheduler-token", "scheduler-password", "Bearer plain", "/bin/sh", "curl https://secret.example/token", "unknown-plain-value"} {
		if strings.Contains(schedulerDisplay, secret) {
			t.Fatalf("scheduler dry-run leaked %q in %q", secret, schedulerDisplay)
		}
	}
	if !strings.Contains(schedulerDisplay, "provider-safe") || !strings.Contains(schedulerDisplay, "action=refresh") || !strings.Contains(schedulerDisplay, "[REDACTED]") {
		t.Fatalf("scheduler safe fields or redaction markers missing: %s", schedulerDisplay)
	}
}

func TestConfigDryRunUsesCurrentRuntimeSelection(t *testing.T) {
	applied := state.AppliedState{SpecCaptured: true, ProxySelection: map[string]string{"general": "old-node"}, Spec: state.DefaultSpecState()}
	view := appliedWithRuntimeSelection(applied, map[string]string{"general": "manual-node"})
	if view.ProxySelection["general"] != "manual-node" || applied.ProxySelection["general"] != "old-node" {
		t.Fatalf("runtime selection overlay failed or mutated durable applied input: view=%v applied=%v", view.ProxySelection, applied.ProxySelection)
	}
	changes := persistedSelectionChanges(view.ProxySelection, map[string]string{"general": "target-node"})
	if len(changes) != 1 || changes[0].Before != "manual-node" {
		t.Fatalf("dry-run before selection did not use current runtime choice: %+v", changes)
	}
}

func TestLargeLastGoodInputFailsClosedBeforeNormalization(t *testing.T) {
	spec := state.DefaultSpecState()
	snapshots := make([]state.LastGoodSnapshot, 0, 5)
	for providerIndex := 0; providerIndex < 5; providerIndex++ {
		providerID := fmt.Sprintf("p-%d", providerIndex)
		spec.Providers = append(spec.Providers, state.ProviderSpec{ID: providerID, Name: providerID, Enabled: true})
		outbounds := make([]map[string]any, 1000)
		for nodeIndex := range outbounds {
			outbounds[nodeIndex] = map[string]any{"type": "trojan", "tag": fmt.Sprintf("node-%d", nodeIndex), "server": "example.com"}
		}
		snapshots = append(snapshots, state.LastGoodSnapshot{ProviderID: providerID, Outbounds: outbounds})
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1}, spec, nil)
	record := configDryRunRecord{Changes: summarizeConfigChanges(applied, spec, "large", snapshots)}
	finalizeConfigChanges(&record)
	if record.ChangesComplete {
		t.Fatal("oversized aggregate last-good input was normalized instead of failing closed")
	}
	encoded, err := json.Marshal(configDryRunResponse(record))
	if err != nil || len(encoded) >= dryRunResponseHardLimit {
		t.Fatalf("bounded failure response invalid: bytes=%d err=%v", len(encoded), err)
	}
}

func TestConfigDryRunRejectsLargeLastGoodFileBeforeHash(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Enabled: true}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(server.cfg.Layout.LastGoodDir(), "p1.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not-json-secret"), provider.MaxDryRunLastGoodBytes/8), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "oversized-lastgood"}))
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized last-good status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConfigApplyRejectsMissingGraphArtifactsBeforeApply(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "missing-candidate"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	dryRunData := decodeEnvelopeData(t, dryRunResp.Body.Bytes())
	if dryRunData["core_candidate"] != nil || dryRunData[legacyRuntimeFixture.CandidateField] != nil || dryRunData["openwrt_plan"] != nil {
		t.Fatalf("prepared artifact leaked to display response: %+v", dryRunData)
	}
	record, err := (configAPI{layout: server.cfg.Layout}).loadDryRun()
	if err != nil {
		t.Fatalf("load server-side prepared graph: %v", err)
	}
	if record.GraphHash == "" || record.WorkerPrepared == nil || record.FabricPrepared == nil || record.CoreCandidate != nil {
		t.Fatalf("dry-run did not persist graph-only artifacts: %+v", record)
	}
	record.WorkerPrepared = nil
	if err := (configAPI{layout: server.cfg.Layout, locks: server.cfg.Locks}).saveDryRun(record); err != nil {
		t.Fatalf("save corrupted graph dry-run: %v", err)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "missing-candidate"}))
	if applyResp.Code != http.StatusPreconditionRequired {
		t.Fatalf("apply missing graph artifacts status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
}

func TestConfigApplyOpenWrtFailureRollsBackGraphAndCleanup(t *testing.T) {
	server, store, netGateRunner := newConfigTestServer(t)
	netGateRunner.failAt = 1
	netGateRunner.failOnce = true
	netGateRunner.failErr = fmt.Errorf("nft exploded")
	spec := state.DefaultSpecState()
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	runtimeBefore := state.RuntimeState{ConfigEpoch: 9, TopologyHash: "old"}
	if err := store.SaveRuntime(runtimeBefore); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "openwrt-fail"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "openwrt-fail"}))
	if applyResp.Code != http.StatusBadGateway {
		t.Fatalf("apply failure status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	journals, err := state.NewTransactionStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 0 {
		t.Fatalf("virgin OpenWrt failure must not leave residual journal: %+v", journals)
	}
	runtimeAfter, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime after failed apply: %v", err)
	}
	if runtimeAfter.ConfigEpoch != runtimeBefore.ConfigEpoch {
		t.Fatalf("failed apply should not advance config epoch: before=%+v after=%+v", runtimeBefore, runtimeAfter)
	}
	// Apply OpenWrt step failed once; empty-baseline rollback may run rescue cleanup.
	if netGateRunner.calls < 1 {
		t.Fatalf("openwrt path should have been attempted, got %d", netGateRunner.calls)
	}
	if server.cfg.Workers.IsRunning(runtimeTestWorkerDescriptors[runtimeCoreA].WorkerID(1)) {
		t.Fatal("failed graph apply left worker running after rollback")
	}
}

func TestConfigApplyGraphGuardUsesExistingDataDir(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	dryRunResp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "missing-parent"}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}

	var checkedPath string
	oldDiskUsage := configDiskUsage
	configDiskUsage = func(path string) (systemDiskView, error) {
		checkedPath = path
		return systemDiskView{Path: path, FreeBytes: 10 * 1024 * 1024}, nil
	}
	defer func() { configDiskUsage = oldDiskUsage }()

	applyResp := serve(t, server, http.MethodPost, "/api/config/apply", mustJSON(t, map[string]string{"dryrun_id": "missing-parent"}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	expected := existingParent(server.cfg.Layout.DataDir)
	if checkedPath != expected {
		t.Fatalf("overlay guard checked %q, want data dir parent %q", checkedPath, expected)
	}
}

func TestConfigApplyDoesNotDeleteNewerDryRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	api := configAPI{layout: layout, locks: lockset.New()}
	if err := api.saveDryRun(configDryRunRecord{ID: "newer"}); err != nil {
		t.Fatal(err)
	}
	if err := api.removeDryRunIfIDLocked("remove-old", "older"); err != nil {
		t.Fatal(err)
	}
	record, err := api.loadDryRunLocked("load-newer")
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != "newer" {
		t.Fatalf("newer dry-run was removed or replaced: %+v", record)
	}
}

func newConfigTestServer(t *testing.T) (*Server, *state.Store, *configTestRunner) {
	server, store, netGateRunner, _ := newConfigTestServerWithControl(t)
	return server, store, netGateRunner
}

func newConfigTestServerWithControl(t *testing.T) (*Server, *state.Store, *configTestRunner, *fakeProxyControl) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	rescueMgr := newTestRescueManager(layout, commitMgr, owners)
	netGateRunner := &configTestRunner{}
	control := &fakeProxyControl{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, control, netGateRunner)
	server := NewServer(Config{
		ListenAddr:    "127.0.0.1:3000",
		Layout:        layout,
		Locks:         locks,
		CommitLane:    commitMgr,
		Store:         store,
		Rescue:        rescueMgr,
		Owners:        owners,
		NetGateRunner: netGateRunner,
		Workers:       workers,
		Fabric:        fabricManager,
		CoreRegistry:  newRuntimeTestCoreRegistry(t),
		ProxyControl:  control,
		Status:        NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store, netGateRunner, control
}

func serve(t *testing.T, server *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytesReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	return resp
}

func bytesReader(body []byte) *bytes.Reader {
	if body == nil {
		return bytes.NewReader(nil)
	}
	return bytes.NewReader(body)
}

func TestConfigDryRunPrefersDurableSelectionOverStaleRuntime(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, TopologyHash: "topo", GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "applied-default"}}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	durableSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 2, TopologyHash: "topo",
		ProxySelection: map[string]string{"general": "durable-live"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "durable-live", Target: "node", Generation: 12, BoundEpoch: 2, BoundTopologyHash: "topo"},
		},
	}
	if err := store.SaveSelectionState(durableSelection); err != nil {
		t.Fatal(err)
	}
	// RuntimeDir may be recreated empty/stale after reboot.
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, TopologyHash: "topo", ProxySelection: map[string]string{"general": "runtime-stale"}}); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodPost, "/api/config/dryrun", mustJSON(t, map[string]string{"op_id": "durable-selection-dryrun"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("dryrun = %d body=%s", resp.Code, resp.Body.String())
	}
	// Display dry-run view intentionally omits internal fields such as proxy_selection.
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["proxy_selection"] != nil {
		t.Fatalf("prepared proxy_selection leaked to display response: %+v", data["proxy_selection"])
	}
	// Authoritative place for dry-run/apply is the persisted record used by apply.
	record, err := (configAPI{layout: server.cfg.Layout}).loadDryRun()
	if err != nil {
		t.Fatalf("load dry-run record: %v", err)
	}
	if record.ProxySelection["general"] != "durable-live" {
		t.Fatalf("dry-run used stale runtime selection: %+v", record.ProxySelection)
	}
	if !record.SelectionCaptured || record.Selections["general"] != durableSelection.Selections["general"] {
		t.Fatalf("dry-run did not bind structured Selection: %+v", record.Selections)
	}
}

func TestConfigNoticeRefreshDoesNotMarkDirtyOrShowTopologyFallback(t *testing.T) {
	server, store, _ := newConfigTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p", Name: "P", Type: provider.TypeManual, Enabled: true}}
	snapshot := func(notice, host string) []state.LastGoodSnapshot {
		return []state.LastGoodSnapshot{{ProviderID: "p", Outbounds: []map[string]any{
			{"tag": "real", "type": "trojan", "server": host, "server_port": 443},
			{"tag": notice, "type": "anytls", "server": "notice.invalid", "server_port": 1},
		}}}
	}
	old, next := snapshot("剩余流量：68 GB", "example.com"), snapshot("剩余流量：67 GB", "example.com")
	rawOld, err := provider.HashEffectiveTopologyFromSnapshots(spec, old)
	if err != nil {
		t.Fatal(err)
	}
	rawNext, err := provider.HashEffectiveTopologyFromSnapshots(spec, next)
	if err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: rawOld}, spec, old)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p", next[0].Outbounds); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodGet, "/api/config/state", nil)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	if decodeEnvelopeData(t, resp.Body.Bytes())["dirty"] != false {
		t.Fatal("notice marks API dirty")
	}
	for _, c := range summarizeConfigChanges(applied, spec, rawNext, next) {
		if c.Category == "实际节点" || c.Title == "有效拓扑或规范化结果变化" {
			t.Fatalf("false preview: %+v", c)
		}
	}
	next = snapshot("剩余流量：67 GB", "changed.example")
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p", next[0].Outbounds); err != nil {
		t.Fatal(err)
	}
	resp = serve(t, server, http.MethodGet, "/api/config/state", nil)
	if resp.Code != 200 || decodeEnvelopeData(t, resp.Body.Bytes())["dirty"] != true {
		t.Fatal("real change no longer dirty")
	}
}
