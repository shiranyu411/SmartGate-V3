package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const MaxSubscriptionBodyBytes = 8 << 20

type LastGoodStore struct {
	layout state.Layout
}

type LastGoodEnvelope struct {
	ProviderID   string                     `json:"provider_id"`
	SavedAt      time.Time                  `json:"saved_at"`
	Count        int                        `json:"count"`
	Subscription *SubscriptionInfo          `json:"subscription,omitempty"`
	Identities   []smartnode.IdentitySource `json:"identities,omitempty"`
	Outbounds    []map[string]any           `json:"outbounds,omitempty"`
	MetadataOnly bool                       `json:"-"`
}

type LastGoodUpdate struct {
	ProviderID   string
	Subscription *SubscriptionInfo
	Identities   []smartnode.IdentitySource
	Outbounds    []map[string]any
}

type ParsedSubscription struct {
	Subscription *SubscriptionInfo
	Identities   []smartnode.IdentitySource
}

type LastGoodRepository interface {
	Exists(providerID string) bool
	SaveMany(ctx context.Context, updates []LastGoodUpdate) (map[string]LastGoodEnvelope, error)
}

func NewLastGoodStore(layout state.Layout) *LastGoodStore {
	return &LastGoodStore{layout: layout}
}

func (s *LastGoodStore) Path(providerID string) (string, error) {
	if err := ValidateProviderID(providerID); err != nil {
		return "", err
	}
	return filepath.Join(s.layout.LastGoodDir(), providerID+".json"), nil
}

func (s *LastGoodStore) Exists(providerID string) bool {
	path, err := s.Path(providerID)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (s *LastGoodStore) Save(providerID string, outbounds []map[string]any) (LastGoodEnvelope, error) {
	identities, err := identitySourcesFromOutbounds(providerID, outbounds)
	if err != nil {
		return LastGoodEnvelope{}, err
	}
	envs, err := s.SaveMany(context.Background(), []LastGoodUpdate{{
		ProviderID: providerID,
		Identities: identities,
	}})
	if err != nil {
		return LastGoodEnvelope{}, err
	}
	return envs[providerID], nil
}

func (s *LastGoodStore) SaveMany(ctx context.Context, updates []LastGoodUpdate) (map[string]LastGoodEnvelope, error) {
	type pendingWrite struct {
		path string
		tmp  string
		body []byte
	}
	saved := make(map[string]LastGoodEnvelope, len(updates))
	if len(updates) == 0 {
		return saved, nil
	}
	var err error
	updates, err = NormalizeLastGoodUpdates(updates)
	if err != nil {
		return nil, err
	}
	if err := validateLastGoodUpdates(updates); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.layout.LastGoodDir(), 0o755); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := s.validatePostSaveBudget(ctx, updates, now); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	pending := make([]pendingWrite, 0, len(updates))
	totalEncoded := 0
	totalNodes := 0
	for _, update := range updates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(update.Identities) == 0 {
			return nil, fmt.Errorf("last-good for provider %s cannot be empty", update.ProviderID)
		}
		totalNodes += len(update.Identities)
		if totalNodes > MaxDryRunLastGoodNodes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
		}
		if _, ok := seen[update.ProviderID]; ok {
			return nil, fmt.Errorf("duplicate last-good update for provider %s", update.ProviderID)
		}
		seen[update.ProviderID] = struct{}{}
		path, err := s.Path(update.ProviderID)
		if err != nil {
			return nil, err
		}
		env := LastGoodEnvelope{
			ProviderID:   update.ProviderID,
			SavedAt:      now,
			Count:        len(update.Identities),
			Subscription: update.Subscription,
			Identities:   update.Identities,
		}
		b, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return nil, err
		}
		totalEncoded += len(b)
		if totalEncoded > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("aggregate encoded last-good exceeds %d bytes", MaxDryRunLastGoodBytes)}
		}
		if old, err := readLastGoodBodyBounded(path); err == nil && bytes.Equal(old, b) {
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, err
			}
			saved[update.ProviderID] = env
			continue
		}
		pending = append(pending, pendingWrite{path: path, tmp: path + ".tmp", body: b})
		saved[update.ProviderID] = env
	}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.WriteFile(item.tmp, item.body, 0o600); err != nil {
			return nil, err
		}
		if err := os.Chmod(item.tmp, 0o600); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, item := range pending {
		if err := os.Rename(item.tmp, item.path); err != nil {
			return nil, err
		}
		if err := os.Chmod(item.path, 0o600); err != nil {
			return nil, err
		}
	}
	return saved, nil
}

