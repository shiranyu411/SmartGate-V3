package commitlane

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/selection"
	"smartgate/internal/state"
)

type selectorControlFake struct {
	calls              [][2]string
	fail               error
	current            string
	connectionsPayload map[string]any
	closedIDs          []string
	closeAllCalled     bool
}

func (f *selectorControlFake) Select(_ context.Context, selector, target string) error {
	f.calls = append(f.calls, [2]string{selector, target})
	if f.fail == nil {
		f.current = target
	}
	return f.fail
}
func (f *selectorControlFake) Selected(context.Context, string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	return f.current, nil
}
func (f *selectorControlFake) Delay(context.Context, string, string, time.Duration) (selection.DelayResult, error) {
	return selection.DelayResult{}, nil
}
func (f *selectorControlFake) Connections(context.Context) (map[string]any, error) {
	if f.connectionsPayload != nil {
		return f.connectionsPayload, nil
	}
	return map[string]any{"connections": []any{}}, nil
}
func (f *selectorControlFake) CloseConnections(context.Context) error {
	f.closeAllCalled = true
	return nil
}
func (f *selectorControlFake) CloseConnection(_ context.Context, id string) error {
	f.closedIDs = append(f.closedIDs, id)
	return nil
}

func selectorTxnFixture(t *testing.T) (*SelectorTxn, *state.Store, state.Layout, *selectorControlFake) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, ProxySelection: map[string]string{"general": "node-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{SchemaVersion: state.AppliedStateSchemaVersion, ConfigEpoch: 2, ProxySelection: map[string]string{"general": "node-a"}}, state.DefaultSpecState(), nil)); err != nil {
		t.Fatal(err)
	}
	failover := state.DefaultFailoverState()
	failover.Pools["general"] = state.FailoverPoolState{PoolID: "general", Current: "node-a"}
	if err := state.NewFailoverStateStore(layout).Save(failover); err != nil {
		t.Fatal(err)
	}
	control := &selectorControlFake{current: "node-a"}
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	return NewSelectorTxn(manager, store, control), store, layout, control
}

func TestSelectorTxnCommitsAllControlState(t *testing.T) {
	defer selection.SetSelectorRebindDelaysForTest(nil)()
	txn, store, layout, control := selectorTxnFixture(t)
	control.connectionsPayload = map[string]any{
		"connections": []any{
			map[string]any{"id": "keep-ai", "chains": []any{"node-x", "Pool_ai"}},
			map[string]any{"id": "drop-gen", "chains": []any{"node-a", "Pool_general"}},
			map[string]any{"id": "keep-direct", "chains": []any{"Direct"}},
		},
	}
	targetFailover, _ := state.NewFailoverStateStore(layout).Load()
	pool := targetFailover.Pools["general"]
	pool.Previous = "node-a"
	pool.Current = "node-b"
	targetFailover.Pools["general"] = pool
	if err := txn.Apply(context.Background(), SelectorTxnApplyRequest{OpID: "selector-ok", PoolID: "general", Selector: "Pool_general", Source: "node-a", Target: "node-b", TargetPool: targetFailover.Pools["general"], ExpectedConfigEpoch: 2}); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 1 || control.calls[0][1] != "node-b" {
		t.Fatalf("selector not applied: %+v", control.calls)
	}
	if control.closeAllCalled {
		t.Fatal("selector apply must not close all connections")
	}
	if len(control.closedIDs) != 1 || control.closedIDs[0] != "drop-gen" {
		t.Fatalf("expected only Pool_general sticky connections closed, got %+v", control.closedIDs)
	}
	runtimeState, _ := store.LoadRuntime()
	applied, _ := store.LoadAppliedState()
	failover, _ := state.NewFailoverStateStore(layout).Load()
	if runtimeState.ProxySelection["general"] != "node-b" || applied.ProxySelection["general"] != "node-a" || failover.Pools["general"].Current != "node-b" {
		t.Fatalf("selector state diverged: runtime=%+v applied=%+v failover=%+v", runtimeState, applied, failover)
	}
	durable, err := store.LoadSelectionState()
	if err != nil || durable.ProxySelection["general"] != "node-b" || durable.ConfigEpoch != 2 {
		t.Fatalf("latest selector was not durably persisted: %+v err=%v", durable, err)
	}
	if _, err := state.NewTransactionStore(layout).Load("selector-ok"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remained: %v", err)
	}
}

func TestSelectorTxnRejectsStaleCallerSourceAgainstLiveControl(t *testing.T) {
	txn, store, _, control := selectorTxnFixture(t)
	control.current = "node-live"
	before, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	err = txn.Apply(context.Background(), SelectorTxnApplyRequest{
		OpID: "selector-stale", PoolID: "general", Selector: "Pool_general", Source: "node-a", Target: "node-b",
		TargetPool: state.FailoverPoolState{PoolID: "general", Current: "node-b"}, ExpectedConfigEpoch: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "selector source changed") {
		t.Fatalf("expected stale source rejection, got %v", err)
	}
	after, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxySelection["general"] != before.ProxySelection["general"] || len(control.calls) != 0 {
		t.Fatalf("stale source mutated state or selector: applied=%+v calls=%+v", after.ProxySelection, control.calls)
	}
}

func TestSelectorTxnRecoverApplyingRollsBackSource(t *testing.T) {
	txn, store, layout, control := selectorTxnFixture(t)
	source, err := txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	target := cloneSelectorState(source)
	target.Runtime.ProxySelection["general"] = "node-b"
	journal := state.TransactionJournal{OpID: "selector-crash", Kind: selectorTransactionKind, Phase: state.TransactionApplying, PoolID: "general", Selector: "Pool_general", Source: source, Target: target}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 1 || control.calls[0][1] != "node-a" {
		t.Fatalf("source selector not restored: %+v", control.calls)
	}
	runtimeState, _ := store.LoadRuntime()
	if runtimeState.ProxySelection["general"] != "node-a" {
		t.Fatalf("source runtime not restored: %+v", runtimeState)
	}
}

func TestSelectorTxnCommittedRecoveryRetryStillCompletesTarget(t *testing.T) {
	txn, _, layout, control := selectorTxnFixture(t)
	source, err := txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	target := cloneSelectorState(source)
	target.Runtime.ProxySelection["general"] = "node-b"
	journal := state.TransactionJournal{OpID: "selector-committed", Kind: selectorTransactionKind, Phase: state.TransactionCommitted, PoolID: "general", Selector: "Pool_general", Source: source, Target: target}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	control.fail = errors.New("clash temporarily unavailable")
	if err := txn.Recover(context.Background()); err == nil {
		t.Fatal("expected first recovery failure")
	}
	retained, err := state.NewTransactionStore(layout).Load(journal.OpID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Phase != state.TransactionCommitted {
		t.Fatalf("committed direction was lost: %+v", retained)
	}
	control.fail = nil
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := control.calls[len(control.calls)-1][1]; got != "node-b" {
		t.Fatalf("retry recovered %q, want target", got)
	}
}

func TestSelectorTxnLoadStatePrefersDurableSelection(t *testing.T) {
	txn, store, _, _ := selectorTxnFixture(t)
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": "durable-live"})); err != nil {
		t.Fatal(err)
	}
	// Stale RuntimeDir content must not become the journal source.
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, ProxySelection: map[string]string{"general": "runtime-stale"}}); err != nil {
		t.Fatal(err)
	}
	source, err := txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if source.Runtime.ProxySelection["general"] != "durable-live" {
		t.Fatalf("selector loadState ignored durable selection: %+v", source.Runtime.ProxySelection)
	}
}
