package commitlane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

// legacyUpgradeFixture writes the on-disk shape a pre-SmartNode release leaves
// behind: pool bindings and selector state keyed by `<provider>:<fingerprint>`,
// a last-good envelope that still carries raw outbounds, an applied capture at
// schema 1 without an execution graph, and no smart_nodes anywhere.
type legacyUpgradeFixture struct {
	layout   state.Layout
	store    *state.Store
	manager  *Manager
	legacyID string
	outbound map[string]any
}

func writeLegacyUpgradeFixture(t *testing.T, extraPoolIDs ...string) legacyUpgradeFixture {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	outbound := map[string]any{
		"type": "shadowsocks", "tag": "rehearse-1", "server": "192.0.2.10", "server_port": float64(8388),
		"method": "aes-128-gcm", "password": "legacy-secret",
	}
	legacyID := provider.LegacyNodeID("rehearse", outbound)
	if !strings.HasPrefix(legacyID, "rehearse:") || len(legacyID) != len("rehearse:")+16 {
		t.Fatalf("unexpected legacy id shape %q", legacyID)
	}
	nodeIDs := append([]string{legacyID}, extraPoolIDs...)
	pools := []map[string]any{
		{"id": "general", "name": "General", "role": "general", "node_ids": nodeIDs},
		{"id": "ai", "name": "AI", "role": "ai", "node_ids": nil},
		{"id": "streaming", "name": "Streaming", "role": "streaming", "node_ids": nil},
		{"id": "banking", "name": "Banking", "role": "banking", "node_ids": nil},
		{"id": "crypto", "name": "Crypto", "role": "crypto", "node_ids": nil},
	}
	spec := map[string]any{
		"schema_version": 1,
		"settings": map[string]any{
			"listen_lan": "192.168.1.1:3000", "allow_wan": false, "discard_provider_rules": true,
			"gateway_mode": "on", "clash_api_bind_loopback": true, "overlay_guard_enabled": true,
		},
		"dns": map[string]any{
			"strategy": "prefer_ipv4", "fakeip_enabled": true, "fakeip_range": "198.18.0.0/15", "final": "direct",
			"direct_servers": []string{"223.5.5.5", "119.29.29.29"}, "proxy_servers": []string{}, "fallback_servers": []string{},
		},
		"providers": []map[string]any{{
			"id": "rehearse", "name": "Rehearse", "type": "manual", "enabled": true, "roles": []string{"general"}, "priority": 10,
		}},
		"business_pools": pools,
	}
	selections := map[string]string{"ai": "Block", "banking": "Block", "crypto": "Block", "general": legacyID, "streaming": "Direct"}
	lastGood := []map[string]any{{"provider_id": "rehearse", "outbounds": []map[string]any{outbound}}}
	applied := map[string]any{
		"schema_version": 1, "config_epoch": 2, "topology_hash": "legacy-topology", "gateway_mode": "on",
		"config_sha256": "legacy-sha", "last_applied_at": "2026-09-03T05:03:26Z",
		"proxy_selection": selections, "spec_captured": true, "spec": spec, "last_good": lastGood,
	}
	selection := map[string]any{"schema_version": 1, "config_epoch": 2, "topology_hash": "legacy-topology", "proxy_selection": selections}
	runtimeState := map[string]any{
		"config_epoch": 2, "topology_hash": "legacy-topology", "gateway_mode": "on", "proxy_selection": selections,
		"node_health": map[string]any{legacyID: map[string]any{"status": "failed", "consecutive_failures": 1}},
	}
	failover := map[string]any{"enabled": true, "cooldown_seconds": 60, "pools": map[string]any{
		"general": map[string]any{"pool_id": "general", "role": "general", "current": legacyID, "previous": legacyID, "last_kind": "sync"},
		"ai":      map[string]any{"pool_id": "ai", "role": "ai", "current": "Block"},
	}}
	envelope := map[string]any{"provider_id": "rehearse", "saved_at": "2026-09-03T05:03:00Z", "count": 1, "outbounds": []map[string]any{outbound}}
	for path, value := range map[string]any{
		layout.SpecPath("spec"):                              spec,
		layout.AppliedStatePath():                            applied,
		layout.SelectionStatePath():                          selection,
		layout.RuntimePath("runtime"):                        runtimeState,
		layout.FailoverStatePath():                           failover,
		filepath.Join(layout.LastGoodDir(), "rehearse.json"): envelope,
	} {
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return legacyUpgradeFixture{
		layout: layout, store: state.NewStore(layout), manager: New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")),
		legacyID: legacyID, outbound: outbound,
	}
}

func (fixture legacyUpgradeFixture) orchestrator() *RecoveryOrchestrator {
	configTxn := NewConfigTxn(fixture.manager, fixture.store, &fakeConfigApplyEngine{})
	return NewRecoveryOrchestrator(configTxn, NewSelectorTxn(fixture.manager, fixture.store, &selectorControlFake{current: "Direct"})).WithLegacyCore(commitCoreA)
}

func TestMigrateLegacyBindingsResolvesV2PoolBindingsToSmartNodes(t *testing.T) {
	fixture := writeLegacyUpgradeFixture(t)
	report, err := fixture.orchestrator().MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || len(report.Resolved) != 1 || len(report.Unresolved) != 0 {
		t.Fatalf("unexpected migration report: %+v", report)
	}
	smartNodeID := report.Resolved[0].SmartNode
	if !strings.HasPrefix(smartNodeID, "sn_") || report.Resolved[0].LegacyID != fixture.legacyID || report.Resolved[0].Core != commitCoreA {
		t.Fatalf("legacy id was not resolved to a SmartNode on the legacy core: %+v", report.Resolved[0])
	}

	spec, err := fixture.store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	node, ok := spec.SmartNodes[smartNodeID]
	if !ok || node.CoreChoice != commitCoreA {
		t.Fatalf("saved spec has no migrated SmartNode with the legacy core: %+v", spec.SmartNodes)
	}
	if len(spec.BusinessPools) == 0 || len(spec.BusinessPools[0].NodeIDs) != 1 || spec.BusinessPools[0].NodeIDs[0] != smartNodeID {
		t.Fatalf("saved general pool was not rebound: %+v", spec.BusinessPools)
	}

	applied, err := fixture.store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.SchemaVersion != state.AppliedStateSchemaVersion {
		t.Fatalf("applied schema was not promoted: %d", applied.SchemaVersion)
	}
	if applied.Spec.BusinessPools[0].NodeIDs[0] != smartNodeID || applied.ProxySelection["general"] != smartNodeID {
		t.Fatalf("applied capture was not rebound: pools=%+v selection=%+v", applied.Spec.BusinessPools[0], applied.ProxySelection)
	}
	if _, ok := applied.Spec.SmartNodes[smartNodeID]; !ok {
		t.Fatalf("applied spec has no SmartNode catalog: %+v", applied.Spec.SmartNodes)
	}
	selection, err := fixture.store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProxySelection["general"] != smartNodeID || selection.Selections["general"].SmartNode != smartNodeID || selection.ProxySelection["ai"] != "Block" {
		t.Fatalf("durable selection was not rebound: %+v", selection)
	}
	runtimeState, err := fixture.store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.ProxySelection["general"] != smartNodeID {
		t.Fatalf("runtime selection was not rebound: %+v", runtimeState.ProxySelection)
	}
	if _, ok := runtimeState.NodeHealth[smartNodeID]; !ok || len(runtimeState.NodeHealth) != 1 {
		t.Fatalf("node health was not re-keyed: %+v", runtimeState.NodeHealth)
	}
	failover, err := state.NewFailoverStateStore(fixture.layout).Load()
	if err != nil {
		t.Fatal(err)
	}
	if failover.Pools["general"].Current != smartNodeID || failover.Pools["general"].Previous != smartNodeID || failover.Pools["ai"].Current != "Block" {
		t.Fatalf("failover state was not rebound: %+v", failover.Pools)
	}
	saved, ok, err := fixture.store.LoadLegacyMigrationReport()
	if err != nil || !ok || len(saved.Resolved) != 1 || saved.MigratedAt.IsZero() {
		t.Fatalf("migration report was not persisted: ok=%v err=%v report=%+v", ok, err, saved)
	}

	// The whole point: the planner that crash-looped V3 on the rehearsal VM
	// must now build a ready graph from the applied capture.
	plan := planner.BuildExecutionGraph(applied.Spec, selection.Selections, state.ExecutionGraph{}, time.Now().UTC())
	if !plan.ReadyToApply {
		t.Fatalf("migrated applied topology still blocks the planner: %+v", plan.Issues)
	}
	var generalBound bool
	for _, binding := range plan.Graph.PoolBindings {
		if binding.Pool == "general" && binding.Target.Kind == "node" && binding.Target.SmartNode == smartNodeID && binding.Target.Core == commitCoreA {
			generalBound = true
		}
	}
	if !generalBound {
		t.Fatalf("general pool is not bound to the migrated SmartNode: %+v", plan.Graph.PoolBindings)
	}

	again, err := fixture.orchestrator().MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("second migration was not a no-op: %+v", again)
	}
}

