package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestNormalizeProviderOutboundsAssignsStableUniqueIDs(t *testing.T) {
	p := state.ProviderSpec{ID: "p1", Name: "Provider 1"}
	outbound := map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"}
	nodes := NormalizeProviderOutbounds(p, []map[string]any{
		outbound,
		outbound,
		{"type": "naive", "tag": "unsupported", "server": "example.org"},
		{"type": "vmess", "tag": "missing-server"},
		{"type": "vmess", "server": "missing-tag.example"},
	})
	if len(nodes) != 5 {
		t.Fatalf("expected 5 nodes, got %+v", nodes)
	}
	if nodes[0].Excluded || nodes[0].ID == "" || nodes[0].ProviderID != "p1" {
		t.Fatalf("expected first node to be usable, got %+v", nodes[0])
	}
	if nodes[1].ID == nodes[0].ID || !nodes[1].Excluded || nodes[1].ExcludedReason != "duplicate" {
		t.Fatalf("duplicate should have independent excluded id, got %+v first=%+v", nodes[1], nodes[0])
	}
	if !nodes[2].Excluded || nodes[2].ExcludedReason != "unsupported_by_registered_cores" {
		t.Fatalf("unsupported node should be excluded, got %+v", nodes[2])
	}
	if !nodes[3].Excluded || nodes[3].ExcludedReason != "missing_server" {
		t.Fatalf("missing server should be excluded, got %+v", nodes[3])
	}
	if !nodes[4].Excluded || nodes[4].ExcludedReason != "missing_tag" {
		t.Fatalf("missing tag should be excluded, got %+v", nodes[4])
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		if seen[node.ID] {
			t.Fatalf("duplicate node id %q in %+v", node.ID, nodes)
		}
		seen[node.ID] = true
	}

	again := NormalizeProviderOutbounds(p, []map[string]any{outbound})
	if again[0].ID != nodes[0].ID {
		t.Fatalf("real node ID should be stable, got %q then %q", nodes[0].ID, again[0].ID)
	}
}

func TestBuildNodeViewsReadsEnabledProvidersLastGoodOnly(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	lastGood := NewLastGoodStore(layout)
	if _, err := lastGood.Save("p1", []map[string]any{{"type": "trojan", "tag": "node-1", "server": "example.com"}}); err != nil {
		t.Fatalf("save p1 last-good: %v", err)
	}
	if _, err := lastGood.Save("disabled", []map[string]any{{"type": "trojan", "tag": "node-2", "server": "example.org"}}); err != nil {
		t.Fatalf("save disabled last-good: %v", err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "p1", Name: "Provider 1", Type: TypeSubscription, Enabled: true, URL: "https://p1.example/sub", FetchMode: FetchDirect},
		{ID: "p2", Name: "Provider 2", Type: TypeSubscription, Enabled: true, URL: "https://p2.example/sub", FetchMode: FetchDirect},
		{ID: "disabled", Name: "Disabled", Type: TypeSubscription, Enabled: false, URL: "https://disabled.example/sub", FetchMode: FetchDirect},
	}

	nodes, err := BuildNodeViews(spec, lastGood)
	if err != nil {
		t.Fatalf("build node views: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ProviderID != "p1" || nodes[0].ProviderName != "Provider 1" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
}

func TestBuildNodeViewsUsesLastGoodIdentityWithoutExposingReferences(t *testing.T) {
	outbound := map[string]any{
		"type": "VLESS", "tag": "node-1", "server": "EXAMPLE.COM.", "server_port": 443, "uuid": "credential-plaintext",
		"tls": map[string]any{"enabled": true, "server_name": "example.com"},
	}
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
		ProviderID: "p1", RawTag: "node-1", Outbound: outbound,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeManual, Enabled: true}}
	nodes, err := BuildNodeViewsFromSnapshots(spec, []state.LastGoodSnapshot{{
		ProviderID: "p1",
		Identities: []smartnode.IdentitySource{source},
		Outbounds:  []map[string]any{outbound},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !reflect.DeepEqual(nodes[0].Identity, source.Identity) {
		t.Fatalf("node view identity differs from last-good identity: nodes=%+v source=%+v", nodes, source.Identity)
	}
	if nodes[0].Type != "vless" || nodes[0].Server != "EXAMPLE.COM." || nodes[0].Port != 443 {
		t.Fatalf("legacy node view fields changed: %+v", nodes[0])
	}
	if nodes[0].Identity.Server != "example.com" {
		t.Fatalf("identity server was not normalized: %+v", nodes[0].Identity)
	}
	body, err := json.Marshal(nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "credential-plaintext") || strings.Contains(string(body), "references") {
		t.Fatalf("node view JSON exposed protected references: %s", body)
	}
}

func TestBuildNodeViewsRejectsExplicitEmptyIdentitySet(t *testing.T) {
	outbound := map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeManual, Enabled: true}}
	_, err := BuildNodeViewsFromSnapshots(spec, []state.LastGoodSnapshot{
		{ProviderID: "p1", Identities: []smartnode.IdentitySource{}, Outbounds: []map[string]any{outbound}},
	})
	if err == nil || !strings.Contains(err.Error(), "identities count 0 does not match outbounds count 1") {
		t.Fatalf("explicit empty identity set was promoted as legacy input: %v", err)
	}
}

func TestSmartNodeCatalogRejectsExplicitEmptyIdentitySet(t *testing.T) {
	outbound := map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeManual, Enabled: true}}
	_, _, err := BuildSmartNodeCatalogFromSnapshots(spec, []state.LastGoodSnapshot{
		{ProviderID: "p1", Identities: []smartnode.IdentitySource{}, Outbounds: []map[string]any{outbound}},
	}, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "identities count 0 does not match outbounds count 1") {
		t.Fatalf("explicit empty identity set was promoted during catalog rebuild: %v", err)
	}
}

