package singbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestBuildMinimalConfigIncludesTunWithoutFakeIPAndOwnerMetadata(t *testing.T) {
	cfg := BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))

	tun := findInbound(t, cfg.Inbounds, "tun-sb")
	if tun["type"] != "tun" || tun["interface_name"] != "tun-sb" {
		t.Fatalf("expected tun-sb inbound, got %+v", cfg.Inbounds)
	}
	addresses, ok := tun["address"].([]string)
	if !ok || !containsString(addresses, "172.19.0.1/30") || !containsString(addresses, "fdfe:dcba:9876::1/126") {
		t.Fatalf("tun-sb must carry IPv4 and IPv6 addresses, got %+v", tun["address"])
	}
	if _, ok := tun["sniff"]; ok {
		t.Fatalf("tun-sb inbound should not use legacy sniff field, got %+v", tun)
	}
	if _, ok := tun["sniff_override_destination"]; ok {
		t.Fatalf("tun-sb inbound should not use legacy sniff_override_destination field, got %+v", tun)
	}
	rules := cfg.Route["rules"].([]map[string]any)
	if len(rules) < 2 || rules[1]["action"] != "sniff" {
		t.Fatalf("expected tun sniff route action after DNS hijack, got %+v", rules)
	}
	inbounds, ok := rules[1]["inbound"].([]string)
	if !ok || !containsString(inbounds, "tun-sb") {
		t.Fatalf("tun sniff route should target tun-sb inbound, got %+v", rules[1])
	}
	clashAPI, ok := cfg.Experimental["clash_api"].(map[string]any)
	if !ok || clashAPI["external_controller"] != "127.0.0.1:9090" {
		t.Fatalf("expected loopback clash api, got %+v", cfg.Experimental)
	}
	if containsDNSServerTag(cfg.DNS["servers"].([]map[string]any), dnsFakeIPTag) {
		t.Fatalf("V3 must not generate a fake-ip DNS server: %+v", cfg.DNS["servers"])
	}
	resources, ok := cfg.Metadata["owner_resources"].([]netgate.OwnedResource)
	if !ok || len(resources) == 0 {
		t.Fatalf("expected owner resources metadata, got %+v", cfg.Metadata["owner_resources"])
	}
}

func TestBuildMinimalConfigIncludesDNSHijackInboundAndRoute(t *testing.T) {
	cfg := BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))

	dnsUDP := findInbound(t, cfg.Inbounds, "dns-in-udp")
	if dnsUDP["type"] != "direct" || dnsUDP["listen"] != "::" || dnsUDP["listen_port"] != 1053 || dnsUDP["network"] != "udp" {
		t.Fatalf("expected dns-in-udp direct inbound on :1053, got %+v", dnsUDP)
	}
	dnsTCP := findInbound(t, cfg.Inbounds, "dns-in-tcp")
	if dnsTCP["type"] != "direct" || dnsTCP["listen"] != "::" || dnsTCP["listen_port"] != 1053 || dnsTCP["network"] != "tcp" {
		t.Fatalf("expected dns-in-tcp direct inbound on :1053, got %+v", dnsTCP)
	}
	rules := cfg.Route["rules"].([]map[string]any)
	if len(rules) == 0 || rules[0]["action"] != "hijack-dns" {
		t.Fatalf("first route rule should hijack DNS, got %+v", rules)
	}
	inbounds, ok := rules[0]["inbound"].([]string)
	if !ok || !containsString(inbounds, "dns-in-udp") || !containsString(inbounds, "dns-in-tcp") {
		t.Fatalf("DNS hijack route should target both DNS inbounds, got %+v", rules[0])
	}
	if cfg.Route["default_domain_resolver"] != "dns-direct" {
		t.Fatalf("expected default domain resolver for sing-box 1.12+ route dials, got %+v", cfg.Route)
	}
}

func TestBuildMinimalConfigWithDNSAppliesSpec(t *testing.T) {
	cfg := BuildMinimalConfigWithDNS(netgate.NewOwnerRegistry("smartgate"), state.DNSSpec{
		Strategy:        "ipv4_only",
		FakeIPEnabled:   false,
		FakeIPRange:     "198.18.0.0/15",
		Final:           "fallback",
		DirectServers:   []string{"1.1.1.1"},
		ProxyServers:    []string{"https://dns.google/dns-query"},
		FallbackServers: []string{"tls://8.8.8.8"},
	})

	if cfg.DNS["strategy"] != "ipv4_only" || cfg.DNS["final"] != "dns-fallback" {
		t.Fatalf("dns strategy/final not applied: %+v", cfg.DNS)
	}
	rulesNoFakeIP, ok := cfg.DNS["rules"].([]map[string]any)
	if !ok || len(rulesNoFakeIP) != 1 || !isRejectUnsupportedDNSQueryRule(rulesNoFakeIP[0]) {
		t.Fatalf("expected only HTTPS/SVCB reject DNS rule when fake-ip disabled: %+v", cfg.DNS["rules"])
	}
	proxy := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-proxy")
	if proxy["type"] != "https" || proxy["server"] != "dns.google" || proxy["path"] != "/dns-query" || proxy["detour"] != "Proxy" {
		t.Fatalf("proxy dns server not generated correctly: %+v", proxy)
	}
	fallback := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-fallback")
	if fallback["type"] != "tls" || fallback["server"] != "8.8.8.8" || fallback["server_port"] != 853 {
		t.Fatalf("fallback dns server not generated correctly: %+v", fallback)
	}
}

