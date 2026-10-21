package state

import "testing"

func TestRuntimeWithDurableSelectionPrefersSelectionState(t *testing.T) {
	root := t.TempDir()
	store := NewStore(Layout{DataDir: root, RuntimeDir: root})
	spec := DefaultSpecState()
	applied := AppliedStateWithTarget(AppliedState{ConfigEpoch: 4, TopologyHash: "topo", GatewayMode: GatewayModeOn, ProxySelection: map[string]string{"general": "applied-default"}}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(SelectionStateForApplied(applied, map[string]string{"general": "durable-live"})); err != nil {
		t.Fatal(err)
	}
	runtimeState := RuntimeState{ConfigEpoch: 0, TopologyHash: "", ProxySelection: map[string]string{"general": "runtime-stale"}}
	got, err := RuntimeWithDurableSelection(store, runtimeState)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxySelection["general"] != "durable-live" {
		t.Fatalf("want durable selection, got %+v", got.ProxySelection)
	}
	if got.ConfigEpoch != 4 || got.TopologyHash != "topo" {
		t.Fatalf("want applied topology metadata, got epoch=%d hash=%q", got.ConfigEpoch, got.TopologyHash)
	}
}

func TestRuntimeWithDurableSelectionRejectsTopologyMismatch(t *testing.T) {
	root := t.TempDir()
	store := NewStore(Layout{DataDir: root, RuntimeDir: root})
	spec := DefaultSpecState()
	applied := AppliedStateWithTarget(AppliedState{ConfigEpoch: 5, TopologyHash: "new", GatewayMode: GatewayModeOn}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	stale := SelectionState{SchemaVersion: SelectionStateSchemaVersion, ConfigEpoch: 4, TopologyHash: "old", ProxySelection: map[string]string{"general": "x"}}
	if err := store.SaveSelectionState(stale); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeWithDurableSelection(store, RuntimeState{}); err == nil {
		t.Fatal("expected topology mismatch error")
	}
}

func TestDataPlaneObservationForMode(t *testing.T) {
	on := DataPlaneObservationForMode(GatewayModeOn)
	if on.RunningGatewayMode != GatewayModeOn || !on.Planes[DataPlaneGateway].Running {
		t.Fatalf("on observation: %+v", on)
	}
	off := DataPlaneObservationForMode(GatewayModeOff)
	if off.RunningGatewayMode != GatewayModeOff || off.Planes[DataPlaneGateway].Running {
		t.Fatalf("off observation: %+v", off)
	}
}

func TestRuntimeWithDurableSelectionFallsBackToAppliedSelection(t *testing.T) {
	root := t.TempDir()
	store := NewStore(Layout{DataDir: root, RuntimeDir: root})
	spec := DefaultSpecState()
	applied := AppliedStateWithTarget(AppliedState{ConfigEpoch: 3, TopologyHash: "applied-topo", GatewayMode: GatewayModeOn, ProxySelection: map[string]string{"general": "applied-default"}}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	got, err := RuntimeWithDurableSelection(store, RuntimeState{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxySelection["general"] != "applied-default" || got.ConfigEpoch != 3 || got.TopologyHash != "applied-topo" {
		t.Fatalf("want applied fallback with metadata, got %+v", got)
	}
}
