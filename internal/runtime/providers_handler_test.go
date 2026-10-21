package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type runtimeProviderFetcher struct {
	calls     []provider.FetchRequest
	resp      map[string]provider.FetchResponse
	afterCall func(provider.FetchRequest)
}

func (f *runtimeProviderFetcher) Fetch(_ context.Context, req provider.FetchRequest) (provider.FetchResponse, error) {
	f.calls = append(f.calls, req)
	if f.afterCall != nil {
		f.afterCall(req)
	}
	return f.resp[req.Provider.ID], nil
}

func TestProviderHandlersDryRunAndApply(t *testing.T) {
	server, store := newProviderTestServer(t)

	payload := providerTxnPayload{
		OpID: "api-provider-1",
		Changes: []provider.Change{{
			Action: provider.ActionUpsert,
			Provider: state.ProviderSpec{
				ID:       "p1",
				Name:     "Provider 1",
				Type:     provider.TypeManual,
				Enabled:  true,
				Priority: 10,
			},
		}},
	}

	dryRunBody := mustJSON(t, payload)
	dryRunReq := httptest.NewRequest(http.MethodPost, "/api/providers/dryrun", bytes.NewReader(dryRunBody))
	dryRunResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(dryRunResp, dryRunReq)
	if dryRunResp.Code != http.StatusOK {
		t.Fatalf("dryrun status = %d body=%s", dryRunResp.Code, dryRunResp.Body.String())
	}

	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after dryrun: %v", err)
	}
	if len(spec.Providers) != 0 {
		t.Fatalf("dryrun should not persist providers: %+v", spec.Providers)
	}

	applyBody := mustJSON(t, payload)
	applyReq := httptest.NewRequest(http.MethodPost, "/api/providers/apply", bytes.NewReader(applyBody))
	applyResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(applyResp, applyReq)
	if applyResp.Code != http.StatusAccepted {
		t.Fatalf("apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}

	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after apply: %v", err)
	}
	if len(spec.Providers) != 1 || spec.Providers[0].ID != "p1" {
		t.Fatalf("apply should persist provider p1, got %+v", spec.Providers)
	}
}

