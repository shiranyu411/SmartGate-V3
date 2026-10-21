package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"smartgate/internal/lockset"
	"smartgate/internal/state"
)

func TestRulesHandlersSaveNormalizedCatalog(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      lockset.New(),
		Store:      store,
	})

	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	rules := state.DesiredRuleCatalog(spec)
	rules = append(rules,
		state.Rule{ID: "user-openai", Source: state.RuleSourceUser, Kind: state.RuleKindDomainSuffix, Value: "OpenAI.COM.", PoolID: "ai", Enabled: true},
		state.Rule{ID: "user-chatgpt", Source: state.RuleSourceUser, Kind: state.RuleKindDomainKeyword, Value: " ChatGPT ", PoolID: "ai", Enabled: true},
		state.Rule{ID: "user-cloudflare", Source: state.RuleSourceUser, Kind: state.RuleKindIPCIDR, Value: "1.1.1.7/24", PoolID: "ai", Enabled: true},
	)
	resp := serve(t, server, http.MethodPut, "/api/rules", mustJSON(t, rulesPutPayload{Rules: rules}))
	if resp.Code != http.StatusOK {
		t.Fatalf("put rules status = %d body=%s", resp.Code, resp.Body.String())
	}
	putData := decodeEnvelopeData(t, resp.Body.Bytes())
	if putData["rules"] == nil || putData["conflicts"] == nil {
		t.Fatalf("put rules response should contain rules and conflicts, got %+v", putData)
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	byID := map[string]state.Rule{}
	for _, rule := range spec.Rules {
		byID[rule.ID] = rule
	}
	if byID["user-openai"].Value != "openai.com" || byID["user-chatgpt"].Value != "chatgpt" || byID["user-cloudflare"].Value != "1.1.1.0/24" {
		t.Fatalf("rules not normalized: %+v", byID)
	}

	resp = serve(t, server, http.MethodGet, "/api/rules", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get rules status = %d body=%s", resp.Code, resp.Body.String())
	}
	got := decodeEnvelopeAs[rulesView](t, resp.Body.Bytes())
	foundOverride, foundGeo := false, false
	for _, rule := range got.Rules {
		if rule.Source == state.RuleSourceGeoDefault {
			foundGeo = true
		}
		if rule.Source == state.RuleSourceBuiltinV2 && rule.Kind == state.RuleKindDomainSuffix && rule.Value == "openai.com" && rule.OverriddenBy == "user-openai" {
			foundOverride = true
		}
	}
	if !foundOverride || !foundGeo {
		t.Fatalf("rules response missing override or GEO projection: override=%t geo=%t", foundOverride, foundGeo)
	}
	if got.Conflicts == nil {
		t.Fatal("rules GET response must include an empty conflicts array")
	}
}

