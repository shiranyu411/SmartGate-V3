package singbox

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const (
	dnsInboundUDPTag = "dns-in-udp"
	dnsInboundTCPTag = "dns-in-tcp"
	dnsListenPort    = 1053

	bootstrapHTTPInboundTag = "bootstrap-http"
	bootstrapHTTPListenPort = 2080
	bootstrapOutboundTag    = "Bootstrap"

	dnsDirectTag   = "dns-direct"
	dnsProxyTag    = "dns-proxy"
	dnsFallbackTag = "dns-fallback"
	dnsFakeIPTag   = "dns-fakeip"
	dnsCNTag       = "dns-cn"
)

var defaultCNDNSServers = []string{"218.106.127.114"}

type MinimalConfig struct {
	Log          map[string]any   `json:"log,omitempty"`
	DNS          map[string]any   `json:"dns"`
	Inbounds     []map[string]any `json:"inbounds"`
	Outbounds    []map[string]any `json:"outbounds"`
	Route        map[string]any   `json:"route"`
	Experimental map[string]any   `json:"experimental,omitempty"`
	Metadata     map[string]any   `json:"-"`
}

func BuildMinimalConfig(owners *netgate.OwnerRegistry) MinimalConfig {
	return BuildMinimalConfigWithDNS(owners, state.DefaultDNSSpec())
}

func BuildMinimalConfigWithDNS(owners *netgate.OwnerRegistry, dns state.DNSSpec) MinimalConfig {
	plan := netgate.RuntimePlan("singbox-minimal", owners)
	effectiveDNS := state.EffectiveDNSSpec(dns)
	cfg := MinimalConfig{
		Log: map[string]any{"level": "info"},
		DNS: buildDNSConfig(effectiveDNS),
		Inbounds: []map[string]any{
			{
				"type":        "direct",
				"tag":         dnsInboundUDPTag,
				"listen":      "::",
				"listen_port": dnsListenPort,
				"network":     "udp",
			},
			{
				"type":        "direct",
				"tag":         dnsInboundTCPTag,
				"listen":      "::",
				"listen_port": dnsListenPort,
				"network":     "tcp",
			},
			{
				"type":           "tun",
				"tag":            "tun-sb",
				"interface_name": "tun-sb",
				"address":        []string{"172.19.0.1/30", "fdfe:dcba:9876::1/126"},
				"auto_route":     true,
				"strict_route":   true,
			},
		},
		Outbounds: []map[string]any{
			{"type": "direct", "tag": "Direct"},
			{"type": "block", "tag": "Block"},
		},
		Route: map[string]any{
			"auto_detect_interface":   true,
			"default_domain_resolver": dnsDirectTag,
			"final":                   "Direct",
			"rules": []map[string]any{
				dnsHijackRouteRule(),
				tunSniffRouteRule(),
				{"ip_is_private": true, "outbound": "Direct"},
			},
		},
		Experimental: map[string]any{
			"clash_api": map[string]any{
				"external_controller": "127.0.0.1:9090",
				"default_mode":        "Rule",
			},
		},
		Metadata: map[string]any{
			"owner_resources": plan.Resources,
		},
	}
	applyRouteDialDNSSplit(&cfg, effectiveDNS)
	ensureRejectUnsupportedDNSQueries(&cfg)
	return cfg
}

func BuildConfigWithNodes(owners *netgate.OwnerRegistry, nodes []provider.NodeView) MinimalConfig {
	return BuildConfigWithPools(owners, nodes, nil, selection.DefaultMaxPoolCandidates)
}

func BuildConfigWithPools(owners *netgate.OwnerRegistry, nodes []provider.NodeView, pools []state.BusinessPoolSpec, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithPoolsAndRules(owners, nodes, pools, nil, maxPoolCandidates)
}

func BuildConfigWithPoolsAndRules(owners *netgate.OwnerRegistry, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithPoolsRulesAndDNS(owners, nodes, pools, rules, state.DefaultDNSSpec(), maxPoolCandidates)
}

func BuildConfigWithPoolsRulesAndDNS(owners *netgate.OwnerRegistry, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, dns state.DNSSpec, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithPoolsRulesDNSAndGatewayMode(owners, nodes, pools, rules, dns, state.GatewayModeOn, maxPoolCandidates)
}

func BuildConfigWithPoolsRulesDNSAndGatewayMode(owners *netgate.OwnerRegistry, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, dns state.DNSSpec, gatewayMode string, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(owners, nodes, pools, rules, dns, state.RemoteRuleSettings{}, gatewayMode, maxPoolCandidates)
}

func BuildConfigWithPoolsRulesDNSRemoteRulesAndGatewayMode(owners *netgate.OwnerRegistry, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, dns state.DNSSpec, remoteRules state.RemoteRuleSettings, gatewayMode string, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithProvidersPoolsRulesDNSRemoteRulesAndGatewayMode(owners, nil, nodes, pools, rules, dns, remoteRules, gatewayMode, maxPoolCandidates)
}