func TestBuildMinimalConfigWithProxyFinalSanitizesUnsafeLocalDirectResolvers(t *testing.T) {
	cfg := BuildMinimalConfigWithDNS(netgate.NewOwnerRegistry("smartgate"), state.DNSSpec{
		Strategy:        "prefer_ipv4",
		FakeIPEnabled:   true,
		FakeIPRange:     "198.18.0.0/15",
		Final:           "proxy",
		DirectServers:   []string{"192.168.0.68", "dhcp://auto"},
		ProxyServers:    []string{"https://cloudflare-dns.com/dns-query"},
		FallbackServers: []string{},
	})

	direct := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-direct")
	if direct["type"] != "udp" || direct["server"] != "1.1.1.1" || direct["server_port"] != 53 {
		t.Fatalf("unsafe local direct resolvers should be sanitized in proxy final mode, got %+v", direct)
	}
	direct2 := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-direct-2")
	if direct2["type"] != "udp" || direct2["server"] != "1.0.0.1" || direct2["server_port"] != 53 {
		t.Fatalf("expected secondary sanitized direct resolver, got %+v", direct2)
	}
	proxy := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-proxy")
	if proxy["server"] != "cloudflare-dns.com" {
		t.Fatalf("proxy dns server should remain intact, got %+v", proxy)
	}
	cn := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-cn")
	if cn["type"] != "udp" || cn["server"] != "192.168.0.68" || cn["server_port"] != 53 {
		t.Fatalf("route dial dns should keep local upstream resolver under dns-cn, got %+v", cn)
	}
	if cfg.Route["default_domain_resolver"] != "dns-cn" {
		t.Fatalf("route dial resolver should use dns-cn, got %+v", cfg.Route)
	}
}

func TestBuildConfigAddsDirectRuleDNSSplitWithoutFakeIP(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesAndDNS(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}}},
		state.RulesFromLegacyGroups(map[string]state.RuleSpec{
			"direct": {
				DomainKeyword: []string{"baidu"},
				DomainSuffix:  []string{"qq.com"},
			},
		}, []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}}}),
		state.DNSSpec{
			Strategy:      "prefer_ipv4",
			FakeIPEnabled: true,
			FakeIPRange:   "198.18.0.0/15",
			Final:         "proxy",
			DirectServers: []string{"192.168.0.68"},
			ProxyServers:  []string{"https://cloudflare-dns.com/dns-query"},
		},
		selection.DefaultMaxPoolCandidates,
	)

	servers := cfg.DNS["servers"].([]map[string]any)
	cn := findDNSServer(t, servers, "dns-cn")
	if cn["type"] != "udp" || cn["server"] != "192.168.0.68" || cn["server_port"] != 53 {
		t.Fatalf("direct-rule dns split should use configured local direct resolver, got %+v", cn)
	}
	direct := findDNSServer(t, servers, "dns-direct")
	if direct["server"] != "1.1.1.1" {
		t.Fatalf("general dns-direct should still be sanitized for proxy final, got %+v", direct)
	}
	if cfg.Route["default_domain_resolver"] != "dns-cn" {
		t.Fatalf("route dial resolver should use local upstream dns split, got %+v", cfg.Route)
	}
	rules := cfg.DNS["rules"].([]map[string]any)
	if len(rules) != 2 || !isRejectUnsupportedDNSQueryRule(rules[0]) {
		t.Fatalf("HTTPS/SVCB reject must stay first in DNS rules, got %+v", rules)
	}
	if rules[1]["server"] != "dns-cn" {
		t.Fatalf("direct-rule dns split should follow the reject rule, got %+v", rules)
	}
	if !containsString(rules[1]["domain_keyword"].([]string), "baidu") || !containsString(rules[1]["domain_suffix"].([]string), "qq.com") {
		t.Fatalf("direct-rule dns split missing domains: %+v", rules[1])
	}
	if containsDNSServerTag(servers, dnsFakeIPTag) {
		t.Fatalf("legacy fake-ip request must not generate a fake-ip server: %+v", servers)
	}
}

func TestBuildMinimalConfigWithDefaultDNSUsesNewSingBoxServerFormat(t *testing.T) {
	cfg := BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))
	servers := cfg.DNS["servers"].([]map[string]any)

	direct := findDNSServer(t, servers, "dns-direct")
	if direct["type"] != "udp" || direct["server"] != "223.5.5.5" || direct["server_port"] != 53 {
		t.Fatalf("default direct DNS should use domestic UDP resolver format, got %+v", direct)
	}
	direct2 := findDNSServer(t, servers, "dns-direct-2")
	if direct2["type"] != "udp" || direct2["server"] != "119.29.29.29" || direct2["server_port"] != 53 {
		t.Fatalf("default secondary direct DNS should use domestic UDP resolver format, got %+v", direct2)
	}
	rules, ok := cfg.DNS["rules"].([]map[string]any)
	if !ok || len(rules) != 1 || !isRejectUnsupportedDNSQueryRule(rules[0]) {
		t.Fatalf("expected only the HTTPS/SVCB reject DNS rule, got %+v", cfg.DNS["rules"])
	}
}

func TestBuildMinimalConfigIgnoresLegacyIPv6FakeIPRequest(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesAndDNS(netgate.NewOwnerRegistry("smartgate"), nil, nil, nil, state.DNSSpec{
		Strategy:      "prefer_ipv6",
		FakeIPEnabled: true,
		FakeIPRange:   "fc00::/18",
		Final:         "direct",
		DirectServers: []string{"2606:4700:4700::1111"},
	}, selection.DefaultMaxPoolCandidates)
	servers := cfg.DNS["servers"].([]map[string]any)
	if containsDNSServerTag(servers, dnsFakeIPTag) {
		t.Fatalf("legacy IPv6 fake-ip request must be ignored: %+v", servers)
	}
	rules := cfg.DNS["rules"].([]map[string]any)
	if len(rules) == 0 || !isRejectUnsupportedDNSQueryRule(rules[0]) || dnsRulesContainDomainServer(rules, nil, dnsFakeIPTag) {
		t.Fatalf("expected no fake-ip DNS rule, got %+v", rules)
	}
}

