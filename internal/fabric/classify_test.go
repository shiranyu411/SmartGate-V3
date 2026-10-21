package fabric

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"smartgate/internal/state"
)

func TestClassifierUsesPriorityAndSpecificity(t *testing.T) {
	classifier, err := NewClassifier([]Rule{
		{ID: "fallback-suffix", Source: "builtin", Kind: RuleDomainSuffix, Value: "example.com", PoolID: "general-pool", Enabled: true, Priority: 100},
		{ID: "specific", Source: "user", Kind: RuleDomainSuffix, Value: "api.example.com", PoolID: "custom-ai", Enabled: true, Priority: 500},
		{ID: "cidr-wide", Kind: RuleIPCIDR, Value: "203.0.113.0/24", PoolID: "general-pool", Enabled: true, Priority: 100},
		{ID: "cidr-specific", Kind: RuleIPCIDR, Value: "203.0.113.8/32", PoolID: "custom-ai", Enabled: true, Priority: 100},
	}, "general-pool", []string{"general-pool", "custom-ai"})
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.ClassifyDomain("API.Example.COM."); got.Target != "custom-ai" || got.RuleID != "specific" {
		t.Fatalf("unexpected domain match: %+v", got)
	}
	if got := classifier.ClassifyIP(netip.MustParseAddr("203.0.113.8")); got.Target != "custom-ai" {
		t.Fatalf("unexpected IP match: %+v", got)
	}
	if got := classifier.ClassifyDomain("unmatched.invalid"); got.Target != "general-pool" || !got.Fallback {
		t.Fatalf("unexpected fallback: %+v", got)
	}
}

func TestClassifierUsesPriorityAcrossDomainMatcherKinds(t *testing.T) {
	matcher := RuleSetMatcher{
		SchemaVersion: ruleSetMatcherSchemaVersion,
		SourceVersion: 3,
		DomainExact:   []string{"api.example.com"},
	}
	body, err := json.Marshal(matcher)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "matcher.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	classifier, err := newClassifier([]Rule{
		{ID: "geo-exact", Source: state.RuleSourceGeoDefault, Kind: RuleGeoSite, Value: "geo-test", PoolID: "direct", Enabled: true, Priority: 600},
		{ID: "user-suffix", Source: state.RuleSourceUser, Kind: RuleDomainSuffix, Value: "example.com", PoolID: "custom-ai", Enabled: true, Priority: 1000},
	}, "general-pool", []string{"general-pool", "custom-ai"}, map[string]RuleSetMatcher{"geo-test": matcher})
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.ClassifyDomain("api.example.com"); got.RuleID != "user-suffix" || got.Target != "custom-ai" {
		t.Fatalf("higher-priority suffix did not override lower-priority GEO exact match: %+v", got)
	}
}

func TestValidateRulesRejectsMultiplePoolOwnership(t *testing.T) {
	conflicts, err := ValidateRules([]Rule{
		{ID: "one", Kind: RuleDomainSuffix, Value: "example.com", PoolID: "one", Enabled: true},
		{ID: "two", Kind: RuleDomainSuffix, Value: "EXAMPLE.COM.", PoolID: "two", Enabled: true},
	}, []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || len(conflicts[0].Pools) != 2 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if _, err := NewClassifier([]Rule{
		{ID: "one", Kind: RuleDomainSuffix, Value: "example.com", PoolID: "one", Enabled: true},
		{ID: "two", Kind: RuleDomainSuffix, Value: "example.com", PoolID: "two", Enabled: true},
	}, "one", []string{"one", "two"}); err == nil {
		t.Fatal("expected classifier construction to reject conflict")
	}
}

func TestClassifierFromSpecUsesRolesNotDisplayNames(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "fallback-42", Name: "Not General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "workloads", Name: "Anything", Role: "ai", Policy: state.TemplatePoolPolicy("ai")},
	}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{
		"generalproxy": {DomainSuffix: []string{"ordinary.example"}},
		"ai":           {DomainSuffix: []string{"model.example"}},
	}, spec.BusinessPools)
	classifier, err := ClassifierFromSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.ClassifyDomain("model.example"); got.Target != "workloads" {
		t.Fatalf("role mapping used a hard-coded pool id: %+v", got)
	}
	if got := classifier.ClassifyDomain("other.example"); got.Target != "fallback-42" {
		t.Fatalf("fallback did not use general role: %+v", got)
	}
}