func BuildConfigWithProvidersPoolsRulesDNSRemoteRulesAndGatewayMode(owners *netgate.OwnerRegistry, providers []state.ProviderSpec, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, dns state.DNSSpec, remoteRules state.RemoteRuleSettings, gatewayMode string, maxPoolCandidates int) MinimalConfig {
	return BuildConfigWithProvidersPoolsRulesDNSRemoteRulesGatewayModeAndSelections(owners, providers, nodes, pools, rules, dns, remoteRules, gatewayMode, maxPoolCandidates, nil)
}

// BuildConfigWithProvidersPoolsRulesDNSRemoteRulesGatewayModeAndSelections
// persists still-valid durable selections into selector defaults. This makes
// the generated config self-contained: a sing-box restart restores the same
// selection even when SmartGate did not initiate the restart.
func BuildConfigWithProvidersPoolsRulesDNSRemoteRulesGatewayModeAndSelections(owners *netgate.OwnerRegistry, providers []state.ProviderSpec, nodes []provider.NodeView, pools []state.BusinessPoolSpec, rules state.RuleCatalog, dns state.DNSSpec, remoteRules state.RemoteRuleSettings, gatewayMode string, maxPoolCandidates int, selected map[string]string) MinimalConfig {
	if maxPoolCandidates <= 0 {
		maxPoolCandidates = selection.DefaultMaxPoolCandidates
	}
	pools = state.EffectiveBusinessPools(pools)
	nodeOutbounds, allNodeTags := nodeOutbounds(nodes)
	globalMode := normalizedGatewayMode(gatewayMode) == state.GatewayModeGlobal
	proxyAvailable := len(allNodeTags) > 0 || globalMode
	cfg := BuildMinimalConfigWithDNS(owners, selection.DNSForGatewayMode(dns, gatewayMode, proxyAvailable))
	if globalMode {
		applyGlobalRouteDefaults(cfg)
	}
	effectiveRules := effectiveRulesForGatewayMode(rules, globalMode)
	applyDirectRuleDNSSplit(&cfg, dns, effectiveRules)
	if len(pools) == 0 {
		remotePlan := remoteRulePlanForGatewayMode(remoteRules, globalMode, nil, proxyAvailable)
		if len(remotePlan.ruleSets) > 0 {
			cfg.Route["rule_set"] = remotePlan.ruleSets
		}
		applyCNDNSSplit(&cfg, remotePlan)
		cfg = configWithDefaultProxy(cfg, nodeOutbounds, allNodeTags, globalMode)
		if len(remotePlan.routeRules) > 0 {
			prependRouteRulesAfterDNSHijack(cfg, remotePlan.routeRules)
		}
		if routeRules := ruleOutboundSpecs(effectiveRules, nil, proxyAvailable); len(routeRules) > 0 {
			prependRouteRulesAfterDNSHijack(cfg, routeRules)
		}
		cfg.Metadata["gateway_mode"] = normalizedGatewayMode(gatewayMode)
		cfg.Metadata["remote_rule_set_count"] = len(remotePlan.ruleSets)
		cfg.Metadata["remote_rule_set_warnings"] = remotePlan.warnings
		ensureRejectUnsupportedDNSQueries(&cfg)
		applyGeoCNProxySafety(&cfg, remotePlan)
		applyBootstrapProxySupport(&cfg, providers, nodes, maxPoolCandidates)
		return cfg
	}
	var selectorTags []string
	poolOutbounds := map[string]string{}
	cfg.Outbounds = append(cfg.Outbounds[:0], nodeOutbounds...)
	for _, selectorState := range selection.BuildSelectorStates(nodes, pools, gatewayMode, maxPoolCandidates, selected) {
		selector := map[string]any{
			"type":      "selector",
			"tag":       selectorState.Selector,
			"outbounds": selectorState.Candidates,
			"default":   selectorState.Selected,
		}
		cfg.Outbounds = append(cfg.Outbounds, selector)
		selectorTags = append(selectorTags, selectorState.Selector)
		poolOutbounds[selectorState.PoolID] = selectorState.Selector
		poolOutbounds[selectorState.Role] = selectorState.Selector
	}
	proxyAvailable = len(selectorTags) > 0
	remotePlan := remoteRulePlanForGatewayMode(remoteRules, globalMode, poolOutbounds, proxyAvailable)
	if len(remotePlan.ruleSets) > 0 {
		cfg.Route["rule_set"] = remotePlan.ruleSets
	}
	applyCNDNSSplit(&cfg, remotePlan)
	cfg.Outbounds = append(cfg.Outbounds,
		map[string]any{
			"type":      "selector",
			"tag":       "Proxy",
			"outbounds": selectorTags,
			"default":   selectorTags[0],
		},
		map[string]any{"type": "direct", "tag": "Direct"},
		map[string]any{"type": "block", "tag": "Block"},
	)
	cfg.Route["final"] = "Proxy"
	if len(remotePlan.routeRules) > 0 {
		prependRouteRulesAfterDNSHijack(cfg, remotePlan.routeRules)
	}
	if routeRules := ruleOutboundSpecs(effectiveRules, poolOutbounds, true); len(routeRules) > 0 {
		prependRouteRulesAfterDNSHijack(cfg, routeRules)
	}
	cfg.Metadata["usable_node_count"] = len(allNodeTags)
	cfg.Metadata["business_pool_count"] = len(pools)
	cfg.Metadata["max_pool_candidates"] = maxPoolCandidates
	cfg.Metadata["gateway_mode"] = normalizedGatewayMode(gatewayMode)
	cfg.Metadata["remote_rule_set_count"] = len(remotePlan.ruleSets)
	cfg.Metadata["remote_rule_set_warnings"] = remotePlan.warnings
	ensureRejectUnsupportedDNSQueries(&cfg)
	applyGeoCNProxySafety(&cfg, remotePlan)
	applyBootstrapProxySupport(&cfg, providers, nodes, maxPoolCandidates)
	return cfg
}

