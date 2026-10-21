package rescue

import (
	"context"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type blockingCleanup struct {
	started chan struct{}
	release chan struct{}
}

type countingCleanup struct{ calls int }

func (c *countingCleanup) Cleanup(context.Context, *lockset.Session) (netgate.CleanupPlan, error) {
	c.calls++
	return netgate.CleanupPlan{}, nil
}

func TestClearWithRecoverySerializesConcurrentEnter(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners)
	if err := manager.Enter(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	recoveryStarted := make(chan struct{})
	releaseRecovery := make(chan struct{})
	clearDone := make(chan error, 1)
	go func() {
		clearDone <- manager.ClearWithRecovery(context.Background(), func() error {
			close(recoveryStarted)
			<-releaseRecovery
			return nil
		})
	}()
	<-recoveryStarted
	enterStarted := make(chan struct{})
	enterDone := make(chan error, 1)
	go func() {
		close(enterStarted)
		enterDone <- manager.Enter(context.Background(), "newer")
	}()
	<-enterStarted
	select {
	case err := <-enterDone:
		t.Fatalf("concurrent enter bypassed rescue transition: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRecovery)
	if err := <-clearDone; err != nil {
		t.Fatal(err)
	}
	if err := <-enterDone; err != nil {
		t.Fatal(err)
	}
	current, err := manager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !current.Active || current.Reason != "newer" {
		t.Fatalf("newer rescue state was lost: %+v", current)
	}
}

func (c blockingCleanup) Cleanup(ctx context.Context, _ *lockset.Session) (netgate.CleanupPlan, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		return netgate.CleanupPlan{}, ctx.Err()
	case <-c.release:
		return netgate.CleanupPlan{Actions: []netgate.CleanupAction{{
			Kind:     netgate.KindTunInterface,
			ID:       "tun-sb",
			OwnerTag: "smartgate:tun:tun-sb",
			Comment:  "owner=smartgate:tun:tun-sb",
		}}}, nil
	}
}

type recordingCleanup struct {
	executed bool
}

func (c *recordingCleanup) Cleanup(_ context.Context, _ *lockset.Session) (netgate.CleanupPlan, error) {
	return netgate.CleanupPlan{
		OpID: "rescue-cleanup",
		Actions: []netgate.CleanupAction{{
			Kind:     netgate.KindTunInterface,
			ID:       "tun-sb",
			OwnerTag: "smartgate:tun:tun-sb",
			Comment:  "owner=smartgate:tun:tun-sb",
			Commands: []netgate.PlannedCommand{{Argv: []string{"sh", "-c", "echo owner=smartgate:tun:tun-sb"}}},
		}},
	}, nil
}

func (c *recordingCleanup) ExecuteCleanup(_ context.Context, _ *lockset.Session, plan netgate.CleanupPlan) (netgate.CleanupResult, error) {
	c.executed = true
	return netgate.CleanupResult{OpID: plan.OpID, Actions: len(plan.Actions), Steps: 1}, nil
}

func TestManagerLifecycle(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	manager := New(
		layout.RescueStatusPath(),
		commitlane.New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")),
		netgate.NewOwnerRegistry("smartgate"),
	)

	active, err := manager.Active()
	if err != nil {
		t.Fatalf("active before enter: %v", err)
	}
	if active {
		t.Fatal("rescue should start inactive")
	}

	if err := manager.Enter(context.Background(), ReasonManual); err != nil {
		t.Fatalf("enter rescue: %v", err)
	}
	active, err = manager.Active()
	if err != nil {
		t.Fatalf("active after enter: %v", err)
	}
	if !active {
		t.Fatal("rescue should be active after enter")
	}

	status, err := manager.Status()
	if err != nil {
		t.Fatalf("status after enter: %v", err)
	}
	if status.Reason != ReasonManual {
		t.Fatalf("unexpected rescue reason: %q", status.Reason)
	}
	if len(status.CleanupPlan.Actions) == 0 {
		t.Fatal("expected rescue to record owner-tagged cleanup plan")
	}

	if err := manager.Clear(context.Background()); err != nil {
		t.Fatalf("clear rescue: %v", err)
	}
	active, err = manager.Active()
	if err != nil {
		t.Fatalf("active after clear: %v", err)
	}
	if active {
		t.Fatal("rescue should be inactive after clear")
	}
}

func TestManagerExecutesAndRecordsCleanupResult(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	cleanup := &recordingCleanup{}
	manager := NewWithCleanup(
		layout.RescueStatusPath(),
		commitlane.New(lockset.New(), layout, owners),
		owners,
		cleanup,
	)

	if err := manager.Enter(context.Background(), ReasonManual); err != nil {
		t.Fatalf("enter rescue: %v", err)
	}
	if !cleanup.executed {
		t.Fatal("expected rescue enter to execute cleanup plan")
	}
	status, err := manager.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Cleanup.Steps != 1 || status.Cleanup.Actions != 1 {
		t.Fatalf("cleanup result not recorded: %+v", status.Cleanup)
	}
}

func TestEnterMarksRescueActiveBeforeCleanupFinishes(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	cleanup := blockingCleanup{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := NewWithCleanup(
		layout.RescueStatusPath(),
		commitlane.New(lockset.New(), layout, owners),
		owners,
		cleanup,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- manager.Enter(context.Background(), ReasonManual)
	}()
	<-cleanup.started

	active, err := manager.Active()
	if err != nil {
		t.Fatalf("active while cleanup waits: %v", err)
	}
	if !active {
		t.Fatal("rescue should be active before cleanup finishes")
	}
	close(cleanup.release)
	if err := <-errCh; err != nil {
		t.Fatalf("enter rescue: %v", err)
	}
}

func TestClearCrashLeavesIncompleteMarkerThatBootRecleans(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	manager := NewWithCleanup(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners, &countingCleanup{})
	if err := NewStore(layout.RescueStatusPath()).SaveState(State{Active: true, Reason: ReasonManual, EnteredAt: time.Now().UTC(), CleanupComplete: true}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = manager.ClearWithRecovery(context.Background(), func() error {
			status, err := manager.Status()
			if err != nil {
				t.Fatal(err)
			}
			if status.CleanupComplete {
				t.Fatal("clear recovery began without durable incomplete marker")
			}
			panic("simulated kill after data-plane recovery")
		})
	}()

	cleanup := &countingCleanup{}
	restarted := NewWithCleanup(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners, cleanup)
	if err := restarted.ResumeIncomplete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup.calls != 1 {
		t.Fatalf("boot did not re-apply fail-open cleanup after interrupted clear: %d", cleanup.calls)
	}
}
