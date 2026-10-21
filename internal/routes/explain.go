package routes

import (
	"net"
	"net/netip"
	"sort"
	"strings"

	"smartgate/internal/fabric"
	"smartgate/internal/state"
)

type Decision string

const (
	DecisionLANBypass Decision = "LAN_Bypass"
	DecisionAI        Decision = "AI"
	DecisionBanking   Decision = "Banking"
	DecisionCrypto    Decision = "Crypto"
	DecisionStreaming Decision = "Streaming"
	DecisionDirect    Decision = "Direct"
	DecisionProxy     Decision = "GeneralProxy"
	DecisionBlock     Decision = "Block"
	DecisionGeo       Decision = "Geo"
	DecisionPool      Decision = "Pool"
)

type ExplainRequest struct {
	Target string `json:"target"`
}

type MatchedRule struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	PoolID string `json:"pool_id"`
}

type Binding struct {
	ID         string `json:"id,omitempty"`
	Pool       string `json:"pool"`
	Generation uint64 `json:"generation,omitempty"`
	SmartNode  string `json:"smart_node,omitempty"`
	Executor   string `json:"executor,omitempty"`
	Core       string `json:"core,omitempty"`
	Worker     string `json:"worker,omitempty"`
	Target     string `json:"target,omitempty"`
}

type Explanation struct {
	Target      string       `json:"target"`
	Decision    Decision     `json:"decision"`
	PoolID      string       `json:"pool_id,omitempty"`
	Pool        string       `json:"pool,omitempty"`
	Reason      string       `json:"reason"`
	Matched     string       `json:"matched,omitempty"`
	MatchedRule *MatchedRule `json:"matched_rule,omitempty"`
	Binding     *Binding     `json:"binding,omitempty"`
	DNSPath     []string     `json:"dns_path,omitempty"`
	Chain       []string     `json:"chain,omitempty"`
}

func Explain(target string) Explanation {
	spec := state.DefaultSpecState()
	return ExplainWithCatalog(target, state.DesiredRuleCatalog(spec), spec.BusinessPools)
}

func ExplainWithCatalog(target string, catalog state.RuleCatalog, pools []state.BusinessPoolSpec) Explanation {
	fallbackPool := fallbackPoolID(pools)
	normalized := normalizeTarget(target)
	chain := []string{"normalize_target"}
	if normalized == "" {
		return Explanation{Target: target, Decision: DecisionBlock, Reason: "empty_target", Chain: append(chain, "block_empty")}
	}
	if ip := net.ParseIP(normalized); ip != nil {
		chain = append(chain, "ip_rules")
		if isLANIP(ip) {
			return Explanation{Target: normalized, Decision: DecisionLANBypass, Reason: "private_or_loopback_ip", Matched: normalized, Chain: append(chain, "lan_bypass")}
		}
		if rule, ok := matchIPRule(ip, catalog, fallbackPool); ok {
			return explanationForRule(normalized, rule, pools, "custom_ip_rule", append(chain, "custom_rules"))
		}
		return fallbackExplanation(normalized, fallbackPool, pools, "public_ip_default_proxy", normalized, append(chain, "default_proxy"))
	}
	host := strings.ToLower(strings.TrimSuffix(normalized, "."))
	chain = append(chain, "domain_rules")
	if rule, ok := matchDomainRule(host, catalog, fallbackPool); ok {
		return explanationForRule(host, rule, pools, "custom_domain_rule", append(chain, "custom_rules"))
	}
	return fallbackExplanation(host, fallbackPool, pools, "default_proxy", "", append(chain, "default_proxy"))
}