func dnsHijackRouteRule() map[string]any {
	return map[string]any{
		"inbound": []string{dnsInboundUDPTag, dnsInboundTCPTag},
		"action":  "hijack-dns",
	}
}

func tunSniffRouteRule() map[string]any {
	return map[string]any{
		"inbound": []string{"tun-sb"},
		"action":  "sniff",
	}
}

func prependRouteRulesAfterDNSHijack(cfg MinimalConfig, routeRules []map[string]any) {
	existing := cfg.Route["rules"].([]map[string]any)
	if len(existing) == 0 {
		cfg.Route["rules"] = routeRules
		return
	}
	insertAt := 1
	for insertAt < len(existing) && existing[insertAt]["action"] == "sniff" {
		insertAt++
	}
	merged := append([]map[string]any{}, existing[:insertAt]...)
	merged = append(merged, routeRules...)
	merged = append(merged, existing[insertAt:]...)
	cfg.Route["rules"] = merged
}

func applyGlobalRouteDefaults(cfg MinimalConfig) {
	existing := cfg.Route["rules"].([]map[string]any)
	filtered := make([]map[string]any, 0, len(existing))
	for _, rule := range existing {
		if isGeoDirectRule(rule) {
			continue
		}
		filtered = append(filtered, rule)
	}
	cfg.Route["rules"] = filtered
}

func isGeoDirectRule(rule map[string]any) bool {
	if rule["outbound"] != "Direct" {
		return false
	}
	ruleSets, ok := rule["rule_set"].([]string)
	if !ok {
		return false
	}
	return len(ruleSets) == 1 && (ruleSets[0] == "geosite-cn" || ruleSets[0] == "geoip-cn")
}

func effectiveRulesForGatewayMode(rules state.RuleCatalog, globalMode bool) state.RuleCatalog {
	effectiveRules := state.EffectiveRuleCatalog(rules)
	if !globalMode {
		return effectiveRules
	}
	filtered := make(state.RuleCatalog, 0, len(effectiveRules))
	for _, rule := range effectiveRules {
		if rule.PoolID == "direct" {
			continue
		}
		filtered = append(filtered, rule)
	}
	for _, rule := range state.DefaultRules() {
		if rule.PoolID == "direct" {
			filtered = append(filtered, rule)
		}
	}
	return filtered
}

type remoteRulePlan struct {
	ruleSets   []map[string]any
	routeRules []map[string]any
	warnings   []string
}

func remoteRulePlanForGatewayMode(settings state.RemoteRuleSettings, globalMode bool, poolOutbounds map[string]string, proxyAvailable bool) remoteRulePlan {
	settings = state.EffectiveRemoteRuleSettings(settings)
	if !settings.Enabled {
		return remoteRulePlan{}
	}
	enabled := enabledRemoteRuleSets(settings)
	plan := remoteRulePlan{}
	for _, item := range enabled {
		if globalMode && item.Target == "direct" && (item.ID == "geosite-cn" || item.ID == "geoip-cn") {
			continue
		}
		ruleSet := map[string]any{
			"type":   remoteRuleType(item),
			"tag":    item.ID,
			"format": remoteRuleFormat(item, settings),
		}
		if remoteRuleType(item) == "local" {
			ruleSet["path"] = item.Path
		} else {
			ruleSet["url"] = item.URL
			if detour := remoteRuleDownloadDetour(item, settings); detour != "" {
				ruleSet["download_detour"] = detour
			}
			if interval := remoteRuleUpdateInterval(item, settings); interval != "" {
				ruleSet["update_interval"] = interval
			}
		}
		plan.ruleSets = append(plan.ruleSets, ruleSet)
		plan.routeRules = append(plan.routeRules, map[string]any{
			"rule_set": []string{item.ID},
			"outbound": remoteRuleOutbound(item, poolOutbounds, proxyAvailable),
		})
	}
	plan.warnings = remoteRuleGuardWarnings(enabled, settings.MemoryGuard)
	return plan
}

