package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/business"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

const (
	SchemaVersion             = 3
	ControlStateSchemaVersion = 2
	LegacySchemaVersion       = 1
	DefaultMaxFileBytes       = 10 << 20
	DefaultMaxTotalBytes      = 64 << 20
)

type Metadata struct {
	SchemaVersion     int       `json:"schema_version"`
	CreatedAt         time.Time `json:"created_at"`
	Kind              string    `json:"kind"`
	SpecHash          string    `json:"spec_hash"`
	BusinessPoolsHash string    `json:"business_pools_hash"`
	LastGoodCount     int       `json:"last_good_count"`
}

func PruneScheduled(layout state.Layout, keep int) ([]string, error) {
	if keep < 0 {
		return nil, fmt.Errorf("scheduled backup retention cannot be negative")
	}
	entries, err := os.ReadDir(layout.BackupsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && scheduledBackupName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, _ := scheduledBackupTimestamp(names[i])
		right, _ := scheduledBackupTimestamp(names[j])
		return left < right
	})
	if len(names) <= keep {
		return nil, nil
	}
	remove := names[:len(names)-keep]
	removed := make([]string, 0, len(remove))
	for _, name := range remove {
		path := filepath.Join(layout.BackupsDir(), name)
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	if len(removed) > 0 {
		if err := syncBackupDir(layout.BackupsDir()); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func scheduledBackupName(name string) bool {
	_, ok := scheduledBackupTimestamp(name)
	return ok
}

func scheduledBackupTimestamp(name string) (int64, bool) {
	if !strings.HasPrefix(name, "scheduled-") || !strings.HasSuffix(name, ".tar.gz") {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, "scheduled-"), ".tar.gz")
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, raw != "" && err == nil
}

func syncBackupDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type Record struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
}

type Payload struct {
	Metadata          Metadata
	Spec              state.SpecState
	Runtime           state.RuntimeState
	Applied           state.AppliedState
	Selection         state.SelectionState
	Failover          state.FailoverState
	LastGood          map[string]json.RawMessage
	AppliedCaptured   bool
	SelectionCaptured bool
	FailoverCaptured  bool
	RuntimeCaptured   bool
}

