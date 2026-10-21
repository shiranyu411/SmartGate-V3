package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	RuleSourceBuiltinV2  = "builtin-v2"
	RuleSourceGeoDefault = "geo-default"
	RuleSourceUser       = "user"

	RuleKindDomainSuffix  = "domain_suffix"
	RuleKindDomainKeyword = "domain_keyword"
	RuleKindIPCIDR        = "ip_cidr"
	RuleKindGeoSite       = "geosite"
	RuleKindGeoIP         = "geoip"
)

type Rule struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Kind         string `json:"kind"`
	Value        string `json:"value"`
	PoolID       string `json:"pool_id"`
	Enabled      bool   `json:"enabled"`
	OverriddenBy string `json:"overridden_by,omitempty"`
}

type RuleCatalog []Rule

type RuleConflict struct {
	RuleKey string   `json:"rule_key"`
	Kind    string   `json:"kind"`
	Value   string   `json:"value"`
	Pools   []string `json:"pools"`
	RuleIDs []string `json:"rule_ids"`
}

func (catalog *RuleCatalog) UnmarshalJSON(body []byte) error {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		*catalog = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var rules []Rule
		if err := json.Unmarshal(body, &rules); err != nil {
			return err
		}
		*catalog = rules
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var groups map[string]RuleSpec
		if err := json.Unmarshal(body, &groups); err != nil {
			return err
		}
		*catalog = RulesFromLegacyOverrides(groups, nil)
		return nil
	}
	return fmt.Errorf("rules must be an array or legacy group object")
}

func DefaultRules() RuleCatalog {
	return RulesFromLegacyGroups(DefaultRuleSpecs(), DefaultBusinessPools())
}

func RulesFromLegacyGroups(groups map[string]RuleSpec, pools []BusinessPoolSpec) RuleCatalog {
	if groups == nil {
		return nil
	}
	roleTargets := poolTargetsByRole(pools)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(RuleCatalog, 0)
	seen := make(map[string]struct{})
	appendRule := func(rule Rule) {
		key := RuleKey(rule.Kind, rule.Value)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, rule)
	}
	for _, group := range keys {
		target := legacyRuleTarget(group, roleTargets)
		spec := groups[group]
		for _, value := range spec.DomainSuffix {
			appendRule(newBuiltinRule(group, RuleKindDomainSuffix, value, target))
		}
		for _, value := range spec.DomainKeyword {
			appendRule(newBuiltinRule(group, RuleKindDomainKeyword, value, target))
		}
		for _, value := range spec.IPCIDR {
			appendRule(newBuiltinRule(group, RuleKindIPCIDR, value, target))
		}
	}
	return NormalizeRuleCatalog(out)
}

func RulesFromLegacyOverrides(overrides map[string]RuleSpec, pools []BusinessPoolSpec) RuleCatalog {
	groups := DefaultRuleSpecs()
	for key, spec := range overrides {
		if isKnownRuleKey(key) {
			groups[key] = spec
		}
	}
	return RulesFromLegacyGroups(groups, pools)
}

func GeoDefaultRules(settings RemoteRuleSettings, pools []BusinessPoolSpec) RuleCatalog {
	settings = EffectiveRemoteRuleSettings(settings)
	roleTargets := poolTargetsByRole(pools)
	out := make(RuleCatalog, 0, len(settings.Sets))
	for _, set := range settings.Sets {
		if !isDefaultGeoRuleSet(set.ID) {
			continue
		}
		kind := RuleKindGeoSite
		if set.ID == "geoip-cn" {
			kind = RuleKindGeoIP
		}
		enabled := settings.Enabled && set.Enabled
		if set.ID == "geosite-category-ads" {
			enabled = enabled && settings.AdBlock.Enabled
		}
		out = append(out, Rule{
			ID:      "geo-default-" + set.ID,
			Source:  RuleSourceGeoDefault,
			Kind:    kind,
			Value:   set.ID,
			PoolID:  resolveRuleTarget(set.Target, roleTargets),
			Enabled: enabled,
		})
	}
	return out
}

