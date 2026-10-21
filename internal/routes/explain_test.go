package routes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smartgate/internal/fabric"
	"smartgate/internal/state"
)

func TestExplainBusinessDomains(t *testing.T) {
	cases := []struct {
		target   string
		decision Decision
		poolID   string
	}{
		{"secure.chase.com", DecisionBanking, "banking"},
		{"login.gov", DecisionBanking, "banking"},
		{"api.coinbase.com", DecisionCrypto, "crypto"},
		{"walletconnect.com", DecisionCrypto, "crypto"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			got := Explain(tc.target)
			if got.Decision != tc.decision || got.PoolID != tc.poolID || got.Matched == "" {
				t.Fatalf("Explain(%q) = %+v", tc.target, got)
			}
		})
	}
}

func TestExplainLANGeoAndDefaultProxy(t *testing.T) {
	cases := []struct {
		target   string
		decision Decision
		poolID   string
	}{
		{"192.168.1.1", DecisionLANBypass, ""},
		{"127.0.0.1", DecisionLANBypass, ""},
		{"example.cn", DecisionProxy, "general"},
		{"example.com", DecisionProxy, "general"},
		{"8.8.8.8", DecisionProxy, "general"},
		{"", DecisionBlock, ""},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			got := Explain(tc.target)
			if got.Decision != tc.decision || got.PoolID != tc.poolID {
				t.Fatalf("Explain(%q) = %+v", tc.target, got)
			}
		})
	}
}

func TestExplainWithGraphUsesPinnedGeoMatcher(t *testing.T) {
	matcher := fabric.RuleSetMatcher{SchemaVersion: 1, SourceVersion: 3, DomainExact: []string{"example.cn"}}
	matcherBody, err := json.Marshal(matcher)
	if err != nil {
		t.Fatal(err)
	}
	layout := state.Layout{DataDir: filepath.Join(t.TempDir(), "data"), RuntimeDir: filepath.Join(t.TempDir(), "run")}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	matcherSum := sha256.Sum256(matcherBody)
	matcherSHA := hex.EncodeToString(matcherSum[:])
	matcherPath := filepath.Join(layout.RuleSetMatcherDir(), matcherSHA+".json")
	if err := os.WriteFile(matcherPath, matcherBody, 0o600); err != nil {
		t.Fatal(err)
	}
	contentBody := []byte(`{"version":3,"rules":[{"domain":"example.cn"}]}`)
	contentSum := sha256.Sum256(contentBody)
	contentSHA := hex.EncodeToString(contentSum[:])
	contentPath := filepath.Join(layout.RuleSetContentDir(), contentSHA+".json")
	if err := os.WriteFile(contentPath, contentBody, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256([]byte("geosite-cn-test-source"))
	graph := state.ExecutionGraph{Fabric: state.FabricGraph{
		Rules: []state.FabricRuleGraph{{
			ID: "geo-default-geosite-cn", Source: state.RuleSourceGeoDefault,
			Kind: state.RuleKindGeoSite, Value: "geosite-cn", Target: "direct", Priority: 600,
		}},
		RuleSets: []state.RuleSetSnapshotRef{{
			ID: "geosite-cn", SourceFingerprint: hex.EncodeToString(fingerprint[:]),
			ContentSHA256: contentSHA, MatcherSHA256: matcherSHA,
			ContentPath: contentPath, MatcherPath: matcherPath,
			Format: "source", SourceType: "local", Size: int64(len(contentBody)),
			FetchedAt: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
			Stats:     state.RuleSetSnapshotStats{DomainExact: 1},
		}},
		FallbackPool: "general",
		Dispatch:     []state.PoolDispatchGraph{{Pool: "general"}},
	}}
	explanation, err := ExplainWithGraph("https://example.cn/path", graph, state.DefaultBusinessPools())
	if err != nil {
		t.Fatal(err)
	}
	if explanation.Decision != DecisionDirect || explanation.PoolID != "direct" || explanation.Reason != "geo_domain_rule" {
		t.Fatalf("unexpected GEO explanation: %+v", explanation)
	}
	if explanation.MatchedRule == nil || explanation.MatchedRule.ID != "geo-default-geosite-cn" || explanation.MatchedRule.Source != state.RuleSourceGeoDefault {
		t.Fatalf("missing GEO matched rule: %+v", explanation)
	}
}

func TestExplainDoesNotUseLocalAdBlockByDefault(t *testing.T) {
	got := Explain("ads.doubleclick.net")
	if got.Decision == DecisionBlock {
		t.Fatalf("local ad block should not be enabled by default: %+v", got)
	}
}