func TestSmartNodeCatalogRejectsDuplicateSnapshots(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeManual, Enabled: true}}
	_, _, err := BuildSmartNodeCatalogFromSnapshots(spec, []state.LastGoodSnapshot{
		{ProviderID: "p1"},
		{ProviderID: "p1"},
	}, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "duplicate last-good snapshot") {
		t.Fatalf("duplicate snapshot was silently overwritten: %v", err)
	}
}

func TestSmartNodeCatalogRejectsTamperedLastGoodIdentity(t *testing.T) {
	outbound := map[string]any{
		"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443, "uuid": "credential",
	}
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: "node-1", Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	source.Identity.Server = "tampered.example"
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeManual, Enabled: true}}
	if _, _, err := BuildSmartNodeCatalogFromSnapshots(spec, []state.LastGoodSnapshot{{
		ProviderID: "p1", Identities: []smartnode.IdentitySource{source}, Outbounds: []map[string]any{outbound},
	}}, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "does not match outbound semantics") {
		t.Fatalf("tampered identity was silently recomputed from outbound: %v", err)
	}
}

func TestNormalizeProviderOutboundsAppliesIncludeExcludeFilters(t *testing.T) {
	p := state.ProviderSpec{
		ID:              "p1",
		Name:            "Provider 1",
		IncludeKeywords: []string{"hk"},
		ExcludeKeywords: []string{"low"},
	}
	nodes := NormalizeProviderOutbounds(p, []map[string]any{
		{"type": "trojan", "tag": "hk-fast", "server": "hk.example"},
		{"type": "trojan", "tag": "us-fast", "server": "us.example"},
		{"type": "trojan", "tag": "hk-low", "server": "hk-low.example"},
	})
	if len(nodes) != 3 {
		t.Fatalf("expected three nodes, got %+v", nodes)
	}
	if nodes[0].Excluded {
		t.Fatalf("hk-fast should pass filters: %+v", nodes[0])
	}
	if !nodes[1].Excluded || nodes[1].ExcludedReason != "not_included" {
		t.Fatalf("us-fast should be not_included: %+v", nodes[1])
	}
	if !nodes[2].Excluded || nodes[2].ExcludedReason != "excluded_by_filter" {
		t.Fatalf("hk-low should be excluded_by_filter: %+v", nodes[2])
	}
}

func TestNormalizeProviderOutboundsExcludesPseudoNodes(t *testing.T) {
	p := state.ProviderSpec{ID: "p1", Name: "Provider 1"}
	nodes := NormalizeProviderOutbounds(p, []map[string]any{
		{"type": "anytls", "tag": "剩余流量：68 GB", "server": "notice.invalid", "server_port": 1},
		{"type": "anytls", "tag": "anytls 香港 1", "server": "node.example", "server_port": 443},
	})
	if len(nodes) != 2 {
		t.Fatalf("expected two nodes, got %+v", nodes)
	}
	if !nodes[0].Excluded || nodes[0].ExcludedReason != "pseudo_node" {
		t.Fatalf("pseudo node should be excluded, got %+v", nodes[0])
	}
	if nodes[1].Excluded {
		t.Fatalf("real node should remain usable, got %+v", nodes[1])
	}
}