func ExplainWithGraph(target string, graph state.ExecutionGraph, pools []state.BusinessPoolSpec) (Explanation, error) {
	normalized := normalizeTarget(target)
	chain := []string{"normalize_target"}
	if normalized == "" {
		return Explanation{Target: target, Decision: DecisionBlock, Reason: "empty_target", Chain: append(chain, "block_empty")}, nil
	}
	if ip := net.ParseIP(normalized); ip != nil {
		chain = append(chain, "ip_rules")
		if isLANIP(ip) {
			return Explanation{Target: normalized, Decision: DecisionLANBypass, Reason: "private_or_loopback_ip", Matched: normalized, Chain: append(chain, "lan_bypass")}, nil
		}
		classifier, err := fabric.ClassifierFromGraph(graph.Fabric)
		if err != nil {
			return Explanation{}, err
		}
		address, ok := netip.AddrFromSlice(ip)
		if !ok {
			return fallbackExplanation(normalized, graph.Fabric.FallbackPool, pools, "public_ip_default_proxy", normalized, append(chain, "default_proxy")), nil
		}
		return explanationForMatch(normalized, classifier.ClassifyIP(address.Unmap()), pools, false, chain), nil
	}
	host := strings.ToLower(strings.TrimSuffix(normalized, "."))
	chain = append(chain, "domain_rules")
	classifier, err := fabric.ClassifierFromGraph(graph.Fabric)
	if err != nil {
		return Explanation{}, err
	}
	return explanationForMatch(host, classifier.ClassifyDomain(host), pools, true, chain), nil
}

func AttachAppliedPath(explanation Explanation, graph state.ExecutionGraph) Explanation {
	poolID := explanation.PoolID
	if poolID == "" {
		return explanation
	}
	for _, binding := range graph.PoolBindings {
		if binding.Pool != poolID {
			continue
		}
		target := binding.Target.Kind
		if target == "" {
			target = binding.Target.SmartNode
		}
		explanation.Binding = &Binding{
			ID: binding.ID, Pool: binding.Pool, Generation: binding.Generation,
			SmartNode: binding.Target.SmartNode, Executor: binding.Target.Executor,
			Core: string(binding.Target.Core), Worker: binding.Target.Worker, Target: target,
		}
		break
	}
	path := []string{"smartgate-dns", "classify"}
	if explanation.MatchedRule != nil {
		path = append(path, "rule:"+explanation.MatchedRule.ID)
	} else {
		path = append(path, "fallback:"+poolID)
	}
	path = append(path, "pool:"+poolID)
	for _, dispatch := range graph.Fabric.Dispatch {
		if dispatch.Pool != poolID {
			continue
		}
		if dispatch.DNSInbound != "" {
			path = append(path, "dns-inbound:"+dispatch.DNSInbound)
		}
		if dispatch.Target != "" {
			path = append(path, "target:"+dispatch.Target)
		}
		break
	}
	if explanation.Binding != nil {
		if explanation.Binding.Worker != "" {
			path = append(path, "worker:"+explanation.Binding.Worker)
		}
		if explanation.Binding.SmartNode != "" {
			path = append(path, "smart-node:"+explanation.Binding.SmartNode)
		}
	}
	explanation.DNSPath = path
	return explanation
}

func matchDomainRule(host string, catalog state.RuleCatalog, fallbackPool string) (state.Rule, bool) {
	for _, rule := range orderedRules(catalog, fallbackPool) {
		switch rule.Kind {
		case state.RuleKindDomainSuffix:
			if domainMatch(host, rule.Value) {
				return rule, true
			}
		case state.RuleKindDomainKeyword:
			if strings.Contains(host, rule.Value) {
				return rule, true
			}
		}
	}
	return state.Rule{}, false
}

func matchIPRule(ip net.IP, catalog state.RuleCatalog, fallbackPool string) (state.Rule, bool) {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return state.Rule{}, false
	}
	address = address.Unmap()
	for _, rule := range orderedRules(catalog, fallbackPool) {
		if rule.Kind != state.RuleKindIPCIDR {
			continue
		}
		if prefix, err := netip.ParsePrefix(rule.Value); err == nil && prefix.Masked().Contains(address) {
			return rule, true
		}
		if exact, err := netip.ParseAddr(rule.Value); err == nil && exact.Unmap() == address {
			return rule, true
		}
	}
	return state.Rule{}, false
}