func TestProviderApplyReturnsLockedDuringRescue(t *testing.T) {
	server, _ := newProviderTestServer(t)

	enterReq := httptest.NewRequest(http.MethodPost, "/api/runtime/rescue/enter", nil)
	enterResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(enterResp, enterReq)
	if enterResp.Code != http.StatusAccepted {
		t.Fatalf("enter rescue status = %d body=%s", enterResp.Code, enterResp.Body.String())
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	statusResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(statusResp, statusReq)
	if statusResp.Code != http.StatusOK {
		t.Fatalf("runtime status code = %d body=%s", statusResp.Code, statusResp.Body.String())
	}
	statusData := decodeEnvelopeData(t, statusResp.Body.Bytes())
	if statusData["phase"] != StatusRescue || statusData["rescue_active"] != true {
		t.Fatalf("expected dynamic rescue status, got %+v", statusData)
	}

	payload := providerTxnPayload{
		OpID: "api-provider-locked",
		Changes: []provider.Change{{
			Action: provider.ActionUpsert,
			Provider: state.ProviderSpec{
				ID:      "p1",
				Name:    "Provider 1",
				Type:    provider.TypeManual,
				Enabled: true,
			},
		}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/providers/apply", bytes.NewReader(mustJSON(t, payload)))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked {
		t.Fatalf("apply during rescue status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestProviderRefreshAllWritesLastGood(t *testing.T) {
	fetcher := &runtimeProviderFetcher{
		resp: map[string]provider.FetchResponse{
			"p1": {
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Subscription-Userinfo": []string{"upload=1024; download=2048; total=8192; expire=1799999999"},
				},
				Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`),
			},
		},
	}
	server, store := newProviderTestServerWithRef(t, fetcher)

	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/providers/refresh-all", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("refresh-all status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	results, ok := data["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("unexpected refresh results: %+v", data["results"])
	}
	result, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected refresh result item: %+v", results[0])
	}
	subscription, ok := result["subscription"].(map[string]any)
	if !ok || subscription["remaining_bytes"].(float64) != 5120 {
		t.Fatalf("subscription info missing from refresh response: %+v", result)
	}
	if _, exists := subscription["raw"]; exists || strings.Contains(resp.Body.String(), "upload=1024") {
		t.Fatalf("refresh response leaked raw subscription metadata: %s", resp.Body.String())
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("expected one fetch call, got %+v", fetcher.calls)
	}
	env, err := provider.NewLastGoodStore(server.cfg.Layout).Load("p1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if env.Count != 1 || len(env.Outbounds) != 0 || len(env.Identities) != 1 || env.Identities[0].RawTag != "node-1" {
		t.Fatalf("unexpected last-good: %+v", env)
	}
	if env.Subscription == nil || env.Subscription.UsedBytes != 3072 || env.Subscription.TotalBytes != 8192 {
		t.Fatalf("subscription info not persisted in last-good: %+v", env.Subscription)
	}
	providersResp := serve(t, server, http.MethodGet, "/api/providers", nil)
	if providersResp.Code != http.StatusOK {
		t.Fatalf("providers status = %d body=%s", providersResp.Code, providersResp.Body.String())
	}
	providersData := decodeEnvelopeData(t, providersResp.Body.Bytes())
	providers, ok := providersData["providers"].([]any)
	if !ok || len(providers) != 1 {
		t.Fatalf("unexpected providers response: %+v", providersData["providers"])
	}
	providerView, ok := providers[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected provider view: %+v", providers[0])
	}
	viewSubscription, ok := providerView["subscription"].(map[string]any)
	if !ok || viewSubscription["expire_unix"].(float64) != 1799999999 {
		t.Fatalf("subscription info missing from providers list: %+v", providerView)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if len(runtimeState.ProviderLogs) != 1 || runtimeState.ProviderLogs[0].ProviderID != "p1" || runtimeState.ProviderLogs[0].Status != "ok" {
		t.Fatalf("refresh log not recorded: %+v", runtimeState.ProviderLogs)
	}
}

func TestProviderLogsSanitizeLegacyErrors(t *testing.T) {
	server, store := newProviderTestServer(t)
	secretURL := "https://user:token@provider.example/sub?key=secret"
	if err := store.SaveRuntime(state.RuntimeState{ProviderLogs: []state.ProviderRefreshLog{{
		Time: time.Now().UTC(), Scope: "all", ProviderID: "p1", Status: "failed", Error: "fetch " + secretURL + " token=abc123",
	}}}); err != nil {
		t.Fatal(err)
	}
	resp := serve(t, server, http.MethodGet, "/api/providers/logs", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("provider logs status=%d body=%s", resp.Code, resp.Body.String())
	}
	for _, secret := range []string{secretURL, "user:token", "/sub?key=secret", "abc123"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("provider log response leaked %q: %s", secret, resp.Body.String())
		}
	}
}

func TestProviderRefreshMarksAliasRenameMetadataOnly(t *testing.T) {
	oldRaw := []map[string]any{{
		"type": "vless", "tag": "old-name", "server": "node.example", "server_port": 443, "uuid": "secret",
	}}
	newRaw := []map[string]any{{
		"type": "vless", "tag": "new-name", "server": "node.example", "server_port": 443, "uuid": "secret",
	}}
	fetcher := &runtimeProviderFetcher{resp: map[string]provider.FetchResponse{
		"p1": {StatusCode: http.StatusOK, Body: mustJSON(t, newRaw)},
	}}
	server, store := newProviderTestServerWithRef(t, fetcher)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID: "p1", Name: "Provider 1", Type: provider.TypeSubscription, Enabled: true,
		URL: "https://p1.example/sub", FetchMode: provider.FetchDirect,
	}}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", oldRaw); err != nil {
		t.Fatal(err)
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var originalID string
	var originalRevision int
	for id, node := range spec.SmartNodes {
		originalID, originalRevision = id, node.Revision
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodPost, "/api/providers/p1/refresh", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("refresh status=%d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	results := data["results"].([]any)
	result := results[0].(map[string]any)
	if result["metadata_only"] != true {
		t.Fatalf("alias rename was not marked metadata-only: %+v", result)
	}
	saved, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.SmartNodes) != 1 || saved.SmartNodes[originalID].Revision != originalRevision {
		t.Fatalf("alias rename changed SmartNode identity or revision: before=%s/r%d after=%+v", originalID, originalRevision, saved.SmartNodes)
	}
}

func TestProviderRefreshDoesNotMutateAppliedRuntimeReferences(t *testing.T) {
	oldRaw := []map[string]any{{"type": "vmess", "tag": "old-node", "server": "old.example"}}
	newRaw := []map[string]any{{"type": "vmess", "tag": "new-node", "server": "new.example"}}
	fetcher := &runtimeProviderFetcher{
		resp: map[string]provider.FetchResponse{
			"p1": {StatusCode: http.StatusOK, Body: mustJSON(t, newRaw)},
		},
	}
	server, store := newProviderTestServerWithRef(t, fetcher)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	oldID := provider.NormalizeProviderOutbounds(spec.Providers[0], oldRaw)[0].ID
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{oldID}, Policy: state.TemplatePoolPolicy("ai")},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", oldRaw); err != nil {
		t.Fatalf("save old last-good: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"ai": oldID, "general": "Direct"},
		NodeHealth:     map[string]state.NodeHealth{oldID: {Status: "ok"}},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	if err := state.NewFailoverStateStore(server.cfg.Layout).Save(state.FailoverState{
		Enabled:         true,
		CooldownSeconds: state.DefaultFailoverCooldownSeconds,
		Pools: map[string]state.FailoverPoolState{
			"ai": {PoolID: "ai", Role: "ai", Current: oldID, Previous: oldID, Terminal: true},
		},
	}); err != nil {
		t.Fatalf("save failover state: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/providers/refresh-all", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("refresh-all status = %d body=%s", resp.Code, resp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if got := spec.BusinessPools[1].NodeIDs; len(got) != 1 || got[0] != oldID {
		t.Fatalf("referenced SmartNode must survive source loss, got %+v", got)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["ai"] != oldID {
		t.Fatalf("provider refresh mutated applied runtime selection, got %+v", runtimeState.ProxySelection)
	}
	if runtimeState.ProxySelection["general"] != "Direct" {
		t.Fatalf("provider refresh mutated unrelated running selection, got %+v", runtimeState.ProxySelection)
	}
	if runtimeState.NodeHealth[oldID].Status != "ok" {
		t.Fatalf("provider refresh mutated running node health, got %+v", runtimeState.NodeHealth)
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatalf("load failover state: %v", err)
	}
	if failoverState.Pools["ai"].Current != oldID {
		t.Fatalf("provider refresh mutated running failover state, got %+v", failoverState.Pools)
	}
	env, err := provider.NewLastGoodStore(server.cfg.Layout).Load("p1")
	if err != nil {
		t.Fatalf("load refreshed last-good: %v", err)
	}
	if len(env.Outbounds) != 0 || len(env.Identities) != 1 || env.Identities[0].RawTag != "new-node" {
		t.Fatalf("expected refreshed identity-only last-good, got %+v", env)
	}
}

func TestProviderManualNodesEndpointWritesLastGood(t *testing.T) {
	server, store := newProviderTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "manual",
		Name:    "Manual",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	secret := "manual-node-secret"
	resp := serve(t, server, http.MethodPut, "/api/providers/manual/nodes", mustJSON(t, manualProviderNodesPayload{
		Outbounds: []map[string]any{{"type": "trojan", "tag": "manual-1", "server": "manual.example", "server_port": 443, "password": secret}},
	}))
	if resp.Code != http.StatusOK {
		t.Fatalf("put manual nodes status = %d body=%s", resp.Code, resp.Body.String())
	}
	env := decodeEnvelopeData(t, resp.Body.Bytes())
	if env["provider_id"] != "manual" || env["count"].(float64) != 1 {
		t.Fatalf("unexpected manual nodes response: %+v", env)
	}
	if strings.Contains(resp.Body.String(), secret) || strings.Contains(resp.Body.String(), "outbounds") {
		t.Fatalf("manual node response leaked credentials: %s", resp.Body.String())
	}

	getResp := serve(t, server, http.MethodGet, "/api/providers/manual/nodes", nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("get manual nodes status = %d body=%s", getResp.Code, getResp.Body.String())
	}
	if strings.Contains(getResp.Body.String(), secret) || strings.Contains(getResp.Body.String(), "outbounds") {
		t.Fatalf("manual node GET leaked credentials: %s", getResp.Body.String())
	}
	lastGood, err := provider.NewLastGoodStore(server.cfg.Layout).Load("manual")
	if err != nil {
		t.Fatalf("load manual last-good: %v", err)
	}
	if lastGood.Count != 1 || len(lastGood.Outbounds) != 0 || len(lastGood.Identities) != 1 || lastGood.Identities[0].RawTag != "manual-1" {
		t.Fatalf("unexpected manual last-good: %+v", lastGood)
	}
}

func TestProviderListMasksWriteOnlySources(t *testing.T) {
	server, store := newProviderTestServer(t)
	secretURL := "https://user:token@provider.example/sub?key=secret"
	secretPath := "/etc/smartgate/providers/private-nodes.json"
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "remote", Name: "Remote", Type: provider.TypeSubscription, Enabled: true, URL: secretURL},
		{ID: "local", Name: "Local", Type: provider.TypeLocalFile, Enabled: true, FilePath: secretPath},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/providers", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("provider list status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{secretURL, "user:token", "/sub?key=secret", secretPath} {
		if strings.Contains(body, secret) {
			t.Fatalf("provider list leaked %q: %s", secret, body)
		}
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	providers := data["providers"].([]any)
	remote := providers[0].(map[string]any)
	if remote["url_configured"] != true || remote["url_masked"] != "https://provider.example" {
		t.Fatalf("unexpected remote source summary: %+v", remote)
	}
	local := providers[1].(map[string]any)
	if local["file_path_configured"] != true {
		t.Fatalf("unexpected local source summary: %+v", local)
	}
}

func TestProviderRefreshOneRejectsMissingBootstrapLastGood(t *testing.T) {
	fetcher := &runtimeProviderFetcher{resp: map[string]provider.FetchResponse{}}
	server, store := newProviderTestServerWithRef(t, fetcher)

	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "boot", Name: "Bootstrap", Type: provider.TypeSubscription, Enabled: true, URL: "https://boot.example/sub", FetchMode: provider.FetchDirect},
		{ID: "p1", Name: "Provider 1", Type: provider.TypeSubscription, Enabled: true, URL: "https://p1.example/sub", FetchMode: provider.FetchViaBootstrap, Bootstrap: "boot"},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/providers/p1/refresh", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("refresh one status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(fetcher.calls) != 0 {
		t.Fatalf("expected no fetch without bootstrap last-good, got %+v", fetcher.calls)
	}
}

func TestProviderRefreshAllReturnsLockedWhenRescueBlocksLastGoodSave(t *testing.T) {
	fetcher := &runtimeProviderFetcher{
		resp: map[string]provider.FetchResponse{
			"p1": {StatusCode: http.StatusOK, Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`)},
		},
	}
	server, store := newProviderTestServerWithRef(t, fetcher)

	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := os.WriteFile(server.cfg.Layout.RescueStatusPath(), []byte(`{"active":true}`), 0o644); err != nil {
		t.Fatalf("write rescue flag: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/providers/refresh-all", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked {
		t.Fatalf("refresh-all during rescue status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("expected fetch before controlled save rejection, got %+v", fetcher.calls)
	}
	if provider.NewLastGoodStore(server.cfg.Layout).Exists("p1") {
		t.Fatal("last-good should not be written when rescue blocks the controlled save")
	}
}

func TestProviderRefreshAllBlocksSaveWhenRescueAppearsAfterFetch(t *testing.T) {
	var server *Server
	fetcher := &runtimeProviderFetcher{
		resp: map[string]provider.FetchResponse{
			"p1": {StatusCode: http.StatusOK, Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`)},
		},
		afterCall: func(provider.FetchRequest) {
			if err := os.WriteFile(server.cfg.Layout.RescueStatusPath(), []byte(`{"active":true}`), 0o644); err != nil {
				panic(err)
			}
		},
	}
	server, store := newProviderTestServerWithRef(t, fetcher)

	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/providers/refresh-all", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked {
		t.Fatalf("refresh-all after rescue appears status = %d body=%s", resp.Code, resp.Body.String())
	}
	if provider.NewLastGoodStore(server.cfg.Layout).Exists("p1") {
		t.Fatal("last-good should not be written when rescue appears before SaveMany")
	}
}

func newProviderTestServer(t *testing.T) (*Server, *state.Store) {
	return newProviderTestServerWithRef(t, nil)
}

func newProviderTestServerWithRef(t *testing.T, fetcher provider.Fetcher) (*Server, *state.Store) {
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
	txn := commitlane.NewProviderTxn(commitMgr, store, state.NewSnapshotStore(layout))
	server := NewServer(Config{
		ListenAddr:  "127.0.0.1:3000",
		Layout:      layout,
		Locks:       locks,
		CommitLane:  commitMgr,
		Store:       store,
		ProviderTxn: txn,
		ProviderRef: provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)).WithStateStore(store), fetcher),
		Rescue:      rescueMgr,
		Owners:      owners,
		Status:      NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return b
}