func applyCNDNSSplit(cfg *MinimalConfig, plan remoteRulePlan) {
	if !remotePlanHasRuleSet(plan, "geosite-cn") || !remotePlanRoutesRuleSetTo(plan, "geosite-cn", "Direct") {
		return
	}
	servers, _ := cfg.DNS["servers"].([]map[string]any)
	if findDNSConfigServer(servers, dnsCNTag) == nil {
		servers = append(dnsServerGroup(dnsCNTag, defaultCNDNSServers, ""), servers...)
		cfg.DNS["servers"] = servers
	}
	rule := map[string]any{
		"rule_set": []string{"geosite-cn"},
		"server":   dnsCNTag,
	}
	rules, _ := cfg.DNS["rules"].([]map[string]any)
	if len(rules) == 0 {
		cfg.DNS["rules"] = []map[string]any{rule}
		return
	}
	if dnsRulesContainRuleSetServer(rules, "geosite-cn", dnsCNTag) {
		return
	}
	cfg.DNS["rules"] = append([]map[string]any{rule}, rules...)
}

// geoCNProxySafetyDomainSuffixes are domains that some geosite-cn snapshots
// still classify as CN (or resolve via CN DNS to polluted 114.x edges). Routing
// them Direct produces TLS "Your connection is not private" failures on PCs
// after connection rebinds. Keep them on Proxy + non-CN DNS instead.
func geoCNProxySafetyDomainSuffixes() []string { return state.GeoCNProxySafetyDomainSuffixes() }

func outboundTagExists(cfg *MinimalConfig, tag string) bool {
	if cfg == nil {
		return false
	}
	for _, item := range cfg.Outbounds {
		if strings.TrimSpace(fmt.Sprint(item["tag"])) == tag {
			return true
		}
	}
	return false
}

func dnsServerTagExists(cfg *MinimalConfig, tag string) bool {
	if cfg == nil || cfg.DNS == nil {
		return false
	}
	servers, _ := cfg.DNS["servers"].([]map[string]any)
	return findDNSConfigServer(servers, tag) != nil
}

// applyGeoCNProxySafety inserts route + DNS exceptions before geosite-cn Direct
// so GFW-sensitive Google assets never take the polluted CN direct path.
func applyGeoCNProxySafety(cfg *MinimalConfig, plan remoteRulePlan) {
	if cfg == nil || cfg.Route == nil {
		return
	}
	if !remotePlanHasRuleSet(plan, "geosite-cn") || !remotePlanRoutesRuleSetTo(plan, "geosite-cn", "Direct") {
		return
	}
	if !outboundTagExists(cfg, "Proxy") {
		return
	}
	suffixes := geoCNProxySafetyDomainSuffixes()
	if len(suffixes) == 0 {
		return
	}

	rules, _ := cfg.Route["rules"].([]map[string]any)
	insertAt := -1
	for i, rule := range rules {
		if isGeoDirectRule(rule) && hasStringRuleSet(rule, "geosite-cn") {
			insertAt = i
			break
		}
	}
	if insertAt >= 0 {
		safety := map[string]any{
			"domain_suffix": append([]string(nil), suffixes...),
			"outbound":      "Proxy",
		}
		// Avoid duplicate inserts when dry-run/build helpers re-enter.
		already := insertAt > 0 && sameStringSliceFromAny(rules[insertAt-1]["domain_suffix"], safety["domain_suffix"]) && rules[insertAt-1]["outbound"] == "Proxy"
		if !already {
			merged := append([]map[string]any{}, rules[:insertAt]...)
			merged = append(merged, safety)
			merged = append(merged, rules[insertAt:]...)
			cfg.Route["rules"] = merged
		}
	}

	if cfg.DNS == nil {
		return
	}
	server := dnsProxyTag
	if dnsServerTagExists(cfg, dnsFakeIPTag) {
		server = dnsFakeIPTag
	} else if !dnsServerTagExists(cfg, dnsProxyTag) {
		return
	}
	dnsRules, _ := cfg.DNS["rules"].([]map[string]any)
	if dnsRulesContainDomainServer(dnsRules, map[string]any{"domain_suffix": suffixes}, server) {
		return
	}
	safetyDNS := map[string]any{
		"domain_suffix": append([]string(nil), suffixes...),
		"server":        server,
	}
	// Keep HTTPS/SVCB reject first when present; otherwise put safety first so
	// it wins over geosite-cn -> dns-cn.
	if len(dnsRules) > 0 && isRejectUnsupportedDNSQueryRule(dnsRules[0]) {
		cfg.DNS["rules"] = append([]map[string]any{dnsRules[0], safetyDNS}, dnsRules[1:]...)
		return
	}
	cfg.DNS["rules"] = append([]map[string]any{safetyDNS}, dnsRules...)
}

func applyRouteDialDNSSplit(cfg *MinimalConfig, dns state.DNSSpec) {
	dns = state.EffectiveDNSSpec(dns)
	if dns.Final != "proxy" || !selection.HasUnsafeLocalDNSResolver(dns.DirectServers) {
		return
	}
	servers, _ := cfg.DNS["servers"].([]map[string]any)
	if findDNSConfigServer(servers, dnsCNTag) == nil {
		servers = append(dnsServerGroup(dnsCNTag, dns.DirectServers, ""), servers...)
		cfg.DNS["servers"] = servers
	}
	cfg.Route["default_domain_resolver"] = dnsCNTag
}

