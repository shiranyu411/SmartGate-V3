package fabric

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"smartgate/internal/state"
)

const (
	RuleDomainSuffix  = "domain_suffix"
	RuleDomainKeyword = "domain_keyword"
	RuleIPCIDR        = "ip_cidr"
	RuleGeoSite       = "geosite"
	RuleGeoIP         = "geoip"

	TargetDirect = "direct"
	TargetBlock  = "block"
)

type Rule struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	PoolID   string `json:"pool_id"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
}

func ClassifierFromGraph(graph state.FabricGraph) (*Classifier, error) {
	rules := make([]Rule, 0, len(graph.Rules))
	for _, rule := range graph.Rules {
		rules = append(rules, Rule{
			ID: rule.ID, Source: rule.Source, Kind: rule.Kind, Value: rule.Value,
			PoolID: rule.Target, Enabled: true, Priority: rule.Priority,
		})
	}
	poolIDs := make([]string, 0, len(graph.Dispatch))
	for _, dispatch := range graph.Dispatch {
		poolIDs = append(poolIDs, dispatch.Pool)
	}
	matchers := make(map[string]RuleSetMatcher, len(graph.RuleSets))
	for _, ref := range graph.RuleSets {
		matcher, err := LoadRuleSetMatcher(ref, defaultRuleSetJSONLimit)
		if err != nil {
			return nil, fmt.Errorf("load GEO rule set %s: %w", ref.ID, err)
		}
		matchers[ref.ID] = matcher
	}
	return newClassifier(rules, graph.FallbackPool, poolIDs, matchers)
}

type RuleConflict struct {
	Kind    string   `json:"kind"`
	Value   string   `json:"value"`
	Pools   []string `json:"pools"`
	RuleIDs []string `json:"rule_ids"`
}

type Match struct {
	Target   string `json:"target"`
	RuleID   string `json:"rule_id,omitempty"`
	Source   string `json:"source,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Value    string `json:"value,omitempty"`
	Fallback bool   `json:"fallback"`
}

type compiledRule struct {
	rule           Rule
	domain         string
	domainKind     domainMatchKind
	prefix         netip.Prefix
	regex          *regexp.Regexp
	subdomainsOnly bool
}

type domainMatchKind uint8

const (
	domainMatchRegex domainMatchKind = iota + 1
	domainMatchKeyword
	domainMatchSuffix
	domainMatchExact
)

type Classifier struct {
	domainRules     []compiledRule
	ipPrefixes      []compiledRule
	fallbackPoolID  string
	allowedPoolIDs  map[string]struct{}
	validationRules []Rule
}

func NewClassifier(rules []Rule, fallbackPoolID string, poolIDs []string) (*Classifier, error) {
	return newClassifier(rules, fallbackPoolID, poolIDs, nil)
}