func (s *LastGoodStore) validatePostSaveBudget(ctx context.Context, updates []LastGoodUpdate, savedAt time.Time) error {
	replaced := make(map[string]struct{}, len(updates))
	totalBytes := 0
	totalNodes := 0
	for _, update := range updates {
		replaced[update.ProviderID] = struct{}{}
		body, err := json.MarshalIndent(LastGoodEnvelope{
			ProviderID:   update.ProviderID,
			SavedAt:      savedAt,
			Count:        len(update.Identities),
			Subscription: update.Subscription,
			Identities:   update.Identities,
		}, "", "  ")
		if err != nil {
			return err
		}
		totalBytes += len(body)
		totalNodes += len(update.Identities)
	}
	entries, err := os.ReadDir(s.layout.LastGoodDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		if _, ok := replaced[providerID]; ok {
			continue
		}
		if err := ValidateProviderID(providerID); err != nil {
			return err
		}
		body, err := readLastGoodBodyBounded(filepath.Join(s.layout.LastGoodDir(), entry.Name()))
		if err != nil {
			return err
		}
		env, err := decodeLastGoodBody(providerID, body)
		if err != nil {
			return err
		}
		totalBytes += len(body)
		totalNodes += len(env.Identities)
		if totalBytes > MaxDryRunLastGoodBytes || totalNodes > MaxDryRunLastGoodNodes {
			break
		}
	}
	if totalBytes > MaxDryRunLastGoodBytes {
		return &TopologyResourceLimitError{Reason: fmt.Sprintf("post-save aggregate files exceed %d bytes", MaxDryRunLastGoodBytes)}
	}
	if totalNodes > MaxDryRunLastGoodNodes {
		return &TopologyResourceLimitError{Reason: fmt.Sprintf("post-save node count exceeds %d", MaxDryRunLastGoodNodes)}
	}
	return nil
}

func (s *LastGoodStore) LoadAll(ctx context.Context) (map[string]LastGoodEnvelope, error) {
	out := map[string]LastGoodEnvelope{}
	entries, err := os.ReadDir(s.layout.LastGoodDir())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	actualTotal := 0
	nodes := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(s.layout.LastGoodDir(), entry.Name())
		body, err := readLastGoodBodyBounded(path)
		if err != nil {
			return nil, err
		}
		actualTotal += len(body)
		if actualTotal > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("aggregate files exceed %d bytes", MaxDryRunLastGoodBytes)}
		}
		env, err := decodeLastGoodBody(providerID, body)
		if err != nil {
			return nil, err
		}
		nodes += len(env.Identities)
		if nodes > MaxDryRunLastGoodNodes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
		}
		out[providerID] = env
	}
	return out, nil
}

func (s *LastGoodStore) ReplaceAll(ctx context.Context, updates []LastGoodUpdate) (map[string]LastGoodEnvelope, error) {
	var err error
	updates, err = NormalizeLastGoodUpdates(updates)
	if err != nil {
		return nil, err
	}
	if err := validateLastGoodUpdates(updates); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.layout.LastGoodDir(), 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.layout.LastGoodDir())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := os.Remove(filepath.Join(s.layout.LastGoodDir(), entry.Name())); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return s.SaveMany(ctx, updates)
}

