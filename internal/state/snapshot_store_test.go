package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestSnapshotStoreRoundTrip(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	store := NewSnapshotStore(layout)
	env := PreimageEnvelope{
		OpID:         "op-1",
		Kind:         "provider_apply",
		TopologyHash: "abc",
		Spec:         DefaultSpecState(),
		Runtime:      RuntimeState{ConfigEpoch: 3},
	}
	if err := store.SavePreimage(env); err != nil {
		t.Fatalf("save preimage: %v", err)
	}

	got, err := store.LoadPreimage(env.OpID)
	if err != nil {
		t.Fatalf("load preimage: %v", err)
	}
	if got.OpID != env.OpID || got.Runtime.ConfigEpoch != env.Runtime.ConfigEpoch {
		t.Fatalf("unexpected envelope: %+v", got)
	}
}

func TestSnapshotStoreRejectsUnsafeOpID(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	store := NewSnapshotStore(layout)
	env := PreimageEnvelope{
		OpID:    "../escape",
		Kind:    "provider_apply",
		Spec:    DefaultSpecState(),
		Runtime: RuntimeState{},
	}
	if err := store.SavePreimage(env); err == nil {
		t.Fatal("expected unsafe op id to be rejected")
	}
	if _, err := store.LoadPreimage(env.OpID); err == nil {
		t.Fatal("expected unsafe op id load to be rejected")
	}
	if err := store.RemovePreimage(env.OpID); err == nil {
		t.Fatal("expected unsafe op id remove to be rejected")
	}
}

func TestSnapshotStoreRejectsDuplicatePreimage(t *testing.T) {
	layout := Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	store := NewSnapshotStore(layout)
	env := PreimageEnvelope{
		OpID:    "apply-1",
		Kind:    "provider_apply",
		Spec:    DefaultSpecState(),
		Runtime: RuntimeState{},
	}
	if err := store.SavePreimage(env); err != nil {
		t.Fatalf("save first preimage: %v", err)
	}
	if err := store.SavePreimage(env); err == nil {
		t.Fatal("expected duplicate preimage to be rejected")
	}
}

func TestSnapshotStoreBoundedDecoderFailsClosedForRecoveryListAndRollback(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewSnapshotStore(layout)
	large := bytes.Repeat([]byte("x"), SnapshotMaxFileBytes+1)
	pendingPath, _ := store.preimagePath("oversized-recovery")
	if err := os.WriteFile(pendingPath, large, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := store.LoadPreimage("oversized-recovery")
	var limitErr *SnapshotResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("startup recovery preimage was not rejected before decode: %T %v", err, err)
	}

	snapshotPath, _ := store.snapshotPath("oversized-rollback")
	if err := os.WriteFile(snapshotPath, large, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadSnapshot("oversized-rollback"); !errors.As(err, &limitErr) {
		t.Fatalf("rollback load was not bounded: %T %v", err, err)
	}
	if _, err := store.ListSnapshots(context.Background()); !errors.As(err, &limitErr) {
		t.Fatalf("snapshot listing was not bounded: %T %v", err, err)
	}
}

func TestSnapshotStoreRejectsOversizedEmbeddedLastGoodOnSave(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	outbounds := make([]map[string]any, SnapshotMaxLastGoodNodes+1)
	for i := range outbounds {
		outbounds[i] = map[string]any{"tag": "node"}
	}
	err := NewSnapshotStore(layout).SaveSnapshot(SnapshotEnvelope{ID: "too-many", Spec: DefaultSpecState(), LastGoodCaptured: true, LastGood: []LastGoodSnapshot{{ProviderID: "p1", Outbounds: outbounds}}})
	var limitErr *SnapshotResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized last-good was marshaled/written: %T %v", err, err)
	}
}