func newClassifier(rules []Rule, fallbackPoolID string, poolIDs []string, matchers map[string]RuleSetMatcher) (*Classifier, error) {
	fallbackPoolID = strings.TrimSpace(fallbackPoolID)
	allowed := make(map[string]struct{}, len(poolIDs))
	for _, poolID := range poolIDs {
		poolID = strings.TrimSpace(poolID)
		if poolID == "" {
			return nil, fmt.Errorf("pool id is required")
		}
		allowed[poolID] = struct{}{}
	}
	if _, ok := allowed[fallbackPoolID]; !ok {
		return nil, fmt.Errorf("fallback pool %q is not configured", fallbackPoolID)
	}
	conflicts, err := ValidateRules(rules, poolIDs)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("rule %s %q has multiple owners: %s", conflicts[0].Kind, conflicts[0].Value, strings.Join(conflicts[0].Pools, ", "))
	}

	classifier := &Classifier{
		fallbackPoolID:  fallbackPoolID,
		allowedPoolIDs:  allowed,
		validationRules: append([]Rule(nil), rules...),
	}
	for _, input := range rules {
		if !input.Enabled {
			continue
		}
		rule, err := normalizeRule(input)
		if err != nil {
			return nil, err
		}
		compiled := compiledRule{rule: rule}
		switch rule.Kind {
		case RuleDomainSuffix:
			compiled.domain = rule.Value
			compiled.domainKind = domainMatchSuffix
			classifier.domainRules = append(classifier.domainRules, compiled)
		case RuleDomainKeyword:
			compiled.domain = rule.Value
			compiled.domainKind = domainMatchKeyword
			classifier.domainRules = append(classifier.domainRules, compiled)
		case RuleIPCIDR:
			prefix, err := netip.ParsePrefix(rule.Value)
			if err != nil {
				return nil, fmt.Errorf("rule %s has invalid CIDR %q: %w", rule.ID, rule.Value, err)
			}
			compiled.prefix = prefix.Masked()
			classifier.ipPrefixes = append(classifier.ipPrefixes, compiled)
		case RuleGeoSite:
			matcher, ok := matchers[rule.Value]
			if !ok {
				return nil, fmt.Errorf("rule %s requires GEO rule set %q", rule.ID, rule.Value)
			}
			for _, domain := range matcher.DomainExact {
				domain = normalizeDomain(domain)
				if domain == "" {
					return nil, fmt.Errorf("rule %s GEO exact domain is empty", rule.ID)
				}
				classifier.domainRules = append(classifier.domainRules, compiledRule{rule: rule, domain: domain, domainKind: domainMatchExact})
			}
			for _, domain := range matcher.DomainSuffix {
				subdomainsOnly := strings.HasPrefix(strings.TrimSpace(domain), ".")
				compiledDomain := normalizeDomain(strings.TrimPrefix(strings.TrimSpace(domain), "."))
				if compiledDomain == "" {
					return nil, fmt.Errorf("rule %s GEO domain suffix is empty", rule.ID)
				}
				classifier.domainRules = append(classifier.domainRules, compiledRule{
					rule: rule, domain: compiledDomain, domainKind: domainMatchSuffix, subdomainsOnly: subdomainsOnly,
				})
			}
			for _, keyword := range matcher.DomainKeyword {
				keyword = strings.ToLower(strings.TrimSpace(keyword))
				if keyword == "" {
					return nil, fmt.Errorf("rule %s GEO domain keyword is empty", rule.ID)
				}
				classifier.domainRules = append(classifier.domainRules, compiledRule{rule: rule, domain: keyword, domainKind: domainMatchKeyword})
			}
			for _, expression := range matcher.DomainRegex {
				compiledRegex, err := regexp.Compile(expression)
				if err != nil {
					return nil, fmt.Errorf("rule %s GEO regex is invalid: %w", rule.ID, err)
				}
				classifier.domainRules = append(classifier.domainRules, compiledRule{rule: rule, domain: expression, domainKind: domainMatchRegex, regex: compiledRegex})
			}
		case RuleGeoIP:
			matcher, ok := matchers[rule.Value]
			if !ok {
				return nil, fmt.Errorf("rule %s requires GEO rule set %q", rule.ID, rule.Value)
			}
			for _, value := range matcher.IPCIDR {
				prefix, err := netip.ParsePrefix(value)
				if err != nil {
					return nil, fmt.Errorf("rule %s GEO CIDR %q is invalid: %w", rule.ID, value, err)
				}
				classifier.ipPrefixes = append(classifier.ipPrefixes, compiledRule{rule: rule, prefix: prefix.Masked()})
			}
		default:
			return nil, fmt.Errorf("rule %s has unsupported kind %q", rule.ID, rule.Kind)
		}
	}
	classifier.sort()
	return classifier, nil
}

func ValidateRules(rules []Rule, poolIDs []string) ([]RuleConflict, error) {
	allowed := map[string]struct{}{TargetDirect: {}, TargetBlock: {}}
	for _, poolID := range poolIDs {
		poolID = strings.TrimSpace(poolID)
		if poolID == "" {
			return nil, fmt.Errorf("pool id is required")
		}
		allowed[poolID] = struct{}{}
	}
	type owners struct {
		pools map[string]struct{}
		ids   map[string]struct{}
	}
	byKey := map[string]*owners{}
	seenIDs := map[string]struct{}{}
	for _, input := range rules {
		if !input.Enabled {
			continue
		}
		rule, err := normalizeRule(input)
		if err != nil {
			return nil, err
		}
		if _, ok := seenIDs[rule.ID]; ok {
			return nil, fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		seenIDs[rule.ID] = struct{}{}
		if _, ok := allowed[rule.PoolID]; !ok {
			return nil, fmt.Errorf("rule %s references unknown pool %q", rule.ID, rule.PoolID)
		}
		key := rule.Kind + "\x00" + rule.Value
		entry := byKey[key]
		if entry == nil {
			entry = &owners{pools: map[string]struct{}{}, ids: map[string]struct{}{}}
			byKey[key] = entry
		}
		entry.pools[rule.PoolID] = struct{}{}
		entry.ids[rule.ID] = struct{}{}
	}
	conflicts := make([]RuleConflict, 0)
	for key, entry := range byKey {
		if len(entry.pools) < 2 {
			continue
		}
		kind, value, _ := strings.Cut(key, "\x00")
		conflicts = append(conflicts, RuleConflict{
			Kind: kind, Value: value, Pools: sortedKeys(entry.pools), RuleIDs: sortedKeys(entry.ids),
		})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Kind != conflicts[j].Kind {
			return conflicts[i].Kind < conflicts[j].Kind
		}
		return conflicts[i].Value < conflicts[j].Value
	})
	return conflicts, nil
}