func validateLastGoodUpdates(updates []LastGoodUpdate) error {
	seen := make(map[string]struct{}, len(updates))
	totalNodes := 0
	totalEncoded := 0
	for _, update := range updates {
		if err := ValidateProviderID(update.ProviderID); err != nil {
			return err
		}
		if len(update.Identities) == 0 {
			return fmt.Errorf("last-good for provider %s cannot be empty", update.ProviderID)
		}
		if _, exists := seen[update.ProviderID]; exists {
			return fmt.Errorf("duplicate last-good update for provider %s", update.ProviderID)
		}
		if len(update.Outbounds) > 0 {
			if err := validateIdentitySources(update.ProviderID, update.Outbounds, update.Identities); err != nil {
				return fmt.Errorf("last-good provider %s: %w", update.ProviderID, err)
			}
		}
		seen[update.ProviderID] = struct{}{}
		totalNodes += len(update.Identities)
		if totalNodes > MaxDryRunLastGoodNodes {
			return &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
		}
		body, err := json.MarshalIndent(LastGoodEnvelope{ProviderID: update.ProviderID, Subscription: update.Subscription, Identities: update.Identities}, "", "  ")
		if err != nil {
			return err
		}
		totalEncoded += len(body)
		if totalEncoded > MaxDryRunLastGoodBytes {
			return &TopologyResourceLimitError{Reason: fmt.Sprintf("aggregate encoded last-good exceeds %d bytes", MaxDryRunLastGoodBytes)}
		}
	}
	return nil
}

func (s *LastGoodStore) PruneExcept(ctx context.Context, keep map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(s.layout.LastGoodDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var removed []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		if _, ok := keep[providerID]; ok {
			continue
		}
		if err := ValidateProviderID(providerID); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(s.layout.LastGoodDir(), entry.Name())); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed = append(removed, providerID)
	}
	return removed, nil
}

func (s *LastGoodStore) Load(providerID string) (LastGoodEnvelope, error) {
	path, err := s.Path(providerID)
	if err != nil {
		return LastGoodEnvelope{}, err
	}
	b, err := readLastGoodBodyBounded(path)
	if err != nil {
		return LastGoodEnvelope{}, err
	}
	return decodeLastGoodBody(providerID, b)
}

func readLastGoodBodyBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxDryRunLastGoodBytes {
		return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("%s is %d bytes", filepath.Base(path), info.Size())}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, MaxDryRunLastGoodBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > MaxDryRunLastGoodBytes {
		return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("%s exceeds %d bytes", filepath.Base(path), MaxDryRunLastGoodBytes)}
	}
	return body, nil
}

func decodeLastGoodBody(providerID string, b []byte) (LastGoodEnvelope, error) {
	var env LastGoodEnvelope
	if err := json.Unmarshal(b, &env); err == nil && env.ProviderID != "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			return LastGoodEnvelope{}, err
		}
		if env.ProviderID != providerID {
			return LastGoodEnvelope{}, fmt.Errorf("last-good provider id mismatch for %s", providerID)
		}
		if len(env.Outbounds) > MaxDryRunLastGoodNodes {
			return LastGoodEnvelope{}, &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
		}
		if _, identitiesPresent := fields["identities"]; !identitiesPresent {
			identities, err := identitySourcesFromOutbounds(providerID, env.Outbounds)
			if err != nil {
				return LastGoodEnvelope{}, err
			}
			env.Identities = identities
		}
		if len(env.Outbounds) > 0 {
			if err := validateIdentitySources(providerID, env.Outbounds, env.Identities); err != nil {
				return LastGoodEnvelope{}, fmt.Errorf("last-good provider %s: %w", providerID, err)
			}
		}
		env.Count = len(env.Identities)
		return env, nil
	}
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return LastGoodEnvelope{}, err
	}
	if len(raw) > MaxDryRunLastGoodNodes {
		return LastGoodEnvelope{}, &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
	}
	identities, err := identitySourcesFromOutbounds(providerID, raw)
	if err != nil {
		return LastGoodEnvelope{}, err
	}
	return LastGoodEnvelope{
		ProviderID: providerID,
		Count:      len(raw),
		Identities: identities,
		Outbounds:  raw,
	}, nil
}

