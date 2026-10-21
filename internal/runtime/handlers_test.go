package runtime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
)

type failingRecoveryEngine struct{}

func (failingRecoveryEngine) Prepare(context.Context, string, configapply.TargetState, int64) (configapply.Prepared, error) {
	return configapply.Prepared{}, errors.New("forced recovery failure")
}

func (failingRecoveryEngine) Checkpoint(string) (configapply.Checkpoint, error) {
	return configapply.Checkpoint{}, nil
}

func (failingRecoveryEngine) Apply(context.Context, configapply.Prepared, map[string]string) (configapply.Result, error) {
	return configapply.Result{}, nil
}

func (failingRecoveryEngine) Rollback(context.Context, configapply.Prepared, map[string]string, configapply.Checkpoint) (configapply.Result, error) {
	return configapply.Result{}, nil
}

type observingCleanup struct {
	calls       int
	contextLive bool
}

func (c *observingCleanup) Cleanup(ctx context.Context, _ *lockset.Session) (netgate.CleanupPlan, error) {
	c.calls++
	c.contextLive = ctx.Err() == nil
	return netgate.CleanupPlan{}, nil
}

func TestHTTPRescueClearRecoversBothJournals(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := commitlane.New(locks, layout, owners)
	rescueMgr := newTestRescueManager(layout, manager, owners)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, GatewayMode: state.GatewayModeOff}, spec, nil)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, CommitInFlight: &state.CommitRef{OpID: "rescue-pending"}}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "rescue-pending", Kind: "config_apply", Phase: state.TransactionApplying,
		Source: state.TransactionState{Spec: spec, LastGoodCaptured: true, Applied: applied, Runtime: state.RuntimeState{ConfigEpoch: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	selectorSource := state.TransactionState{
		Applied:          applied,
		Runtime:          state.RuntimeState{ConfigEpoch: 2, ProxySelection: map[string]string{"general": "node-a"}},
		FailoverCaptured: true,
		Failover:         state.DefaultFailoverState(),
	}
	selectorTarget := selectorSource
	selectorTarget.Runtime.ProxySelection = map[string]string{"general": "node-b"}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "selector-pending", Kind: "selector_apply", Phase: state.TransactionApplying,
		Source: selectorSource, Target: selectorTarget, PoolID: "general", Selector: "Pool_general",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rescue.NewStore(layout.RescueStatusPath()).Save(rescue.ReasonManual, netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	runner := &configTestRunner{}
	control := &fakeProxyControl{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, control, runner)
	configTxn := commitlane.NewConfigTxn(manager, store, configapply.Engine{
		Owners: owners, NetGateRunner: runner,
		Workers: workers, Fabric: fabricManager,
	})
	selectorTxn := commitlane.NewSelectorTxn(manager, store, control)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: locks, CommitLane: manager,
		Store: store, ConfigTxn: configTxn, SelectorTxn: selectorTxn, Rescue: rescueMgr, Owners: owners, NetGateRunner: runner,
		Workers: workers, Fabric: fabricManager, ProxyControl: control,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	resp := serve(t, server, http.MethodPost, "/api/runtime/rescue/clear", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("clear rescue status=%d body=%s", resp.Code, resp.Body.String())
	}
	active, err := rescueMgr.Active()
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("rescue flag remained after successful pending recovery")
	}
	if _, err := state.NewTransactionStore(layout).Load("rescue-pending"); err == nil {
		t.Fatal("pending journal remained after rescue clear")
	}
	if _, err := state.NewTransactionStore(layout).Load("selector-pending"); err == nil {
		t.Fatal("selector journal remained after rescue clear")
	}
	if control.selectSelector != "Pool_general" || control.selectOutbound != "node-a" {
		t.Fatalf("selector journal was not restored to its durable source: selector=%q outbound=%q", control.selectSelector, control.selectOutbound)
	}
	if runner.calls == 0 {
		t.Fatal("rescue clear did not reconcile data plane")
	}
}

func TestRescueClearRecoveryFailureKeepsFlagAndReappliesCleanup(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := commitlane.New(locks, layout, owners)
	cleanup := &observingCleanup{}
	rescueMgr := rescue.NewWithCleanup(layout.RescueStatusPath(), manager, owners, cleanup)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, GatewayMode: state.GatewayModeOff}, spec, nil)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, CommitInFlight: &state.CommitRef{OpID: "rescue-failing"}}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "rescue-failing", Kind: "config_apply", Phase: state.TransactionApplying,
		Source: state.TransactionState{Spec: spec, LastGoodCaptured: true, Applied: applied, Runtime: state.RuntimeState{ConfigEpoch: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rescue.NewStore(layout.RescueStatusPath()).Save(rescue.ReasonManual, netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	configTxn := commitlane.NewConfigTxn(manager, store, failingRecoveryEngine{})
	control := &fakeProxyControl{}
	selectorTxn := commitlane.NewSelectorTxn(manager, store, control)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: locks, CommitLane: manager,
		Store: store, ConfigTxn: configTxn, SelectorTxn: selectorTxn, Rescue: rescueMgr, Owners: owners, ProxyControl: control,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	resp := serve(t, server, http.MethodPost, "/api/runtime/rescue/clear", nil)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("clear rescue status=%d body=%s", resp.Code, resp.Body.String())
	}
	active, err := rescueMgr.Active()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("rescue flag was cleared after failed recovery")
	}
	if cleanup.calls != 1 || !cleanup.contextLive {
		t.Fatalf("fresh rescue cleanup was not executed: calls=%d live=%v", cleanup.calls, cleanup.contextLive)
	}
	journal, err := state.NewTransactionStore(layout).Load("rescue-failing")
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != state.TransactionRecoveryRequired {
		t.Fatalf("journal phase=%q want %q", journal.Phase, state.TransactionRecoveryRequired)
	}
}