func TestBuildConfigRejectsHTTPSAndSVCBDNSQueriesFirst(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}}},
		state.RulesFromLegacyGroups(map[string]state.RuleSpec{
			"direct": {
				DomainKeyword: []string{"baidu"},
				DomainSuffix:  []string{"qq.com"},
			},
		}, []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}}}),
		state.DNSSpec{
			Strategy:      "prefer_ipv4",
			FakeIPEnabled: true,
			FakeIPRange:   "198.18.0.0/15",
			Final:         "proxy",
			DirectServers: []string{"192.168.0.68"},
			ProxyServers:  []string{"https://cloudflare-dns.com/dns-query"},
		},
		state.DefaultRemoteRuleSettings(),
		state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates,
	)

	rules := cfg.DNS["rules"].([]map[string]any)
	if len(rules) == 0 || !isRejectUnsupportedDNSQueryRule(rules[0]) {
		t.Fatalf("HTTPS/SVCB reject must be first DNS rule after CN/direct splits, got %+v", rules)
	}
	queryTypes := rules[0]["query_type"].([]string)
	if !containsString(queryTypes, "HTTPS") || !containsString(queryTypes, "SVCB") {
		t.Fatalf("reject rule should cover HTTPS and SVCB, got %+v", queryTypes)
	}
	// ensureReject should keep a single reject even though buildDNSConfig also inserts one.
	rejectCount := 0
	for _, rule := range rules {
		if isRejectUnsupportedDNSQueryRule(rule) {
			rejectCount++
		}
	}
	if rejectCount != 1 {
		t.Fatalf("expected exactly one HTTPS/SVCB reject rule, got %d in %+v", rejectCount, rules)
	}
}

func TestGeneratedConfigPassesSingBoxCheckWhenAvailable(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box binary not available")
	}
	cfg := BuildConfigWithPoolsRulesAndDNS(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com", "uuid": "00000000-0000-0000-0000-000000000000"})},
		[]state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}}},
		state.DefaultRules(),
		state.DefaultDNSSpec(),
		selection.DefaultMaxPoolCandidates,
	)
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	out, err := exec.Command(binary, "check", "-c", configPath).CombinedOutput()
	if err != nil {
		t.Fatalf("sing-box check failed: %v\n%s", err, out)
	}
}

func TestBuildConfigGlobalModeRoutesPublicTrafficThroughProxy(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesDNSAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		nil,
		nil,
		state.DefaultDNSSpec(),
		state.GatewayModeGlobal,
		selection.DefaultMaxPoolCandidates,
	)

	if cfg.Route["final"] != "Proxy" {
		t.Fatalf("global mode should send default traffic to Proxy, got %+v", cfg.Route)
	}
	if cfg.Route["default_domain_resolver"] != "dns-direct" || cfg.DNS["final"] != "dns-proxy" {
		t.Fatalf("global mode should proxy client DNS without looping route dial DNS, route=%+v dns=%+v", cfg.Route, cfg.DNS)
	}
	proxyDNS := findDNSServer(t, cfg.DNS["servers"].([]map[string]any), "dns-proxy")
	if proxyDNS["detour"] != "Proxy" {
		t.Fatalf("global mode proxy DNS should detour through Proxy, got %+v", proxyDNS)
	}
	assertNoGeoDirectRules(t, cfg)
	assertHasPrivateDirectRule(t, cfg)
	assertHasProtectedDirectDomains(t, cfg)
	proxy := findOutbound(t, cfg.Outbounds, "Proxy")
	proxyOutbounds, ok := proxy["outbounds"].([]string)
	if !ok || !containsString(proxyOutbounds, "Pool_general") || containsString(proxyOutbounds, "Direct") {
		t.Fatalf("global mode Proxy selector should fail closed without Direct fallback, got %+v", proxy)
	}
	general := findOutbound(t, cfg.Outbounds, "Pool_general")
	generalOutbounds, ok := general["outbounds"].([]string)
	if !ok || !containsString(generalOutbounds, "node-1") || !containsString(generalOutbounds, "Block") || containsString(generalOutbounds, "Direct") {
		t.Fatalf("global mode Pool_general should fail closed through Block, got %+v", general)
	}
	if cfg.Metadata["gateway_mode"] != state.GatewayModeGlobal {
		t.Fatalf("global mode metadata missing: %+v", cfg.Metadata)
	}
}

func TestBuildConfigGlobalModeFailsClosedWithoutNodes(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesDNSAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		nil,
		nil,
		nil,
		state.DefaultDNSSpec(),
		state.GatewayModeGlobal,
		selection.DefaultMaxPoolCandidates,
	)

	if cfg.Route["final"] != "Proxy" || cfg.DNS["final"] != "dns-proxy" {
		t.Fatalf("global mode without nodes should still fail closed through Proxy selector, route=%+v dns=%+v", cfg.Route, cfg.DNS)
	}
	proxy := findOutbound(t, cfg.Outbounds, "Proxy")
	outbounds, ok := proxy["outbounds"].([]string)
	if !ok || !containsString(outbounds, "Pool_general") || containsString(outbounds, "Direct") {
		t.Fatalf("global mode without nodes should route through fail-closed business pools, got %+v", proxy)
	}
	general := findOutbound(t, cfg.Outbounds, "Pool_general")
	generalOutbounds, ok := general["outbounds"].([]string)
	if !ok || len(generalOutbounds) != 1 || generalOutbounds[0] != "Block" || general["default"] != "Block" {
		t.Fatalf("global mode without nodes should use Block-only general pool, got %+v", general)
	}
	assertNoGeoDirectRules(t, cfg)
}

func TestBuildConfigGlobalModeBusinessPoolsFailClosed(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesDNSAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{
			{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}},
			{ID: "streaming", Name: "Streaming", Role: "streaming", NodeIDs: nil},
		},
		nil,
		state.DefaultDNSSpec(),
		state.GatewayModeGlobal,
		selection.DefaultMaxPoolCandidates,
	)

	general := findOutbound(t, cfg.Outbounds, "Pool_general")
	generalOutbounds, ok := general["outbounds"].([]string)
	if !ok || !containsString(generalOutbounds, "node-1") || !containsString(generalOutbounds, "Block") || containsString(generalOutbounds, "Direct") {
		t.Fatalf("global general pool should use Block fallback, got %+v", general)
	}
	streaming := findOutbound(t, cfg.Outbounds, "Pool_streaming")
	streamingOutbounds, ok := streaming["outbounds"].([]string)
	if !ok || len(streamingOutbounds) != 1 || streamingOutbounds[0] != "Block" {
		t.Fatalf("global empty pool should fail closed to Block, got %+v", streaming)
	}
}

