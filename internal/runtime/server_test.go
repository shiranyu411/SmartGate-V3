package runtime

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"smartgate/internal/state"
)

func TestValidateLANOnly(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{addr: "192.168.1.1:3000", ok: true},
		{addr: "127.0.0.1:3000", ok: true},
		{addr: "0.0.0.0:3000", ok: false},
		{addr: "8.8.8.8:3000", ok: false},
	}
	for _, tc := range cases {
		err := ValidateLANOnly(tc.addr)
		if tc.ok && err != nil {
			t.Fatalf("%s should be allowed: %v", tc.addr, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s should be rejected", tc.addr)
		}
	}
}

func TestCurrentStatusFallsBackToAppliedTopologyWhenRuntimeEmpty(t *testing.T) {
	layout := testLayout(t)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch:  9,
		TopologyHash: "status-topo",
		GatewayMode:  state.GatewayModeOn,
	}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	// Empty RuntimeDir: no runtime.json written.
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Store:      store,
		Status:     NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	status := server.currentStatus()
	if status.ConfigEpoch != 9 || status.TopologyHash != "status-topo" || status.GatewayMode != state.GatewayModeOn {
		t.Fatalf("status did not fall back to applied topology: %+v", status)
	}
}

func TestCurrentStatusSurfacesStartupRecoveryErrorAndLegacyMigration(t *testing.T) {
	layout := testLayout(t)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if err := store.SaveRuntime(state.RuntimeState{RecoveryError: "execution graph blocked by node_not_found"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLegacyMigrationReport(state.LegacyBindingMigrationReport{
		MigratedAt: time.Now().UTC(),
		Resolved:   []state.LegacyBindingResolution{{LegacyID: "p:0123456789abcdef", SmartNode: "sn_0123456789abcdef"}},
		Unresolved: []state.LegacyBindingReference{{Scope: "spec_pool", Pool: "general", LegacyID: "p:fedcba9876543210"}},
		Warnings:   []string{"Pool general 的旧节点 p:fedcba9876543210 无法匹配到任何 SmartNode，已解除绑定"},
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Store:      store,
		Status:     NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	status := server.currentStatus()
	if status.RecoveryError == "" {
		t.Fatalf("startup recovery error is not visible: %+v", status)
	}
	if status.LegacyMigration == nil || status.LegacyMigration.Resolved != 1 || len(status.LegacyMigration.Unresolved) != 1 || len(status.LegacyMigration.Warnings) != 1 {
		t.Fatalf("legacy migration report is not visible: %+v", status.LegacyMigration)
	}
	resp := serve(t, server, http.MethodGet, "/api/runtime/status", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, needle := range []string{`"recovery_error"`, `"legacy_migration"`, `"unresolved"`, `"warnings"`, `p:fedcba9876543210`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("runtime status body lacks %s: %s", needle, body)
		}
	}
}

func TestRuntimeCoreDryRunEndpointRetired(t *testing.T) {
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
	})
	resp := serve(t, server, http.MethodPost, "/api/runtime/singbox/dryrun", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("retired sing-box dry-run endpoint status=%d body=%s", resp.Code, resp.Body.String())
	}
}