type successRecoveryEngine struct{}

func (successRecoveryEngine) Prepare(context.Context, string, configapply.TargetState, int64) (configapply.Prepared, error) {
	return configapply.Prepared{}, nil
}

func (successRecoveryEngine) Checkpoint(string) (configapply.Checkpoint, error) {
	return configapply.Checkpoint{}, nil
}

func (successRecoveryEngine) Apply(context.Context, configapply.Prepared, map[string]string) (configapply.Result, error) {
	return configapply.Result{}, nil
}

func (successRecoveryEngine) Rollback(context.Context, configapply.Prepared, map[string]string, configapply.Checkpoint) (configapply.Result, error) {
	return configapply.Result{}, nil
}

func TestHTTPRuntimeRecoverClearsCommittedJournal(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := commitlane.New(locks, layout, owners)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	oldApplied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 1, TopologyHash: "old", GatewayMode: state.GatewayModeOff,
		ProxySelection: map[string]string{"general": "old-node"},
	}, spec, nil)
	newApplied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 2, TopologyHash: "new", GatewayMode: state.GatewayModeOff,
		ProxySelection: map[string]string{"general": "new-node"},
	}, spec, nil)
	newRuntime := state.RuntimeState{ConfigEpoch: 2, TopologyHash: "new", ProxySelection: map[string]string{"general": "new-node"}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	// Crash after selection/runtime advanced but applied lagging + Committed journal residual.
	if err := store.SaveAppliedState(oldApplied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(newApplied, newRuntime.ProxySelection)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(newRuntime); err != nil {
		t.Fatal(err)
	}
	journal := state.TransactionJournal{
		OpID: "http-committed-crash", Kind: "config_apply", Phase: state.TransactionCommitted,
		Source: state.TransactionState{Applied: oldApplied},
		Target: state.TransactionState{Spec: newApplied.Spec, LastGoodCaptured: true, Applied: newApplied, Runtime: newRuntime},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}

	configTxn := commitlane.NewConfigTxn(manager, store, successRecoveryEngine{})
	control := &fakeProxyControl{selectedLookup: func(string) (string, error) { return "new-node", nil }}
	selectorTxn := commitlane.NewSelectorTxn(manager, store, control)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: locks, CommitLane: manager,
		Store: store, ConfigTxn: configTxn, SelectorTxn: selectorTxn,
		Rescue: newTestRescueManager(layout, manager, owners), Owners: owners, ProxyControl: control,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})

	// Before recover: selection ahead of applied makes authoritative reads fail.
	if resp := serve(t, server, http.MethodGet, "/api/config/state", nil); resp.Code != http.StatusInternalServerError {
		t.Fatalf("pre-recover config state status=%d body=%s", resp.Code, resp.Body.String())
	}

	resp := serve(t, server, http.MethodPost, "/api/runtime/recover", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("recover status=%d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["status"] != "recovered" {
		t.Fatalf("recover data=%+v", data)
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal remained after recover: %v", err)
	}

	stateResp := serve(t, server, http.MethodGet, "/api/config/state", nil)
	if stateResp.Code != http.StatusOK {
		t.Fatalf("post-recover config state status=%d body=%s", stateResp.Code, stateResp.Body.String())
	}
}

func TestHTTPRuntimeRecoverMethodNotAllowed(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := commitlane.New(locks, layout, owners)
	store := state.NewStore(layout)
	configTxn := commitlane.NewConfigTxn(manager, store, successRecoveryEngine{})
	selectorTxn := commitlane.NewSelectorTxn(manager, store, &fakeProxyControl{})
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: locks, CommitLane: manager,
		Store: store, ConfigTxn: configTxn, SelectorTxn: selectorTxn,
		Rescue: newTestRescueManager(layout, manager, owners), Owners: owners,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	resp := serve(t, server, http.MethodGet, "/api/runtime/recover", nil)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}