func TestBuildConfigWithNodesAddsProxySelectorAndSkipsExcluded(t *testing.T) {
	nodes := []provider.NodeView{
		configTestNode(t, provider.NodeView{
			ID:         "p1:node-good",
			ProviderID: "p1",
			Tag:        "node-good",
		}, map[string]any{"type": "vmess", "tag": "node-good", "server": "example.com"}),
		{
			ID:             "p1:unsupported:bad:1",
			ProviderID:     "p1",
			Type:           "naive",
			Tag:            "bad",
			Excluded:       true,
			ExcludedReason: "unsupported_type",
		},
	}
	cfg := BuildConfigWithNodes(netgate.NewOwnerRegistry("smartgate"), nodes)

	if cfg.Route["final"] != "Proxy" {
		t.Fatalf("expected final Proxy route, got %+v", cfg.Route)
	}
	if len(cfg.Outbounds) != 9 {
		t.Fatalf("expected one node, five pool selectors, proxy, direct, block; got %+v", cfg.Outbounds)
	}
	if cfg.Outbounds[0]["tag"] != "p1:node-good" || cfg.Outbounds[0]["type"] != "vmess" {
		t.Fatalf("provider node outbound not rewritten with stable node id: %+v", cfg.Outbounds[0])
	}
	general := findOutbound(t, cfg.Outbounds, "Pool_general")
	if general["type"] != "selector" || general["default"] != "p1:node-good" {
		t.Fatalf("unexpected general pool selector: %+v", general)
	}
	generalOutbounds, ok := general["outbounds"].([]string)
	if !ok || !containsString(generalOutbounds, "p1:node-good") || !containsString(generalOutbounds, "Direct") {
		t.Fatalf("default general pool should include node and Direct fallback, got %+v", general)
	}
	selector := findOutbound(t, cfg.Outbounds, "Proxy")
	if selector["type"] != "selector" || selector["default"] != "Pool_general" {
		t.Fatalf("unexpected selector outbound: %+v", selector)
	}
	proxyOutbounds, ok := selector["outbounds"].([]string)
	if !ok || !containsString(proxyOutbounds, "Pool_general") || !containsString(proxyOutbounds, "Pool_ai") {
		t.Fatalf("default proxy selector should include business pools, got %+v", selector)
	}
	if cfg.Metadata["usable_node_count"] != 1 {
		t.Fatalf("unexpected metadata: %+v", cfg.Metadata)
	}
}

func TestDefaultBusinessPoolsUseProviderRolesForCandidates(t *testing.T) {
	cfg := BuildConfigWithNodes(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{
			configTestNode(t, provider.NodeView{
				ID:    "node-ai",
				Roles: []string{"ai", "crypto"},
				Tag:   "node-ai",
			}, map[string]any{"type": "vmess", "tag": "node-ai", "server": "ai.example.com"}),
			configTestNode(t, provider.NodeView{
				ID:  "node-general",
				Tag: "node-general",
			}, map[string]any{"type": "vmess", "tag": "node-general", "server": "general.example.com"}),
		},
	)

	ai := findOutbound(t, cfg.Outbounds, "Pool_ai")
	aiOutbounds, ok := ai["outbounds"].([]string)
	if !ok || !containsString(aiOutbounds, "node-ai") || containsString(aiOutbounds, "node-general") || !containsString(aiOutbounds, "Block") {
		t.Fatalf("AI pool should use provider role candidates and fail closed, got %+v", ai)
	}
	crypto := findOutbound(t, cfg.Outbounds, "Pool_crypto")
	cryptoOutbounds, ok := crypto["outbounds"].([]string)
	if !ok || !containsString(cryptoOutbounds, "node-ai") || !containsString(cryptoOutbounds, "Block") || containsString(cryptoOutbounds, "Direct") {
		t.Fatalf("crypto pool should use role candidates and fail closed, got %+v", crypto)
	}
}

func TestBuildConfigAddsBootstrapHTTPProxyForViaBootstrapProviders(t *testing.T) {
	nodes := []provider.NodeView{
		configTestNode(t, provider.NodeView{
			ID:         "provider1:node-a",
			ProviderID: "provider1",
			Tag:        "node-a",
		}, map[string]any{"type": "vmess", "tag": "node-a", "server": "p1.example.com"}),
		configTestNode(t, provider.NodeView{
			ID:         "provider2:node-b",
			ProviderID: "provider2",
			Tag:        "node-b",
		}, map[string]any{"type": "trojan", "tag": "node-b", "server": "p2.example.com"}),
	}
	providers := []state.ProviderSpec{
		{ID: "provider1", Name: "Provider 1", Type: provider.TypeSubscription, Enabled: true, FetchMode: provider.FetchViaBootstrap, Bootstrap: "provider2"},
		{ID: "provider2", Name: "Provider 2", Type: provider.TypeSubscription, Enabled: true, FetchMode: provider.FetchDirect},
	}
	cfg := BuildConfigWithProvidersPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		providers,
		nodes,
		nil,
		nil,
		state.DefaultDNSSpec(),
		state.RemoteRuleSettings{},
		state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates,
	)

	inbound := findInbound(t, cfg.Inbounds, "bootstrap-http")
	if inbound["type"] != "http" || inbound["listen"] != "127.0.0.1" || inbound["listen_port"] != 2080 {
		t.Fatalf("unexpected bootstrap inbound: %+v", inbound)
	}
	bootstrap := findOutbound(t, cfg.Outbounds, "Bootstrap")
	outbounds, ok := bootstrap["outbounds"].([]string)
	if !ok || len(outbounds) != 1 || outbounds[0] != "provider2:node-b" || bootstrap["default"] != "provider2:node-b" {
		t.Fatalf("bootstrap selector should only use provider2 nodes, got %+v", bootstrap)
	}
	foundRoute := false
	for _, rule := range cfg.Route["rules"].([]map[string]any) {
		if rule["outbound"] != "Bootstrap" {
			continue
		}
		inbounds, ok := rule["inbound"].([]string)
		if ok && containsString(inbounds, "bootstrap-http") {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Fatalf("missing bootstrap inbound route in %+v", cfg.Route["rules"])
	}
	if cfg.Metadata["bootstrap_proxy_url"] != provider.DefaultBootstrapProxyURL || cfg.Metadata["bootstrap_node_count"] != 1 {
		t.Fatalf("unexpected bootstrap metadata: %+v", cfg.Metadata)
	}
}