func orderedRules(catalog state.RuleCatalog, fallbackPool string) state.RuleCatalog {
	rules := make(state.RuleCatalog, 0, len(catalog))
	for _, rule := range state.EffectiveRuleCatalog(catalog) {
		if rule.Enabled && rule.OverriddenBy == "" && rule.Kind != state.RuleKindGeoSite && rule.Kind != state.RuleKindGeoIP {
			rules = append(rules, rule)
		}
	}
	sort.SliceStable(rules, func(i, j int) bool {
		left, right := routePriority(rules[i], fallbackPool), routePriority(rules[j], fallbackPool)
		if left != right {
			return left > right
		}
		if len(rules[i].Value) != len(rules[j].Value) {
			return len(rules[i].Value) > len(rules[j].Value)
		}
		return rules[i].ID < rules[j].ID
	})
	return rules
}

func routePriority(rule state.Rule, fallbackPool string) int {
	if rule.Source == state.RuleSourceUser {
		return 1000
	}
	switch rule.PoolID {
	case "block":
		return 700
	case "direct":
		return 600
	case fallbackPool:
		return 100
	default:
		return 500
	}
}

func explanationForRule(target string, rule state.Rule, pools []state.BusinessPoolSpec, reason string, chain []string) Explanation {
	return Explanation{
		Target: target, Decision: decisionForPool(rule.PoolID, pools), PoolID: rule.PoolID, Pool: rule.PoolID,
		Reason: reason, Matched: rule.Value, Chain: chain,
		MatchedRule: &MatchedRule{ID: rule.ID, Source: rule.Source, Kind: rule.Kind, Value: rule.Value, PoolID: rule.PoolID},
	}
}

func explanationForMatch(target string, match fabric.Match, pools []state.BusinessPoolSpec, domain bool, chain []string) Explanation {
	if match.Fallback {
		reason := "public_ip_default_proxy"
		matched := target
		if domain {
			reason = "default_proxy"
			matched = ""
		}
		return fallbackExplanation(target, match.Target, pools, reason, matched, append(chain, "default_proxy"))
	}
	reason := "custom_ip_rule"
	step := "custom_rules"
	if domain {
		reason = "custom_domain_rule"
	}
	if match.Kind == fabric.RuleGeoSite {
		reason = "geo_domain_rule"
		step = "geo_rules"
	} else if match.Kind == fabric.RuleGeoIP {
		reason = "geo_ip_rule"
		step = "geo_rules"
	}
	return explanationForRule(target, state.Rule{
		ID: match.RuleID, Source: match.Source, Kind: match.Kind, Value: match.Value,
		PoolID: match.Target, Enabled: true,
	}, pools, reason, append(chain, step))
}

func fallbackExplanation(target, fallbackPool string, pools []state.BusinessPoolSpec, reason, matched string, chain []string) Explanation {
	return Explanation{Target: target, Decision: decisionForPool(fallbackPool, pools), PoolID: fallbackPool, Pool: fallbackPool, Reason: reason, Matched: matched, Chain: chain}
}

func decisionForPool(poolID string, pools []state.BusinessPoolSpec) Decision {
	switch poolID {
	case "block":
		return DecisionBlock
	case "direct":
		return DecisionDirect
	}
	role := ""
	for _, pool := range state.EffectiveBusinessPools(pools) {
		if pool.ID == poolID {
			role = pool.Role
			break
		}
	}
	switch role {
	case "general":
		return DecisionProxy
	case "ai":
		return DecisionAI
	case "banking":
		return DecisionBanking
	case "crypto":
		return DecisionCrypto
	case "streaming":
		return DecisionStreaming
	default:
		return DecisionPool
	}
}

func fallbackPoolID(pools []state.BusinessPoolSpec) string {
	for _, pool := range state.EffectiveBusinessPools(pools) {
		if pool.Role == "general" {
			return pool.ID
		}
	}
	return "general"
}

func normalizeTarget(target string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimPrefix(target, "https://")
	if host, _, err := net.SplitHostPort(target); err == nil {
		return host
	}
	if idx := strings.IndexByte(target, '/'); idx >= 0 {
		target = target[:idx]
	}
	return strings.TrimSpace(target)
}

func domainMatch(host, domain string) bool {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func isLANIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
