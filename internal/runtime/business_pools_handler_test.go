package runtime

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"smartgate/internal/business"
	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestBusinessPoolHandlersDryRunAndApply(t *testing.T) {
	server, store := newBusinessPoolTestServer(t)

	payload := businessPoolTxnPayload{
		OpID: "api-pool-1",
		Changes: []business.Change{{
			Action: business.ActionUpsert,
			Pool: state.BusinessPoolSpec{
				ID:      "ai",
				Name:    "AI",
				Role:    "ai",
				NodeIDs: []string{"n1", "n2"},
			},
		}},
	}

	dryRunReq := httptest.NewRequest(http.MethodPost, "/api/business-pools/dryrun", bytes.NewReader(mustJSON(t, payload)))
	dryRunResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(dryRunResp, dryRunReq)
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after dryrun: %v", err)
	}
	if len(spec.BusinessPools) != len(state.DefaultBusinessPools()) {
		t.Fatalf("dryrun should not persist business pools: %+v", spec.BusinessPools)
	}

	applyReq := httptest.NewRequest(http.MethodPost, "/api/business-pools/apply", bytes.NewReader(mustJSON(t, payload)))
	applyResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(applyResp, applyReq)
	if applyResp.Code != http.StatusAccepted {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after apply: %v", err)
	}
	if len(spec.BusinessPools) != 5 || spec.BusinessPools[1].ID != "ai" {
		t.Fatalf("apply should persist ai pool, got %+v", spec.BusinessPools)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/business-pools", nil)
	listResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResp.Code, listResp.Body.String())
	}
}

func TestBusinessPoolApplyReturnsLockedDuringRescue(t *testing.T) {
	server, _ := newBusinessPoolTestServer(t)
	enterReq := httptest.NewRequest(http.MethodPost, "/api/runtime/rescue/enter", nil)
	enterResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(enterResp, enterReq)
	if enterResp.Code != http.StatusAccepted {
		t.Fatalf("enter rescue status = %d body=%s", enterResp.Code, enterResp.Body.String())
	}

	payload := businessPoolTxnPayload{
		OpID: "api-pool-locked",
		Changes: []business.Change{{
			Action: business.ActionUpsert,
			Pool: state.BusinessPoolSpec{
				ID:      "ai",
				Name:    "AI",
				Role:    "ai",
				NodeIDs: []string{"n1"},
			},
		}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/business-pools/apply", bytes.NewReader(mustJSON(t, payload)))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked {
		t.Fatalf("apply during rescue status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func newBusinessPoolTestServer(t *testing.T) (*Server, *state.Store) {
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
	poolTxn := commitlane.NewBusinessPoolTxn(commitMgr, store, state.NewSnapshotStore(layout))
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      locks,
		CommitLane: commitMgr,
		Store:      store,
		PoolTxn:    poolTxn,
		Rescue:     rescueMgr,
		Owners:     owners,
		Status:     NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}