func TestBuildConfigWithPoolsCreatesSelectorsCapsCandidatesAndFailClosed(t *testing.T) {
	nodes := make([]provider.NodeView, 0, 12)
	nodeIDs := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		id := "p1:node-" + string(rune('a'+i))
		nodeIDs = append(nodeIDs, id)
		nodes = append(nodes, configTestNode(t, provider.NodeView{ID: id, Tag: id}, map[string]any{"type": "trojan", "tag": id, "server": "example.com"}))
	}
	pools := []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", NodeIDs: nodeIDs},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"missing-node"}},
	}
	cfg := BuildConfigWithPools(netgate.NewOwnerRegistry("smartgate"), nodes, pools, 10)

	general := findOutbound(t, cfg.Outbounds, "Pool_general")
	generalOutbounds, ok := general["outbounds"].([]string)
	if !ok || len(generalOutbounds) != 11 || !containsString(generalOutbounds, "Direct") || general["default"] != nodeIDs[0] {
		t.Fatalf("general pool should cap at 10 candidates, got %+v", general)
	}
	ai := findOutbound(t, cfg.Outbounds, "Pool_ai")
	aiOutbounds, ok := ai["outbounds"].([]string)
	if !ok || len(aiOutbounds) != 1 || aiOutbounds[0] != "Block" || ai["default"] != "Block" {
		t.Fatalf("empty AI pool should fail closed to Block, got %+v", ai)
	}
}

func TestGeneratedConfigDefaultsToDurableSelector(t *testing.T) {
	nodes := []provider.NodeView{
		configTestNode(t, provider.NodeView{ID: "node-a", Tag: "node-a"}, map[string]any{"type": "trojan", "tag": "node-a", "server": "a.example.com"}),
		configTestNode(t, provider.NodeView{ID: "node-b", Tag: "node-b"}, map[string]any{"type": "trojan", "tag": "node-b", "server": "b.example.com"}),
	}
	pools := []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-a", "node-b"}}}
	cfg := BuildConfigWithProvidersPoolsRulesDNSRemoteRulesGatewayModeAndSelections(
		netgate.NewOwnerRegistry("smartgate"), nil, nodes, pools, nil,
		state.DefaultDNSSpec(), state.RemoteRuleSettings{}, state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates, map[string]string{"general": "node-b"},
	)

	selector := findOutbound(t, cfg.Outbounds, "Pool_general")
	if selector["default"] != "node-b" {
		t.Fatalf("generated selector default does not match durable selection: %+v", selector)
	}

	invalid := BuildConfigWithProvidersPoolsRulesDNSRemoteRulesGatewayModeAndSelections(
		netgate.NewOwnerRegistry("smartgate"), nil, nodes, pools, nil,
		state.DefaultDNSSpec(), state.RemoteRuleSettings{}, state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates, map[string]string{"general": "removed-node"},
	)
	if got := findOutbound(t, invalid.Outbounds, "Pool_general")["default"]; got != "node-a" {
		t.Fatalf("unavailable durable selection must fail closed to generated fallback, got %v", got)
	}
}

func TestBuildConfigDoesNotLeakSmartGateMetadataIntoCoreOutbounds(t *testing.T) {
	cfg := BuildConfigWithPools(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{
			{ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-1"}},
			{ID: "ai", Name: "AI", Role: "ai", NodeIDs: nil},
		},
		selection.DefaultMaxPoolCandidates,
	)

	for _, outbound := range cfg.Outbounds {
		if _, ok := outbound["_smartgate"]; ok {
			t.Fatalf("sing-box outbound must not contain SmartGate-only metadata: %+v", outbound)
		}
	}
	if len(cfg.Metadata) == 0 {
		t.Fatalf("top-level metadata should remain available")
	}
}

func TestBuildConfigSensitivePoolKeepsBlockFallback(t *testing.T) {
	cfg := BuildConfigWithPools(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"node-1"}}},
		selection.DefaultMaxPoolCandidates,
	)

	ai := findOutbound(t, cfg.Outbounds, "Pool_ai")
	outbounds, ok := ai["outbounds"].([]string)
	if !ok || !containsString(outbounds, "node-1") || !containsString(outbounds, "Block") {
		t.Fatalf("sensitive pool should include node and Block fallback, got %+v", ai)
	}
	if ai["default"] != "node-1" {
		t.Fatalf("sensitive pool should keep first node as default, got %+v", ai)
	}
}

func TestBuildConfigWithRulesAddsRouteRulesBeforeDefaults(t *testing.T) {
	cfg := BuildConfigWithPoolsAndRules(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"node-1"}}},
		state.RulesFromLegacyGroups(map[string]state.RuleSpec{
			"ai":    {DomainSuffix: []string{"example-ai.com"}},
			"block": {IPCIDR: []string{"203.0.113.0/24"}},
		}, []state.BusinessPoolSpec{{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"node-1"}}}),
		selection.DefaultMaxPoolCandidates,
	)
	rules := cfg.Route["rules"].([]map[string]any)
	if len(rules) < 4 {
		t.Fatalf("expected custom rules before defaults: %+v", rules)
	}
	if rules[0]["action"] != "hijack-dns" {
		t.Fatalf("DNS hijack rule must stay first, got %+v", rules[0])
	}
	if rules[1]["action"] != "sniff" {
		t.Fatalf("tun sniff rule must stay before routing rules, got %+v", rules[1])
	}
	if rules[2]["outbound"] != "Block" || rules[3]["outbound"] != "Pool_ai" {
		t.Fatalf("unexpected custom rule outbounds: %+v", rules[:4])
	}
}