func TestMigrateLegacyBindingsDropsUnresolvableReferencesWithWarnings(t *testing.T) {
	stale := "rehearse:0123456789abcdef"
	fixture := writeLegacyUpgradeFixture(t, stale)
	report, err := fixture.orchestrator().MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || len(report.Resolved) != 1 {
		t.Fatalf("resolvable binding was lost: %+v", report)
	}
	found := 0
	for _, item := range report.Unresolved {
		if item.LegacyID == stale && item.Pool == "general" {
			found++
		}
	}
	if found == 0 {
		t.Fatalf("stale binding was not reported: %+v", report.Unresolved)
	}
	if len(report.Warnings) == 0 || !strings.Contains(strings.Join(report.Warnings, "\n"), stale) {
		t.Fatalf("stale binding produced no visible warning: %+v", report.Warnings)
	}
	spec, err := fixture.store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range spec.BusinessPools[0].NodeIDs {
		if id == stale || strings.Contains(id, ":") {
			t.Fatalf("stale legacy id survived migration: %+v", spec.BusinessPools[0].NodeIDs)
		}
	}
	if len(spec.BusinessPools[0].NodeIDs) != 1 {
		t.Fatalf("general pool should keep exactly the resolvable node: %+v", spec.BusinessPools[0].NodeIDs)
	}
}

