package fabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestParseRuleSetSourceNormalizesSupportedMatchers(t *testing.T) {
	body := []byte(`{
  "version": 1,
  "rules": [{
    "domain": ["Example.COM.", "example.com"],
    "domain_suffix": [".Sub.Example.COM.", "example.net"],
    "domain_keyword": [" ChatGPT ", "chatgpt"],
    "domain_regex": "^api\\.example\\.org$",
    "ip_cidr": ["203.0.113.42/24", "2001:db8::1/32"]
  }]
}`)

	matcher, err := parseRuleSetSource(body, 32, 256)
	if err != nil {
		t.Fatal(err)
	}
	if matcher.SchemaVersion != ruleSetMatcherSchemaVersion || matcher.SourceVersion != 1 {
		t.Fatalf("unexpected matcher versions: %+v", matcher)
	}
	assertStringsEqual(t, matcher.DomainExact, []string{"example.com"})
	assertStringsEqual(t, matcher.DomainSuffix, []string{".sub.example.com", "example.net"})
	assertStringsEqual(t, matcher.DomainKeyword, []string{"chatgpt"})
	assertStringsEqual(t, matcher.DomainRegex, []string{`^api\.example\.org$`})
	assertStringsEqual(t, matcher.IPCIDR, []string{"203.0.113.0/24", "2001:db8::/32"})
}