func (classifier *Classifier) ClassifyDomain(domain string) Match {
	domain = normalizeDomain(domain)
	if domain != "" {
		for _, rule := range classifier.domainRules {
			if compiledDomainMatches(rule, domain) {
				return matchForRule(rule.rule)
			}
		}
	}
	return Match{Target: classifier.fallbackPoolID, Fallback: true}
}

func (classifier *Classifier) ClassifyIP(address netip.Addr) Match {
	address = address.Unmap()
	if address.IsValid() {
		for _, rule := range classifier.ipPrefixes {
			if rule.prefix.Contains(address) {
				return matchForRule(rule.rule)
			}
		}
	}
	return Match{Target: classifier.fallbackPoolID, Fallback: true}
}

func (classifier *Classifier) FallbackPoolID() string {
	if classifier == nil {
		return ""
	}
	return classifier.fallbackPoolID
}

func (classifier *Classifier) sort() {
	sort.SliceStable(classifier.domainRules, func(i, j int) bool {
		left, right := classifier.domainRules[i], classifier.domainRules[j]
		if left.rule.Priority != right.rule.Priority {
			return left.rule.Priority > right.rule.Priority
		}
		if left.domainKind != right.domainKind {
			return left.domainKind > right.domainKind
		}
		if len(left.domain) != len(right.domain) {
			return len(left.domain) > len(right.domain)
		}
		if left.subdomainsOnly != right.subdomainsOnly {
			return left.subdomainsOnly
		}
		return left.rule.ID < right.rule.ID
	})
	sort.SliceStable(classifier.ipPrefixes, func(i, j int) bool {
		left, right := classifier.ipPrefixes[i], classifier.ipPrefixes[j]
		if left.rule.Priority != right.rule.Priority {
			return left.rule.Priority > right.rule.Priority
		}
		if left.prefix.Bits() != right.prefix.Bits() {
			return left.prefix.Bits() > right.prefix.Bits()
		}
		return left.rule.ID < right.rule.ID
	})
}

func compiledDomainMatches(rule compiledRule, domain string) bool {
	switch rule.domainKind {
	case domainMatchExact:
		return domain == rule.domain
	case domainMatchSuffix:
		return (!rule.subdomainsOnly && domain == rule.domain) || strings.HasSuffix(domain, "."+rule.domain)
	case domainMatchKeyword:
		return strings.Contains(domain, rule.domain)
	case domainMatchRegex:
		return rule.regex != nil && rule.regex.MatchString(domain)
	default:
		return false
	}
}

func normalizeRule(input Rule) (Rule, error) {
	rule := input
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Source = strings.TrimSpace(rule.Source)
	rule.Kind = strings.ToLower(strings.TrimSpace(rule.Kind))
	rule.PoolID = strings.TrimSpace(rule.PoolID)
	if rule.ID == "" {
		return Rule{}, fmt.Errorf("rule id is required")
	}
	if rule.PoolID == "" {
		return Rule{}, fmt.Errorf("rule %s pool id is required", rule.ID)
	}
	switch rule.Kind {
	case RuleDomainSuffix:
		rule.Value = normalizeDomain(rule.Value)
	case RuleDomainKeyword:
		rule.Value = strings.ToLower(strings.TrimSpace(rule.Value))
	case RuleIPCIDR:
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rule.Value))
		if err != nil {
			return Rule{}, fmt.Errorf("rule %s has invalid CIDR %q: %w", rule.ID, rule.Value, err)
		}
		rule.Value = prefix.Masked().String()
	case RuleGeoSite, RuleGeoIP:
		rule.Value = strings.ToLower(strings.TrimSpace(rule.Value))
	default:
		return Rule{}, fmt.Errorf("rule %s has unsupported kind %q", rule.ID, rule.Kind)
	}
	if rule.Value == "" {
		return Rule{}, fmt.Errorf("rule %s value is required", rule.ID)
	}
	return rule, nil
}

func normalizeDomain(domain string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
}

func matchForRule(rule Rule) Match {
	return Match{Target: rule.PoolID, RuleID: rule.ID, Source: rule.Source, Kind: rule.Kind, Value: rule.Value}
}

func sortedKeys[T any](values map[string]T) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