func TestMigrateLegacyBindingsDropsStaleNodeHealthAsWarning(t *testing.T) {
	fixture := writeLegacyUpgradeFixture(t)
	stale := "stale-provider:0123456789abcdef"
	runtimeState, err := fixture.store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.NodeHealth == nil {
		runtimeState.NodeHealth = map[string]state.NodeHealth{}
	}
	runtimeState.NodeHealth[stale] = state.NodeHealth{Status: "failed", ConsecutiveFailures: 2}
	if err := fixture.store.SaveRuntime(runtimeState); err != nil {
		t.Fatal(err)
	}

	report, err := fixture.orchestrator().MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || len(report.Unresolved) != 0 {
		t.Fatalf("stale health history should not remain an unresolved binding: %+v", report)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), stale) {
		t.Fatalf("stale health history was not surfaced as a warning: %+v", report.Warnings)
	}
	savedRuntime, err := fixture.store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := savedRuntime.NodeHealth[stale]; ok {
		t.Fatalf("stale health history survived migration: %+v", savedRuntime.NodeHealth)
	}
}

func TestMigrateLegacyBindingsUnbindsPoolWhenNothingResolves(t *testing.T) {
	fixture := writeLegacyUpgradeFixture(t)
	// Remove the raw outbounds everywhere so no fingerprint can be rebuilt.
	if err := os.Remove(filepath.Join(fixture.layout.LastGoodDir(), "rehearse.json")); err != nil {
		t.Fatal(err)
	}
	applied, err := fixture.store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	applied.LastGood = nil
	if err := fixture.store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	report, err := fixture.orchestrator().MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || len(report.Resolved) != 0 || len(report.Unresolved) == 0 {
		t.Fatalf("unexpected report without last-good: %+v", report)
	}
	spec, err := fixture.store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.BusinessPools[0].NodeIDs) != 0 {
		t.Fatalf("unresolvable binding was kept: %+v", spec.BusinessPools[0].NodeIDs)
	}
	selection, err := fixture.store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := selection.ProxySelection["general"]; ok {
		t.Fatalf("unresolvable selection was kept: %+v", selection.ProxySelection)
	}
	if _, ok := selection.Selections["general"]; ok {
		t.Fatalf("unresolvable pool selection was kept: %+v", selection.Selections)
	}
	applied, err = fixture.store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	plan := planner.BuildExecutionGraph(applied.Spec, selection.Selections, state.ExecutionGraph{}, time.Now().UTC())
	if !plan.ReadyToApply {
		t.Fatalf("unbound pool still blocks the planner: %+v", plan.Issues)
	}
	for _, binding := range plan.Graph.PoolBindings {
		if binding.Pool == "general" && binding.Target.Kind != state.NoCandidateDirect {
			t.Fatalf("general pool without candidates should fall back to its no-candidate policy: %+v", binding)
		}
	}
}

func TestMigrateLegacyBindingsIsNoOpOnNativeState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.BusinessPools[0].NodeIDs = []string{"sn_0123456789abcdef"}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	orchestrator := NewRecoveryOrchestrator(NewConfigTxn(manager, store, &fakeConfigApplyEngine{}), NewSelectorTxn(manager, store, &selectorControlFake{current: "Direct"}))
	report, err := orchestrator.MigrateLegacyBindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report != nil {
		t.Fatalf("native SmartNode state was treated as legacy: %+v", report)
	}
	if _, ok, err := store.LoadLegacyMigrationReport(); err != nil || ok {
		t.Fatalf("no-op migration wrote a report: ok=%v err=%v", ok, err)
	}
}

func TestRecoverRunsLegacyMigrationBeforeStartupReconcile(t *testing.T) {
	fixture := writeLegacyUpgradeFixture(t)
	engine := &fakeConfigApplyEngine{}
	configTxn := NewConfigTxn(fixture.manager, fixture.store, engine)
	orchestrator := NewRecoveryOrchestrator(configTxn, NewSelectorTxn(fixture.manager, fixture.store, &selectorControlFake{current: "Direct"})).WithLegacyCore(commitCoreA)
	if err := orchestrator.Recover(context.Background()); err != nil {
		t.Fatalf("startup recovery on migrated V2 state failed: %v", err)
	}
	spec, err := fixture.store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range spec.BusinessPools {
		for _, id := range pool.NodeIDs {
			if strings.Contains(id, ":") {
				t.Fatalf("recovery left a legacy binding in pool %s: %v", pool.ID, pool.NodeIDs)
			}
		}
	}
}