func Create(ctx context.Context, layout state.Layout, store *state.Store, kind string) (Record, error) {
	if kind == "" {
		kind = "manual"
	}
	spec, err := store.LoadSpec()
	if err != nil {
		return Record{}, err
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		return Record{}, err
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		return Record{}, err
	}
	appliedState.LastGood, err = provider.NormalizeLastGoodSnapshots(appliedState.LastGood)
	if err != nil {
		return Record{}, err
	}
	if appliedState.SchemaVersion == 0 {
		appliedState = state.NormalizeAppliedState(appliedState)
	}
	var selection state.SelectionState
	if appliedState.SchemaVersion != 0 && appliedState.SpecCaptured {
		selection, err = store.LoadSelectionForApplied(appliedState)
		if err != nil {
			return Record{}, err
		}
		runtimeState.ProxySelection = selection.ProxySelection
	}
	failoverState, err := state.NewFailoverStateStore(layout).Load()
	if err != nil {
		return Record{}, err
	}
	lastGood, err := readLastGood(ctx, layout)
	if err != nil {
		return Record{}, err
	}
	meta := Metadata{
		SchemaVersion:     SchemaVersion,
		CreatedAt:         time.Now().UTC(),
		Kind:              kind,
		SpecHash:          provider.HashSpec(spec),
		BusinessPoolsHash: business.HashPools(spec.BusinessPools),
		LastGoodCount:     len(lastGood),
	}
	id := fmt.Sprintf("%s-%d", kind, meta.CreatedAt.UnixNano())
	path := filepath.Join(layout.BackupsDir(), id+".tar.gz")
	tmp := path + ".tmp"
	if err := os.MkdirAll(layout.BackupsDir(), 0o755); err != nil {
		return Record{}, err
	}
	if err := writeBundleWithSelection(ctx, tmp, meta, spec, runtimeState, appliedState, selection, failoverState, lastGood); err != nil {
		_ = os.Remove(tmp)
		return Record{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Record{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return Record{}, err
	}
	if err := syncBackupDir(layout.BackupsDir()); err != nil {
		return Record{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Record{}, err
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return Record{}, err
	}
	return Record{ID: id, Path: path, Size: info.Size(), SHA256: sum, Metadata: meta, CreatedAt: meta.CreatedAt}, nil
}

func Parse(ctx context.Context, layout state.Layout, path string, maxFileBytes int64) (Payload, error) {
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultMaxFileBytes
	}
	safePath, err := safeBackupPath(layout, path)
	if err != nil {
		return Payload{}, err
	}
	f, err := os.Open(safePath)
	if err != nil {
		return Payload{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Payload{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	payload := Payload{LastGood: map[string]json.RawMessage{}}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return Payload{}, err
		}
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Payload{}, err
		}
		if header.Typeflag != tar.TypeReg {
			return Payload{}, fmt.Errorf("backup contains unsupported entry type for %s", header.Name)
		}
		name, ok := safeEntryName(header.Name)
		if !ok {
			return Payload{}, fmt.Errorf("backup contains unsafe path %q", header.Name)
		}
		if header.Size > maxFileBytes {
			return Payload{}, fmt.Errorf("backup file %s too large: %d > %d", name, header.Size, maxFileBytes)
		}
		total += header.Size
		if total > DefaultMaxTotalBytes {
			return Payload{}, fmt.Errorf("backup total size too large: %d > %d", total, DefaultMaxTotalBytes)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return Payload{}, err
		}
		if int64(len(body)) > maxFileBytes {
			return Payload{}, fmt.Errorf("backup file %s too large", name)
		}
		if err := payload.accept(name, body); err != nil {
			return Payload{}, err
		}
	}
	if payload.Metadata.SchemaVersion != SchemaVersion && payload.Metadata.SchemaVersion != ControlStateSchemaVersion && payload.Metadata.SchemaVersion != LegacySchemaVersion {
		return Payload{}, fmt.Errorf("unsupported backup schema version %d", payload.Metadata.SchemaVersion)
	}
	if payload.Spec.SchemaVersion == 0 {
		return Payload{}, fmt.Errorf("backup missing spec.json")
	}
	if provider.HashSpec(payload.Spec) != payload.Metadata.SpecHash {
		return Payload{}, fmt.Errorf("backup spec hash does not match metadata")
	}
	if business.HashPools(payload.Spec.BusinessPools) != payload.Metadata.BusinessPoolsHash {
		return Payload{}, fmt.Errorf("backup business pools hash does not match metadata")
	}
	if len(payload.LastGood) != payload.Metadata.LastGoodCount {
		return Payload{}, fmt.Errorf("backup last-good count does not match metadata")
	}
	if payload.Metadata.SchemaVersion >= ControlStateSchemaVersion && (!payload.AppliedCaptured || !payload.FailoverCaptured) {
		return Payload{}, fmt.Errorf("backup schema %d missing applied or failover state", payload.Metadata.SchemaVersion)
	}
	if payload.Metadata.SchemaVersion >= ControlStateSchemaVersion && !payload.RuntimeCaptured {
		return Payload{}, fmt.Errorf("backup schema %d missing runtime state", payload.Metadata.SchemaVersion)
	}
	if payload.Metadata.SchemaVersion == SchemaVersion && payload.AppliedCaptured && payload.Applied.SpecCaptured && !payload.SelectionCaptured {
		return Payload{}, fmt.Errorf("backup schema %d missing selection state", SchemaVersion)
	}
	if payload.AppliedCaptured && payload.Applied.SchemaVersion == 0 {
		return Payload{}, fmt.Errorf("backup applied state has no schema version")
	}
	if payload.SelectionCaptured {
		if payload.Selection.SchemaVersion == 0 {
			return Payload{}, fmt.Errorf("backup selection state has no schema version")
		}
		if payload.AppliedCaptured && payload.Applied.SpecCaptured && !state.SelectionStateMatchesApplied(payload.Selection, payload.Applied) {
			return Payload{}, fmt.Errorf("backup selection state does not match applied topology")
		}
	}
	if payload.FailoverCaptured && payload.Failover.CooldownSeconds <= 0 {
		return Payload{}, fmt.Errorf("backup failover state has invalid cooldown")
	}
	return payload, nil
}

func (p *Payload) accept(name string, body []byte) error {
	switch {
	case name == "metadata.json":
		if p.Metadata.SchemaVersion != 0 {
			return fmt.Errorf("backup contains duplicate metadata.json")
		}
		return json.Unmarshal(body, &p.Metadata)
	case name == "spec.json":
		if p.Spec.SchemaVersion != 0 {
			return fmt.Errorf("backup contains duplicate spec.json")
		}
		return json.Unmarshal(body, &p.Spec)
	case name == "runtime.json":
		if p.RuntimeCaptured {
			return fmt.Errorf("backup contains duplicate runtime.json")
		}
		p.RuntimeCaptured = true
		return json.Unmarshal(body, &p.Runtime)
	case name == "applied.json":
		if p.AppliedCaptured {
			return fmt.Errorf("backup contains duplicate applied.json")
		}
		p.AppliedCaptured = true
		return json.Unmarshal(body, &p.Applied)
	case name == "selection.json":
		if p.SelectionCaptured {
			return fmt.Errorf("backup contains duplicate selection.json")
		}
		p.SelectionCaptured = true
		return json.Unmarshal(body, &p.Selection)
	case name == "failover.json":
		if p.FailoverCaptured {
			return fmt.Errorf("backup contains duplicate failover.json")
		}
		p.FailoverCaptured = true
		return json.Unmarshal(body, &p.Failover)
	case strings.HasPrefix(name, "lastgood/") && strings.HasSuffix(name, ".json"):
		providerID := strings.TrimSuffix(strings.TrimPrefix(name, "lastgood/"), ".json")
		if err := provider.ValidateProviderID(providerID); err != nil {
			return err
		}
		var env provider.LastGoodEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return err
		}
		if env.ProviderID != providerID {
			return fmt.Errorf("last-good entry %s contains provider_id %q", name, env.ProviderID)
		}
		p.LastGood[providerID] = append(json.RawMessage(nil), body...)
		return nil
	default:
		return fmt.Errorf("backup contains unexpected file %s", name)
	}
}

func writeBundle(ctx context.Context, path string, meta Metadata, spec state.SpecState, runtimeState state.RuntimeState, applied state.AppliedState, failover state.FailoverState, lastGood map[string][]byte) error {
	return writeBundleWithSelection(ctx, path, meta, spec, runtimeState, applied, state.SelectionState{}, failover, lastGood)
}

func writeBundleWithSelection(ctx context.Context, path string, meta Metadata, spec state.SpecState, runtimeState state.RuntimeState, applied state.AppliedState, selection state.SelectionState, failover state.FailoverState, lastGood map[string][]byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()
	if err := addJSON(ctx, tw, "metadata.json", meta); err != nil {
		return err
	}
	if err := addJSON(ctx, tw, "spec.json", spec); err != nil {
		return err
	}
	if err := addJSON(ctx, tw, "runtime.json", runtimeState); err != nil {
		return err
	}
	if err := addJSON(ctx, tw, "applied.json", applied); err != nil {
		return err
	}
	if selection.SchemaVersion != 0 {
		if err := addJSON(ctx, tw, "selection.json", selection); err != nil {
			return err
		}
	}
	if err := addJSON(ctx, tw, "failover.json", failover); err != nil {
		return err
	}
	for providerID, body := range lastGood {
		if err := addBytes(ctx, tw, "lastgood/"+providerID+".json", body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func addJSON(ctx context.Context, tw *tar.Writer, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return addBytes(ctx, tw, name, b)
}

func addBytes(ctx context.Context, tw *tar.Writer, name string, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func readLastGood(ctx context.Context, layout state.Layout) (map[string][]byte, error) {
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(layout))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(snapshots))
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		envelope := provider.LastGoodEnvelopeFromSnapshot(snapshot)
		body, err := json.MarshalIndent(envelope, "", "  ")
		if err != nil {
			return nil, err
		}
		out[snapshot.ProviderID] = body
	}
	return out, nil
}

func safeBackupPath(layout state.Layout, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("backup path is required")
	}
	clean := filepath.Clean(raw)
	root := filepath.Clean(layout.BackupsDir())
	rel, err := filepath.Rel(root, clean)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("backup path must be inside SmartGate backups directory")
	}
	if filepath.Ext(clean) != ".gz" {
		return "", fmt.Errorf("backup path must be a .tar.gz file")
	}
	return clean, nil
}

func safeEntryName(name string) (string, bool) {
	clean := filepath.Clean(name)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", false
	}
	return clean, clean == name
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