func TestBuildConfigDoesNotAddLocalAdBlockByDefault(t *testing.T) {
	cfg := BuildConfigWithNodes(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
	)

	rules := cfg.Route["rules"].([]map[string]any)
	for _, rule := range rules {
		if rule["outbound"] == "Block" {
			t.Fatalf("local adblock must not be generated by default; use remote geo adblock instead: %+v", rules)
		}
	}
}

func TestBuildConfigDoesNotAddDefaultRemoteGeoFallbackRuleSets(t *testing.T) {
	cfg := BuildConfigWithNodes(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
	)

	if _, ok := cfg.Route["rule_set"]; ok {
		t.Fatalf("remote geo fallback rule sets must not be generated by default: %+v", cfg.Route["rule_set"])
	}
	rules := cfg.Route["rules"].([]map[string]any)
	if hasRouteRuleSetOutbound(rules, "geosite-cn", "Direct") || hasRouteRuleSetOutbound(rules, "geoip-cn", "Direct") {
		t.Fatalf("remote geo fallback route rules must not be generated by default: %+v", rules)
	}
	if hasRouteRuleSetOutbound(rules, "geosite-category-ai-not-cn", "Pool_ai") || hasRouteRuleSetOutbound(rules, "geosite-category-media", "Pool_streaming") || hasRouteRuleSetOutbound(rules, "geosite-category-cryptocurrency", "Pool_crypto") {
		t.Fatalf("business geosite rule-sets must not be enabled by default: %+v", rules)
	}
	dnsRules, ok := cfg.DNS["rules"].([]map[string]any)
	if !ok {
		t.Fatalf("expected dns rules, got %+v", cfg.DNS["rules"])
	}
	for _, rule := range dnsRules {
		if hasRuleSet(rule, "geosite-cn") || hasRuleSet(rule, "geoip-cn") {
			t.Fatalf("dns rules must not depend on remote geo fallback by default: %+v", dnsRules)
		}
	}
}

func TestDefaultRemoteRuleSettingsFilterDeprecatedBusinessGeoRuleSets(t *testing.T) {
	remote := state.DefaultRemoteRuleSettings()
	remote.Sets = append(remote.Sets, state.RemoteRuleSet{
		ID:       "geosite-category-ai-not-cn",
		Name:     "Old AI",
		Enabled:  true,
		Kind:     "geo",
		Target:   "proxy",
		Outbound: "Pool_ai",
		Format:   "binary",
		Type:     "remote",
		URL:      "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-category-ai-!cn.srs",
	})
	effective := state.EffectiveRemoteRuleSettings(remote)
	for _, set := range effective.Sets {
		if set.ID == "geosite-category-ai-not-cn" {
			t.Fatalf("deprecated business geosite rule-set should be filtered: %+v", effective.Sets)
		}
	}
}

func TestBuildConfigRemoteAdBlockCanBeEnabled(t *testing.T) {
	remote := state.DefaultRemoteRuleSettings()
	remote.AdBlock.Enabled = true
	for i := range remote.Sets {
		if remote.Sets[i].Kind == "adblock" {
			remote.Sets[i].Enabled = true
		}
	}
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		nil,
		nil,
		state.DefaultDNSSpec(),
		remote,
		state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates,
	)

	ruleSets := cfg.Route["rule_set"].([]map[string]any)
	if findRouteRuleSet(t, ruleSets, "geosite-category-ads")["url"] == "" {
		t.Fatalf("adblock remote rule-set missing URL: %+v", ruleSets)
	}
	rules := cfg.Route["rules"].([]map[string]any)
	if !hasRouteRuleSetOutbound(rules, "geosite-category-ads", "Block") {
		t.Fatalf("adblock route rule missing: %+v", rules)
	}
}

func TestEnabledRemoteRuleSetsAdblockThreeState(t *testing.T) {
	base := state.DefaultRemoteRuleSettings()
	// Ensure we have a known adblock set with URL for enablement checks.
	var adsIdx = -1
	for i, item := range base.Sets {
		if item.ID == "geosite-category-ads" {
			adsIdx = i
			break
		}
	}
	if adsIdx < 0 {
		t.Fatal("default remote rules missing geosite-category-ads")
	}

	hasAds := func(settings state.RemoteRuleSettings) bool {
		for _, item := range enabledRemoteRuleSets(settings) {
			if item.ID == "geosite-category-ads" {
				return true
			}
		}
		return false
	}

	// master off → never
	offMaster := base
	offMaster.Enabled = false
	offMaster.AdBlock.Enabled = true
	offMaster.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	offMaster.Sets[adsIdx].Enabled = true
	// enabledRemoteRuleSets does not gate on settings.Enabled itself (caller plan does),
	// so check the three adblock-specific gates below; settings.Enabled is asserted via plan.
	_ = offMaster

	// ad_block off + item on → not enabled
	adOff := base
	adOff.AdBlock.Enabled = false
	adOff.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	adOff.Sets[adsIdx].Enabled = true
	if hasAds(adOff) {
		t.Fatal("adblock set must stay off when ad_block.enabled=false")
	}

	// ad_block on + item off → not enabled
	itemOff := base
	itemOff.AdBlock.Enabled = true
	itemOff.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	itemOff.Sets[adsIdx].Enabled = false
	if hasAds(itemOff) {
		t.Fatal("adblock set must stay off when item.enabled=false")
	}

	// all on → enabled (even if ad_block.sets is empty — sets no longer gate)
	allOn := base
	allOn.AdBlock.Enabled = true
	allOn.AdBlock.Sets = nil
	allOn.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	allOn.Sets[adsIdx].Enabled = true
	if !hasAds(allOn) {
		t.Fatal("adblock set must enable when ad_block.enabled and item.enabled are true")
	}

	// ad_block.sets listing a different id must NOT disable a separately enabled adblock set
	setsIgnore := base
	setsIgnore.AdBlock.Enabled = true
	setsIgnore.AdBlock.Sets = []string{"not-this-set"}
	setsIgnore.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	setsIgnore.Sets[adsIdx].Enabled = true
	if !hasAds(setsIgnore) {
		t.Fatal("ad_block.sets must not gate per-set enablement anymore")
	}

	// settings.Enabled=false plan path: BuildConfig should omit ads even if item+ad_block on
	remoteOff := base
	remoteOff.Enabled = false
	remoteOff.AdBlock.Enabled = true
	remoteOff.Sets = append([]state.RemoteRuleSet(nil), base.Sets...)
	remoteOff.Sets[adsIdx].Enabled = true
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		nil, nil, state.DefaultDNSSpec(), remoteOff, state.GatewayModeOn, selection.DefaultMaxPoolCandidates,
	)
	if ruleSets, _ := cfg.Route["rule_set"].([]map[string]any); ruleSets != nil {
		for _, rs := range ruleSets {
			if rs["tag"] == "geosite-category-ads" {
				t.Fatalf("settings.Enabled=false must omit adblock rule_set, got %+v", ruleSets)
			}
		}
	}
}