func applyDirectRuleDNSSplit(cfg *MinimalConfig, dns state.DNSSpec, rules state.RuleCatalog) {
	var suffixes, keywords []string
	for _, rule := range rules {
		if !rule.Enabled || rule.OverriddenBy != "" || rule.PoolID != "direct" {
			continue
		}
		switch rule.Kind {
		case state.RuleKindDomainSuffix:
			suffixes = append(suffixes, rule.Value)
		case state.RuleKindDomainKeyword:
			keywords = append(keywords, rule.Value)
		}
	}
	if len(suffixes) == 0 && len(keywords) == 0 {
		return
	}
	dns = state.EffectiveDNSSpec(dns)
	if len(dns.DirectServers) == 0 {
		return
	}
	servers, _ := cfg.DNS["servers"].([]map[string]any)
	if findDNSConfigServer(servers, dnsCNTag) == nil {
		servers = append(dnsServerGroup(dnsCNTag, dns.DirectServers, ""), servers...)
		cfg.DNS["servers"] = servers
	}
	rule := map[string]any{"server": dnsCNTag}
	if len(suffixes) > 0 {
		rule["domain_suffix"] = append([]string(nil), suffixes...)
	}
	if len(keywords) > 0 {
		rule["domain_keyword"] = append([]string(nil), keywords...)
	}
	rulesList, _ := cfg.DNS["rules"].([]map[string]any)
	if dnsRulesContainDomainServer(rulesList, rule, dnsCNTag) {
		return
	}
	cfg.DNS["rules"] = append([]map[string]any{rule}, rulesList...)
}

func remotePlanHasRuleSet(plan remoteRulePlan, id string) bool {
	for _, item := range plan.ruleSets {
		if item["tag"] == id {
			return true
		}
	}
	return false
}

func remotePlanRoutesRuleSetTo(plan remoteRulePlan, id, outbound string) bool {
	for _, item := range plan.routeRules {
		if item["outbound"] != outbound {
			continue
		}
		if hasStringRuleSet(item, id) {
			return true
		}
	}
	return false
}

func findDNSConfigServer(servers []map[string]any, tag string) map[string]any {
	for _, item := range servers {
		if item["tag"] == tag {
			return item
		}
	}
	return nil
}

func dnsRulesContainRuleSetServer(rules []map[string]any, ruleSet, server string) bool {
	for _, item := range rules {
		if item["server"] == server && hasStringRuleSet(item, ruleSet) {
			return true
		}
	}
	return false
}

func dnsRulesContainDomainServer(rules []map[string]any, want map[string]any, server string) bool {
	for _, item := range rules {
		if item["server"] != server {
			continue
		}
		if sameStringSliceFromAny(item["domain_suffix"], want["domain_suffix"]) && sameStringSliceFromAny(item["domain_keyword"], want["domain_keyword"]) {
			return true
		}
	}
	return false
}

