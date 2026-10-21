package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestSnapshotHandlersCreateDryRunAndRollback(t *testing.T) {
	server, store := newSnapshotTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	secretURL := "https://user:token@snapshot.example/sub?key=secret"
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: provider.TypeSubscription, Enabled: true, URL: secretURL}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{{"type": "trojan", "tag": "old", "server": "old.example"}}); err != nil {
		t.Fatalf("save old last-good: %v", err)
	}

	createResp := serve(t, server, http.MethodPost, "/api/snapshots", mustJSON(t, snapshotCreatePayload{
		ID:          "snap-1",
		Description: "before edit",
	}))
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create snapshot status = %d body=%s", createResp.Code, createResp.Body.String())
	}
	if strings.Contains(createResp.Body.String(), secretURL) || strings.Contains(createResp.Body.String(), "old.example") || strings.Contains(createResp.Body.String(), "spec") {
		t.Fatalf("snapshot create response leaked snapshot contents: %s", createResp.Body.String())
	}

	spec.Providers = []state.ProviderSpec{{ID: "p2", Name: "Provider 2", Type: provider.TypeManual, Enabled: true}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save modified spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p2", []map[string]any{{"type": "trojan", "tag": "new", "server": "new.example"}}); err != nil {
		t.Fatalf("save new last-good: %v", err)
	}

	listResp := serve(t, server, http.MethodGet, "/api/snapshots", nil)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list snapshot status = %d body=%s", listResp.Code, listResp.Body.String())
	}
	listData := decodeEnvelopeData(t, listResp.Body.Bytes())
	items := listData["snapshots"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "snap-1" {
		t.Fatalf("unexpected snapshot list: %+v", listData)
	}

	dryRunResp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/dryrun", mustJSON(t, snapshotRollbackDryRunPayload{
		DryRunID:   "rollback-1",
		SnapshotID: "snap-1",
	}))
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("rollback dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	dryRunData := decodeEnvelopeData(t, dryRunResp.Body.Bytes())
	if dryRunData["snapshot_id"] != "snap-1" {
		t.Fatalf("unexpected dryrun data: %+v", dryRunData)
	}
	if dryRunData["id"] != "rollback-1" || dryRunData["gateway_mode"] == "" || dryRunData["config_epoch"] == nil || dryRunData["runtime_action"] == nil {
		t.Fatalf("rollback dryrun should expose frontend-compatible fields: %+v", dryRunData)
	}
	if dryRunData["rollback_plan"] != nil || dryRunData["plan"] != nil || dryRunData["openwrt_plan"] != nil || dryRunData[legacyRuntimeFixture.CandidateField] != nil {
		t.Fatalf("rollback dryrun leaked the server-side prepared artifact: %+v", dryRunData)
	}
	if dryRunData["changes_complete"] != true {
		t.Fatalf("rollback dryrun summary should be complete: %+v", dryRunData)
	}
	changes, ok := dryRunData["changes"].([]any)
	if !ok || len(changes) == 0 {
		t.Fatalf("rollback dryrun should expose readable changes: %+v", dryRunData)
	}
	foundProviderChange := false
	for _, raw := range changes {
		change := raw.(map[string]any)
		if change["category"] == "节点来源" && strings.Contains(change["title"].(string), "p1") {
			foundProviderChange = true
		}
	}
	if !foundProviderChange {
		t.Fatalf("rollback provider change missing: %+v", changes)
	}

	applyResp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/apply", mustJSON(t, snapshotRollbackPayload{
		DryRunID:   "rollback-1",
		SnapshotID: "snap-1",
	}))
	if applyResp.Code != http.StatusOK {
		t.Fatalf("rollback apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	applyData := decodeEnvelopeData(t, applyResp.Body.Bytes())
	if applyData["status"] != "ok" || applyData["config_epoch"] == nil || applyData["topology_hash"] == nil {
		t.Fatalf("rollback apply should expose frontend-compatible fields: %+v", applyData)
	}
	gotSpec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if len(gotSpec.Providers) != 1 || gotSpec.Providers[0].ID != "p1" {
		t.Fatalf("expected rollback to p1 spec, got %+v", gotSpec.Providers)
	}
	if provider.NewLastGoodStore(server.cfg.Layout).Exists("p2") {
		t.Fatal("rollback should remove p2 last-good")
	}
	env, err := provider.NewLastGoodStore(server.cfg.Layout).Load("p1")
	if err != nil {
		t.Fatalf("load p1 last-good: %v", err)
	}
	if len(env.Outbounds) != 0 || len(env.Identities) != 1 || env.Identities[0].RawTag != "old" {
		t.Fatalf("expected old last-good after rollback, got %+v", env)
	}
	if runner, ok := server.cfg.NetGateRunner.(*configTestRunner); !ok || runner.calls == 0 {
		t.Fatalf("snapshot rollback did not reconcile the OpenWrt data plane: %#v", server.cfg.NetGateRunner)
	}
}

func TestSnapshotRollbackPreviewUsesCurrentRuntimeSelection(t *testing.T) {
	plan := commitlane.SnapshotRollbackPlan{
		TargetSpec: state.DefaultSpecState(), CurrentSpec: state.DefaultSpecState(),
		CurrentApplied: state.AppliedState{SpecCaptured: true, Spec: state.DefaultSpecState(), ProxySelection: map[string]string{"general": "stale-applied"}},
		CurrentRuntime: state.RuntimeState{ProxySelection: map[string]string{"general": "manual-current"}},
		TargetRuntime:  state.RuntimeState{ProxySelection: map[string]string{"general": "snapshot-target"}},
	}
	view := snapshotRollbackDryRunResponse(snapshotRollbackDryRunRecord{ID: "selection-preview", SnapshotID: "snap", Plan: plan})
	found := false
	for _, change := range view.Changes {
		if change.Category == "持久选择器" && change.Before == "manual-current" && change.After == "snapshot-target" {
			found = true
		}
		if strings.Contains(change.Before, "stale-applied") {
			t.Fatalf("rollback preview exposed stale applied selection: %+v", view.Changes)
		}
	}
	if !found {
		t.Fatalf("rollback preview did not show current durable selection: %+v", view.Changes)
	}
}

func TestSnapshotRollbackApplyRejectsIncompleteReadableSummary(t *testing.T) {
	server, store := newSnapshotTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if resp := serve(t, server, http.MethodPost, "/api/snapshots", mustJSON(t, snapshotCreatePayload{ID: "snap-incomplete"})); resp.Code != http.StatusCreated {
		t.Fatalf("create snapshot status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/dryrun", mustJSON(t, snapshotRollbackDryRunPayload{DryRunID: "rollback-incomplete", SnapshotID: "snap-incomplete"})); resp.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d body=%s", resp.Code, resp.Body.String())
	}
	path := filepath.Join(server.cfg.Layout.RuntimeDir, "snapshot-rollback-dryrun.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record snapshotRollbackDryRunRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.ChangesComplete = false
	updated, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/apply", mustJSON(t, snapshotRollbackPayload{DryRunID: "rollback-incomplete", SnapshotID: "snap-incomplete"}))
	if resp.Code != http.StatusPreconditionRequired {
		t.Fatalf("incomplete summary apply status=%d body=%s", resp.Code, resp.Body.String())
	}
	if runner := server.cfg.NetGateRunner.(*configTestRunner); runner.calls != 0 {
		t.Fatalf("incomplete summary reached data plane: %d commands", runner.calls)
	}
}

func TestSnapshotRollbackApplyRequiresMatchingDryRun(t *testing.T) {
	server, _ := newSnapshotTestServer(t)
	resp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/apply", mustJSON(t, snapshotRollbackPayload{
		DryRunID:   "missing",
		SnapshotID: "snap-1",
	}))
	if resp.Code != http.StatusPreconditionRequired {
		t.Fatalf("rollback without dryrun status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSnapshotRollbackRejectsSourceChangedAfterDryRun(t *testing.T) {
	server, store := newSnapshotTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if resp := serve(t, server, http.MethodPost, "/api/snapshots", mustJSON(t, snapshotCreatePayload{ID: "snap-stale"})); resp.Code != http.StatusCreated {
		t.Fatalf("create snapshot status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/dryrun", mustJSON(t, snapshotRollbackDryRunPayload{DryRunID: "rollback-stale", SnapshotID: "snap-stale"})); resp.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d body=%s", resp.Code, resp.Body.String())
	}
	spec.Providers = []state.ProviderSpec{{ID: "changed", Name: "Changed", Type: provider.TypeManual, Enabled: true}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodPost, "/api/snapshots/rollback/apply", mustJSON(t, snapshotRollbackPayload{DryRunID: "rollback-stale", SnapshotID: "snap-stale"}))
	if resp.Code != http.StatusConflict {
		t.Fatalf("stale rollback status=%d body=%s", resp.Code, resp.Body.String())
	}
	if runner := server.cfg.NetGateRunner.(*configTestRunner); runner.calls != 0 {
		t.Fatalf("stale rollback reached data plane: %d commands", runner.calls)
	}
}

func TestSnapshotListReturnsEmptyArray(t *testing.T) {
	server, _ := newSnapshotTestServer(t)
	resp := serve(t, server, http.MethodGet, "/api/snapshots", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("list snapshots status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	items, ok := data["snapshots"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("empty snapshot list should be an array, got %+v", data["snapshots"])
	}
}

func TestSnapshotApplyDoesNotDeleteNewerRollbackDryRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	api := snapshotAPI{layout: layout}
	if err := api.saveRollbackDryRun(snapshotRollbackDryRunRecord{ID: "newer", SnapshotID: "snap-new"}); err != nil {
		t.Fatal(err)
	}
	if err := api.removeRollbackDryRunIfMatch("older", "snap-old"); err != nil {
		t.Fatal(err)
	}
	record, err := api.loadRollbackDryRun()
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != "newer" || record.SnapshotID != "snap-new" {
		t.Fatalf("newer rollback dry-run was removed: %+v", record)
	}
}

func newSnapshotTestServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	netGateRunner := &configTestRunner{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, nil, netGateRunner)
	server := NewServer(Config{
		ListenAddr:    "127.0.0.1:3000",
		Layout:        layout,
		Locks:         locks,
		CommitLane:    commitMgr,
		Store:         store,
		SnapshotTxn:   commitlane.NewSnapshotTxn(commitMgr, store, state.NewSnapshotStore(layout)),
		Owners:        owners,
		NetGateRunner: netGateRunner,
		Workers:       workers,
		Fabric:        fabricManager,
		Status:        NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}