func TestRulesHandlersAllowSystemRuleToggleButRejectDeletionAndMutation(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *state.Store, state.RuleCatalog, state.Rule) {
		t.Helper()
		layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
		if err := layout.Ensure(); err != nil {
			t.Fatal(err)
		}
		store := state.NewStore(layout)
		server := NewServer(Config{ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: lockset.New(), Store: store})
		spec, err := store.LoadSpec()
		if err != nil {
			t.Fatal(err)
		}
		rules := state.DesiredRuleCatalog(spec)
		for _, rule := range rules {
			if rule.Source != state.RuleSourceUser && rule.Kind == state.RuleKindDomainSuffix {
				return server, store, rules, rule
			}
		}
		t.Fatal("system domain rule not found")
		return nil, nil, nil, state.Rule{}
	}

	t.Run("toggle", func(t *testing.T) {
		server, store, rules, systemRule := newServer(t)
		for index := range rules {
			if rules[index].ID == systemRule.ID {
				rules[index].Enabled = false
			}
		}
		resp := serve(t, server, http.MethodPut, "/api/rules", mustJSON(t, rulesPutPayload{Rules: rules}))
		if resp.Code != http.StatusOK {
			t.Fatalf("disable system rule status=%d body=%s", resp.Code, resp.Body.String())
		}
		spec, err := store.LoadSpec()
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range spec.Rules {
			if rule.ID == systemRule.ID {
				if rule.Enabled {
					t.Fatalf("system rule toggle was not persisted: %+v", rule)
				}
				return
			}
		}
		t.Fatalf("system rule %s disappeared", systemRule.ID)
	})

	t.Run("delete", func(t *testing.T) {
		server, _, rules, systemRule := newServer(t)
		filtered := make(state.RuleCatalog, 0, len(rules)-1)
		for _, rule := range rules {
			if rule.ID != systemRule.ID {
				filtered = append(filtered, rule)
			}
		}
		resp := serve(t, server, http.MethodPut, "/api/rules", mustJSON(t, rulesPutPayload{Rules: filtered}))
		if resp.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Body.String(), "cannot be deleted") {
			t.Fatalf("system rule deletion status=%d body=%s", resp.Code, resp.Body.String())
		}
	})

	t.Run("mutate", func(t *testing.T) {
		server, _, rules, systemRule := newServer(t)
		for index := range rules {
			if rules[index].ID == systemRule.ID {
				rules[index].PoolID = "block"
				if systemRule.PoolID == "block" {
					rules[index].PoolID = "direct"
				}
			}
		}
		resp := serve(t, server, http.MethodPut, "/api/rules", mustJSON(t, rulesPutPayload{Rules: rules}))
		if resp.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Body.String(), "immutable") {
			t.Fatalf("system rule mutation status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
}

func TestRulesHandlersAllowConflictThenResolveIdempotently(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: lockset.New(), Store: store})
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	rules := state.DesiredRuleCatalog(spec)
	rules = append(rules,
		state.Rule{ID: "user-shared-ai", Source: state.RuleSourceUser, Kind: state.RuleKindDomainSuffix, Value: "shared.example", PoolID: "ai", Enabled: true},
		state.Rule{ID: "user-shared-streaming", Source: state.RuleSourceUser, Kind: state.RuleKindDomainSuffix, Value: "shared.example", PoolID: "streaming", Enabled: true},
	)
	resp := serve(t, server, http.MethodPut, "/api/rules", mustJSON(t, rulesPutPayload{Rules: rules}))
	if resp.Code != http.StatusOK {
		t.Fatalf("conflicting draft should save, status=%d body=%s", resp.Code, resp.Body.String())
	}
	view := decodeEnvelopeAs[rulesView](t, resp.Body.Bytes())
	if len(view.Conflicts) != 1 || view.Conflicts[0].RuleKey != state.RuleKey(state.RuleKindDomainSuffix, "shared.example") {
		t.Fatalf("unexpected conflicts: %+v", view.Conflicts)
	}
	resp = serve(t, server, http.MethodGet, "/api/rules/conflicts", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get conflicts status=%d body=%s", resp.Code, resp.Body.String())
	}
	conflicts := decodeEnvelopeAs[[]state.RuleConflict](t, resp.Body.Bytes())
	if len(conflicts) != 1 {
		t.Fatalf("conflict was not persisted: %+v", conflicts)
	}
	payload := ruleConflictResolvePayload{RuleKey: conflicts[0].RuleKey, KeepPool: "ai", OpID: "rule-resolve-001"}
	invalid := payload
	invalid.KeepPool = "general"
	invalid.OpID = "rule-resolve-invalid"
	resp = serve(t, server, http.MethodPost, "/api/rules/resolve-conflict", mustJSON(t, invalid))
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-owner keep_pool should fail, status=%d body=%s", resp.Code, resp.Body.String())
	}
	stillConflicting := decodeEnvelopeAs[[]state.RuleConflict](t, serve(t, server, http.MethodGet, "/api/rules/conflicts", nil).Body.Bytes())
	if len(stillConflicting) != 1 {
		t.Fatalf("invalid resolution changed the conflict: %+v", stillConflicting)
	}
	for attempt := 0; attempt < 2; attempt++ {
		resp = serve(t, server, http.MethodPost, "/api/rules/resolve-conflict", mustJSON(t, payload))
		if resp.Code != http.StatusOK {
			t.Fatalf("resolve attempt %d status=%d body=%s", attempt, resp.Code, resp.Body.String())
		}
		view = decodeEnvelopeAs[rulesView](t, resp.Body.Bytes())
		if len(view.Conflicts) != 0 {
			t.Fatalf("resolve attempt %d left conflicts: %+v", attempt, view.Conflicts)
		}
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	kept, removed := false, true
	for _, rule := range spec.Rules {
		if rule.ID == "user-shared-ai" {
			kept = true
		}
		if rule.ID == "user-shared-streaming" {
			removed = false
		}
	}
	if !kept || !removed {
		t.Fatalf("resolution did not retain only keep_pool rule: %+v", spec.Rules)
	}
}

