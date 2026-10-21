package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smartgate/internal/state"
)

func TestLoadLastGoodSnapshotsBoundedRejectsLargeFileBeforeDecode(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.LastGoodDir(), "p1.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxDryRunLastGoodBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadLastGoodSnapshotsBounded(context.Background(), NewLastGoodStore(layout))
	var limitErr *TopologyResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("large file was decoded instead of rejected by stat gate: %T %v", err, err)
	}
}

func TestLoadLastGoodSnapshotsBoundedSupportsLegacyArray(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "p1.json"), []byte(`[{"type":"trojan","tag":"legacy","server":"example.com"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := LoadLastGoodSnapshotsBounded(context.Background(), NewLastGoodStore(layout))
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Outbounds) != 0 || len(snapshots[0].Identities) != 1 || snapshots[0].Identities[0].RawTag != "legacy" {
		t.Fatalf("legacy last-good was not promoted to identity-only: snapshots=%+v err=%v", snapshots, err)
	}
}

func TestHashEffectiveTopologyRejectsOversizedInMemorySnapshotsBeforeMarshal(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Enabled: true}}
	snapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"type": "trojan", "tag": "large", "server": string(bytes.Repeat([]byte("s"), MaxDryRunLastGoodBytes))}}}}
	_, err := HashEffectiveTopologyFromSnapshots(spec, snapshots)
	var limitErr *TopologyResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized topology reached whole-object marshal: %T %v", err, err)
	}
}

func TestHashEffectiveTopologyFromSnapshotsIsStableAndContentSensitive(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "p1", Name: "Provider 1", Type: TypeSubscription, URL: "https://p1.example/sub", Enabled: true},
		{ID: "p2", Name: "Provider 2", Type: TypeSubscription, URL: "https://p2.example/sub", Enabled: true},
		{ID: "disabled", Name: "Disabled", Type: TypeSubscription, URL: "https://disabled.example/sub", Enabled: false},
	}
	first := []state.LastGoodSnapshot{
		{ProviderID: "p2", Subscription: &state.SubscriptionInfo{UpdatedAt: time.Now()}, Outbounds: []map[string]any{{"type": "vmess", "tag": "two", "server": "two.example"}}},
		{ProviderID: "disabled", Outbounds: []map[string]any{{"type": "vmess", "tag": "ignored", "server": "ignored.example"}}},
		{ProviderID: "p1", Outbounds: []map[string]any{{"type": "vmess", "tag": "one", "server": "one.example"}}},
	}
	second := []state.LastGoodSnapshot{
		{ProviderID: "p1", Subscription: &state.SubscriptionInfo{Raw: "different metadata"}, Outbounds: []map[string]any{{"type": "vmess", "tag": "one", "server": "one.example"}}},
		{ProviderID: "p2", Outbounds: []map[string]any{{"type": "vmess", "tag": "two", "server": "two.example"}}},
	}
	hash1, err := HashEffectiveTopologyFromSnapshots(spec, first)
	if err != nil {
		t.Fatal(err)
	}
	hash2, err := HashEffectiveTopologyFromSnapshots(spec, second)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("order or subscription metadata changed hash: %s != %s", hash1, hash2)
	}
	second[1].Outbounds[0]["server"] = "changed.example"
	changed, err := HashEffectiveTopologyFromSnapshots(spec, second)
	if err != nil {
		t.Fatal(err)
	}
	if changed == hash1 {
		t.Fatal("outbound content should change topology hash")
	}
}

func TestHashEffectiveTopologyIgnoresProviderAndAliasRename(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Old Provider", Type: TypeSubscription, URL: "https://provider.example/sub", Enabled: true}}
	firstSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{
		"type": "vless", "tag": "old-name", "server": "node.example", "server_port": 443, "uuid": "secret",
	}}}}
	reconciled, _, err := ReconcileSmartNodes(spec, firstSnapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	firstHash, err := HashEffectiveTopologyFromSnapshots(reconciled, firstSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	reconciled.Providers[0].Name = "New Provider"
	renamedSnapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{
		"type": "vless", "tag": "new-name", "server": "node.example", "server_port": 443, "uuid": "secret",
	}}}}
	renamed, report, err := ReconcileSmartNodes(reconciled, renamedSnapshots, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Revised) != 0 || len(report.MetadataOnly) != 1 {
		t.Fatalf("rename should be metadata-only: %+v", report)
	}
	renamedHash, err := HashEffectiveTopologyFromSnapshots(renamed, renamedSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	if renamedHash != firstHash {
		t.Fatalf("provider/alias rename changed execution hash: %s != %s", firstHash, renamedHash)
	}
}

func TestHashEffectiveTopologyFromSnapshotsRejectsDuplicates(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeSubscription, URL: "https://p1.example/sub", Enabled: true}}
	_, err := HashEffectiveTopologyFromSnapshots(spec, []state.LastGoodSnapshot{{ProviderID: "p1"}, {ProviderID: "p1"}})
	if err == nil {
		t.Fatal("expected duplicate last-good snapshot to fail")
	}
}

func TestHashEffectiveTopologyUsesSnapshotHasher(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: TypeSubscription, URL: "https://p1.example/sub", Enabled: true}}
	store := NewLastGoodStore(layout)
	if _, err := store.Save("p1", []map[string]any{{"type": "vmess", "tag": "one", "server": "one.example"}}); err != nil {
		t.Fatal(err)
	}
	fromDisk, err := HashEffectiveTopology(context.Background(), spec, store)
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshots, err := HashEffectiveTopologyFromSnapshots(spec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"type": "vmess", "tag": "one", "server": "one.example"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if fromDisk != fromSnapshots {
		t.Fatalf("disk and snapshot topology hashes differ: %s != %s", fromDisk, fromSnapshots)
	}
}

func TestExecutionTopologyIgnoresNoticesButKeepsRealChanges(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p", Name: "P", Type: TypeManual, Enabled: true}}
	snapshot := func(notice string, port int) []state.LastGoodSnapshot {
		return []state.LastGoodSnapshot{{ProviderID: "p", Outbounds: []map[string]any{
			{"tag": "real", "type": "trojan", "server": "example.com", "server_port": port, "password": "fixture-secret"},
			{"tag": notice, "type": "anytls", "server": "notice.invalid", "server_port": 1},
		}}}
	}
	old, next := snapshot("剩余流量：68 GB", 443), snapshot("剩余流量：67 GB", 443)
	before, err := HashEffectiveTopologyFromSnapshots(spec, old)
	if err != nil {
		t.Fatal(err)
	}
	after, err := HashEffectiveTopologyFromSnapshots(spec, next)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("notice alone marked operational topology changed")
	}
	changed, err := HashEffectiveTopologyFromSnapshots(spec, snapshot("剩余流量：67 GB", 8443))
	if err != nil {
		t.Fatal(err)
	}
	if before == changed {
		t.Fatal("real port change was suppressed")
	}
	next[0].Outbounds[0]["password"] = "changed-secret"
	changed, err = HashEffectiveTopologyFromSnapshots(spec, next)
	if err != nil {
		t.Fatal(err)
	}
	if before == changed {
		t.Fatal("credential change was suppressed")
	}
}