// NormalizeLastGoodUpdates promotes restore/recovery input that predates the
// identities field and removes the legacy representation after validation.
func NormalizeLastGoodUpdates(updates []LastGoodUpdate) ([]LastGoodUpdate, error) {
	normalized := make([]LastGoodUpdate, len(updates))
	copy(normalized, updates)
	for index := range normalized {
		if normalized[index].Identities == nil {
			identities, err := identitySourcesFromOutbounds(normalized[index].ProviderID, normalized[index].Outbounds)
			if err != nil {
				return nil, err
			}
			normalized[index].Identities = identities
		}
		if len(normalized[index].Outbounds) > 0 {
			if err := validateIdentitySources(normalized[index].ProviderID, normalized[index].Outbounds, normalized[index].Identities); err != nil {
				return nil, fmt.Errorf("last-good provider %s: %w", normalized[index].ProviderID, err)
			}
		}
		normalized[index].Outbounds = nil
	}
	return normalized, nil
}

func NormalizeLastGoodSnapshots(snapshots []state.LastGoodSnapshot) ([]state.LastGoodSnapshot, error) {
	updates := make([]LastGoodUpdate, len(snapshots))
	for index, snapshot := range snapshots {
		updates[index] = LastGoodUpdate{
			ProviderID:   snapshot.ProviderID,
			Subscription: snapshot.Subscription,
			Identities:   snapshot.Identities,
			Outbounds:    snapshot.Outbounds,
		}
	}
	normalized, err := NormalizeLastGoodUpdates(updates)
	if err != nil {
		return nil, err
	}
	out := make([]state.LastGoodSnapshot, len(normalized))
	for index, update := range normalized {
		out[index] = state.LastGoodSnapshot{
			ProviderID:   update.ProviderID,
			SavedAt:      snapshots[index].SavedAt,
			Subscription: update.Subscription,
			Identities:   update.Identities,
		}
	}
	return out, nil
}

func LastGoodEnvelopeFromSnapshot(snapshot state.LastGoodSnapshot) LastGoodEnvelope {
	return LastGoodEnvelope{
		ProviderID:   snapshot.ProviderID,
		SavedAt:      snapshot.SavedAt,
		Count:        len(snapshot.Identities),
		Subscription: snapshot.Subscription,
		Identities:   snapshot.Identities,
	}
}

func identitySourcesFromOutbounds(providerID string, outbounds []map[string]any) ([]smartnode.IdentitySource, error) {
	identities := make([]smartnode.IdentitySource, 0, len(outbounds))
	for _, outbound := range outbounds {
		tag := outboundString(outbound, "tag")
		source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
			ProviderID: providerID,
			RawTag:     tag,
			Outbound:   outbound,
		})
		if err != nil {
			return nil, fmt.Errorf("build identity for %s: %w", tag, err)
		}
		identities = append(identities, source)
	}
	return identities, nil
}

func validateIdentitySources(providerID string, outbounds []map[string]any, identities []smartnode.IdentitySource) error {
	if len(identities) != len(outbounds) {
		return fmt.Errorf("identities count %d does not match outbounds count %d", len(identities), len(outbounds))
	}
	for index, source := range identities {
		expected, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
			ProviderID: providerID,
			RawTag:     outboundString(outbounds[index], "tag"),
			Outbound:   outbounds[index],
		})
		if err != nil {
			return fmt.Errorf("build identity %d: %w", index, err)
		}
		if !reflect.DeepEqual(source.Identity, expected.Identity) {
			return fmt.Errorf("identity %d does not match outbound semantics", index)
		}
		if source.RawTag != expected.RawTag {
			return fmt.Errorf("identity %d tag %q does not match outbound tag %q", index, source.RawTag, expected.RawTag)
		}
		if !equalReferenceMaps(source.References, expected.References) {
			return fmt.Errorf("identity %d references do not match outbound source", index)
		}
	}
	return nil
}