func TestBuildConfigLocalGeoFallbackRuleSetsUsePath(t *testing.T) {
	remote := state.DefaultRemoteRuleSettings()
	for i := range remote.Sets {
		switch remote.Sets[i].ID {
		case "geosite-cn":
			remote.Sets[i].Enabled = true
			remote.Sets[i].Type = "local"
			remote.Sets[i].URL = ""
			remote.Sets[i].Path = "/etc/sing-box/rule-set/geosite-cn.srs"
		case "geoip-cn":
			remote.Sets[i].Enabled = true
			remote.Sets[i].Type = "local"
			remote.Sets[i].URL = ""
			remote.Sets[i].Path = "/etc/sing-box/rule-set/geoip-cn.srs"
		}
	}
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		nil,
		nil,
		state.DefaultDNSSpec(),
		remote,
		state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates,
	)

	ruleSets := cfg.Route["rule_set"].([]map[string]any)
	geosite := findRouteRuleSet(t, ruleSets, "geosite-cn")
	if geosite["type"] != "local" || geosite["path"] != "/etc/sing-box/rule-set/geosite-cn.srs" {
		t.Fatalf("geosite-cn should use local path: %+v", geosite)
	}
	if _, ok := geosite["url"]; ok {
		t.Fatalf("local rule-set should not include url: %+v", geosite)
	}
	geoip := findRouteRuleSet(t, ruleSets, "geoip-cn")
	if geoip["type"] != "local" || geoip["path"] != "/etc/sing-box/rule-set/geoip-cn.srs" {
		t.Fatalf("geoip-cn should use local path: %+v", geoip)
	}
	rules := cfg.Route["rules"].([]map[string]any)
	if !hasRouteRuleSetOutbound(rules, "geosite-cn", "Direct") || !hasRouteRuleSetOutbound(rules, "geoip-cn", "Direct") {
		t.Fatalf("local geo fallback route rules missing: %+v", rules)
	}
}

func TestBuildConfigGlobalModeDropsGeoDirectRemoteRules(t *testing.T) {
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		nil,
		nil,
		state.DefaultDNSSpec(),
		state.DefaultRemoteRuleSettings(),
		state.GatewayModeGlobal,
		selection.DefaultMaxPoolCandidates,
	)

	if _, ok := cfg.Route["rule_set"]; ok {
		t.Fatalf("global mode should drop geo direct fallback rule sets when no adblock is enabled: %+v", cfg.Route["rule_set"])
	}
	for _, rule := range cfg.Route["rules"].([]map[string]any) {
		if hasRuleSet(rule, "geosite-cn") || hasRuleSet(rule, "geoip-cn") {
			t.Fatalf("global mode should not route geo direct rule sets: %+v", cfg.Route["rules"])
		}
	}
}

func findOutbound(t *testing.T, outbounds []map[string]any, tag string) map[string]any {
	t.Helper()
	for _, outbound := range outbounds {
		if outbound["tag"] == tag {
			return outbound
		}
	}
	t.Fatalf("missing outbound tag %s in %+v", tag, outbounds)
	return nil
}

func configTestNode(t *testing.T, view provider.NodeView, outbound map[string]any) provider.NodeView {
	t.Helper()
	copy := cloneOutbound(outbound)
	if _, ok := copy["server_port"]; !ok {
		copy["server_port"] = 443
	}
	providerID := view.ProviderID
	if providerID == "" {
		providerID = "test"
	}
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
		ProviderID: providerID, RawTag: view.Tag, Outbound: copy,
	})
	if err != nil {
		t.Fatal(err)
	}
	view.Type = source.Identity.Protocol
	view.Tag = source.RawTag
	view.Server = source.Identity.Server
	view.Port = source.Identity.Port
	view.Identity = source.Identity
	view.References = source.References
	return view
}

func findInbound(t *testing.T, inbounds []map[string]any, tag string) map[string]any {
	t.Helper()
	for _, inbound := range inbounds {
		if inbound["tag"] == tag {
			return inbound
		}
	}
	t.Fatalf("missing inbound tag %s in %+v", tag, inbounds)
	return nil
}

func findDNSServer(t *testing.T, servers []map[string]any, tag string) map[string]any {
	t.Helper()
	for _, server := range servers {
		if server["tag"] == tag {
			return server
		}
	}
	t.Fatalf("missing dns server tag %s in %+v", tag, servers)
	return nil
}

func containsDNSServerTag(servers []map[string]any, tag string) bool {
	for _, server := range servers {
		if server["tag"] == tag {
			return true
		}
	}
	return false
}

func findRouteRuleSet(t *testing.T, ruleSets []map[string]any, tag string) map[string]any {
	t.Helper()
	for _, ruleSet := range ruleSets {
		if ruleSet["tag"] == tag {
			return ruleSet
		}
	}
	t.Fatalf("missing route rule_set tag %s in %+v", tag, ruleSets)
	return nil
}

