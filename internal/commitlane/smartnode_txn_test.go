package commitlane

import (
	"context"
	"errors"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestSmartNodeCoreChoicePersistsDesiredState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Type: provider.TypeManual, Enabled: true}}
	raw := []map[string]any{{
		"type": "vless", "tag": "xhttp-node", "server": "node.example", "server_port": 443,
		"uuid": "secret", "transport": map[string]any{"type": "xhttp", "path": "/api"},
	}}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", raw); err != nil {
		t.Fatal(err)
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(layout))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	nodeID := firstSmartNodeID(t, spec.SmartNodes)
	spec.BusinessPools[1].NodeIDs = []string{nodeID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewSmartNodeTxn(manager, store, state.NewSnapshotStore(layout))

	result, err := txn.SetCoreChoice(context.Background(), SmartNodeCoreChoiceRequest{
		OpID: "core-choice-xray", SmartNodeID: nodeID, Core: commitCoreB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Dirty || len(result.AffectedPools) != 1 || result.AffectedPools[0] != "ai" {
		t.Fatalf("unexpected core choice result: %+v", result)
	}
	saved, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SmartNodes[nodeID].CoreChoice != commitCoreB {
		t.Fatalf("core choice was not persisted: %+v", saved.SmartNodes[nodeID])
	}
	if journals, err := state.NewTransactionStore(layout).List(); err != nil || len(journals) != 0 {
		t.Fatalf("successful transaction left a journal: journals=%+v err=%v", journals, err)
	}
}

func TestSmartNodeCoreChoiceEnforcesCapability(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Type: provider.TypeManual, Enabled: true}}
	raw := []map[string]any{{
		"type": "hysteria2", "tag": "hy2", "server": "hy.example", "server_port": 443, "password": "secret",
	}}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", raw); err != nil {
		t.Fatal(err)
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(layout))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	nodeID := firstSmartNodeID(t, spec.SmartNodes)
	txn := NewSmartNodeTxn(New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")), store, state.NewSnapshotStore(layout))
	_, err = txn.SetCoreChoice(context.Background(), SmartNodeCoreChoiceRequest{
		OpID: "core-choice-unsupported", SmartNodeID: nodeID, Core: commitCoreB,
	})
	var txnErr *SmartNodeTxnError
	if !errors.As(err, &txnErr) || txnErr.Code != "capability_unsupported" {
		t.Fatalf("unsupported core was not rejected: %T %v", err, err)
	}
}

func TestSmartNodeCoreChoiceBuildsValidationMessageFromRegisteredCores(t *testing.T) {
	defer configureTestCoreEvaluators()
	supported := func(smartnode.NodeIdentity, []string) smartnode.CoreCapability {
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}, ReasonTexts: []string{}}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: "alpha", Evaluate: supported},
		{Core: "beta", Evaluate: supported},
	})
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	txn := NewSmartNodeTxn(
		New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")),
		state.NewStore(layout),
		state.NewSnapshotStore(layout),
	)
	_, err := txn.SetCoreChoice(context.Background(), SmartNodeCoreChoiceRequest{
		OpID: "core-choice-invalid", SmartNodeID: "node", Core: "unknown",
	})
	var txnErr *SmartNodeTxnError
	if !errors.As(err, &txnErr) || txnErr.Message != "core must be alpha or beta" {
		t.Fatalf("unexpected core validation error: %T %v", err, err)
	}
}

func firstSmartNodeID(t *testing.T, nodes map[string]smartnode.SmartNode) string {
	t.Helper()
	for id := range nodes {
		return id
	}
	t.Fatal("missing SmartNode")
	return ""
}
