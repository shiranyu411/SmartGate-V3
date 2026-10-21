package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/business"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestCreateAndParseBackup(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{"n1"}}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{SchemaVersion: 1, ConfigEpoch: 7, TopologyHash: topologyHash, ProxySelection: map[string]string{"ai": "n1"}}, spec, []state.LastGoodSnapshot{{
		ProviderID: "p1",
		Outbounds:  []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}},
	}})
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatalf("save applied: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 7, TopologyHash: topologyHash, ProxySelection: map[string]string{"ai": "runtime-stale"}}); err != nil {
		t.Fatal(err)
	}
	archivedSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 7, TopologyHash: topologyHash,
		ProxySelection: map[string]string{"ai": "n1"},
		Selections: map[string]state.PoolSelection{
			"ai": {SmartNode: "n1", Target: "node", Generation: 11, BoundEpoch: 7, BoundTopologyHash: topologyHash},
		},
	}
	if err := store.SaveSelectionState(archivedSelection); err != nil {
		t.Fatal(err)
	}
	if err := state.NewFailoverStateStore(layout).Save(state.FailoverState{Enabled: false, Locks: map[string]state.FailoverLock{}, Pools: map[string]state.FailoverPoolState{}}); err != nil {
		t.Fatalf("save failover: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}

	record, err := Create(context.Background(), layout, store, "manual")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if record.Size == 0 || record.SHA256 == "" {
		t.Fatalf("backup record missing size/hash: %+v", record)
	}
	info, err := os.Stat(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %o, want 600", got)
	}
	parsed, err := Parse(context.Background(), layout, record.Path, DefaultMaxFileBytes)
	if err != nil {
		t.Fatalf("parse backup: %v", err)
	}
	if parsed.Spec.BusinessPools[0].ID != "ai" || len(parsed.LastGood) != 1 || !parsed.AppliedCaptured || !parsed.SelectionCaptured || !parsed.FailoverCaptured {
		t.Fatalf("unexpected parsed payload: %+v", parsed)
	}
	if parsed.Applied.ConfigEpoch != 7 || parsed.Failover.Enabled {
		t.Fatalf("backup did not preserve applied/failover state: applied=%+v failover=%+v", parsed.Applied, parsed.Failover)
	}
	if len(parsed.Applied.LastGood) != 1 || len(parsed.Applied.LastGood[0].Identities) != 1 || len(parsed.Applied.LastGood[0].Outbounds) != 0 {
		t.Fatalf("backup did not migrate applied last-good to identities: %+v", parsed.Applied.LastGood)
	}
	appliedJSON := readBackupEntry(t, record.Path, "applied.json")
	if strings.Contains(string(appliedJSON), `"outbounds"`) {
		t.Fatalf("new backup rewrote legacy applied outbounds: %s", appliedJSON)
	}
	if parsed.Selection.Selections["ai"] != archivedSelection.Selections["ai"] || parsed.Runtime.ProxySelection["ai"] != "n1" {
		t.Fatalf("backup did not preserve durable Selection: selection=%+v runtime=%+v", parsed.Selection, parsed.Runtime)
	}
}

func readBackupEntry(t *testing.T, path, name string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			t.Fatalf("backup entry %s is missing", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != name {
			continue
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
}

func TestCreateRejectsOversizedLastGoodBeforeArchive(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if _, err := store.SaveSpec(state.DefaultSpecState()); err != nil {
		t.Fatal(err)
	}
	lastGoodPath := filepath.Join(layout.LastGoodDir(), "p1.json")
	if err := os.WriteFile(lastGoodPath, bytes.Repeat([]byte("x"), provider.MaxDryRunLastGoodBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), layout, store, "manual"); err == nil {
		t.Fatal("oversized last-good input was archived")
	}
	entries, err := os.ReadDir(layout.BackupsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed backup left artifacts: %+v", entries)
	}
}

func TestParseRejectsMetadataAndLastGoodIdentityMismatch(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec := state.DefaultSpecState()
	meta := Metadata{SchemaVersion: SchemaVersion, CreatedAt: time.Now().UTC(), Kind: "manual", SpecHash: "wrong", BusinessPoolsHash: business.HashPools(spec.BusinessPools), LastGoodCount: 0}
	path := filepath.Join(layout.BackupsDir(), "bad-metadata.tar.gz")
	if err := writeBundle(context.Background(), path, meta, spec, state.RuntimeState{}, state.AppliedState{SchemaVersion: 1}, state.NormalizeFailoverState(state.FailoverState{}), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(context.Background(), layout, path, DefaultMaxFileBytes); err == nil || !strings.Contains(err.Error(), "spec hash") {
		t.Fatalf("metadata mismatch error=%v", err)
	}

	env, err := json.Marshal(provider.LastGoodEnvelope{ProviderID: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	meta.SpecHash = provider.HashSpec(spec)
	meta.LastGoodCount = 1
	path = filepath.Join(layout.BackupsDir(), "bad-provider-id.tar.gz")
	if err := writeBundle(context.Background(), path, meta, spec, state.RuntimeState{}, state.AppliedState{SchemaVersion: 1}, state.NormalizeFailoverState(state.FailoverState{}), map[string][]byte{"p1": env}); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(context.Background(), layout, path, DefaultMaxFileBytes); err == nil || !strings.Contains(err.Error(), "provider_id") {
		t.Fatalf("provider identity mismatch error=%v", err)
	}
}

func TestParseRejectsInvalidCurrentSchemaControlState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec := state.DefaultSpecState()
	meta := Metadata{
		SchemaVersion: SchemaVersion, CreatedAt: time.Now().UTC(), Kind: "manual",
		SpecHash: provider.HashSpec(spec), BusinessPoolsHash: business.HashPools(spec.BusinessPools), LastGoodCount: 0,
	}
	path := filepath.Join(layout.BackupsDir(), "invalid-control-state.tar.gz")
	if err := writeBundle(context.Background(), path, meta, spec, state.RuntimeState{}, state.AppliedState{}, state.FailoverState{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(context.Background(), layout, path, DefaultMaxFileBytes); err == nil || !strings.Contains(err.Error(), "applied state") {
		t.Fatalf("invalid current-schema control state error=%v", err)
	}
}

func TestParseRejectsOversizedFileBeforeRestore(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	path := filepath.Join(layout.BackupsDir(), "oversized.tar.gz")
	if err := writeOversizedBackup(path); err != nil {
		t.Fatalf("write oversized backup: %v", err)
	}
	if _, err := Parse(context.Background(), layout, path, 8); err == nil {
		t.Fatal("expected oversized file to be rejected")
	}
}

func TestPruneScheduledKeepsNewestAndNeverDeletesManual(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scheduled-9.tar.gz", "scheduled-10.tar.gz", "scheduled-11.tar.gz", "manual-1.tar.gz", "scheduled-not-a-time.tar.gz"} {
		if err := os.WriteFile(filepath.Join(layout.BackupsDir(), name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneScheduled(layout, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "scheduled-9.tar.gz" {
		t.Fatalf("unexpected removed backups: %+v", removed)
	}
	for _, name := range []string{"scheduled-10.tar.gz", "scheduled-11.tar.gz", "manual-1.tar.gz", "scheduled-not-a-time.tar.gz"} {
		if _, err := os.Stat(filepath.Join(layout.BackupsDir(), name)); err != nil {
			t.Fatalf("backup %s should remain: %v", name, err)
		}
	}
}

func writeOversizedBackup(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	body := []byte(`{"schema_version":1}`)
	if err := tw.WriteHeader(&tar.Header{Name: "metadata.json", Mode: 0o600, Size: int64(len(body))}); err != nil {
		return err
	}
	_, err = tw.Write(body)
	return err
}