func findDNSRuleByServer(t *testing.T, rules []map[string]any, server string) map[string]any {
	t.Helper()
	for _, rule := range rules {
		if rule["server"] == server {
			return rule
		}
	}
	t.Fatalf("missing dns rule for server %s in %+v", server, rules)
	return nil
}

func hasRouteRuleSetOutbound(rules []map[string]any, tag, outbound string) bool {
	for _, rule := range rules {
		if rule["outbound"] == outbound && hasRuleSet(rule, tag) {
			return true
		}
	}
	return false
}

func hasRuleSet(rule map[string]any, tag string) bool {
	ruleSets, ok := rule["rule_set"].([]string)
	if !ok {
		return false
	}
	for _, item := range ruleSets {
		if item == tag {
			return true
		}
	}
	return false
}

func assertNoGeoDirectRules(t *testing.T, cfg MinimalConfig) {
	t.Helper()
	for _, rule := range cfg.Route["rules"].([]map[string]any) {
		if rule["outbound"] != "Direct" {
			continue
		}
		ruleSets, ok := rule["rule_set"].([]string)
		if ok && len(ruleSets) == 1 && (ruleSets[0] == "geosite-cn" || ruleSets[0] == "geoip-cn") {
			t.Fatalf("global mode should not keep geo direct rule: %+v", rule)
		}
	}
}

func assertHasPrivateDirectRule(t *testing.T, cfg MinimalConfig) {
	t.Helper()
	for _, rule := range cfg.Route["rules"].([]map[string]any) {
		if rule["ip_is_private"] == true && rule["outbound"] == "Direct" {
			return
		}
	}
	t.Fatalf("global mode should keep LAN/private protection direct rule: %+v", cfg.Route["rules"])
}

func assertHasProtectedDirectDomains(t *testing.T, cfg MinimalConfig) {
	t.Helper()
	for _, rule := range cfg.Route["rules"].([]map[string]any) {
		if rule["outbound"] != "Direct" {
			continue
		}
		suffixes, ok := rule["domain_suffix"].([]string)
		if ok && containsString(suffixes, "lan") && containsString(suffixes, "local") {
			return
		}
	}
	t.Fatalf("global mode should keep LAN/local protected direct domains: %+v", cfg.Route["rules"])
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDNSServerParsesSupportedAddressForms(t *testing.T) {
	cases := []struct {
		address string
		want    string
	}{
		{"dhcp://auto", "type=dhcp interface=auto"},
		{"1.1.1.1", "type=udp server=1.1.1.1 server_port=53"},
		{"1.1.1.1:5353", "type=udp server=1.1.1.1 server_port=5353"},
		{"udp://9.9.9.9:9953", "type=udp server=9.9.9.9 server_port=9953"},
		{"tls://8.8.8.8", "type=tls server=8.8.8.8 server_port=853"},
		{"https://dns.google/dns-query", "type=https server=dns.google path=/dns-query"},
	}
	for _, tc := range cases {
		server := dnsServer("test", tc.address)
		for _, token := range strings.Split(tc.want, " ") {
			parts := strings.SplitN(token, "=", 2)
			if server[parts[0]] != valueForToken(parts[1]) {
				t.Fatalf("dnsServer(%q) %s = %v, want %s; server=%+v", tc.address, parts[0], server[parts[0]], parts[1], server)
			}
		}
	}
}

func valueForToken(value string) any {
	switch value {
	case "53":
		return 53
	case "853":
		return 853
	case "5353":
		return 5353
	case "9953":
		return 9953
	default:
		return value
	}
}

func TestBuildConfigGeoCNProxySafety(t *testing.T) {
	remote := state.DefaultRemoteRuleSettings()
	remote.Enabled = true
	for i := range remote.Sets {
		switch remote.Sets[i].ID {
		case "geosite-cn":
			remote.Sets[i].Enabled = true
			remote.Sets[i].Type = "local"
			remote.Sets[i].URL = ""
			remote.Sets[i].Path = "/etc/sing-box/rule-set/geosite-cn.srs"
		case "geoip-cn":
			remote.Sets[i].Enabled = true
			remote.Sets[i].Type = "local"
			remote.Sets[i].URL = ""
			remote.Sets[i].Path = "/etc/sing-box/rule-set/geoip-cn.srs"
		}
	}
	cfg := BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(
		netgate.NewOwnerRegistry("smartgate"),
		[]provider.NodeView{configTestNode(t, provider.NodeView{ID: "node-1"}, map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"})},
		[]state.BusinessPoolSpec{{ID: "general", Role: "general", Name: "General", NodeIDs: []string{"node-1"}}},
		nil,
		state.DefaultDNSSpec(),
		remote,
		state.GatewayModeOn,
		selection.DefaultMaxPoolCandidates,
	)
	rules, _ := cfg.Route["rules"].([]map[string]any)
	foundSafety := false
	for i, rule := range rules {
		if rule["outbound"] == "Proxy" && sameStringSliceFromAny(rule["domain_suffix"], geoCNProxySafetyDomainSuffixes()) {
			foundSafety = true
			// Must sit before geosite-cn Direct.
			laterHasGeo := false
			for _, next := range rules[i+1:] {
				if isGeoDirectRule(next) && hasStringRuleSet(next, "geosite-cn") {
					laterHasGeo = true
					break
				}
			}
			if !laterHasGeo {
				t.Fatalf("geo safety rule must appear before geosite-cn Direct: %+v", rules)
			}
			break
		}
	}
	if !foundSafety {
		t.Fatalf("missing geo CN proxy safety route rule: %+v", rules)
	}
	dnsRules, _ := cfg.DNS["rules"].([]map[string]any)
	if dnsRulesContainDomainServer(dnsRules, map[string]any{"domain_suffix": geoCNProxySafetyDomainSuffixes()}, dnsFakeIPTag) {
		t.Fatalf("worker config must not own fake-ip DNS routing: %+v", dnsRules)
	}
}