func decodeEnvelopeAs[T any](t *testing.T, body []byte) T {
	t.Helper()
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, body)
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env)
	}
	encoded, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("encode envelope data: %v", err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode envelope data: %v data=%s", err, encoded)
	}
	return out
}

func TestRuleSetHandlersSaveRemoteRulesWithGuard(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      lockset.New(),
		Store:      store,
	})

	resp := serve(t, server, http.MethodGet, "/api/rule-sets", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get rule-sets status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["sets"] == nil || data["memory_guard"] == nil || data["ad_block"] == nil {
		t.Fatalf("rule-sets response should include defaults and guard: %+v", data)
	}

	payload := state.DefaultRemoteRuleSettings()
	payload.AdBlock.Enabled = true
	resp = serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, payload))
	if resp.Code != http.StatusOK {
		t.Fatalf("put rule-sets status = %d body=%s", resp.Code, resp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if !spec.RemoteRules.AdBlock.Enabled {
		t.Fatalf("ad block setting not persisted: %+v", spec.RemoteRules)
	}

	resp = serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, map[string]any{"enabled": false}))
	if resp.Code != http.StatusOK {
		t.Fatalf("disable rule-sets status = %d body=%s", resp.Code, resp.Body.String())
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after disable: %v", err)
	}
	if spec.RemoteRules.Enabled {
		t.Fatalf("explicit enabled=false should be persisted: %+v", spec.RemoteRules)
	}

	payload.AdBlock.Enabled = true
	for i := range payload.Sets {
		if payload.Sets[i].Kind == "adblock" {
			// B6 three-state gate: hard limits only count sets that are actually effective.
			payload.Sets[i].Enabled = true
			payload.Sets[i].EstimatedBytes = payload.MemoryGuard.MaxSetBytes + 1
		}
	}
	resp = serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, payload))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("oversized rule-set should be rejected, status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestRuleSetHandlersAllowLocalRuleSetPaths(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Layout:       layout,
		Locks:        lockset.New(),
		Store:        store,
		CoreRegistry: newRuntimeTestCoreRegistry(t),
	})

	payload := state.DefaultRemoteRuleSettings()
	payload.Sets = append(payload.Sets, state.RemoteRuleSet{
		ID:      "geoip-cn-local",
		Name:    "GeoIP CN Local",
		Enabled: true,
		Kind:    "geo",
		Target:  "direct",
		Format:  "binary",
		Type:    "local",
		Path:    "/etc/runtime-test-core/rule-set/geoip-cn.srs",
	})
	resp := serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, payload))
	if resp.Code != http.StatusOK {
		t.Fatalf("local rule-set should save, status = %d body=%s", resp.Code, resp.Body.String())
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	var local state.RemoteRuleSet
	for _, item := range spec.RemoteRules.Sets {
		if item.ID == "geoip-cn-local" {
			local = item
			break
		}
	}
	if local.Type != "local" || local.Path != "/etc/runtime-test-core/rule-set/geoip-cn.srs" || local.URL != "" {
		t.Fatalf("local rule-set not normalized correctly: %+v", local)
	}
}

func TestRuleSetHandlersRejectLocalRuleSetPathsWithoutCoreRegistry(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      lockset.New(),
		Store:      store,
	})

	payload := state.DefaultRemoteRuleSettings()
	payload.Sets = append(payload.Sets, state.RemoteRuleSet{
		ID:      "geoip-cn-local",
		Name:    "GeoIP CN Local",
		Enabled: true,
		Kind:    "geo",
		Target:  "direct",
		Format:  "binary",
		Type:    "local",
		Path:    "/etc/runtime-test-core/rule-set/geoip-cn.srs",
	})
	resp := serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, payload))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "core registry is required") {
		t.Fatalf("local rule-set without registry status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestRuleSetHandlersSkipGuardWhenRemoteRulesDisabled(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      lockset.New(),
		Store:      store,
	})

	payload := state.DefaultRemoteRuleSettings()
	payload.Enabled = false
	payload.AdBlock.Enabled = true
	for i := range payload.Sets {
		payload.Sets[i].EstimatedBytes = payload.MemoryGuard.MaxSetBytes + 1
	}
	resp := serve(t, server, http.MethodPut, "/api/rule-sets", mustJSON(t, payload))
	if resp.Code != http.StatusOK {
		t.Fatalf("disabled oversized rule-sets should save, status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["warnings"] != nil {
		t.Fatalf("disabled rule-sets should not warn: %+v", data)
	}
}