func equalReferenceMaps(left, right map[string]json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftBody := range left {
		rightBody, exists := right[key]
		if !exists {
			return false
		}
		var leftValue, rightValue any
		if json.Unmarshal(leftBody, &leftValue) != nil || json.Unmarshal(rightBody, &rightValue) != nil || !reflect.DeepEqual(leftValue, rightValue) {
			return false
		}
	}
	return true
}

func ParseSubscriptionPayload(providerID string, body []byte) (ParsedSubscription, error) {
	if err := ValidateProviderID(providerID); err != nil {
		return ParsedSubscription{}, err
	}
	if len(body) > MaxSubscriptionBodyBytes {
		return ParsedSubscription{}, fmt.Errorf("subscription body too large: %d > %d", len(body), MaxSubscriptionBodyBytes)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ParsedSubscription{}, fmt.Errorf("subscription body is empty")
	}
	prefix := strings.ToLower(string(body[:min(len(body), 128)]))
	if strings.HasPrefix(prefix, "<") || strings.Contains(prefix, "<html") {
		return ParsedSubscription{}, fmt.Errorf("subscription body looks like html")
	}

	var list []map[string]any
	if err := json.Unmarshal(body, &list); err == nil {
		return admittedIdentitySources(providerID, list)
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		parsed, uriErr := parseURIListPayload(providerID, body)
		if uriErr == nil {
			return parsed, nil
		}
		parsed, ecosystemErr := parseYAMLSubscriptionPayload(providerID, body)
		if ecosystemErr == nil {
			return parsed, nil
		}
		return ParsedSubscription{}, fmt.Errorf("subscription body is neither supported JSON, URI list, nor ecosystem YAML: json=%v uri=%v yaml=%v", err, uriErr, ecosystemErr)
	}
	if raw, ok := obj["outbounds"].([]any); ok {
		for _, item := range raw {
			if outbound, ok := item.(map[string]any); ok {
				list = append(list, outbound)
			}
		}
		return admittedIdentitySources(providerID, list)
	}
	if _, hasType := obj["type"].(string); hasType {
		return admittedIdentitySources(providerID, []map[string]any{obj})
	}
	return ParsedSubscription{}, fmt.Errorf("subscription JSON does not contain outbounds")
}

func admittedIdentitySources(providerID string, in []map[string]any) (ParsedSubscription, error) {
	parsed := ParsedSubscription{
		Subscription: ParseSubscriptionInfoFromOutbounds(in, time.Now()),
		Identities:   make([]smartnode.IdentitySource, 0, len(in)),
	}
	for _, ob := range in {
		tag, _ := ob["tag"].(string)
		if strings.TrimSpace(tag) == "" {
			continue
		}
		if outboundString(ob, "server") == "" {
			continue
		}
		node, source, err := smartnode.NormalizeWithSource(smartnode.RawNode{
			ProviderID: providerID,
			RawTag:     strings.TrimSpace(tag),
			Outbound:   ob,
		})
		if err != nil {
			return ParsedSubscription{}, fmt.Errorf("normalize subscription node %q: %w", tag, err)
		}
		if !hasAdmissibleCore(node.Capability) {
			continue
		}
		parsed.Identities = append(parsed.Identities, source)
	}
	if len(parsed.Identities) == 0 {
		return ParsedSubscription{}, fmt.Errorf("subscription contains no supported node identities")
	}
	return parsed, nil
}

func hasAdmissibleCore(matrix smartnode.CapabilityMatrix) bool {
	for _, capability := range matrix {
		if capability.Status == smartnode.CapabilitySupported || capability.Status == smartnode.CapabilityDegraded {
			return true
		}
	}
	return false
}
