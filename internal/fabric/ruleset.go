package fabric

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	xproxy "golang.org/x/net/proxy"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

const (
	ruleSetMatcherSchemaVersion = 1
	defaultRuleSetContentLimit  = int64(32 << 20)
	defaultRuleSetJSONLimit     = int64(32 << 20)
	defaultRuleSetNFTLimit      = int64(32 << 20)
	defaultRuleSetEntryLimit    = 500_000
	defaultRuleSetStringLimit   = 4 << 10
	defaultRuleSetHTTPTimeout   = 45 * time.Second
)

type RuleSetMatcher struct {
	SchemaVersion int      `json:"schema_version"`
	SourceVersion int      `json:"source_version"`
	DomainExact   []string `json:"domain_exact,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	DomainRegex   []string `json:"domain_regex,omitempty"`
	IPCIDR        []string `json:"ip_cidr,omitempty"`
}

type RuleSetDecompiler interface {
	Decompile(context.Context, string, string, int64) error
}

type ShellRuleSetDecompiler struct {
	Binary string
}

func (decompiler ShellRuleSetDecompiler) Decompile(ctx context.Context, inputPath, outputPath string, maxOutputBytes int64) error {
	if maxOutputBytes <= 0 || maxOutputBytes > defaultRuleSetJSONLimit {
		return fmt.Errorf("invalid decompiled rule-set output limit")
	}
	binary := strings.TrimSpace(decompiler.Binary)
	if binary == "" {
		return fmt.Errorf("rule-set decompiler binary is not configured")
	}
	command := exec.CommandContext(ctx, binary, "rule-set", "decompile", "--output", outputPath, inputPath)
	stderr := &boundedRuleSetOutput{limit: 8 << 10}
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start rule-set decompiler: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				message := strings.TrimSpace(stderr.String())
				if message == "" {
					return fmt.Errorf("rule-set decompile failed: %w", err)
				}
				return fmt.Errorf("rule-set decompile failed: %s", message)
			}
			info, statErr := os.Lstat(outputPath)
			if statErr != nil {
				return fmt.Errorf("stat decompiled rule set: %w", statErr)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("decompiled rule set is not a regular file")
			}
			if info.Size() > maxOutputBytes {
				return fmt.Errorf("decompiled rule set exceeds %d bytes", maxOutputBytes)
			}
			return nil
		case <-ticker.C:
			info, err := os.Lstat(outputPath)
			if err == nil && info.Size() > maxOutputBytes {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("decompiled rule set exceeds %d bytes", maxOutputBytes)
			}
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-done
			return ctx.Err()
		}
	}
}

type RuleSetStore struct {
	Layout            state.Layout
	Decompiler        RuleSetDecompiler
	HTTPClientFactory func(string) (*http.Client, error)
	ProxyAddress      func() (string, error)
	Now               func() time.Time
	MaxJSONBytes      int64
	MaxEntries        int
	MaxStringBytes    int
}

func NewRuleSetStore(layout state.Layout) *RuleSetStore {
	return &RuleSetStore{Layout: layout}
}

func (store *RuleSetStore) Resolve(ctx context.Context, spec state.SpecState, graph state.FabricGraph, owners *netgate.OwnerRegistry) (state.FabricGraph, []string, error) {
	if store == nil {
		return graph, nil, fmt.Errorf("Gateway Fabric rule-set store is not configured")
	}
	if err := store.Layout.Ensure(); err != nil {
		return graph, nil, err
	}
	requiredKinds := map[string]map[string]struct{}{}
	for index := range graph.Rules {
		rule := &graph.Rules[index]
		if rule.Kind != RuleGeoSite && rule.Kind != RuleGeoIP {
			continue
		}
		kinds := requiredKinds[rule.Value]
		if kinds == nil {
			kinds = map[string]struct{}{}
			requiredKinds[rule.Value] = kinds
		}
		kinds[rule.Kind] = struct{}{}
		if rule.Kind == RuleGeoIP {
			rule.IPSets = geoIPSetNames(rule.ID)
		}
	}
	if len(requiredKinds) == 0 {
		graph.RuleSets = nil
		return graph, nil, nil
	}

	settings := state.EffectiveRemoteRuleSettings(spec.RemoteRules)
	if !settings.Enabled {
		return graph, nil, fmt.Errorf("GEO rules require remote rule sets to be enabled")
	}
	configured := make(map[string]state.RemoteRuleSet, len(settings.Sets))
	for _, item := range settings.Sets {
		configured[item.ID] = item
	}
	prior := make(map[string]state.RuleSetSnapshotRef, len(graph.RuleSets))
	for _, ref := range graph.RuleSets {
		prior[ref.ID] = ref
	}
	ids := make([]string, 0, len(requiredKinds))
	for id := range requiredKinds {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	resolved := make([]state.RuleSetSnapshotRef, 0, len(ids))
	var warnings []string
	totalBytes := int64(0)
	for _, id := range ids {
		item, ok := configured[id]
		if !ok {
			return graph, warnings, fmt.Errorf("GEO rule set %s is not configured", id)
		}
		if !remoteRuleSetEnabled(settings, item) {
			return graph, warnings, fmt.Errorf("GEO rule set %s is not enabled", id)
		}
		ref, warning, err := store.resolveOne(ctx, settings, item, prior[id], graph.RuleSetsPinned)
		if err != nil {
			return graph, warnings, fmt.Errorf("resolve GEO rule set %s: %w", id, err)
		}
		matcher, err := store.validateSnapshot(ref)
		if err != nil {
			return graph, warnings, fmt.Errorf("validate GEO rule set %s: %w", id, err)
		}
		for kind := range requiredKinds[id] {
			switch kind {
			case RuleGeoSite:
				if len(matcher.DomainExact)+len(matcher.DomainSuffix)+len(matcher.DomainKeyword)+len(matcher.DomainRegex) == 0 {
					return graph, warnings, fmt.Errorf("GEO rule set %s has no domain matchers", id)
				}
			case RuleGeoIP:
				if len(matcher.IPCIDR) == 0 {
					return graph, warnings, fmt.Errorf("GEO rule set %s has no IP prefixes", id)
				}
			}
		}
		resolved = append(resolved, ref)
		totalBytes += ref.Size
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if settings.MemoryGuard.MaxEnabledSets > 0 && len(resolved) > settings.MemoryGuard.MaxEnabledSets {
		return graph, warnings, fmt.Errorf("resolved GEO rule sets exceed max_enabled_sets")
	}
	if settings.MemoryGuard.MaxTotalBytes > 0 && totalBytes > int64(settings.MemoryGuard.MaxTotalBytes) {
		return graph, warnings, fmt.Errorf("resolved GEO rule sets exceed max_total_bytes")
	}
	graph.RuleSets = resolved
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	for index := range graph.Rules {
		rule := &graph.Rules[index]
		if rule.Kind != RuleGeoIP || rule.IPSets == nil {
			continue
		}
		ref, ok := ruleSetRefByID(graph.RuleSets, rule.Value)
		if !ok {
			return graph, warnings, fmt.Errorf("GeoIP rule %s has no resolved rule set %s", rule.ID, rule.Value)
		}
		matcher, err := LoadRuleSetMatcher(ref, store.maxJSONBytes())
		if err != nil {
			return graph, warnings, err
		}
		ipv4, ipv6 := splitRuleSetPrefixes(matcher.IPCIDR)
		var definition strings.Builder
		owner := nftQuote(owners.Comment(netgate.KindFabricGeoSet, rule.ID))
		writeStaticPrefixSet(&definition, rule.IPSets.IPv4, "ipv4_addr", ipv4, owner)
		writeStaticPrefixSet(&definition, rule.IPSets.IPv6, "ipv6_addr", ipv6, owner)
		body := []byte(definition.String())
		hash := sha256Hex(body)
		path := filepath.Join(store.Layout.RuleSetNFTDir(), hash+".nft")
		if err := writeRuleSetFile(path, body, 0o644); err != nil {
			return graph, warnings, err
		}
		rule.IPSets.DefinitionPath = path
		rule.IPSets.DefinitionSHA256 = hash
	}
	return graph, warnings, nil
}

func (store *RuleSetStore) resolveOne(ctx context.Context, settings state.RemoteRuleSettings, item state.RemoteRuleSet, prior state.RuleSetSnapshotRef, pinned bool) (state.RuleSetSnapshotRef, string, error) {
	fingerprint := ruleSetSourceFingerprint(item, settings)
	if pinned {
		if prior.ID != item.ID || prior.SourceFingerprint != fingerprint {
			return state.RuleSetSnapshotRef{}, "", fmt.Errorf("pinned snapshot is unavailable")
		}
		if _, err := store.validateSnapshot(prior); err != nil {
			return state.RuleSetSnapshotRef{}, "", err
		}
		return prior, "", nil
	}

	if remoteRuleSetType(item) == "local" {
		// Local rule sets are immutable for the duration of a prepare unless
		// their source bytes changed. Reuse the previously validated snapshot
		// to avoid invoking the binary decompiler on every dry-run.
		if prior.ID == item.ID && prior.SourceFingerprint == fingerprint {
			body, readErr := store.readRuleSetContent(ctx, settings, item, defaultRuleSetContentLimit)
			if readErr == nil && prior.ContentSHA256 == sha256Hex(body) {
				if _, validateErr := store.validateSnapshot(prior); validateErr == nil {
					return prior, "", nil
				}
			}
		}
		ref, err := store.createSnapshot(ctx, settings, item, fingerprint)
		return ref, "", err
	}

	candidate := prior
	if cached, err := store.loadSourceRecord(fingerprint); err == nil && cached.FetchedAt.After(candidate.FetchedAt) {
		candidate = cached
	}
	interval, err := ruleSetUpdateInterval(item, settings)
	if err != nil {
		return state.RuleSetSnapshotRef{}, "", err
	}
	if candidate.ID == item.ID && candidate.SourceFingerprint == fingerprint && store.now().Sub(candidate.FetchedAt) < interval {
		if _, err := store.validateSnapshot(candidate); err == nil {
			return candidate, "", nil
		}
	}

	ref, err := store.createSnapshot(ctx, settings, item, fingerprint)
	if err == nil {
		return ref, "", nil
	}
	if candidate.ID == item.ID && candidate.SourceFingerprint == fingerprint {
		if _, candidateErr := store.validateSnapshot(candidate); candidateErr == nil {
			return candidate, "GEO 规则集 " + item.ID + " 刷新失败；已继续使用上次验证通过的快照", nil
		}
	}
	return state.RuleSetSnapshotRef{}, "", err
}

func (store *RuleSetStore) createSnapshot(ctx context.Context, settings state.RemoteRuleSettings, item state.RemoteRuleSet, fingerprint string) (state.RuleSetSnapshotRef, error) {
	maxSetBytes := int64(settings.MemoryGuard.MaxSetBytes)
	if maxSetBytes <= 0 {
		maxSetBytes = 4 << 20
	}
	if maxSetBytes > defaultRuleSetContentLimit {
		return state.RuleSetSnapshotRef{}, fmt.Errorf("max_set_bytes exceeds the %d byte hard limit", defaultRuleSetContentLimit)
	}
	body, err := store.readRuleSetContent(ctx, settings, item, maxSetBytes)
	if err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	contentSum := sha256.Sum256(body)
	contentSHA := hex.EncodeToString(contentSum[:])
	format := remoteRuleSetFormat(item, settings)
	extension := ".json"
	if format == "binary" {
		extension = ".srs"
	}
	contentPath := filepath.Join(store.Layout.RuleSetContentDir(), contentSHA+extension)
	if err := writeRuleSetFile(contentPath, body, 0o644); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}

	sourceJSON := body
	if format == "binary" {
		decompiler := store.Decompiler
		if decompiler == nil {
			decompiler = ShellRuleSetDecompiler{}
		}
		output, err := os.CreateTemp(store.Layout.RuleSetRuntimeDir(), "decompiled-*.json")
		if err != nil {
			return state.RuleSetSnapshotRef{}, err
		}
		outputPath := output.Name()
		if err := output.Close(); err != nil {
			_ = os.Remove(outputPath)
			return state.RuleSetSnapshotRef{}, err
		}
		if err := os.Remove(outputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return state.RuleSetSnapshotRef{}, err
		}
		decompileErr := decompiler.Decompile(ctx, contentPath, outputPath, store.maxJSONBytes())
		if decompileErr == nil {
			sourceJSON, decompileErr = readRuleSetFile(outputPath, store.maxJSONBytes())
		}
		_ = os.Remove(outputPath)
		if decompileErr != nil {
			return state.RuleSetSnapshotRef{}, decompileErr
		}
	}
	matcher, err := parseRuleSetSource(sourceJSON, store.maxEntries(), store.maxStringBytes())
	if err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	matcherJSON, err := json.Marshal(matcher)
	if err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	matcherSum := sha256.Sum256(matcherJSON)
	matcherSHA := hex.EncodeToString(matcherSum[:])
	matcherPath := filepath.Join(store.Layout.RuleSetMatcherDir(), matcherSHA+".json")
	if err := writeRuleSetFile(matcherPath, matcherJSON, 0o644); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	ref := state.RuleSetSnapshotRef{
		ID: item.ID, SourceFingerprint: fingerprint, ContentSHA256: contentSHA, MatcherSHA256: matcherSHA,
		ContentPath: contentPath, MatcherPath: matcherPath, Format: format, SourceType: remoteRuleSetType(item),
		Size: int64(len(body)), FetchedAt: store.now(), Stats: matcherStats(matcher),
	}
	if _, err := store.validateSnapshot(ref); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	if err := store.saveSourceRecord(ref); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	return ref, nil
}

func (store *RuleSetStore) readRuleSetContent(ctx context.Context, settings state.RemoteRuleSettings, item state.RemoteRuleSet, maxBytes int64) ([]byte, error) {
	if remoteRuleSetType(item) == "local" {
		body, err := readRuleSetFile(item.Path, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("read local rule set: %w", err)
		}
		return body, nil
	}
	proxyAddress, err := ruleSetDownloadProxy(item, settings, store)
	if err != nil {
		return nil, err
	}
	clientFactory := store.HTTPClientFactory
	if clientFactory == nil {
		clientFactory = defaultRuleSetHTTPClient
	}
	client, err := clientFactory(proxyAddress)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build remote rule-set request")
	}
	request.Header.Set("User-Agent", "SmartGate/3 rule-set snapshot")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("remote rule-set request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("remote rule-set request returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return nil, fmt.Errorf("remote rule set exceeds max_set_bytes")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read remote rule set: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("remote rule set exceeds max_set_bytes")
	}
	return body, nil
}

func (store *RuleSetStore) validateSnapshot(ref state.RuleSetSnapshotRef) (RuleSetMatcher, error) {
	cacheRoot, err := validateRuleSetReference(ref)
	if err != nil {
		return RuleSetMatcher{}, err
	}
	if filepath.Clean(cacheRoot) != filepath.Clean(store.Layout.RuleSetCacheDir()) {
		return RuleSetMatcher{}, fmt.Errorf("rule-set cache path is outside the configured data directory")
	}
	if err := validateManagedRuleSetFile(store.Layout.RuleSetContentDir(), ref.ContentPath, filepath.Base(ref.ContentPath), defaultRuleSetContentLimit); err != nil {
		return RuleSetMatcher{}, err
	}
	if ref.Size <= 0 || ref.Size > defaultRuleSetContentLimit {
		return RuleSetMatcher{}, fmt.Errorf("rule-set content size is outside the allowed range")
	}
	content, err := readRuleSetFile(ref.ContentPath, defaultRuleSetContentLimit)
	if err != nil {
		return RuleSetMatcher{}, err
	}
	if int64(len(content)) != ref.Size || sha256Hex(content) != ref.ContentSHA256 {
		return RuleSetMatcher{}, fmt.Errorf("rule-set content hash mismatch")
	}
	matcher, err := loadRuleSetMatcher(ref, store.maxJSONBytes(), store.maxEntries(), store.maxStringBytes())
	if err != nil {
		return RuleSetMatcher{}, err
	}
	if matcherStats(matcher) != ref.Stats {
		return RuleSetMatcher{}, fmt.Errorf("rule-set matcher statistics mismatch")
	}
	return matcher, nil
}

func LoadRuleSetMatcher(ref state.RuleSetSnapshotRef, maxBytes int64) (RuleSetMatcher, error) {
	return loadRuleSetMatcher(ref, maxBytes, defaultRuleSetEntryLimit, defaultRuleSetStringLimit)
}

func loadRuleSetMatcher(ref state.RuleSetSnapshotRef, maxBytes int64, maxEntries, maxStringBytes int) (RuleSetMatcher, error) {
	if _, err := validateRuleSetReference(ref); err != nil {
		return RuleSetMatcher{}, err
	}
	maxBytes = boundedRuleSetInt64(maxBytes, defaultRuleSetJSONLimit)
	maxEntries = boundedRuleSetInt(maxEntries, defaultRuleSetEntryLimit)
	maxStringBytes = boundedRuleSetInt(maxStringBytes, defaultRuleSetStringLimit)
	if err := validateManagedRuleSetFile(filepath.Dir(ref.MatcherPath), ref.MatcherPath, ref.MatcherSHA256+".json", maxBytes); err != nil {
		return RuleSetMatcher{}, err
	}
	body, err := readRuleSetFile(ref.MatcherPath, maxBytes)
	if err != nil {
		return RuleSetMatcher{}, err
	}
	if sha256Hex(body) != ref.MatcherSHA256 {
		return RuleSetMatcher{}, fmt.Errorf("rule-set matcher hash mismatch")
	}
	var matcher RuleSetMatcher
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&matcher); err != nil {
		return RuleSetMatcher{}, fmt.Errorf("decode rule-set matcher: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RuleSetMatcher{}, err
	}
	if matcher.SchemaVersion != ruleSetMatcherSchemaVersion {
		return RuleSetMatcher{}, fmt.Errorf("unsupported rule-set matcher schema %d", matcher.SchemaVersion)
	}
	if err := validateRuleSetMatcher(matcher, maxEntries, maxStringBytes); err != nil {
		return RuleSetMatcher{}, err
	}
	return matcher, nil
}

func (store *RuleSetStore) sourceRecordPath(fingerprint string) string {
	return filepath.Join(store.Layout.RuleSetSourcesDir(), fingerprint+".json")
}

func (store *RuleSetStore) loadSourceRecord(fingerprint string) (state.RuleSetSnapshotRef, error) {
	if !validSHA256Hex(fingerprint) {
		return state.RuleSetSnapshotRef{}, fmt.Errorf("invalid rule-set source fingerprint")
	}
	body, err := readRuleSetFile(store.sourceRecordPath(fingerprint), 64<<10)
	if err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	var ref state.RuleSetSnapshotRef
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ref); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	if ref.SourceFingerprint != fingerprint {
		return state.RuleSetSnapshotRef{}, fmt.Errorf("rule-set source cache fingerprint mismatch")
	}
	if _, err := store.validateSnapshot(ref); err != nil {
		return state.RuleSetSnapshotRef{}, err
	}
	return ref, nil
}

func (store *RuleSetStore) saveSourceRecord(ref state.RuleSetSnapshotRef) error {
	if !validSHA256Hex(ref.SourceFingerprint) {
		return fmt.Errorf("invalid rule-set source fingerprint")
	}
	body, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return overwriteRuleSetFile(store.sourceRecordPath(ref.SourceFingerprint), body, 0o644)
}

func (store *RuleSetStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func (store *RuleSetStore) maxJSONBytes() int64 {
	return boundedRuleSetInt64(store.MaxJSONBytes, defaultRuleSetJSONLimit)
}

func (store *RuleSetStore) maxEntries() int {
	return boundedRuleSetInt(store.MaxEntries, defaultRuleSetEntryLimit)
}

func (store *RuleSetStore) maxStringBytes() int {
	return boundedRuleSetInt(store.MaxStringBytes, defaultRuleSetStringLimit)
}

type ruleSetSourceEnvelope struct {
	Version int               `json:"version"`
	Rules   []json.RawMessage `json:"rules"`
}

type ruleSetSourceRule struct {
	Type          string     `json:"type,omitempty"`
	Domain        stringList `json:"domain,omitempty"`
	DomainSuffix  stringList `json:"domain_suffix,omitempty"`
	DomainKeyword stringList `json:"domain_keyword,omitempty"`
	DomainRegex   stringList `json:"domain_regex,omitempty"`
	IPCIDR        stringList `json:"ip_cidr,omitempty"`
	Invert        bool       `json:"invert,omitempty"`
}

type stringList []string

func (list *stringList) UnmarshalJSON(body []byte) error {
	var values []string
	if err := json.Unmarshal(body, &values); err == nil {
		*list = values
		return nil
	}
	var value string
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("must be a string or string array")
	}
	*list = []string{value}
	return nil
}

func parseRuleSetSource(body []byte, maxEntries, maxStringBytes int) (RuleSetMatcher, error) {
	maxEntries = boundedRuleSetInt(maxEntries, defaultRuleSetEntryLimit)
	maxStringBytes = boundedRuleSetInt(maxStringBytes, defaultRuleSetStringLimit)
	var envelope ruleSetSourceEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return RuleSetMatcher{}, fmt.Errorf("decode rule-set source: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RuleSetMatcher{}, err
	}
	if envelope.Version < 1 || envelope.Version > 4 {
		return RuleSetMatcher{}, fmt.Errorf("unsupported rule-set source version %d", envelope.Version)
	}
	if len(envelope.Rules) > maxEntries {
		return RuleSetMatcher{}, fmt.Errorf("rule-set source exceeds %d rule entries", maxEntries)
	}
	matcher := RuleSetMatcher{SchemaVersion: ruleSetMatcherSchemaVersion, SourceVersion: envelope.Version}
	for index, raw := range envelope.Rules {
		var rule ruleSetSourceRule
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&rule); err != nil {
			return RuleSetMatcher{}, fmt.Errorf("decode rule-set rule %d: %w", index, err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return RuleSetMatcher{}, err
		}
		if rule.Type != "" && rule.Type != "default" {
			return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d uses unsupported type %q", index, rule.Type)
		}
		if rule.Invert {
			return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d uses unsupported invert semantics", index)
		}
		for _, value := range rule.Domain {
			normalized, err := normalizeRuleSetDomain(value, false, maxStringBytes)
			if err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d domain: %w", index, err)
			}
			matcher.DomainExact = append(matcher.DomainExact, normalized)
		}
		for _, value := range rule.DomainSuffix {
			normalized, err := normalizeRuleSetDomain(value, true, maxStringBytes)
			if err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d domain_suffix: %w", index, err)
			}
			matcher.DomainSuffix = append(matcher.DomainSuffix, normalized)
		}
		for _, value := range rule.DomainKeyword {
			normalized := strings.ToLower(strings.TrimSpace(value))
			if err := validateRuleSetString(normalized, maxStringBytes); err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d domain_keyword: %w", index, err)
			}
			matcher.DomainKeyword = append(matcher.DomainKeyword, normalized)
		}
		for _, value := range rule.DomainRegex {
			value = strings.TrimSpace(value)
			if err := validateRuleSetString(value, maxStringBytes); err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d domain_regex: %w", index, err)
			}
			if _, err := regexp.Compile(value); err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d domain_regex is invalid: %w", index, err)
			}
			matcher.DomainRegex = append(matcher.DomainRegex, value)
		}
		for _, value := range rule.IPCIDR {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err != nil {
				return RuleSetMatcher{}, fmt.Errorf("rule-set rule %d ip_cidr is invalid: %w", index, err)
			}
			matcher.IPCIDR = append(matcher.IPCIDR, prefix.Masked().String())
		}
		if matcherEntryCount(matcher) > maxEntries {
			return RuleSetMatcher{}, fmt.Errorf("rule-set source exceeds %d matcher entries", maxEntries)
		}
	}
	matcher.DomainExact = sortedUnique(matcher.DomainExact)
	matcher.DomainSuffix = sortedUnique(matcher.DomainSuffix)
	matcher.DomainKeyword = sortedUnique(matcher.DomainKeyword)
	matcher.DomainRegex = sortedUnique(matcher.DomainRegex)
	matcher.IPCIDR = sortedUniquePrefixes(matcher.IPCIDR)
	if matcherEntryCount(matcher) == 0 {
		return RuleSetMatcher{}, fmt.Errorf("rule-set source contains no supported matchers")
	}
	if err := validateRuleSetMatcher(matcher, maxEntries, maxStringBytes); err != nil {
		return RuleSetMatcher{}, err
	}
	return matcher, nil
}

func validateRuleSetMatcher(matcher RuleSetMatcher, maxEntries, maxStringBytes int) error {
	maxEntries = boundedRuleSetInt(maxEntries, defaultRuleSetEntryLimit)
	maxStringBytes = boundedRuleSetInt(maxStringBytes, defaultRuleSetStringLimit)
	if matcher.SchemaVersion != ruleSetMatcherSchemaVersion {
		return fmt.Errorf("unsupported rule-set matcher schema %d", matcher.SchemaVersion)
	}
	if matcher.SourceVersion < 1 || matcher.SourceVersion > 4 {
		return fmt.Errorf("unsupported rule-set source version %d", matcher.SourceVersion)
	}
	if matcherEntryCount(matcher) == 0 {
		return fmt.Errorf("rule-set matcher contains no entries")
	}
	if matcherEntryCount(matcher) > maxEntries {
		return fmt.Errorf("rule-set matcher exceeds %d entries", maxEntries)
	}
	for _, value := range matcher.DomainExact {
		normalized, err := normalizeRuleSetDomain(value, false, maxStringBytes)
		if err != nil || normalized != value {
			return fmt.Errorf("rule-set matcher contains invalid exact domain %q", value)
		}
	}
	for _, value := range matcher.DomainSuffix {
		normalized, err := normalizeRuleSetDomain(value, true, maxStringBytes)
		if err != nil || normalized != value {
			return fmt.Errorf("rule-set matcher contains invalid domain suffix %q", value)
		}
	}
	for _, value := range matcher.DomainKeyword {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if err := validateRuleSetString(normalized, maxStringBytes); err != nil || normalized != value {
			return fmt.Errorf("rule-set matcher contains invalid domain keyword %q", value)
		}
	}
	for _, value := range matcher.DomainRegex {
		if err := validateRuleSetString(value, maxStringBytes); err != nil || strings.TrimSpace(value) != value {
			return fmt.Errorf("rule-set matcher contains invalid domain regex")
		}
		if _, err := regexp.Compile(value); err != nil {
			return fmt.Errorf("rule-set matcher contains invalid domain regex: %w", err)
		}
	}
	for _, value := range matcher.IPCIDR {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Masked().String() != value {
			return fmt.Errorf("rule-set matcher contains invalid IP prefix %q", value)
		}
	}
	for name, values := range map[string][]string{
		"domain_exact": matcher.DomainExact, "domain_suffix": matcher.DomainSuffix,
		"domain_keyword": matcher.DomainKeyword, "domain_regex": matcher.DomainRegex,
	} {
		if !strictlySortedUnique(values) {
			return fmt.Errorf("rule-set matcher %s entries are not sorted and unique", name)
		}
	}
	expectedPrefixes := sortedUniquePrefixes(append([]string(nil), matcher.IPCIDR...))
	if !equalStrings(expectedPrefixes, matcher.IPCIDR) {
		return fmt.Errorf("rule-set matcher ip_cidr entries are not sorted and unique")
	}
	return nil
}

func normalizeRuleSetDomain(value string, suffix bool, maxBytes int) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	leadingDot := suffix && strings.HasPrefix(value, ".")
	value = strings.TrimPrefix(value, ".")
	if err := validateRuleSetString(value, maxBytes); err != nil {
		return "", err
	}
	if strings.ContainsAny(value, " /:@") {
		return "", fmt.Errorf("contains invalid domain characters")
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) >= 0 {
		return "", fmt.Errorf("contains invalid domain whitespace or control characters")
	}
	if leadingDot {
		return "." + value, nil
	}
	return value, nil
}

func validateRuleSetString(value string, maxBytes int) error {
	if maxBytes <= 0 || maxBytes > defaultRuleSetStringLimit {
		return fmt.Errorf("invalid string limit")
	}
	if value == "" {
		return fmt.Errorf("value is required")
	}
	if len(value) > maxBytes {
		return fmt.Errorf("value exceeds %d bytes", maxBytes)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("value contains control characters")
	}
	return nil
}

func matcherStats(matcher RuleSetMatcher) state.RuleSetSnapshotStats {
	return state.RuleSetSnapshotStats{
		DomainExact: len(matcher.DomainExact), DomainSuffix: len(matcher.DomainSuffix),
		DomainKeyword: len(matcher.DomainKeyword), DomainRegex: len(matcher.DomainRegex), IPCIDR: len(matcher.IPCIDR),
	}
}

func matcherEntryCount(matcher RuleSetMatcher) int {
	return len(matcher.DomainExact) + len(matcher.DomainSuffix) + len(matcher.DomainKeyword) + len(matcher.DomainRegex) + len(matcher.IPCIDR)
}

func remoteRuleSetEnabled(settings state.RemoteRuleSettings, item state.RemoteRuleSet) bool {
	if !settings.Enabled || !item.Enabled {
		return false
	}
	if item.Kind == "adblock" {
		// The legacy set list is compatibility metadata, not an enable switch.
		return settings.AdBlock.Enabled
	}
	return true
}

func remoteRuleSetType(item state.RemoteRuleSet) string {
	value := strings.ToLower(strings.TrimSpace(item.Type))
	if value == "" {
		return "remote"
	}
	return value
}

func remoteRuleSetFormat(item state.RemoteRuleSet, settings state.RemoteRuleSettings) string {
	value := strings.ToLower(strings.TrimSpace(item.Format))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(settings.DefaultFormat))
	}
	if value == "" {
		return "binary"
	}
	return value
}

func ruleSetSourceFingerprint(item state.RemoteRuleSet, settings state.RemoteRuleSettings) string {
	source := item.URL
	if remoteRuleSetType(item) == "local" {
		source = filepath.Clean(item.Path)
	}
	value := strings.Join([]string{item.ID, remoteRuleSetType(item), remoteRuleSetFormat(item, settings), source}, "\x00")
	return sha256Hex([]byte(value))
}

func ruleSetUpdateInterval(item state.RemoteRuleSet, settings state.RemoteRuleSettings) (time.Duration, error) {
	raw := strings.TrimSpace(item.UpdateInterval)
	if raw == "" {
		raw = strings.TrimSpace(settings.UpdateInterval)
	}
	if raw == "" {
		raw = "24h"
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("update_interval must be a positive duration")
	}
	return interval, nil
}

func ruleSetDownloadProxy(item state.RemoteRuleSet, settings state.RemoteRuleSettings, store *RuleSetStore) (string, error) {
	detour := strings.TrimSpace(item.DownloadDetour)
	if detour == "" {
		detour = strings.TrimSpace(settings.DownloadDetour)
	}
	switch strings.ToLower(detour) {
	case "", "direct":
		return "", nil
	case "proxy":
		if store == nil || store.ProxyAddress == nil {
			return "", fmt.Errorf("Proxy rule-set download requires an applied General Worker path")
		}
		address, err := store.ProxyAddress()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(address) == "" {
			return "", fmt.Errorf("Proxy rule-set download requires an applied General Worker path")
		}
		return address, nil
	default:
		return "", fmt.Errorf("unsupported rule-set download_detour %q", detour)
	}
}

func defaultRuleSetHTTPClient(proxyAddress string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	if proxyAddress != "" {
		dialer, err := xproxy.SOCKS5("tcp", proxyAddress, nil, &net.Dialer{Timeout: 10 * time.Second})
		if err != nil {
			return nil, err
		}
		if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
			transport.DialContext = contextDialer.DialContext
		} else {
			transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   defaultRuleSetHTTPTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}, nil
}

func geoIPSetNames(ruleID string) *state.FabricRuleIPSetNames {
	sum := sha256.Sum256([]byte(ruleID))
	suffix := hex.EncodeToString(sum[:])[:12]
	return &state.FabricRuleIPSetNames{IPv4: "sggeo_" + suffix + "_4", IPv6: "sggeo_" + suffix + "_6"}
}

func ruleSetRefByID(refs []state.RuleSetSnapshotRef, id string) (state.RuleSetSnapshotRef, bool) {
	for _, ref := range refs {
		if ref.ID == id {
			return ref, true
		}
	}
	return state.RuleSetSnapshotRef{}, false
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func sortedUniquePrefixes(values []string) []string {
	values = sortedUnique(values)
	sort.SliceStable(values, func(i, j int) bool {
		left, _ := netip.ParsePrefix(values[i])
		right, _ := netip.ParsePrefix(values[j])
		if left.Addr().BitLen() != right.Addr().BitLen() {
			return left.Addr().BitLen() < right.Addr().BitLen()
		}
		if left.Bits() != right.Bits() {
			return left.Bits() < right.Bits()
		}
		return left.Addr().Less(right.Addr())
	})
	return values
}

func writeRuleSetFile(path string, body []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("existing rule-set cache path is not a regular file")
		}
		if info.Size() != int64(len(body)) {
			return fmt.Errorf("existing rule-set cache file has unexpected size")
		}
		existing, err := readRuleSetFile(path, int64(len(body)))
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, body) {
			return fmt.Errorf("existing rule-set cache file has unexpected content")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomicRuleSetFile(path, body, mode)
}

func overwriteRuleSetFile(path string, body []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("existing rule-set cache path is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomicRuleSetFile(path, body, mode)
}

func writeAtomicRuleSetFile(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".ruleset-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readRuleSetFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("rule-set read limit must be positive")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("rule-set path is not a regular file")
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("rule-set file exceeds %d bytes", maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("rule-set file changed while opening")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("rule-set file exceeds %d bytes", maxBytes)
	}
	finalInfo, err := os.Lstat(path)
	if err != nil || !finalInfo.Mode().IsRegular() || !os.SameFile(info, finalInfo) {
		return nil, fmt.Errorf("rule-set file changed while reading")
	}
	return body, nil
}

func pathWithin(root, path string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(root) != root || filepath.Clean(path) != path {
		return fmt.Errorf("rule-set cache paths must be absolute and clean")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("rule-set cache path escapes its managed directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve rule-set cache directory: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve rule-set cache path: %w", err)
	}
	resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) || filepath.IsAbs(resolvedRelative) {
		return fmt.Errorf("rule-set cache path resolves outside its managed directory")
	}
	return nil
}

func validateManagedRuleSetFile(root, path, expectedBase string, maxBytes int64) error {
	if err := pathWithin(filepath.Clean(root), filepath.Clean(path)); err != nil {
		return err
	}
	if filepath.Base(path) != expectedBase {
		return fmt.Errorf("rule-set cache filename does not match its digest")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rule-set cache path is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return fmt.Errorf("rule-set cache file exceeds %d bytes", maxBytes)
	}
	return nil
}

func validateRuleSetReference(ref state.RuleSetSnapshotRef) (string, error) {
	if strings.TrimSpace(ref.ID) == "" || !validSHA256Hex(ref.SourceFingerprint) || !validSHA256Hex(ref.ContentSHA256) || !validSHA256Hex(ref.MatcherSHA256) {
		return "", fmt.Errorf("snapshot reference is incomplete or invalid")
	}
	if ref.Format != "binary" && ref.Format != "source" {
		return "", fmt.Errorf("snapshot reference has unsupported format %q", ref.Format)
	}
	if ref.SourceType != "remote" && ref.SourceType != "local" {
		return "", fmt.Errorf("snapshot reference has unsupported source type %q", ref.SourceType)
	}
	if ref.Size <= 0 || ref.Size > defaultRuleSetContentLimit || ref.FetchedAt.IsZero() {
		return "", fmt.Errorf("snapshot reference has invalid size or timestamp")
	}
	counts := []int{ref.Stats.DomainExact, ref.Stats.DomainSuffix, ref.Stats.DomainKeyword, ref.Stats.DomainRegex, ref.Stats.IPCIDR}
	var totalEntries int64
	for _, count := range counts {
		if count < 0 || count > defaultRuleSetEntryLimit {
			return "", fmt.Errorf("snapshot reference has invalid matcher statistics")
		}
		totalEntries += int64(count)
	}
	if totalEntries > int64(defaultRuleSetEntryLimit) {
		return "", fmt.Errorf("snapshot reference has invalid matcher statistics")
	}
	if !filepath.IsAbs(ref.ContentPath) || !filepath.IsAbs(ref.MatcherPath) || filepath.Clean(ref.ContentPath) != ref.ContentPath || filepath.Clean(ref.MatcherPath) != ref.MatcherPath {
		return "", fmt.Errorf("snapshot cache paths must be absolute and clean")
	}
	contentExtension := ".json"
	if ref.Format == "binary" {
		contentExtension = ".srs"
	}
	contentDir := filepath.Dir(ref.ContentPath)
	if filepath.Base(contentDir) != "content" || filepath.Base(ref.ContentPath) != ref.ContentSHA256+contentExtension {
		return "", fmt.Errorf("snapshot content path does not match its digest")
	}
	cacheRoot := filepath.Dir(contentDir)
	expectedMatcher := filepath.Join(cacheRoot, "matchers", ref.MatcherSHA256+".json")
	if ref.MatcherPath != expectedMatcher {
		return "", fmt.Errorf("snapshot matcher path does not match its digest")
	}
	return cacheRoot, nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func strictlySortedUnique(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func boundedRuleSetInt64(value, hardLimit int64) int64 {
	if value <= 0 || value > hardLimit {
		return hardLimit
	}
	return value
}

func boundedRuleSetInt(value, hardLimit int) int {
	if value <= 0 || value > hardLimit {
		return hardLimit
	}
	return value
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing JSON value")
}

type boundedRuleSetOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (output *boundedRuleSetOutput) Write(body []byte) (int, error) {
	written := len(body)
	remaining := output.limit - output.buffer.Len()
	if remaining > 0 {
		if len(body) > remaining {
			body = body[:remaining]
		}
		_, _ = output.buffer.Write(body)
	}
	return written, nil
}

func (output *boundedRuleSetOutput) String() string {
	return output.buffer.String()
}