func sameStringSliceFromAny(a, b any) bool {
	left, leftOK := a.([]string)
	right, rightOK := b.([]string)
	if !leftOK || !rightOK {
		return a == nil && b == nil
	}
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func hasStringRuleSet(rule map[string]any, want string) bool {
	ruleSets, ok := rule["rule_set"].([]string)
	if !ok {
		return false
	}
	for _, item := range ruleSets {
		if item == want {
			return true
		}
	}
	return false
}

func enabledRemoteRuleSets(settings state.RemoteRuleSettings) []state.RemoteRuleSet {
	seen := map[string]struct{}{}
	var out []state.RemoteRuleSet
	for _, item := range settings.Sets {
		if item.ID == "" {
			continue
		}
		if remoteRuleType(item) == "local" && item.Path == "" {
			continue
		}
		if remoteRuleType(item) != "local" && item.URL == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		// Adblock sets: settings.Enabled is applied by callers via EffectiveRemoteRuleSettings
		// / remoteRulePlan; effective enablement is item.Enabled && ad_block.enabled.
		// ad_block.sets is retained for API compat only and no longer gates enablement.
		enabled := item.Enabled
		if item.Kind == "adblock" && !settings.AdBlock.Enabled {
			enabled = false
		}
		if !enabled {
			continue
		}
		out = append(out, item)
	}
	return out
}

func remoteRuleGuardWarnings(sets []state.RemoteRuleSet, guard state.RuleMemoryGuard) []string {
	var warnings []string
	if guard.MaxEnabledSets > 0 && len(sets) > guard.MaxEnabledSets {
		warnings = append(warnings, "启用的远程规则集数量超过上限")
	}
	if guard.WarnEnabledSets > 0 && len(sets) > guard.WarnEnabledSets {
		warnings = append(warnings, "启用的远程规则集数量超过建议值")
	}
	total := 0
	for _, item := range sets {
		total += item.EstimatedBytes
		if guard.MaxSetBytes > 0 && item.EstimatedBytes > guard.MaxSetBytes {
			warnings = append(warnings, "远程规则集 "+item.ID+" 超过单个规则集大小上限")
		}
	}
	if guard.MaxTotalBytes > 0 && total > guard.MaxTotalBytes {
		warnings = append(warnings, "远程规则集总大小超过上限")
	}
	if guard.WarnTotalBytes > 0 && total > guard.WarnTotalBytes {
		warnings = append(warnings, "远程规则集总大小超过建议值")
	}
	return warnings
}

func remoteRuleType(item state.RemoteRuleSet) string {
	if item.Type != "" {
		return item.Type
	}
	return "remote"
}

func remoteRuleFormat(item state.RemoteRuleSet, settings state.RemoteRuleSettings) string {
	if item.Format != "" {
		return item.Format
	}
	if settings.DefaultFormat != "" {
		return settings.DefaultFormat
	}
	return "binary"
}

func remoteRuleDownloadDetour(item state.RemoteRuleSet, settings state.RemoteRuleSettings) string {
	if item.DownloadDetour != "" {
		return item.DownloadDetour
	}
	return settings.DownloadDetour
}

func remoteRuleUpdateInterval(item state.RemoteRuleSet, settings state.RemoteRuleSettings) string {
	if item.UpdateInterval != "" {
		return item.UpdateInterval
	}
	return settings.UpdateInterval
}

func remoteRuleOutbound(item state.RemoteRuleSet, poolOutbounds map[string]string, proxyAvailable bool) string {
	if item.Outbound != "" {
		return resolveRemoteRuleOutbound(item.Outbound, poolOutbounds, proxyAvailable)
	}
	switch item.Target {
	case "block":
		return "Block"
	case "direct":
		return "Direct"
	case "proxy":
		if proxyAvailable {
			return "Proxy"
		}
		return "Direct"
	default:
		if proxyAvailable {
			return "Proxy"
		}
		return "Direct"
	}
}

func resolveRemoteRuleOutbound(outbound string, poolOutbounds map[string]string, proxyAvailable bool) string {
	switch outbound {
	case "Direct", "Block":
		return outbound
	case "Proxy":
		if proxyAvailable {
			return "Proxy"
		}
		return "Direct"
	}
	if tag := poolOutbounds[outbound]; tag != "" {
		return tag
	}
	if poolID, ok := selection.PoolIDFromTag(outbound); ok {
		if tag := poolOutbounds[poolID]; tag != "" {
			return tag
		}
	}
	if proxyAvailable {
		return "Proxy"
	}
	return "Direct"
}

func normalizedGatewayMode(mode string) string {
	return state.EffectiveGatewayMode(mode)
}

func buildDNSConfig(dns state.DNSSpec) map[string]any {
	dns = state.EffectiveDNSSpec(dns)
	dns = selection.SanitizeDNSForProxyFinal(dns)
	rules := []map[string]any{rejectUnsupportedDNSQueryRule()}
	if dns.FakeIPEnabled {
		rules = append(rules, map[string]any{
			"query_type": fakeIPQueryTypes(dns.FakeIPRange),
			"server":     dnsFakeIPTag,
		})
	}
	return map[string]any{
		"final":    dnsFinalTag(dns.Final),
		"strategy": dns.Strategy,
		"servers":  dnsServers(dns),
		"rules":    rules,
	}
}

// rejectUnsupportedDNSQueryRule drops HTTPS/SVCB (TYPE65/64) lookups quickly.
// Modern mobile clients often prefer these records; many upstream resolvers
// (especially parent CPE DNS) hang instead of answering, which surfaces as
// app-level timeouts for domestic services like Baidu Netdisk.
func rejectUnsupportedDNSQueryRule() map[string]any {
	return map[string]any{
		"query_type": []string{"HTTPS", "SVCB"},
		"action":     "reject",
	}
}

func isRejectUnsupportedDNSQueryRule(rule map[string]any) bool {
	if rule == nil || rule["action"] != "reject" {
		return false
	}
	types, ok := stringSliceFromAny(rule["query_type"])
	if !ok || len(types) == 0 {
		return false
	}
	hasHTTPS, hasSVCB := false, false
	for _, item := range types {
		switch strings.ToUpper(strings.TrimSpace(item)) {
		case "HTTPS":
			hasHTTPS = true
		case "SVCB":
			hasSVCB = true
		}
	}
	return hasHTTPS && hasSVCB
}

func stringSliceFromAny(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		out := make([]string, len(typed))
		copy(out, typed)
		return out, true
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

// ensureRejectUnsupportedDNSQueries keeps the reject rule first after later
// DNS split helpers prepend geosite/direct rules.
func ensureRejectUnsupportedDNSQueries(cfg *MinimalConfig) {
	if cfg == nil || cfg.DNS == nil {
		return
	}
	rules, _ := cfg.DNS["rules"].([]map[string]any)
	filtered := make([]map[string]any, 0, len(rules)+1)
	for _, rule := range rules {
		if isRejectUnsupportedDNSQueryRule(rule) {
			continue
		}
		filtered = append(filtered, rule)
	}
	cfg.DNS["rules"] = append([]map[string]any{rejectUnsupportedDNSQueryRule()}, filtered...)
}

func dnsServers(dns state.DNSSpec) []map[string]any {
	servers := dnsServerGroup(dnsDirectTag, dns.DirectServers, "")
	servers = append(servers, dnsServerGroup(dnsProxyTag, dns.ProxyServers, "Proxy")...)
	servers = append(servers, dnsServerGroup(dnsFallbackTag, dns.FallbackServers, "")...)
	if dns.FakeIPEnabled {
		fakeIP := map[string]any{
			"tag":                           dnsFakeIPTag,
			"type":                          "fakeip",
			fakeIPRangeKey(dns.FakeIPRange): dns.FakeIPRange,
		}
		servers = append(servers, fakeIP)
	}
	return servers
}

func fakeIPQueryTypes(cidr string) []string {
	if fakeIPRangeKey(cidr) == "inet6_range" {
		return []string{"AAAA"}
	}
	return []string{"A"}
}

func fakeIPRangeKey(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err == nil && ip.To4() == nil {
		return "inet6_range"
	}
	return "inet4_range"
}

func dnsServerGroup(tag string, addresses []string, detour string) []map[string]any {
	out := make([]map[string]any, 0, len(addresses))
	for i, address := range addresses {
		serverTag := tag
		if i > 0 {
			serverTag = tag + "-" + strconv.Itoa(i+1)
		}
		server := dnsServer(serverTag, address)
		if detour != "" {
			server["detour"] = detour
		}
		out = append(out, server)
	}
	return out
}

func dnsServer(tag, address string) map[string]any {
	if strings.EqualFold(address, "dhcp://auto") {
		return map[string]any{"tag": tag, "type": "dhcp", "interface": "auto"}
	}
	parsed, err := url.Parse(address)
	if err == nil && parsed.Scheme != "" {
		switch parsed.Scheme {
		case "https":
			server := map[string]any{"tag": tag, "type": "https", "server": parsed.Hostname()}
			if port := parsed.Port(); port != "" {
				if p, ok := parsePort(port); ok {
					server["server_port"] = p
				}
			}
			if parsed.Path != "" {
				server["path"] = parsed.Path
			}
			return server
		case "tls":
			server := map[string]any{"tag": tag, "type": "tls", "server": parsed.Hostname()}
			if port := parsed.Port(); port != "" {
				if p, ok := parsePort(port); ok {
					server["server_port"] = p
				}
			} else {
				server["server_port"] = 853
			}
			return server
		case "udp":
			return dnsUDPServer(tag, parsed.Hostname(), parsed.Port())
		}
	}
	host, port, err := net.SplitHostPort(address)
	if err == nil {
		return dnsUDPServer(tag, host, port)
	}
	return dnsUDPServer(tag, address, "")
}

func dnsUDPServer(tag, host, port string) map[string]any {
	server := map[string]any{"tag": tag, "type": "udp", "server": host}
	if p, ok := parsePort(port); ok {
		server["server_port"] = p
	} else {
		server["server_port"] = 53
	}
	return server
}

func parsePort(port string) (int, bool) {
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

func dnsFinalTag(final string) string {
	switch final {
	case "proxy":
		return dnsProxyTag
	case "fallback":
		return dnsFallbackTag
	default:
		return dnsDirectTag
	}
}

func configWithDefaultProxy(cfg MinimalConfig, nodeOutbounds []map[string]any, proxyTags []string, globalMode bool) MinimalConfig {
	if len(proxyTags) == 0 {
		if globalMode {
			cfg.Outbounds = []map[string]any{
				{
					"type":      "selector",
					"tag":       "Proxy",
					"outbounds": []string{"Block"},
					"default":   "Block",
				},
				{"type": "direct", "tag": "Direct"},
				{"type": "block", "tag": "Block"},
			}
			cfg.Route["final"] = "Proxy"
		}
		cfg.Metadata["usable_node_count"] = 0
		return cfg
	}
	selectorTags := append([]string{}, proxyTags...)
	fallback := "Direct"
	if globalMode {
		fallback = "Block"
	}
	if !selection.ContainsCandidate(selectorTags, fallback) {
		selectorTags = append(selectorTags, fallback)
	}
	cfg.Outbounds = append(nodeOutbounds,
		map[string]any{
			"type":      "selector",
			"tag":       "Proxy",
			"outbounds": selectorTags,
			"default":   proxyTags[0],
		},
		map[string]any{"type": "direct", "tag": "Direct"},
		map[string]any{"type": "block", "tag": "Block"},
	)
	cfg.Route["final"] = "Proxy"
	cfg.Metadata["usable_node_count"] = len(proxyTags)
	return cfg
}

func applyBootstrapProxySupport(cfg *MinimalConfig, providers []state.ProviderSpec, nodes []provider.NodeView, maxCandidates int) {
	bootstrapProviders := bootstrapProviderIDs(providers)
	if len(bootstrapProviders) == 0 {
		return
	}
	var candidates []string
	for _, node := range nodes {
		if node.Excluded {
			continue
		}
		if !bootstrapProviders[node.ProviderID] {
			continue
		}
		candidates = append(candidates, node.ID)
		if maxCandidates > 0 && len(candidates) >= maxCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return
	}
	cfg.Inbounds = append(cfg.Inbounds, map[string]any{
		"type":        "http",
		"tag":         bootstrapHTTPInboundTag,
		"listen":      "127.0.0.1",
		"listen_port": bootstrapHTTPListenPort,
	})
	cfg.Outbounds = insertOutboundBeforeSystemOutbounds(cfg.Outbounds, map[string]any{
		"type":      "selector",
		"tag":       bootstrapOutboundTag,
		"outbounds": candidates,
		"default":   candidates[0],
	})
	prependRouteRulesAfterDNSHijack(*cfg, []map[string]any{{
		"inbound":  []string{bootstrapHTTPInboundTag},
		"outbound": bootstrapOutboundTag,
	}})
	cfg.Metadata["bootstrap_proxy_url"] = provider.DefaultBootstrapProxyURL
	cfg.Metadata["bootstrap_provider_count"] = len(bootstrapProviders)
	cfg.Metadata["bootstrap_node_count"] = len(candidates)
}

func bootstrapProviderIDs(providers []state.ProviderSpec) map[string]bool {
	out := map[string]bool{}
	for _, item := range providers {
		if !item.Enabled || item.FetchMode != provider.FetchViaBootstrap || item.Bootstrap == "" {
			continue
		}
		out[item.Bootstrap] = true
	}
	return out
}

func insertOutboundBeforeSystemOutbounds(outbounds []map[string]any, outbound map[string]any) []map[string]any {
	insertAt := len(outbounds)
	for i, item := range outbounds {
		tag, _ := item["tag"].(string)
		if tag == "Direct" || tag == "Block" {
			insertAt = i
			break
		}
	}
	out := make([]map[string]any, 0, len(outbounds)+1)
	out = append(out, outbounds[:insertAt]...)
	out = append(out, outbound)
	out = append(out, outbounds[insertAt:]...)
	return out
}

func ruleOutboundSpecs(rules state.RuleCatalog, poolOutbounds map[string]string, proxyAvailable bool) []map[string]any {
	if len(rules) == 0 {
		return nil
	}
	type ruleGroup struct {
		priority int
		poolID   string
		outbound string
		suffixes []string
		keywords []string
		cidrs    []string
	}
	groups := map[string]*ruleGroup{}
	for _, rule := range state.EffectiveRuleCatalog(rules) {
		if !rule.Enabled || rule.OverriddenBy != "" {
			continue
		}
		outbound, ok := outboundForRule(rule.PoolID, poolOutbounds, proxyAvailable)
		if !ok {
			continue
		}
		priority := ruleRoutePriority(rule)
		key := fmt.Sprintf("%04d\x00%s\x00%s", 9999-priority, rule.PoolID, outbound)
		group := groups[key]
		if group == nil {
			group = &ruleGroup{priority: priority, poolID: rule.PoolID, outbound: outbound}
			groups[key] = group
		}
		switch rule.Kind {
		case state.RuleKindDomainSuffix:
			group.suffixes = append(group.suffixes, rule.Value)
		case state.RuleKindDomainKeyword:
			group.keywords = append(group.keywords, rule.Value)
		case state.RuleKindIPCIDR:
			group.cidrs = append(group.cidrs, rule.Value)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		spec := map[string]any{"outbound": group.outbound}
		if len(group.suffixes) > 0 {
			spec["domain_suffix"] = group.suffixes
		}
		if len(group.keywords) > 0 {
			spec["domain_keyword"] = group.keywords
		}
		if len(group.cidrs) > 0 {
			spec["ip_cidr"] = group.cidrs
		}
		out = append(out, spec)
	}
	return out
}

func outboundForRule(poolID string, poolOutbounds map[string]string, proxyAvailable bool) (string, bool) {
	switch poolID {
	case "block":
		return "Block", true
	case "direct":
		return "Direct", true
	case "generalproxy", "proxy":
		if tag := poolOutbounds["general"]; tag != "" {
			return tag, true
		}
		return "Proxy", proxyAvailable
	default:
		if tag := poolOutbounds[poolID]; tag != "" {
			return tag, true
		}
		return "Proxy", proxyAvailable
	}
}

func ruleRoutePriority(rule state.Rule) int {
	if rule.Source == state.RuleSourceUser {
		return 1000
	}
	switch rule.PoolID {
	case "block":
		return 700
	case "direct":
		return 600
	default:
		return 400
	}
}

func nodeOutbounds(nodes []provider.NodeView) ([]map[string]any, []string) {
	var outbounds []map[string]any
	var tags []string
	for _, node := range nodes {
		if node.Excluded {
			continue
		}
		outbound, err := CompileIdentityOutbound(state.NodeExecutor{ID: node.ID}, smartnode.IdentitySource{
			RawTag: node.Tag, Identity: node.Identity, References: node.References,
		})
		if err != nil {
			panic(fmt.Errorf("compile node %s: %w", node.ID, err))
		}
		outbounds = append(outbounds, outbound)
		tags = append(tags, node.ID)
	}
	return outbounds, tags
}

func cloneOutbound(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
