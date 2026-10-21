package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"smartgate/internal/auth"
	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
)

// This file is the whole-surface contract test for the management API. It
// closes pre-hardware audit findings S1-1 (systematic sensitive echo), S1-2
// (query-string tokens), and S2-2 (envelope drift) by checking every
// registered route rather than a hand-picked subset:
//
//   - the route table below must list every mux.HandleFunc pattern in this
//     package, so a new endpoint cannot ship without being covered;
//   - every /api route requires a Bearer token and rejects ?token= (except
//     /api/events), returning the standard error envelope;
//   - every JSON response, success or failure, is {ok, data} or
//     {ok:false, error:{code,message[,details]}} with ok matching the status;
//   - no response, including validation errors that echo input, contains any
//     of the sensitive values planted into spec, last-good, scheduler, Wi-Fi,
//     or request bodies.

const (
	plantedSubToken      = "PLANTED-SUB-TOKEN-8f3a91c2"
	plantedSubHost       = "planted-airport.invalid"
	plantedUserAgent     = "planted-agent/9.9.9"
	plantedNodePassword  = "PLANTED-NODE-PASSWORD-77c1"
	plantedNodeUUID      = "PLANTED-UUID-0000-1111-2222"
	plantedNodeTag       = "planted-node"
	plantedSchedSecret   = "scheduler-secret"
	plantedWiFiKey       = "PLANTED-WIFI-KEY-c0ffee"
	plantedAdminPassword = "PLANTED-ADMIN-PASSWORD-1234"
	plantedBodySecret    = "request-secret"
)

// sensitiveMarkers are the values that must never appear in any response.
// Deliberately absent: plantedUserAgent (user_agent is a plain read/write
// field by contract) and plantedSubHost (url_masked keeps scheme+host so the
// operator can recognise the provider; only the path/query/userinfo of a
// subscription URL is secret).
var sensitiveMarkers = []string{
	plantedSubToken,
	plantedNodePassword,
	plantedNodeUUID,
	plantedSchedSecret,
	plantedWiFiKey,
	plantedAdminPassword,
	plantedBodySecret,
}

type contractRoute struct {
	pattern  string   // exact string passed to mux.HandleFunc
	probe    string   // concrete request path (differs for prefix patterns)
	methods  []string // methods the handler accepts
	public   bool     // reachable without a Bearer token
	envelope bool     // responses use the JSON API envelope
	body     []byte   // request body for the first method in methods, if any
	query    string   // optional query string for the probe
}

