package commitlane

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestWithCommitBlocksWhenRescueIsActive(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := saveRescueFlag(layout); err != nil {
		t.Fatalf("save rescue flag: %v", err)
	}

	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	_, err := manager.WithCommit(context.Background(), CommitRequest{
		OpID:      "apply-1",
		Kind:      "apply",
		StartedAt: time.Now(),
	}, func(_ *lockset.Session) error {
		return nil
	})
	if !errors.Is(err, ErrRescueActive) {
		t.Fatalf("expected ErrRescueActive, got %v", err)
	}
}

func TestWithCommitDoesNotHoldRescueGateDuringBody(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))

	bodyStarted := make(chan struct{})
	releaseBody := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.WithCommit(context.Background(), CommitRequest{
			OpID:      "apply-1",
			Kind:      "apply",
			StartedAt: time.Now(),
		}, func(_ *lockset.Session) error {
			close(bodyStarted)
			<-releaseBody
			return nil
		})
		errCh <- err
	}()
	<-bodyStarted

	if err := manager.WithRescueGate(context.Background(), "rescue-enter-gate", func() error {
		return saveRescueFlag(layout)
	}); err != nil {
		t.Fatalf("expected rescue gate to be available during apply body: %v", err)
	}
	close(releaseBody)
	if err := <-errCh; err != nil {
		t.Fatalf("apply body should finish after rescue gate closes future work: %v", err)
	}
}

func TestWithCommitRechecksRescueAfterWaitingForCommitLane(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	manager := New(locks, layout, netgate.NewOwnerRegistry("smartgate"))

	holder := locks.Session("holder")
	releaseCommit, err := holder.Acquire(lockset.LevelCommitLane)
	if err != nil {
		t.Fatalf("hold commit lane: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		entered := false
		_, err := manager.WithCommit(context.Background(), CommitRequest{
			OpID:      "apply-1",
			Kind:      "apply",
			StartedAt: time.Now(),
		}, func(_ *lockset.Session) error {
			entered = true
			return nil
		})
		if entered && err == nil {
			err = errors.New("apply entered body after rescue flag appeared")
		}
		errCh <- err
	}()

	if err := manager.WithRescueGate(context.Background(), "rescue-enter-gate", func() error {
		return saveRescueFlag(layout)
	}); err != nil {
		t.Fatalf("save rescue flag: %v", err)
	}
	releaseCommit()
	if err := <-errCh; !errors.Is(err, ErrRescueActive) {
		t.Fatalf("expected ErrRescueActive after L1 wait, got %v", err)
	}
}

func TestWithCommitAllowsRescueOperation(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := saveRescueFlag(layout); err != nil {
		t.Fatalf("save rescue flag: %v", err)
	}

	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	result, err := manager.WithCommit(context.Background(), CommitRequest{
		OpID:      "rescue-1",
		Kind:      "rescue",
		StartedAt: time.Now(),
	}, func(_ *lockset.Session) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected rescue commit to pass, got %v", err)
	}
	if result == nil || result.OpID != "rescue-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestWithCommitAllowsRecoveryDuringRescue(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := saveRescueFlag(layout); err != nil {
		t.Fatalf("save rescue flag: %v", err)
	}

	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	result, err := manager.WithCommit(context.Background(), CommitRequest{
		OpID:      "recover-1",
		Kind:      "recovery",
		StartedAt: time.Now(),
	}, func(_ *lockset.Session) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected recovery commit to pass, got %v", err)
	}
	if result == nil || result.Kind != "recovery" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestWithCommitRejectsUnsafeOpID(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	_, err := manager.WithCommit(context.Background(), CommitRequest{
		OpID:      "../escape",
		Kind:      "apply",
		StartedAt: time.Now(),
	}, func(_ *lockset.Session) error {
		t.Fatal("unsafe op id should not enter commit body")
		return nil
	})
	if err == nil {
		t.Fatal("expected unsafe op id to be rejected")
	}
}

func TestWithCommitBlocksNewMutationWhenRecoveryJournalExists(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "broken-apply", Kind: "config_apply", Phase: state.TransactionRecoveryRequired,
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := manager.WithCommit(context.Background(), CommitRequest{OpID: "next-apply", Kind: "config_apply"}, func(*lockset.Session) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrRecoveryRequired) || called {
		t.Fatalf("unresolved journal did not block mutation: err=%v called=%v", err, called)
	}
	_, err = manager.WithCommit(context.Background(), CommitRequest{OpID: "recover", Kind: "recovery"}, func(*lockset.Session) error { return nil })
	if err != nil {
		t.Fatalf("recovery must bypass journal gate: %v", err)
	}
}

func saveRescueFlag(layout state.Layout) error {
	return os.WriteFile(layout.RescueStatusPath(), []byte("active"), 0o644)
}
