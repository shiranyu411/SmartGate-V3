package state

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestCutoverDrainStoreRoundTripAndPrivateMode(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	drain := CutoverDrainState{
		Pool: "general", OldGeneration: 7, Remaining: 2, Deadline: time.Now().Add(time.Minute),
		Status: CutoverDrainStatusRetrying, LastError: "cleanup failed", RetryCount: 2,
		Binding:  PoolBinding{Pool: "general", Generation: 7},
		Dispatch: PoolDispatchGraph{Pool: "general", Generation: 7},
	}
	store := NewCutoverDrainStore(layout)
	if err := store.Save(drain); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cutover drain mode = %o, want 600", got)
	}
	drains, err := store.List()
	if err != nil || len(drains) != 1 || drains[0].Pool != drain.Pool || drains[0].RetryCount != drain.RetryCount {
		t.Fatalf("round trip drains=%+v err=%v", drains, err)
	}
	if err := os.Chmod(layout.CutoverDrainsPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(drain); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("unchanged cutover drain mode = %o, want 600", got)
	}
	if err := store.Remove(drain.Pool); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.CutoverDrainsPath()); !os.IsNotExist(err) {
		t.Fatalf("removed cutover drain file remains: %v", err)
	}
}

func TestCutoverDrainStoreConcurrentPoolUpdatesDoNotOverwrite(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewCutoverDrainStore(layout)
	var wg sync.WaitGroup
	for index, pool := range []string{"general", "ai", "streaming", "gaming"} {
		wg.Add(1)
		go func(pool string, generation uint64) {
			defer wg.Done()
			if err := store.Save(CutoverDrainState{
				Pool: pool, OldGeneration: generation, Deadline: time.Now().Add(time.Minute),
				Status:   CutoverDrainStatusDraining,
				Binding:  PoolBinding{Pool: pool, Generation: generation},
				Dispatch: PoolDispatchGraph{Pool: pool, Generation: generation},
			}); err != nil {
				t.Errorf("save %s: %v", pool, err)
			}
		}(pool, uint64(index+1))
	}
	wg.Wait()
	drains, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(drains) != 4 {
		t.Fatalf("concurrent saves lost drains: %+v", drains)
	}
}

func TestCutoverDrainStoreDoesNotRewriteUnchangedDrain(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewCutoverDrainStore(layout)
	drain := CutoverDrainState{
		Pool: "general", OldGeneration: 7, Remaining: 2, Deadline: time.Now().Add(time.Minute),
		Status:   CutoverDrainStatusDraining,
		Binding:  PoolBinding{Pool: "general", Generation: 7},
		Dispatch: PoolDispatchGraph{Pool: "general", Generation: 7},
	}
	if err := store.Save(drain); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.List()
	if err != nil || len(before) != 1 || before[0].UpdatedAt.IsZero() {
		t.Fatalf("initial drain state = %+v err=%v", before, err)
	}

	if err := store.Save(drain); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.List()
	if err != nil || len(after) != 1 {
		t.Fatalf("unchanged drain state = %+v err=%v", after, err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("unchanged drain rewrote its durable file")
	}
	if !after[0].UpdatedAt.Equal(before[0].UpdatedAt) {
		t.Fatalf("unchanged drain updated timestamp: before=%s after=%s", before[0].UpdatedAt, after[0].UpdatedAt)
	}

	drain.Remaining = 1
	if err := store.Save(drain); err != nil {
		t.Fatal(err)
	}
	changedInfo, err := os.Stat(layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.List()
	if err != nil || len(changed) != 1 || changed[0].Remaining != 1 {
		t.Fatalf("changed drain state = %+v err=%v", changed, err)
	}
	if os.SameFile(afterInfo, changedInfo) {
		t.Fatal("changed drain did not rewrite its durable file")
	}
	if !changed[0].UpdatedAt.After(before[0].UpdatedAt) {
		t.Fatalf("changed drain did not advance timestamp: before=%s after=%s", before[0].UpdatedAt, changed[0].UpdatedAt)
	}
}
