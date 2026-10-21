package runtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"smartgate/internal/backup"
	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestBackupCreateAndRestoreHandlers(t *testing.T) {
	server, store := newBackupTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	secretURL := "https://user:token@backup.example/sub?key=secret"
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: provider.TypeSubscription, Enabled: true, URL: secretURL}}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"n1"}}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := state.NewFailoverStateStore(server.cfg.Layout).Save(state.FailoverState{Enabled: false, Locks: map[string]state.FailoverLock{}, Pools: map[string]state.FailoverPoolState{}}); err != nil {
		t.Fatalf("save backed up failover state: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}

	createBody := mustJSON(t, backupCreatePayload{OpID: "backup-create-1", Kind: "manual"})
	createReq := httptest.NewRequest(http.MethodPost, "/api/backups/create", bytes.NewReader(createBody))
	createResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", createResp.Code, createResp.Body.String())
	}
	createData := decodeEnvelopeData(t, createResp.Body.Bytes())
	recordPath, ok := createData["path"].(string)
	if !ok || recordPath == "" {
		t.Fatalf("backup create envelope missing path: %+v", createData)
	}
	if strings.Contains(createResp.Body.String(), secretURL) || strings.Contains(createResp.Body.String(), "example.com") {
		t.Fatalf("backup create response leaked archive contents: %s", createResp.Body.String())
	}

	mutated := state.DefaultSpecState()
	mutated.Settings.GatewayMode = state.GatewayModeOff
	mutated.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	if _, err := store.SaveSpec(mutated); err != nil {
		t.Fatalf("save mutated spec: %v", err)
	}
	if err := state.NewFailoverStateStore(server.cfg.Layout).Save(state.FailoverState{Enabled: true, Locks: map[string]state.FailoverLock{}, Pools: map[string]state.FailoverPoolState{}}); err != nil {
		t.Fatalf("save mutated failover state: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("stale", []map[string]any{{"type": "trojan", "tag": "stale-node", "server": "stale.example"}}); err != nil {
		t.Fatalf("save stale last-good: %v", err)
	}

	restoreBody := mustJSON(t, backupRestorePayload{OpID: "backup-restore-1", Path: recordPath})
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/backups/restore", bytes.NewReader(restoreBody))
	restoreResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(restoreResp, restoreReq)
	if restoreResp.Code != http.StatusAccepted {
		t.Fatalf("restore status = %d body=%s", restoreResp.Code, restoreResp.Body.String())
	}
	if strings.Contains(restoreResp.Body.String(), secretURL) || strings.Contains(restoreResp.Body.String(), "example.com") || strings.Contains(restoreResp.Body.String(), "providers") {
		t.Fatalf("backup restore response leaked restored state: %s", restoreResp.Body.String())
	}
	_ = decodeEnvelopeData(t, restoreResp.Body.Bytes())
	restored, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load restored spec: %v", err)
	}
	if len(restored.BusinessPools) != 1 || restored.BusinessPools[0].ID != "ai" {
		t.Fatalf("restore should restore backed up spec, got %+v", restored.BusinessPools)
	}
	if !provider.NewLastGoodStore(server.cfg.Layout).Exists("p1") {
		t.Fatal("restore should keep backed up last-good")
	}
	if provider.NewLastGoodStore(server.cfg.Layout).Exists("stale") {
		t.Fatal("restore should remove last-good entries that are not in the backup")
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatal(err)
	}
	if failoverState.Enabled {
		t.Fatalf("restore preserved current failover enabled state instead of archive: %+v", failoverState)
	}
	if runner, ok := server.cfg.NetGateRunner.(*configTestRunner); !ok || runner.calls == 0 {
		t.Fatalf("restore did not reconcile the OpenWrt data plane: %#v", server.cfg.NetGateRunner)
	}
}

func TestBackupRestoreReturnsLockedDuringRescue(t *testing.T) {
	server, store := newBackupTestServer(t)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	record, err := backup.Create(context.Background(), server.cfg.Layout, store, "manual")
	if err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	enterReq := httptest.NewRequest(http.MethodPost, "/api/runtime/rescue/enter", nil)
	enterResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(enterResp, enterReq)
	if enterResp.Code != http.StatusAccepted {
		t.Fatalf("enter rescue status = %d body=%s", enterResp.Code, enterResp.Body.String())
	}
	restoreBody := mustJSON(t, backupRestorePayload{OpID: "backup-restore-locked", Path: record.Path})
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/backups/restore", bytes.NewReader(restoreBody))
	restoreResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(restoreResp, restoreReq)
	if restoreResp.Code != http.StatusLocked {
		t.Fatalf("restore during rescue status = %d body=%s", restoreResp.Code, restoreResp.Body.String())
	}
}

func newBackupTestServer(t *testing.T) (*Server, *state.Store) {
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
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, state.NewSnapshotStore(layout), layout)
	netGateRunner := &configTestRunner{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, nil, netGateRunner)
	server := NewServer(Config{
		ListenAddr:    "127.0.0.1:3000",
		Layout:        layout,
		Locks:         locks,
		CommitLane:    commitMgr,
		Store:         store,
		BackupTxn:     backupTxn,
		Rescue:        rescueMgr,
		Owners:        owners,
		NetGateRunner: netGateRunner,
		Workers:       workers,
		Fabric:        fabricManager,
		Status:        NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}