func DesiredRuleCatalog(spec SpecState) RuleCatalog {
	out := make(RuleCatalog, 0, len(spec.Rules)+len(spec.RemoteRules.Sets))
	geoEnabled := map[string]bool{}
	for _, rule := range spec.Rules {
		if rule.Source == RuleSourceGeoDefault {
			geoEnabled[rule.ID] = rule.Enabled
			continue
		}
		out = append(out, rule)
	}
	for _, rule := range GeoDefaultRules(spec.RemoteRules, spec.BusinessPools) {
		if enabled, ok := geoEnabled[rule.ID]; ok {
			rule.Enabled = enabled
		}
		out = append(out, rule)
	}
	return NormalizeRuleCatalogForPools(out, spec.BusinessPools)
}

func NormalizeRuleCatalog(in RuleCatalog) RuleCatalog {
	if in == nil {
		return nil
	}
	out := make(RuleCatalog, 0, len(in))
	for _, rule := range in {
		rule.ID = strings.TrimSpace(rule.ID)
		rule.Source = strings.TrimSpace(strings.ToLower(rule.Source))
		rule.Kind = strings.TrimSpace(strings.ToLower(rule.Kind))
		rule.Value = normalizeRuleValue(rule.Kind, rule.Value)
		rule.PoolID = normalizeReservedTarget(rule.PoolID)
		rule.OverriddenBy = ""
		out = append(out, rule)
	}
	return out
}

func NormalizeRuleCatalogForPools(in RuleCatalog, pools []BusinessPoolSpec) RuleCatalog {
	out := NormalizeRuleCatalog(in)
	roleTargets := poolTargetsByRole(pools)
	for index := range out {
		out[index].PoolID = resolveRuleTarget(out[index].PoolID, roleTargets)
	}
	return out
}

func RuleCatalogToLegacyGroups(rules RuleCatalog, pools []BusinessPoolSpec) map[string]RuleSpec {
	out := map[string]RuleSpec{}
	poolRoles := roleByPoolID(pools)
	for _, rule := range EffectiveRuleCatalog(rules) {
		if !rule.Enabled {
			continue
		}
		group := rule.PoolID
		switch group {
		case "direct", "block":
		default:
			role := poolRoles[group]
			if role == "general" {
				group = "generalproxy"
			} else if isKnownRuleKey(role) {
				group = role
			} else {
				continue
			}
		}
		spec := out[group]
		switch rule.Kind {
		case RuleKindDomainSuffix:
			spec.DomainSuffix = append(spec.DomainSuffix, rule.Value)
		case RuleKindDomainKeyword:
			spec.DomainKeyword = append(spec.DomainKeyword, rule.Value)
		case RuleKindIPCIDR:
			spec.IPCIDR = append(spec.IPCIDR, rule.Value)
		default:
			continue
		}
		out[group] = spec
	}
	return out
}

func EffectiveRuleCatalog(rules RuleCatalog) RuleCatalog {
	out := append(RuleCatalog(nil), NormalizeRuleCatalog(rules)...)
	userByKey := map[string][]string{}
	for _, rule := range out {
		if rule.Enabled && rule.Source == RuleSourceUser {
			key := RuleKey(rule.Kind, rule.Value)
			userByKey[key] = append(userByKey[key], rule.ID)
		}
	}
	for index := range out {
		if out[index].Source == RuleSourceUser || !out[index].Enabled {
			continue
		}
		ids := userByKey[RuleKey(out[index].Kind, out[index].Value)]
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		out[index].OverriddenBy = ids[0]
	}
	return out
}

func RuleKey(kind, value string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	return kind + "\x00" + normalizeRuleValue(kind, value)
}

func ValidRuleSource(source string) bool {
	switch source {
	case RuleSourceBuiltinV2, RuleSourceGeoDefault, RuleSourceUser:
		return true
	default:
		return false
	}
}