func contractRoutes(t *testing.T) []contractRoute {
	t.Helper()
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}
	getPut := []string{http.MethodGet, http.MethodPut}
	getPost := []string{http.MethodGet, http.MethodPost}
	opID := func(id string) []byte { return mustJSON(t, map[string]any{"op_id": id}) }
	return []contractRoute{
		{pattern: "/", probe: "/", methods: get, public: true},
		{pattern: "/healthz", probe: "/healthz", methods: get, public: true},
		{pattern: "/api/runtime/locks", probe: "/api/runtime/locks", methods: get},

		{pattern: "/api/auth/check", probe: "/api/auth/check", methods: get, public: true, envelope: true},
		{pattern: "/api/auth/login", probe: "/api/auth/login", methods: post, public: true, envelope: true, body: mustJSON(t, map[string]string{"password": plantedBodySecret})},
		{pattern: "/api/auth/logout", probe: "/api/auth/logout", methods: post, public: true, envelope: true},
		{pattern: "/api/auth/password", probe: "/api/auth/password", methods: []string{http.MethodPut}, public: true, envelope: true, body: mustJSON(t, map[string]string{"old_password": plantedBodySecret, "new_password": plantedBodySecret + "-new"})},
		{pattern: "/api/auth/setup", probe: "/api/auth/setup", methods: post, public: true, envelope: true, body: mustJSON(t, map[string]string{"password": plantedBodySecret})},

		{pattern: "/api/version", probe: "/api/version", methods: get, envelope: true},
		{pattern: "/api/cores", probe: "/api/cores", methods: get, envelope: true},
		{pattern: "/api/events", probe: "/api/events", methods: get},

		{pattern: "/api/runtime/status", probe: "/api/runtime/status", methods: get, envelope: true},
		{pattern: "/api/runtime/rescue/enter", probe: "/api/runtime/rescue/enter", methods: post, envelope: true, body: opID("contract-rescue-enter")},
		{pattern: "/api/runtime/rescue/clear", probe: "/api/runtime/rescue/clear", methods: post, envelope: true, body: opID("contract-rescue-clear")},
		{pattern: "/api/runtime/recover", probe: "/api/runtime/recover", methods: post, envelope: true, body: opID("contract-recover")},
		{pattern: "/api/runtime/openwrt/dryrun", probe: "/api/runtime/openwrt/dryrun", methods: post, envelope: true, body: opID("contract-openwrt")},
		{pattern: "/api/runtime/openwrt/rescue-cleanup/dryrun", probe: "/api/runtime/openwrt/rescue-cleanup/dryrun", methods: post, envelope: true, body: opID("contract-openwrt-rescue")},

		{pattern: "/api/system/status", probe: "/api/system/status", methods: get, envelope: true},
		{pattern: "/api/system/network/diagnostics", probe: "/api/system/network/diagnostics", methods: get, envelope: true},
		{pattern: "/api/system/network/repair", probe: "/api/system/network/repair", methods: post, envelope: true, body: mustJSON(t, map[string]any{"confirm": false})},
		{pattern: "/api/system/logs", probe: "/api/system/logs", methods: get, envelope: true},
		{pattern: "/api/system/service/", probe: "/api/system/service/sing-box/restart", methods: post, envelope: true, body: opID("contract-service")},
		{pattern: "/api/system/reboot", probe: "/api/system/reboot", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-reboot", "confirm": false})},
		{pattern: "/api/system/dns/flush", probe: "/api/system/dns/flush", methods: post, envelope: true, body: opID("contract-dns-flush")},

		{pattern: "/api/wifi/status", probe: "/api/wifi/status", methods: get, envelope: true},
		{pattern: "/api/wifi/scan", probe: "/api/wifi/scan", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-wifi-scan", "radio": "radio0"})},
		{pattern: "/api/wifi/clients", probe: "/api/wifi/clients", methods: get, envelope: true},
		{pattern: "/api/wifi/config", probe: "/api/wifi/config", methods: getPut, envelope: true},

		{pattern: "/api/connections", probe: "/api/connections", methods: get, envelope: true},
		{pattern: "/api/connections/close", probe: "/api/connections/close", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-close", "ids": []string{"missing"}})},

		{pattern: "/api/config/state", probe: "/api/config/state", methods: get, envelope: true},
		{pattern: "/api/config/dryrun", probe: "/api/config/dryrun", methods: post, envelope: true, body: opID("contract-config-dryrun")},
		{pattern: "/api/config/apply", probe: "/api/config/apply", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-config-apply", "dryrun_id": "missing"})},
		{pattern: "/api/config/discard", probe: "/api/config/discard", methods: post, envelope: true, body: opID("contract-config-discard")},
		{pattern: "/api/gateway/mode", probe: "/api/gateway/mode", methods: getPost, envelope: true},
		{pattern: "/api/overview", probe: "/api/overview", methods: get, envelope: true},
		{pattern: "/api/dns", probe: "/api/dns", methods: getPut, envelope: true},

		{pattern: "/api/workers", probe: "/api/workers", methods: get, envelope: true},
		{pattern: "/api/workers/", probe: "/api/workers/missing-worker", methods: get, envelope: true},

		{pattern: "/api/routes/explain", probe: "/api/routes/explain", methods: post, envelope: true, body: mustJSON(t, map[string]any{"target": "example.com"})},
		{pattern: "/api/rules", probe: "/api/rules", methods: getPut, envelope: true},
		{pattern: "/api/rules/conflicts", probe: "/api/rules/conflicts", methods: get, envelope: true},
		{pattern: "/api/rules/resolve-conflict", probe: "/api/rules/resolve-conflict", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-resolve", "rule_id": "missing", "resolution": "keep_user"})},
		{pattern: "/api/rule-sets", probe: "/api/rule-sets", methods: getPut, envelope: true},

		{pattern: "/api/nodes", probe: "/api/nodes", methods: get, envelope: true},
		{pattern: "/api/nodes/delay-all", probe: "/api/nodes/delay-all", methods: post, envelope: true},
		{pattern: "/api/nodes/", probe: "/api/nodes/" + plantedNodeTag + "/delay", methods: post, envelope: true, body: opID("contract-node-delay")},
		{pattern: "/api/proxy-groups", probe: "/api/proxy-groups", methods: get, envelope: true},
		{pattern: "/api/proxy-groups/", probe: "/api/proxy-groups/general/select", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-select", "node_id": "missing"})},

		{pattern: "/api/providers", probe: "/api/providers", methods: get, envelope: true},
		{pattern: "/api/providers/dryrun", probe: "/api/providers/dryrun", methods: post, envelope: true, body: mustJSON(t, map[string]any{
			"op_id": "contract-provider-dryrun",
			"changes": []map[string]any{{"action": "upsert", "provider": map[string]any{
				"id": "planted", "name": "Planted", "type": "subscription", "enabled": true, "fetch_mode": "direct",
				"url": "https://" + plantedSubHost + "/sub?token=" + plantedSubToken, "user_agent": plantedUserAgent, "priority": 10,
			}}},
		})},
		{pattern: "/api/providers/apply", probe: "/api/providers/apply", methods: post, envelope: true, body: mustJSON(t, map[string]any{
			"op_id": "contract-provider-apply-invalid",
			"changes": []map[string]any{{"action": "upsert", "provider": map[string]any{
				"id": "bad id!", "name": "Bad", "type": "subscription", "enabled": true, "fetch_mode": "direct",
				"url": "ftp://user:" + plantedBodySecret + "@" + plantedSubHost + "/sub",
			}}},
		})},
		{pattern: "/api/providers/logs", probe: "/api/providers/logs", methods: get, envelope: true},
		{pattern: "/api/providers/refresh-all", probe: "/api/providers/refresh-all", methods: post, envelope: true, body: opID("contract-refresh-all")},
		{pattern: "/api/providers/", probe: "/api/providers/manual/nodes", methods: getPut, envelope: true},

		{pattern: "/api/business-pools", probe: "/api/business-pools", methods: get, envelope: true},
		{pattern: "/api/business-pools/dryrun", probe: "/api/business-pools/dryrun", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-pools-dryrun", "changes": []any{}})},
		{pattern: "/api/business-pools/apply", probe: "/api/business-pools/apply", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-pools-apply", "changes": []any{}})},

		{pattern: "/api/backups/create", probe: "/api/backups/create", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-backup", "kind": "manual"})},
		{pattern: "/api/backups/restore", probe: "/api/backups/restore", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-restore", "backup_id": "missing"})},

		{pattern: "/api/snapshots", probe: "/api/snapshots", methods: getPost, envelope: true},
		{pattern: "/api/snapshots/rollback/dryrun", probe: "/api/snapshots/rollback/dryrun", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-rollback-dryrun", "snapshot_id": "missing"})},
		{pattern: "/api/snapshots/rollback/apply", probe: "/api/snapshots/rollback/apply", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-rollback-apply", "dryrun_id": "missing"})},

		{pattern: "/api/scheduler", probe: "/api/scheduler", methods: getPost, envelope: true},
		{pattern: "/api/scheduler/", probe: "/api/scheduler/planted-shell", methods: []string{http.MethodPut, http.MethodDelete}, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-sched-put", "name": "x", "type": scheduler.TaskCustomShell, "enabled": false, "schedule": "@every 1h", "spec": map[string]any{"command": "/bin/true", "password": plantedBodySecret}})},
		{pattern: "/api/scheduler/run-now", probe: "/api/scheduler/run-now", methods: post, envelope: true, body: mustJSON(t, map[string]any{"op_id": "contract-run-now", "task_id": "missing"})},
	}
}

// newContractTestServer wires every optional Config dependency so that all
// handlers register, enables auth, and plants sensitive values everywhere the
// product persists operator-provided material.
func newContractTestServer(t *testing.T) (*Server, string) {
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
	netGateRunner := &configTestRunner{}
	control := &fakeProxyControl{}
	workers, fabricManager := newRuntimeTestDataPlane(t, owners, control, netGateRunner)
	snapshotStore := state.NewSnapshotStore(layout)
	fetcher := &runtimeProviderFetcher{resp: map[string]provider.FetchResponse{}}
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)).WithStateStore(store), fetcher)
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, snapshotStore, layout)
	runner := scheduler.NewRunner(store, refresher).WithBackupCreator(testSchedulerBackupCreator{txn: backupTxn})
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"uci", "show", "wireless"}): {output: strings.Join([]string{
			"wireless.radio0=wifi-device",
			"wireless.radio0.type='mac80211'",
			"wireless.radio0.band='2g'",
			"wireless.radio0.disabled='0'",
			"wireless.default_radio0=wifi-iface",
			"wireless.default_radio0.device='radio0'",
			"wireless.default_radio0.network='lan'",
			"wireless.default_radio0.mode='ap'",
			"wireless.default_radio0.ssid='SmartGate'",
			"wireless.default_radio0.encryption='psk2'",
			"wireless.default_radio0.key='" + plantedWiFiKey + "'",
			"wireless.default_radio0.disabled='0'",
		}, "\n")},
		systemTestKey([]string{"logread", "-l", "200"}): {output: "daemon.info smartgate: provider planted fetched https://" + plantedSubHost + "/sub?token=" + plantedSubToken + "\n"},
	}}
	authStore := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))

	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{
			ID: "planted", Name: "Planted Subscription", Type: provider.TypeSubscription, Enabled: true,
			FetchMode: provider.FetchDirect,
			URL:       "https://" + plantedSubHost + "/sub?token=" + plantedSubToken,
			UserAgent: plantedUserAgent, Priority: 10, Roles: []string{"general"},
		},
		{ID: "manual", Name: "Manual", Type: provider.TypeManual, Enabled: true, Roles: []string{"general"}},
	}
	spec.Scheduler = append(spec.Scheduler, state.SchedulerTaskSpec{
		ID: "planted-shell", Name: "Planted", Type: scheduler.TaskCustomShell, Enabled: false, Schedule: "@every 1h",
		Spec: map[string]any{"command": "/bin/sh", "args": []any{"-c", "echo " + plantedSchedSecret}, "password": plantedSchedSecret},
	})
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Save("manual", []map[string]any{
		{"type": "trojan", "tag": plantedNodeTag, "server": "node.example", "server_port": 443, "password": plantedNodePassword},
		{"type": "vmess", "tag": plantedNodeTag + "-2", "server": "node2.example", "server_port": 443, "uuid": plantedNodeUUID},
	}); err != nil {
		t.Fatalf("save manual last-good: %v", err)
	}

	server := NewServer(Config{
		ListenAddr:    "127.0.0.1:3000",
		StaticDir:     t.TempDir(),
		Layout:        layout,
		Locks:         locks,
		CommitLane:    commitMgr,
		Store:         store,
		ProviderTxn:   commitlane.NewProviderTxn(commitMgr, store, snapshotStore),
		PoolTxn:       commitlane.NewBusinessPoolTxn(commitMgr, store, snapshotStore),
		BackupTxn:     backupTxn,
		SnapshotTxn:   commitlane.NewSnapshotTxn(commitMgr, store, snapshotStore),
		ProviderRef:   refresher,
		Scheduler:     runner,
		Rescue:        rescueMgr,
		Owners:        owners,
		NetGateRunner: netGateRunner,
		SystemRunner:  systemRunner,
		Workers:       workers,
		Fabric:        fabricManager,
		CoreRegistry:  newRuntimeTestCoreRegistry(t),
		ProxyControl:  control,
		Status:        NewStatus("127.0.0.1:3000", state.RuntimeState{}),
		Auth:          authStore,
		AuthRequired:  true,
	})

	setup := serve(t, server, http.MethodPost, "/api/auth/setup", mustJSON(t, map[string]string{"password": plantedAdminPassword}))
	if setup.Code != http.StatusOK {
		t.Fatalf("auth setup status = %d body=%s", setup.Code, setup.Body.String())
	}
	token, _ := decodeEnvelopeData(t, setup.Body.Bytes())["token"].(string)
	if token == "" {
		t.Fatal("auth setup did not return a token")
	}
	return server, token
}

func contractRequest(t *testing.T, server *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytesReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if strings.HasPrefix(req.URL.Path, "/api/events") {
		// SSE streams until the client goes away; give it one tick and cancel.
		ctx, cancel := context.WithTimeout(req.Context(), 150*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
	}
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	return resp
}

// assertEnvelope enforces the docs/API_CONTRACT.md response shape.
func assertEnvelope(t *testing.T, label string, resp *httptest.ResponseRecorder) {
	t.Helper()
	if resp.Code == http.StatusNoContent || resp.Body.Len() == 0 {
		t.Fatalf("%s: JSON API must always return an envelope body (status %d)", label, resp.Code)
	}
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: Content-Type = %q, want application/json (status %d body=%s)", label, ct, resp.Code, truncateBody(resp))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatalf("%s: body is not a JSON object: %v body=%s", label, err, truncateBody(resp))
	}
	okRaw, has := raw["ok"]
	if !has {
		t.Fatalf("%s: envelope missing ok: %s", label, truncateBody(resp))
	}
	var ok bool
	if err := json.Unmarshal(okRaw, &ok); err != nil {
		t.Fatalf("%s: ok is not boolean: %s", label, truncateBody(resp))
	}
	for key := range raw {
		switch key {
		case "ok", "data", "error":
		default:
			t.Fatalf("%s: unexpected top-level envelope key %q: %s", label, key, truncateBody(resp))
		}
	}
	if ok != (resp.Code < 400) {
		t.Fatalf("%s: ok=%v does not match status %d: %s", label, ok, resp.Code, truncateBody(resp))
	}
	if ok {
		if _, hasErr := raw["error"]; hasErr {
			t.Fatalf("%s: success envelope carries error: %s", label, truncateBody(resp))
		}
		return
	}
	if _, hasData := raw["data"]; hasData {
		t.Fatalf("%s: error envelope carries top-level data: %s", label, truncateBody(resp))
	}
	var payload struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(raw["error"], &payload); err != nil {
		t.Fatalf("%s: error is not structured: %v body=%s", label, err, truncateBody(resp))
	}
	if strings.TrimSpace(payload.Code) == "" || strings.TrimSpace(payload.Message) == "" {
		t.Fatalf("%s: error envelope needs non-empty code and message: %s", label, truncateBody(resp))
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(payload.Code) {
		t.Fatalf("%s: error code %q is not snake_case: %s", label, payload.Code, truncateBody(resp))
	}
}

func assertNoSensitiveEcho(t *testing.T, label string, resp *httptest.ResponseRecorder) {
	t.Helper()
	body := resp.Body.String()
	for _, marker := range sensitiveMarkers {
		if strings.Contains(body, marker) {
			t.Fatalf("%s: response echoed sensitive value %q (status %d): %s", label, marker, resp.Code, truncateBody(resp))
		}
	}
	for key, values := range resp.Header() {
		for _, value := range values {
			for _, marker := range sensitiveMarkers {
				if strings.Contains(value, marker) {
					t.Fatalf("%s: header %s echoed sensitive value %q", label, key, marker)
				}
			}
		}
	}
}

func truncateBody(resp *httptest.ResponseRecorder) string {
	body := resp.Body.String()
	if len(body) > 600 {
		return body[:600] + "…"
	}
	return body
}

func TestAPIContractRouteTableCoversEveryRegisteredHandler(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`HandleFunc\("(/[^"]*)"`)
	registered := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllSubmatch(src, -1) {
			registered[string(match[1])] = name
		}
	}
	if len(registered) == 0 {
		t.Fatal("no HandleFunc registrations found; scan is broken")
	}
	covered := map[string]bool{}
	for _, route := range contractRoutes(t) {
		if covered[route.pattern] {
			t.Fatalf("route table lists %q twice", route.pattern)
		}
		covered[route.pattern] = true
		if !strings.HasPrefix(route.probe, strings.TrimSuffix(route.pattern, "/")) {
			t.Fatalf("probe %q does not fall under pattern %q", route.probe, route.pattern)
		}
		if len(route.methods) == 0 {
			t.Fatalf("route %q has no methods", route.pattern)
		}
	}
	var missing, stale []string
	for path, file := range registered {
		if !covered[path] {
			missing = append(missing, path+" ("+file+")")
		}
	}
	for path := range covered {
		if _, ok := registered[path]; !ok {
			stale = append(stale, path)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("contract route table out of sync.\nregistered but not covered: %v\ncovered but no longer registered: %v", missing, stale)
	}
}

func TestAPIContractAuthGateOnEveryRoute(t *testing.T) {
	server, token := newContractTestServer(t)
	for _, route := range contractRoutes(t) {
		if route.public || !isAPIPath(route.probe) {
			continue
		}
		route := route
		t.Run(route.pattern, func(t *testing.T) {
			method := route.methods[0]
			unauth := contractRequest(t, server, method, route.probe, "", route.body)
			if unauth.Code != http.StatusUnauthorized {
				t.Fatalf("no token: status = %d, want 401: %s", unauth.Code, truncateBody(unauth))
			}
			assertEnvelope(t, "no token", unauth)
			assertNoSensitiveEcho(t, "no token", unauth)

			bad := contractRequest(t, server, method, route.probe, "not-a-real-token", route.body)
			if bad.Code != http.StatusUnauthorized {
				t.Fatalf("bad token: status = %d, want 401: %s", bad.Code, truncateBody(bad))
			}
			assertEnvelope(t, "bad token", bad)

			sep := "?"
			if strings.Contains(route.probe, "?") {
				sep = "&"
			}
			viaQuery := contractRequest(t, server, method, route.probe+sep+"token="+token, "", route.body)
			if route.pattern == "/api/events" {
				if viaQuery.Code == http.StatusUnauthorized {
					t.Fatalf("/api/events must accept ?token= for EventSource: %s", truncateBody(viaQuery))
				}
				return
			}
			if viaQuery.Code != http.StatusUnauthorized {
				t.Fatalf("query token: status = %d, want 401: %s", viaQuery.Code, truncateBody(viaQuery))
			}
			assertEnvelope(t, "query token", viaQuery)
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(viaQuery.Body.Bytes(), &env)
			if env.Error.Code != "query_token_not_allowed" {
				t.Fatalf("query token: error.code = %q, want query_token_not_allowed", env.Error.Code)
			}
			if strings.Contains(viaQuery.Body.String(), token) {
				t.Fatal("query token: response echoed the token")
			}
		})
	}
}

func TestAPIContractMethodNotAllowedIsEnveloped(t *testing.T) {
	server, token := newContractTestServer(t)
	for _, route := range contractRoutes(t) {
		if !route.envelope {
			continue
		}
		route := route
		t.Run(route.pattern, func(t *testing.T) {
			resp := contractRequest(t, server, http.MethodTrace, route.probe, token, nil)
			if resp.Code != http.StatusMethodNotAllowed {
				t.Fatalf("TRACE status = %d, want 405: %s", resp.Code, truncateBody(resp))
			}
			assertEnvelope(t, "TRACE", resp)
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(resp.Body.Bytes(), &env)
			if env.Error.Code != "method_not_allowed" {
				t.Fatalf("TRACE error.code = %q, want method_not_allowed", env.Error.Code)
			}
		})
	}
}

func TestAPIContractEveryRouteRespondsWithoutSensitiveEcho(t *testing.T) {
	server, token := newContractTestServer(t)
	for _, route := range contractRoutes(t) {
		route := route
		t.Run(route.pattern, func(t *testing.T) {
			for i, method := range route.methods {
				var body []byte
				if i == 0 {
					body = route.body
				}
				if method == http.MethodPut && body == nil {
					// Send deliberately invalid JSON that carries a secret so the
					// decode error path is exercised without mutating state.
					body = []byte(`{"planted": "` + plantedBodySecret + `"`)
				}
				label := method + " " + route.probe
				resp := contractRequest(t, server, method, route.probe+route.query, token, body)
				if resp.Code >= 500 && resp.Code != http.StatusNotImplemented && resp.Code != http.StatusServiceUnavailable && resp.Code != http.StatusInsufficientStorage {
					t.Fatalf("%s: unexpected server error %d: %s", label, resp.Code, truncateBody(resp))
				}
				if resp.Code == http.StatusMethodNotAllowed {
					t.Fatalf("%s: route table claims this method is accepted but handler returned 405", label)
				}
				if route.envelope {
					assertEnvelope(t, label, resp)
				}
				assertNoSensitiveEcho(t, label, resp)
			}
		})
	}
}

func TestAPIContractValidationErrorsDoNotEchoInput(t *testing.T) {
	server, token := newContractTestServer(t)
	garbage := []byte(`{"planted": "` + plantedBodySecret + `", "url": "https://` + plantedSubHost + `/x?token=` + plantedSubToken + `"`)
	for _, route := range contractRoutes(t) {
		if !route.envelope {
			continue
		}
		route := route
		for _, method := range route.methods {
			if method == http.MethodGet || method == http.MethodDelete {
				continue
			}
			t.Run(method+" "+route.pattern, func(t *testing.T) {
				resp := contractRequest(t, server, method, route.probe, token, garbage)
				if resp.Code >= 500 {
					t.Fatalf("malformed body caused %d: %s", resp.Code, truncateBody(resp))
				}
				assertEnvelope(t, "malformed body", resp)
				assertNoSensitiveEcho(t, "malformed body", resp)
			})
		}
	}
}

func TestAPIContractProviderValidationDoesNotEchoSourceURL(t *testing.T) {
	server, token := newContractTestServer(t)
	cases := map[string]map[string]any{
		"bad scheme with userinfo": {"id": "p1", "name": "P", "type": "subscription", "enabled": true, "fetch_mode": "direct",
			"url": "ftp://user:" + plantedBodySecret + "@" + plantedSubHost + "/sub?token=" + plantedSubToken},
		"deep link without url": {"id": "p2", "name": "P", "type": "subscription", "enabled": true, "fetch_mode": "direct",
			"url": "sing-box://import-remote-profile?name=" + plantedBodySecret},
		"bad user agent": {"id": "p3", "name": "P", "type": "subscription", "enabled": true, "fetch_mode": "direct",
			"url": "https://" + plantedSubHost + "/sub?token=" + plantedSubToken, "user_agent": "bad\r\n" + plantedBodySecret},
		"unknown type": {"id": "p4", "name": "P", "type": "not-a-type", "enabled": true,
			"url": "https://" + plantedSubHost + "/sub?token=" + plantedSubToken},
		"via_bootstrap without bootstrap": {"id": "p5", "name": "P", "type": "subscription", "enabled": true, "fetch_mode": "via_bootstrap",
			"url": "https://" + plantedSubHost + "/sub?token=" + plantedSubToken},
	}
	for name, providerPayload := range cases {
		t.Run(name, func(t *testing.T) {
			body := mustJSON(t, map[string]any{"op_id": "contract-" + strings.ReplaceAll(name, " ", "-"), "changes": []map[string]any{{"action": "upsert", "provider": providerPayload}}})
			resp := contractRequest(t, server, http.MethodPost, "/api/providers/dryrun", token, body)
			if resp.Code < 400 || resp.Code >= 500 {
				t.Fatalf("status = %d, want 4xx: %s", resp.Code, truncateBody(resp))
			}
			assertEnvelope(t, name, resp)
			assertNoSensitiveEcho(t, name, resp)
		})
	}
}

func TestAPIContractProviderViewExposesUserAgentButNotURL(t *testing.T) {
	server, token := newContractTestServer(t)
	resp := contractRequest(t, server, http.MethodGet, "/api/providers", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, truncateBody(resp))
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	providers, _ := data["providers"].([]any)
	var planted map[string]any
	for _, item := range providers {
		if view, ok := item.(map[string]any); ok && view["id"] == "planted" {
			planted = view
		}
	}
	if planted == nil {
		t.Fatalf("planted provider missing from view: %s", truncateBody(resp))
	}
	if planted["user_agent"] != plantedUserAgent {
		t.Fatalf("user_agent = %v, want %q (plain read/write field)", planted["user_agent"], plantedUserAgent)
	}
	if planted["url_configured"] != true {
		t.Fatalf("url_configured = %v, want true", planted["url_configured"])
	}
	if _, has := planted["url"]; has {
		t.Fatalf("provider view must not expose url: %v", planted)
	}
	if masked, _ := planted["url_masked"].(string); masked != "https://"+plantedSubHost {
		t.Fatalf("url_masked = %q, want scheme+host only (https://%s)", masked, plantedSubHost)
	}
}

func TestAPIContractManualNodeSecretsNeverLeaveTheServer(t *testing.T) {
	server, token := newContractTestServer(t)
	paths := []string{"/api/nodes", "/api/proxy-groups", "/api/overview", "/api/config/state", "/api/providers/manual/nodes", "/api/business-pools", "/api/routes/explain"}
	for _, path := range paths {
		method := http.MethodGet
		var body []byte
		if path == "/api/routes/explain" {
			method = http.MethodPost
			body = mustJSON(t, map[string]any{"target": "node.example"})
		}
		resp := contractRequest(t, server, method, path, token, body)
		assertNoSensitiveEcho(t, method+" "+path, resp)
	}
	dryRun := contractRequest(t, server, http.MethodPost, "/api/config/dryrun", token, mustJSON(t, map[string]any{"op_id": "contract-dryrun-secrets"}))
	assertNoSensitiveEcho(t, "config dryrun", dryRun)
	if dryRun.Code < 500 {
		assertEnvelope(t, "config dryrun", dryRun)
	}
	logs := contractRequest(t, server, http.MethodGet, "/api/system/logs", token, nil)
	assertNoSensitiveEcho(t, "system logs", logs)
}

func TestAPIContractRequestBodyLimitIsEnveloped(t *testing.T) {
	server, token := newContractTestServer(t)
	huge := bytes.Repeat([]byte("a"), 8<<20)
	resp := contractRequest(t, server, http.MethodPost, "/api/providers/dryrun", token, huge)
	if resp.Code != http.StatusRequestEntityTooLarge && resp.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d: %s", resp.Code, truncateBody(resp))
	}
	assertEnvelope(t, "oversized body", resp)
}