func TestRuleSetResolveAdBlockIgnoresLegacySetList(t *testing.T) {
	store := newRuleSetTestStore(t)
	path := filepath.Join(t.TempDir(), "ads.json")
	if err := os.WriteFile(path, []byte(`{"version":3,"rules":[{"domain":"ads.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := state.DefaultRemoteRuleSettings()
	settings.AdBlock.Enabled = true
	settings.AdBlock.Sets = []string{"unrelated-legacy-id"}
	for i := range settings.Sets {
		if settings.Sets[i].ID == "geosite-category-ads" {
			settings.Sets[i].Enabled = true
			settings.Sets[i].Type = "local"
			settings.Sets[i].Format = "source"
			settings.Sets[i].Path = path
		}
	}
	graph := state.FabricGraph{Rules: []state.FabricRuleGraph{{ID: "ads", Kind: RuleGeoSite, Value: "geosite-category-ads"}}}
	resolved, _, err := store.Resolve(context.Background(), state.SpecState{RemoteRules: settings}, graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.RuleSets) != 1 || resolved.RuleSets[0].ID != "geosite-category-ads" {
		t.Fatalf("adblock matcher was not pinned: %+v", resolved.RuleSets)
	}
	matcher, err := store.validateSnapshot(resolved.RuleSets[0])
	if err != nil {
		t.Fatal(err)
	}
	assertStringsEqual(t, matcher.DomainExact, []string{"ads.example"})
}

func TestParseRuleSetSourceRejectsUnsupportedOrUnboundedInput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		maxEntries int
		maxString  int
	}{
		{name: "unknown envelope field", body: `{"version":1,"rules":[],"extra":true}`, maxEntries: 8, maxString: 64},
		{name: "unknown rule field", body: `{"version":1,"rules":[{"domain":"example.com","process_name":"curl"}]}`, maxEntries: 8, maxString: 64},
		{name: "unsupported type", body: `{"version":1,"rules":[{"type":"logical","domain":"example.com"}]}`, maxEntries: 8, maxString: 64},
		{name: "invert", body: `{"version":1,"rules":[{"domain":"example.com","invert":true}]}`, maxEntries: 8, maxString: 64},
		{name: "invalid regex", body: `{"version":1,"rules":[{"domain_regex":"("}]}`, maxEntries: 8, maxString: 64},
		{name: "invalid cidr", body: `{"version":1,"rules":[{"ip_cidr":"203.0.113.999/24"}]}`, maxEntries: 8, maxString: 64},
		{name: "empty", body: `{"version":1,"rules":[{}]}`, maxEntries: 8, maxString: 64},
		{name: "entry limit", body: `{"version":1,"rules":[{"domain":["a.example","b.example"]}]}`, maxEntries: 1, maxString: 64},
		{name: "rule limit", body: `{"version":1,"rules":[{"domain":"a.example"},{"domain":"b.example"}]}`, maxEntries: 1, maxString: 64},
		{name: "string limit", body: `{"version":1,"rules":[{"domain":"long.example"}]}`, maxEntries: 8, maxString: 4},
		{name: "domain whitespace", body: "{\"version\":1,\"rules\":[{\"domain\":\"bad\\n.example\"}]}", maxEntries: 8, maxString: 64},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseRuleSetSource([]byte(test.body), test.maxEntries, test.maxString); err == nil {
				t.Fatal("expected rule-set source to be rejected")
			}
		})
	}
}

func TestRuleSetSnapshotRejectsTamperedReferencesAndSymlinks(t *testing.T) {
	store := newRuleSetTestStore(t)
	ref := createSourceSnapshot(t, store, "snapshot", `{"version":1,"rules":[{"domain":"example.com"}]}`)

	tests := []struct {
		name   string
		mutate func(state.RuleSetSnapshotRef) state.RuleSetSnapshotRef
	}{
		{name: "negative size", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef { next.Size = -1; return next }},
		{name: "oversized size", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef {
			next.Size = defaultRuleSetContentLimit + 1
			return next
		}},
		{name: "zero timestamp", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef {
			next.FetchedAt = time.Time{}
			return next
		}},
		{name: "invalid fingerprint", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef {
			next.SourceFingerprint = strings.ToUpper(next.SourceFingerprint)
			return next
		}},
		{name: "content digest filename", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef {
			next.ContentSHA256 = strings.Repeat("0", 64)
			return next
		}},
		{name: "matcher escape", mutate: func(next state.RuleSetSnapshotRef) state.RuleSetSnapshotRef {
			next.MatcherPath = filepath.Join(filepath.Dir(store.Layout.RuleSetCacheDir()), filepath.Base(next.MatcherPath))
			return next
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.validateSnapshot(test.mutate(ref)); err == nil {
				t.Fatal("expected tampered snapshot to be rejected")
			}
		})
	}

	t.Run("content symlink", func(t *testing.T) {
		linkStore := newRuleSetTestStore(t)
		linkRef := createSourceSnapshot(t, linkStore, "content-link", `{"version":1,"rules":[{"domain":"example.com"}]}`)
		realPath := linkRef.ContentPath + ".real"
		if err := os.Rename(linkRef.ContentPath, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, linkRef.ContentPath); err != nil {
			t.Fatal(err)
		}
		if _, err := linkStore.validateSnapshot(linkRef); err == nil {
			t.Fatal("expected content symlink to be rejected")
		}
	})

	t.Run("matcher symlink", func(t *testing.T) {
		linkStore := newRuleSetTestStore(t)
		linkRef := createSourceSnapshot(t, linkStore, "matcher-link", `{"version":1,"rules":[{"domain":"example.com"}]}`)
		realPath := linkRef.MatcherPath + ".real"
		if err := os.Rename(linkRef.MatcherPath, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, linkRef.MatcherPath); err != nil {
			t.Fatal(err)
		}
		if _, err := linkStore.validateSnapshot(linkRef); err == nil {
			t.Fatal("expected matcher symlink to be rejected")
		}
	})
}

func TestLoadRuleSetMatcherRejectsSemanticTampering(t *testing.T) {
	store := newRuleSetTestStore(t)
	base := createSourceSnapshot(t, store, "matcher", `{"version":1,"rules":[{"domain":"example.com"}]}`)
	tests := []struct {
		name       string
		matcher    RuleSetMatcher
		maxEntries int
	}{
		{name: "noncanonical exact", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, DomainExact: []string{"Example.COM"}}, maxEntries: 8},
		{name: "invalid regex", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, DomainRegex: []string{"("}}, maxEntries: 8},
		{name: "noncanonical cidr", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, IPCIDR: []string{"203.0.113.42/24"}}, maxEntries: 8},
		{name: "duplicates", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, DomainExact: []string{"example.com", "example.com"}}, maxEntries: 8},
		{name: "unsorted", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, DomainExact: []string{"z.example", "a.example"}}, maxEntries: 8},
		{name: "entry limit", matcher: RuleSetMatcher{SchemaVersion: 1, SourceVersion: 1, DomainExact: []string{"a.example", "b.example"}}, maxEntries: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := writeMatcherForReference(t, store, base, test.matcher)
			if _, err := loadRuleSetMatcher(ref, defaultRuleSetJSONLimit, test.maxEntries, defaultRuleSetStringLimit); err == nil {
				t.Fatal("expected tampered matcher to be rejected")
			}
		})
	}
}

func TestRuleSetSourceRecordCanAdvanceToNewContent(t *testing.T) {
	store := newRuleSetTestStore(t)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	sourcePath := filepath.Join(t.TempDir(), "rules.json")
	item := state.RemoteRuleSet{ID: "mutable-source", Type: "local", Format: "source", Path: sourcePath}
	settings := state.DefaultRemoteRuleSettings()
	fingerprint := ruleSetSourceFingerprint(item, settings)

	if err := os.WriteFile(sourcePath, []byte(`{"version":1,"rules":[{"domain":"one.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.createSnapshot(context.Background(), settings, item, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := os.WriteFile(sourcePath, []byte(`{"version":1,"rules":[{"domain":"two.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.createSnapshot(context.Background(), settings, item, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentSHA256 == second.ContentSHA256 {
		t.Fatal("expected refreshed source content to produce a new snapshot")
	}
	cached, err := store.loadSourceRecord(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if cached.ContentSHA256 != second.ContentSHA256 || !cached.FetchedAt.Equal(second.FetchedAt) {
		t.Fatalf("source record did not advance: got %+v want %+v", cached, second)
	}
}

func TestRuleSetResolveReusesFreshCacheAndFallsBackAfterRefreshFailure(t *testing.T) {
	store := newRuleSetTestStore(t)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	body := []byte(`{"version":1,"rules":[{"domain_suffix":"example.com"}]}`)
	requests := 0
	fail := false
	store.HTTPClientFactory = func(string) (*http.Client, error) {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			if fail {
				return nil, errors.New("offline")
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          io.NopCloser(bytes.NewReader(body)),
				ContentLength: int64(len(body)),
			}, nil
		})}, nil
	}
	settings := state.DefaultRemoteRuleSettings()
	settings.DownloadDetour = "Direct"
	item := state.RemoteRuleSet{
		ID: "remote-cache", Enabled: true, Type: "remote", Format: "source",
		URL: "https://rules.example/cache.json", DownloadDetour: "Direct", UpdateInterval: "24h",
	}

	first, warning, err := store.resolveOne(context.Background(), settings, item, state.RuleSetSnapshotRef{}, false)
	if err != nil || warning != "" {
		t.Fatalf("initial resolve failed: warning=%q err=%v", warning, err)
	}
	now = now.Add(time.Hour)
	second, warning, err := store.resolveOne(context.Background(), settings, item, state.RuleSetSnapshotRef{}, false)
	if err != nil || warning != "" {
		t.Fatalf("fresh cache resolve failed: warning=%q err=%v", warning, err)
	}
	if requests != 1 || second.ContentSHA256 != first.ContentSHA256 {
		t.Fatalf("fresh cache was not reused: requests=%d first=%+v second=%+v", requests, first, second)
	}

	now = now.Add(25 * time.Hour)
	fail = true
	third, warning, err := store.resolveOne(context.Background(), settings, item, first, false)
	if err != nil {
		t.Fatal(err)
	}
	if warning == "" || third.ContentSHA256 != first.ContentSHA256 || requests != 2 {
		t.Fatalf("stale verified snapshot was not reused: warning=%q requests=%d third=%+v", warning, requests, third)
	}
}

func TestShellRuleSetDecompilerFeedsBinarySnapshot(t *testing.T) {
	store := newRuleSetTestStore(t)
	fixturePath := filepath.Join(t.TempDir(), "decompiled.json")
	if err := os.WriteFile(fixturePath, []byte(`{"version":1,"rules":[{"domain":"binary.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SMARTGATE_TEST_RULESET_JSON", fixturePath)
	scriptPath := filepath.Join(t.TempDir(), "fake-sing-box")
	script := "#!/bin/sh\nset -eu\n[ \"$1\" = rule-set ]\n[ \"$2\" = decompile ]\n[ \"$3\" = --output ]\ncp \"$SMARTGATE_TEST_RULESET_JSON\" \"$4\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store.Decompiler = ShellRuleSetDecompiler{Binary: scriptPath}
	binaryPath := filepath.Join(t.TempDir(), "rules.srs")
	if err := os.WriteFile(binaryPath, []byte("test-binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := state.DefaultRemoteRuleSettings()
	item := state.RemoteRuleSet{ID: "binary", Type: "local", Format: "binary", Path: binaryPath}
	ref, err := store.createSnapshot(context.Background(), settings, item, ruleSetSourceFingerprint(item, settings))
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := store.validateSnapshot(ref)
	if err != nil {
		t.Fatal(err)
	}
	assertStringsEqual(t, matcher.DomainExact, []string{"binary.example"})
}

func TestRealRuleSetFixturesWhenAvailable(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		binary = "/opt/homebrew/bin/sing-box"
		if _, statErr := os.Stat(binary); statErr != nil {
			t.Skip("sing-box is not available")
		}
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		kind string
	}{
		{name: "geosite-cn", path: filepath.Join(repositoryRoot, ".tmp", "geo-inspect", "geosite-cn.srs"), kind: RuleGeoSite},
		{name: "geoip-cn", path: filepath.Join(repositoryRoot, ".tmp", "geo-inspect", "geoip-cn.srs"), kind: RuleGeoIP},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := os.Stat(test.path); err != nil {
				t.Skip("local fixture is not available")
			}
			store := newRuleSetTestStore(t)
			store.Decompiler = ShellRuleSetDecompiler{Binary: binary}
			settings := state.DefaultRemoteRuleSettings()
			item := state.RemoteRuleSet{ID: test.name, Type: "local", Format: "binary", Path: test.path}
			ref, err := store.createSnapshot(context.Background(), settings, item, ruleSetSourceFingerprint(item, settings))
			if err != nil {
				t.Fatal(err)
			}
			matcher, err := store.validateSnapshot(ref)
			if err != nil {
				t.Fatal(err)
			}
			if test.kind == RuleGeoSite && len(matcher.DomainExact)+len(matcher.DomainSuffix)+len(matcher.DomainKeyword)+len(matcher.DomainRegex) == 0 {
				t.Fatal("real geosite fixture produced no domain matchers")
			}
			if test.kind == RuleGeoIP && len(matcher.IPCIDR) == 0 {
				t.Fatal("real geoip fixture produced no IP prefixes")
			}
		})
	}
}

func TestDispatchRejectsTamperedGeoIPDefinition(t *testing.T) {
	store := newRuleSetTestStore(t)
	ref := createSourceSnapshot(t, store, "geoip-test", `{"version":1,"rules":[{"ip_cidr":["203.0.113.0/24","2001:db8::/32"]}]}`)
	owners := netgate.NewOwnerRegistry("smartgate")
	rule := state.FabricRuleGraph{ID: "geo-rule", Kind: RuleGeoIP, Value: ref.ID, Target: "general", Priority: 100, IPSets: geoIPSetNames("geo-rule")}
	matcher, err := LoadRuleSetMatcher(ref, defaultRuleSetJSONLimit)
	if err != nil {
		t.Fatal(err)
	}
	ipv4, ipv6 := splitRuleSetPrefixes(matcher.IPCIDR)
	var definition strings.Builder
	owner := nftQuote(owners.Comment(netgate.KindFabricGeoSet, rule.ID))
	writeStaticPrefixSet(&definition, rule.IPSets.IPv4, "ipv4_addr", ipv4, owner)
	writeStaticPrefixSet(&definition, rule.IPSets.IPv6, "ipv6_addr", ipv6, owner)
	body := []byte(definition.String())
	rule.IPSets.DefinitionSHA256 = sha256Hex(body)
	rule.IPSets.DefinitionPath = filepath.Join(store.Layout.RuleSetNFTDir(), rule.IPSets.DefinitionSHA256+".nft")
	if err := writeRuleSetFile(rule.IPSets.DefinitionPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	graph := geoDispatchGraph(ref, rule)
	if _, err := BuildDispatchDryRunPlan("geo-valid", owners, graph, DispatchPlanOptions{LANDevice: "br-lan"}); err != nil {
		t.Fatal(err)
	}

	t.Run("content injection", func(t *testing.T) {
		injected := rule
		injectedBody := []byte("  set injected { type ipv4_addr; }\n")
		injected.IPSets = cloneIPSets(rule.IPSets)
		injected.IPSets.DefinitionSHA256 = sha256Hex(injectedBody)
		injected.IPSets.DefinitionPath = filepath.Join(store.Layout.RuleSetNFTDir(), injected.IPSets.DefinitionSHA256+".nft")
		if err := writeRuleSetFile(injected.IPSets.DefinitionPath, injectedBody, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildDispatchDryRunPlan("geo-injected", owners, geoDispatchGraph(ref, injected), DispatchPlanOptions{LANDevice: "br-lan"}); err == nil {
			t.Fatal("expected injected nft definition to be rejected")
		}
	})

	t.Run("path escape", func(t *testing.T) {
		escaped := rule
		escaped.IPSets = cloneIPSets(rule.IPSets)
		escaped.IPSets.DefinitionPath = filepath.Join(t.TempDir(), escaped.IPSets.DefinitionSHA256+".nft")
		if err := os.WriteFile(escaped.IPSets.DefinitionPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildDispatchDryRunPlan("geo-escaped", owners, geoDispatchGraph(ref, escaped), DispatchPlanOptions{LANDevice: "br-lan"}); err == nil {
			t.Fatal("expected escaped nft definition to be rejected")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		symlinkRule := rule
		symlinkRule.IPSets = cloneIPSets(rule.IPSets)
		realPath := symlinkRule.IPSets.DefinitionPath + ".real"
		if err := os.Rename(symlinkRule.IPSets.DefinitionPath, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, symlinkRule.IPSets.DefinitionPath); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildDispatchDryRunPlan("geo-symlink", owners, geoDispatchGraph(ref, symlinkRule), DispatchPlanOptions{LANDevice: "br-lan"}); err == nil {
			t.Fatal("expected nft symlink to be rejected")
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newRuleSetTestStore(t *testing.T) *RuleSetStore {
	t.Helper()
	root := t.TempDir()
	layout := state.Layout{DataDir: filepath.Join(root, "data"), RuntimeDir: filepath.Join(root, "run")}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	return NewRuleSetStore(layout)
}

func createSourceSnapshot(t *testing.T, store *RuleSetStore, id, source string) state.RuleSetSnapshotRef {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), id+".json")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := state.DefaultRemoteRuleSettings()
	item := state.RemoteRuleSet{ID: id, Type: "local", Format: "source", Path: sourcePath}
	ref, err := store.createSnapshot(context.Background(), settings, item, ruleSetSourceFingerprint(item, settings))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func writeMatcherForReference(t *testing.T, store *RuleSetStore, ref state.RuleSetSnapshotRef, matcher RuleSetMatcher) state.RuleSetSnapshotRef {
	t.Helper()
	body, err := json.Marshal(matcher)
	if err != nil {
		t.Fatal(err)
	}
	ref.MatcherSHA256 = sha256Hex(body)
	ref.MatcherPath = filepath.Join(store.Layout.RuleSetMatcherDir(), ref.MatcherSHA256+".json")
	ref.Stats = matcherStats(matcher)
	if err := os.WriteFile(ref.MatcherPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return ref
}

func geoDispatchGraph(ref state.RuleSetSnapshotRef, rule state.FabricRuleGraph) state.FabricGraph {
	return state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: state.FabricDNSGraph{
			Listen: "127.0.0.1:1053", RedirectLAN53: true, RejectHTTPSRecords: true, BlockDoT853: true,
			PoolSets: map[string]state.FabricPoolSets{
				TargetDirect: {IPv4: "sg_direct_4", IPv6: "sg_direct_6"},
				TargetBlock:  {IPv4: "sg_block_4", IPv6: "sg_block_6"},
				"general":    {IPv4: "sg_general_4", IPv6: "sg_general_6"},
			},
		},
		Rules:        []state.FabricRuleGraph{rule},
		RuleSets:     []state.RuleSetSnapshotRef{ref},
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{{
			Pool: "general", Binding: "bind-general", Generation: 1, Mark: 0x50000101, MarkMask: 0xffffffff,
			Table: 201, Port: 12001, DNSPort: 13001, Target: "w_test",
		}},
	}
}

func cloneIPSets(input *state.FabricRuleIPSetNames) *state.FabricRuleIPSetNames {
	copy := *input
	return &copy
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !equalStrings(got, want) {
		t.Fatalf("unexpected values: got %q want %q", got, want)
	}
}