func ValidRuleKind(kind string) bool {
	switch kind {
	case RuleKindDomainSuffix, RuleKindDomainKeyword, RuleKindIPCIDR, RuleKindGeoSite, RuleKindGeoIP:
		return true
	default:
		return false
	}
}

func NormalizeRule(rule Rule) (Rule, error) {
	rule = NormalizeRuleCatalog(RuleCatalog{rule})[0]
	if rule.ID == "" {
		return Rule{}, fmt.Errorf("rule id is required")
	}
	if !ValidRuleSource(rule.Source) {
		return Rule{}, fmt.Errorf("rule %s has invalid source %q", rule.ID, rule.Source)
	}
	if !ValidRuleKind(rule.Kind) {
		return Rule{}, fmt.Errorf("rule %s has invalid kind %q", rule.ID, rule.Kind)
	}
	if rule.Value == "" {
		return Rule{}, fmt.Errorf("rule %s value is required", rule.ID)
	}
	if rule.PoolID == "" {
		return Rule{}, fmt.Errorf("rule %s pool_id is required", rule.ID)
	}
	if rule.Kind == RuleKindIPCIDR {
		if _, err := netip.ParsePrefix(rule.Value); err != nil {
			if _, addrErr := netip.ParseAddr(rule.Value); addrErr != nil {
				return Rule{}, fmt.Errorf("rule %s has invalid IP/CIDR %q", rule.ID, rule.Value)
			}
		}
	}
	return rule, nil
}

func ValidateRuleCatalog(rules RuleCatalog, pools []BusinessPoolSpec) (RuleCatalog, []RuleConflict, error) {
	allowed := map[string]struct{}{"direct": {}, "block": {}}
	for _, pool := range EffectiveBusinessPools(pools) {
		poolID := strings.TrimSpace(pool.ID)
		if poolID == "" {
			return nil, nil, fmt.Errorf("business pool id is required")
		}
		allowed[poolID] = struct{}{}
	}
	normalized := NormalizeRuleCatalogForPools(rules, pools)
	seenIDs := map[string]struct{}{}
	type owners struct {
		kind  string
		value string
		pools map[string]struct{}
		ids   map[string]struct{}
	}
	byKey := map[string]*owners{}
	for index := range normalized {
		rule, err := NormalizeRule(normalized[index])
		if err != nil {
			return nil, nil, err
		}
		if _, exists := seenIDs[rule.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		seenIDs[rule.ID] = struct{}{}
		if _, ok := allowed[rule.PoolID]; !ok {
			return nil, nil, fmt.Errorf("rule %s references unknown pool %q", rule.ID, rule.PoolID)
		}
		normalized[index] = rule
		if !rule.Enabled || rule.Source != RuleSourceUser {
			continue
		}
		key := RuleKey(rule.Kind, rule.Value)
		entry := byKey[key]
		if entry == nil {
			entry = &owners{kind: rule.Kind, value: rule.Value, pools: map[string]struct{}{}, ids: map[string]struct{}{}}
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
		conflicts = append(conflicts, RuleConflict{
			RuleKey: key,
			Kind:    entry.kind,
			Value:   entry.value,
			Pools:   sortedStringSet(entry.pools),
			RuleIDs: sortedStringSet(entry.ids),
		})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].RuleKey < conflicts[j].RuleKey })
	return normalized, conflicts, nil
}

