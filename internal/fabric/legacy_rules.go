package fabric

import (
	"fmt"
	"strings"

	"smartgate/internal/state"
)

func RulesFromSpec(spec state.SpecState) ([]Rule, string, error) {
	pools := state.EffectiveBusinessPools(spec.BusinessPools)
	poolIDs := make([]string, 0, len(pools))
	allowed := map[string]struct{}{TargetDirect: {}, TargetBlock: {}}
	fallbackPoolID := ""
	for _, pool := range pools {
		poolID := strings.TrimSpace(pool.ID)
		if poolID == "" {
			return nil, "", fmt.Errorf("business pool id is required")
		}
		if _, exists := allowed[poolID]; exists {
			return nil, "", fmt.Errorf("business pool id %q is reserved", poolID)
		}
		allowed[poolID] = struct{}{}
		poolIDs = append(poolIDs, poolID)
		if strings.TrimSpace(pool.Role) == "general" {
			if fallbackPoolID != "" && fallbackPoolID != poolID {
				return nil, "", fmt.Errorf("multiple general fallback pools are configured")
			}
			fallbackPoolID = poolID
		}
	}
	if fallbackPoolID == "" {
		return nil, "", fmt.Errorf("a pool with role general is required as the fabric fallback")
	}

	catalog := state.EffectiveRuleCatalog(state.DesiredRuleCatalog(spec))
	if state.EffectiveGatewayMode(spec.Settings.GatewayMode) == state.GatewayModeGlobal {
		filtered := make(state.RuleCatalog, 0, len(catalog))
		for _, rule := range catalog {
			if rule.PoolID != TargetDirect {
				filtered = append(filtered, rule)
			}
		}
		for _, rule := range state.DefaultRules() {
			if rule.PoolID == TargetDirect {
				filtered = append(filtered, rule)
			}
		}
		catalog = state.EffectiveRuleCatalog(filtered)
	}
	rules := make([]Rule, 0, len(catalog))
	for _, item := range catalog {
		if !item.Enabled || item.OverriddenBy != "" {
			continue
		}
		normalized, err := state.NormalizeRule(item)
		if err != nil {
			return nil, "", err
		}
		if _, ok := allowed[normalized.PoolID]; !ok {
			return nil, "", fmt.Errorf("rule %s references unknown pool %q", normalized.ID, normalized.PoolID)
		}
		rules = append(rules, Rule{
			ID: normalized.ID, Source: normalized.Source, Kind: normalized.Kind,
			Value: normalized.Value, PoolID: normalized.PoolID, Enabled: normalized.Enabled,
			Priority: rulePriority(normalized, fallbackPoolID),
		})
	}
	rules = withGeoCNProxySafety(rules, fallbackPoolID)
	conflicts, err := ValidateRules(rules, poolIDs)
	if err != nil {
		return nil, "", err
	}
	if len(conflicts) > 0 {
		return nil, "", fmt.Errorf("rule %s %q has multiple owners: %s", conflicts[0].Kind, conflicts[0].Value, strings.Join(conflicts[0].Pools, ", "))
	}
	return rules, fallbackPoolID, nil
}

func ClassifierFromSpec(spec state.SpecState) (*Classifier, error) {
	rules, fallbackPoolID, err := RulesFromSpec(spec)
	if err != nil {
		return nil, err
	}
	pools := state.EffectiveBusinessPools(spec.BusinessPools)
	poolIDs := make([]string, 0, len(pools))
	for _, pool := range pools {
		poolIDs = append(poolIDs, pool.ID)
	}
	return NewClassifier(rules, fallbackPoolID, poolIDs)
}

func rulePriority(rule state.Rule, fallbackPoolID string) int {
	if rule.Source == state.RuleSourceUser {
		return 1000
	}
	if rule.Source == state.RuleSourceGeoDefault && rule.PoolID == TargetDirect {
		return 200
	}
	switch rule.PoolID {
	case TargetBlock:
		return 700
	case TargetDirect:
		return 600
	case fallbackPoolID:
		return 100
	default:
		return 500
	}
}

func withGeoCNProxySafety(rules []Rule, fallback string) []Rule {
	enabled := false
	explicit := map[string]bool{}
	for _, rule := range rules {
		if rule.Kind == RuleGeoSite && rule.Value == "geosite-cn" && rule.PoolID == TargetDirect {
			enabled = true
		}
		if rule.Kind == RuleDomainSuffix {
			explicit[rule.Value] = true
		}
	}
	if !enabled {
		return rules
	}
	for _, suffix := range state.GeoCNProxySafetyDomainSuffixes() {
		if explicit[suffix] {
			continue
		}
		rules = append(rules, Rule{ID: "geo-cn-safety-" + suffix, Source: state.RuleSourceGeoDefault, Kind: RuleDomainSuffix, Value: suffix, PoolID: fallback, Enabled: true, Priority: 350})
	}
	return rules
}
