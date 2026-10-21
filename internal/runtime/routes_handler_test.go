package runtime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/planner"
	"smartgate/internal/routes"
	"smartgate/internal/state"
)

func TestRouteExplainHandler(t *testing.T) {
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000"})
	body := mustJSON(t, routes.ExplainRequest{Target: "https://secure.chase.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/routes/explain", bytes.NewReader(body))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("explain status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	explanationBody, _ := json.Marshal(data)
	var explanation routes.Explanation
	if err := json.Unmarshal(explanationBody, &explanation); err != nil {
		t.Fatalf("decode explanation: %v", err)
	}
	if explanation.Decision != routes.DecisionBanking || explanation.PoolID != "banking" {
		t.Fatalf("unexpected explanation: %+v", explanation)
	}
}

func TestRouteExplainUsesStoredRules(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{
		"block": {DomainSuffix: []string{"example.com"}},
	}, spec.BusinessPools)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      lockset.New(),
		Store:      store,
	})
	body := mustJSON(t, routes.ExplainRequest{Target: "https://www.example.com/path"})
	req := httptest.NewRequest(http.MethodPost, "/api/routes/explain", bytes.NewReader(body))
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("explain status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	explanationBody, _ := json.Marshal(data)
	var explanation routes.Explanation
	if err := json.Unmarshal(explanationBody, &explanation); err != nil {
		t.Fatalf("decode explanation: %v", err)
	}
	if explanation.Decision != routes.DecisionBlock || explanation.Reason != "custom_domain_rule" {
		t.Fatalf("unexpected custom explanation: %+v", explanation)
	}
}

func TestRouteExplainUsesAppliedRulesOverDesiredDraft(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	pools := []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "banking", Name: "Banking", Role: "banking", Policy: state.TemplatePoolPolicy("banking")},
	}
	desired := state.DefaultSpecState()
	desired.BusinessPools = pools
	desired.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{
		"block": {DomainSuffix: []string{"example.com"}},
	}, pools)
	if _, err := store.SaveSpec(desired); err != nil {
		t.Fatalf("save desired spec: %v", err)
	}
	appliedSpec := desired
	appliedSpec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{
		"banking": {DomainSuffix: []string{"example.com"}},
	}, pools)
	plan := planner.BuildExecutionGraph(appliedSpec, nil, state.ExecutionGraph{}, time.Now().UTC())
	if !plan.ReadyToApply {
		t.Fatalf("applied graph is not ready: %+v", plan.Issues)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 1, TopologyHash: "route-explain-applied", ExecutionGraph: plan.Graph,
		ProxySelection: map[string]string{"general": "Direct", "banking": "Block"},
	}, appliedSpec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatalf("save applied state: %v", err)
	}
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: lockset.New(), Store: store})
	resp := serve(t, server, http.MethodPost, "/api/routes/explain", mustJSON(t, routes.ExplainRequest{Target: "www.example.com"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("explain status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	explanationBody, _ := json.Marshal(data)
	var explanation routes.Explanation
	if err := json.Unmarshal(explanationBody, &explanation); err != nil {
		t.Fatalf("decode explanation: %v", err)
	}
	if explanation.Decision != routes.DecisionBanking || explanation.PoolID != "banking" || explanation.MatchedRule == nil {
		t.Fatalf("desired draft leaked into applied explanation: %+v", explanation)
	}
}