func RuleConflicts(spec SpecState) ([]RuleConflict, error) {
	rules := NormalizeRuleCatalogForPools(DesiredRuleCatalog(spec), spec.BusinessPools)
	type owners struct {
		kind  string
		value string
		pools map[string]struct{}
		ids   map[string]struct{}
	}
	byKey := map[string]*owners{}
	for _, input := range rules {
		if !input.Enabled || input.Source != RuleSourceUser {
			continue
		}
		rule, err := NormalizeRule(input)
		if err != nil {
			return nil, err
		}
		key := RuleKey(rule.Kind, rule.Value)
		entry := byKey[key]
		if entry == nil {
			entry = &owners{kind: rule.Kind, value: rule.Value, pools: map[string]struct{}{}, ids: map[string]struct{}{}}
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
		conflicts = append(conflicts, RuleConflict{
			RuleKey: key, Kind: entry.kind, Value: entry.value,
			Pools: sortedStringSet(entry.pools), RuleIDs: sortedStringSet(entry.ids),
		})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].RuleKey < conflicts[j].RuleKey })
	return conflicts, nil
}

func sortedStringSet(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for value := range in {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func SystemRuleByID(spec SpecState) map[string]Rule {
	out := map[string]Rule{}
	for _, rule := range DesiredRuleCatalog(spec) {
		if rule.Source != RuleSourceUser {
			out[rule.ID] = rule
		}
	}
	return out
}

func newBuiltinRule(group, kind, value, target string) Rule {
	normalized := normalizeRuleValue(kind, value)
	hash := sha256.Sum256([]byte(group + "\x00" + kind + "\x00" + normalized))
	return Rule{
		ID:      "builtin-v2-" + hex.EncodeToString(hash[:8]),
		Source:  RuleSourceBuiltinV2,
		Kind:    kind,
		Value:   normalized,
		PoolID:  target,
		Enabled: true,
	}
}

func normalizeRuleValue(kind, value string) string {
	value = strings.TrimSpace(value)
	switch kind {
	case RuleKindDomainSuffix, RuleKindDomainKeyword:
		value = strings.ToLower(value)
		value = strings.TrimPrefix(value, ".")
		value = strings.TrimSuffix(value, ".")
	case RuleKindIPCIDR:
		if prefix, err := netip.ParsePrefix(value); err == nil {
			prefix = prefix.Masked()
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			value = prefix.Masked().String()
		} else if address, err := netip.ParseAddr(value); err == nil {
			address = address.Unmap()
			value = netip.PrefixFrom(address, address.BitLen()).String()
		}
	case RuleKindGeoSite, RuleKindGeoIP:
		value = strings.ToLower(value)
	}
	return value
}

func poolTargetsByRole(pools []BusinessPoolSpec) map[string]string {
	if pools == nil {
		pools = DefaultBusinessPools()
	}
	out := map[string]string{}
	for _, pool := range pools {
		out[pool.ID] = pool.ID
		if pool.Role != "" {
			out[pool.Role] = pool.ID
		}
	}
	return out
}

func roleByPoolID(pools []BusinessPoolSpec) map[string]string {
	out := map[string]string{}
	for _, pool := range EffectiveBusinessPools(pools) {
		out[pool.ID] = pool.Role
	}
	return out
}

func legacyRuleTarget(group string, roleTargets map[string]string) string {
	switch group {
	case "direct", "block":
		return group
	case "generalproxy":
		if target := roleTargets["general"]; target != "" {
			return target
		}
		return "general"
	default:
		if target := roleTargets[group]; target != "" {
			return target
		}
		return group
	}
}

func resolveRuleTarget(target string, roleTargets map[string]string) string {
	target = normalizeReservedTarget(target)
	if target == "direct" || target == "block" {
		return target
	}
	if strings.EqualFold(target, "proxy") {
		if fallback := roleTargets["general"]; fallback != "" {
			return fallback
		}
		return target
	}
	if mapped := roleTargets[target]; mapped != "" {
		return mapped
	}
	return target
}

func normalizeReservedTarget(target string) string {
	target = strings.TrimSpace(target)
	switch strings.ToLower(target) {
	case "direct":
		return "direct"
	case "block":
		return "block"
	default:
		return target
	}
}

func isDefaultGeoRuleSet(id string) bool {
	switch id {
	case "geosite-category-ads", "geosite-cn", "geoip-cn":
		return true
	default:
		return false
	}
}
