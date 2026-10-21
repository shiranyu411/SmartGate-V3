package state

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestAppliedStateRoundTripAndMissing(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	missing, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if missing.TopologyHash != "" || missing.ConfigEpoch != 0 {
		t.Fatalf("unexpected missing applied state: %+v", missing)
	}
	now := time.Now().UTC().Truncate(time.Second)
	want := AppliedState{
		ConfigEpoch:    7,
		TopologyHash:   "topology",
		GatewayMode:    GatewayModeGlobal,
		ConfigSHA256:   "config",
		LastAppliedAt:  &now,
		ProxySelection: map[string]string{"general": "node-1"},
	}
	if err := store.SaveAppliedState(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != AppliedStateSchemaVersion || got.ConfigEpoch != want.ConfigEpoch || got.TopologyHash != want.TopologyHash || got.ProxySelection["general"] != "node-1" {
		t.Fatalf("unexpected applied state: %+v", got)
	}
}

func TestLoadAppliedStatePromotesLegacySchemaOnWrite(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema_version":1,"config_epoch":2,"topology_hash":"legacy","gateway_mode":"on","spec_captured":true,"spec":{"schema_version":1},"proxy_selection":{"general":"p:0123456789abcdef"}}`)
	if err := os.WriteFile(layout.AppliedStatePath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.SchemaVersion != AppliedStateSchemaVersion || !applied.SpecCaptured || applied.ConfigEpoch != 2 || applied.ProxySelection["general"] != "p:0123456789abcdef" {
		t.Fatalf("legacy applied capture was not promoted intact: %+v", applied)
	}
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(layout.AppliedStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"schema_version": 2`) {
		t.Fatalf("rewritten applied state still advertises the legacy schema: %s", body)
	}
	if missing, err := store.LoadAppliedState(); err != nil || missing.SchemaVersion == 0 {
		// Sanity: a loaded file always carries the current schema.
		t.Fatalf("reloaded applied state: %+v err=%v", missing, err)
	}
}

func TestAppliedStateFromRuntimeCopiesSelections(t *testing.T) {
	runtimeState := RuntimeState{ConfigEpoch: 2, TopologyHash: "hash", ProxySelection: map[string]string{"ai": "node-a"}}
	applied := AppliedStateFromRuntime(runtimeState, GatewayModeOn)
	runtimeState.ProxySelection["ai"] = "changed"
	if applied.ProxySelection["ai"] != "node-a" {
		t.Fatalf("selection was not copied: %+v", applied.ProxySelection)
	}
}

func TestAppliedStateWithTargetPersistsRollbackSource(t *testing.T) {
	spec := DefaultSpecState()
	spec.Settings.GatewayMode = GatewayModeGlobal
	lastGood := []LastGoodSnapshot{{
		ProviderID: "provider-a",
		Outbounds:  []map[string]any{{"type": "direct", "tag": "node-a"}},
	}}
	applied := AppliedStateWithTarget(AppliedState{ConfigEpoch: 3}, spec, lastGood)

	if !applied.SpecCaptured || applied.Spec.Settings.GatewayMode != GatewayModeGlobal {
		t.Fatalf("applied target was not captured: %+v", applied)
	}
	if len(applied.LastGood) != 1 || applied.LastGood[0].ProviderID != "provider-a" {
		t.Fatalf("last-good target was not captured: %+v", applied.LastGood)
	}

	lastGood[0].ProviderID = "mutated"
	if applied.LastGood[0].ProviderID != "provider-a" {
		t.Fatal("captured last-good slice aliases the caller")
	}
}

func TestLoadAppliedTopologyUsesCapturedSpecAndDurableSelection(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	appliedSpec := DefaultSpecState()
	appliedSpec.Providers = []ProviderSpec{{ID: "applied", Name: "Applied", Type: "manual", Enabled: true}}
	savedSpec := DefaultSpecState()
	savedSpec.Providers = []ProviderSpec{{ID: "saved", Name: "Saved", Type: "manual", Enabled: true}}
	if _, err := store.SaveSpec(savedSpec); err != nil {
		t.Fatal(err)
	}
	applied := AppliedStateWithTarget(AppliedState{
		ConfigEpoch: 4, TopologyHash: "applied-hash", ProxySelection: map[string]string{"general": "commit-node"},
	}, appliedSpec, []LastGoodSnapshot{{ProviderID: "applied", Outbounds: []map[string]any{{"tag": "applied-node"}}}})
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(SelectionStateForApplied(applied, map[string]string{"general": "live-node"})); err != nil {
		t.Fatal(err)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Spec.Providers) != 1 || topology.Spec.Providers[0].ID != "applied" || topology.LastGood[0].ProviderID != "applied" {
		t.Fatalf("saved topology leaked into applied view: %+v", topology)
	}
	if topology.ProxySelection["general"] != "live-node" {
		t.Fatalf("durable selection was not authoritative: %+v", topology.ProxySelection)
	}
}

func TestLoadAppliedTopologyFailsClosedWithoutCapture(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	if err := store.SaveAppliedState(AppliedState{ConfigEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(RuntimeState{ConfigEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAppliedTopology(); err == nil {
		t.Fatal("expected missing applied topology capture to fail closed")
	}
}

func TestLoadAppliedTopologyRejectsMismatchedDurableSelection(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	applied := AppliedStateWithTarget(AppliedState{ConfigEpoch: 2, TopologyHash: "a"}, DefaultSpecState(), nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(SelectionState{ConfigEpoch: 3, TopologyHash: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAppliedTopology(); err == nil {
		t.Fatal("expected mismatched durable selection to fail closed")
	}
}
