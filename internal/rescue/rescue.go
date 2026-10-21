package rescue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
)

const (
	ReasonManual = "manual"
	// ReasonStartupRecovery marks a daemon that could not reconcile its durable
	// state at boot. The LAN stays fail-open and the management API stays up so
	// the operator can repair the topology and clear rescue instead of the
	// process crash-looping under procd.
	ReasonStartupRecovery = "startup_recovery_failed"
)

type State struct {
	Active          bool                  `json:"active"`
	Reason          string                `json:"reason"`
	EnteredAt       time.Time             `json:"entered_at"`
	CleanupPlan     netgate.CleanupPlan   `json:"cleanup_plan,omitempty"`
	Cleanup         netgate.CleanupResult `json:"cleanup,omitempty"`
	CleanupComplete bool                  `json:"cleanup_complete"`
}

type Manager struct {
	store      *Store
	commit     *commitlane.Manager
	owners     *netgate.OwnerRegistry
	cleanup    netgate.CleanupExecutor
	transition sync.Mutex
}

func New(path string, commit *commitlane.Manager, owners *netgate.OwnerRegistry) *Manager {
	return NewWithCleanup(path, commit, owners, netgate.OwnerCleanup{Owners: owners, Execute: netgate.ShouldExecuteOpenWrtCleanup()})
}

func NewWithCleanup(path string, commit *commitlane.Manager, owners *netgate.OwnerRegistry, cleanup netgate.CleanupExecutor) *Manager {
	return &Manager{store: NewStore(path), commit: commit, owners: owners, cleanup: cleanup}
}

func (m *Manager) Enter(ctx context.Context, reason string) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	return m.enter(ctx, reason)
}

func (m *Manager) enter(ctx context.Context, reason string) error {
	entered := State{Active: true, Reason: reason, EnteredAt: time.Now().UTC(), CleanupComplete: false}
	if err := m.commit.WithRescueGate(ctx, "rescue-enter-gate", func() error {
		return m.store.SaveState(entered)
	}); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	req := commitlane.CommitRequest{
		OpID:      "rescue-enter",
		Kind:      "rescue",
		StartedAt: time.Now(),
	}
	_, err := m.commit.WithCommit(cleanupCtx, req, func(session *lockset.Session) error {
		if err := m.store.SaveState(entered); err != nil {
			return err
		}
		if m.cleanup == nil {
			entered.CleanupComplete = true
			return m.store.SaveState(entered)
		}
		cleanupPlan, err := m.cleanup.Cleanup(cleanupCtx, session)
		if err != nil {
			return err
		}
		entered.CleanupPlan = cleanupPlan
		if err := m.store.SaveState(entered); err != nil {
			return err
		}
		executor, ok := m.cleanup.(netgate.CleanupPlanExecutor)
		if !ok {
			entered.CleanupComplete = true
			return m.store.SaveState(entered)
		}
		cleanupResult, err := executor.ExecuteCleanup(cleanupCtx, session, cleanupPlan)
		entered.Cleanup = cleanupResult
		entered.CleanupComplete = err == nil
		if saveErr := m.store.SaveState(entered); saveErr != nil {
			return saveErr
		}
		return err
	})
	return err
}

func (m *Manager) Active() (bool, error) {
	return m.store.Active()
}

// ResumeIncomplete replays a rescue transition that was made durable before
// cleanup completed. This closes the crash window between saving the active
// flag and disabling the managed data plane.
func (m *Manager) ResumeIncomplete(ctx context.Context) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	current, err := m.store.Load()
	if err != nil {
		return err
	}
	if !current.Active || current.CleanupComplete {
		return nil
	}
	return m.enter(ctx, current.Reason)
}

func (m *Manager) Clear(ctx context.Context) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	return m.clear(ctx)
}

func (m *Manager) clear(ctx context.Context) error {
	req := commitlane.CommitRequest{
		OpID:      "rescue-clear",
		Kind:      "rescue",
		StartedAt: time.Now(),
	}
	_, err := m.commit.WithCommit(ctx, req, func(_ *lockset.Session) error {
		return m.store.Clear()
	})
	return err
}

// ClearWithRecovery serializes the full rescue transition. A concurrent Enter
// cannot insert itself between data-plane recovery and removal of the rescue
// flag. On failure it re-applies cleanup before releasing the transition.
func (m *Manager) ClearWithRecovery(ctx context.Context, recoverPending func() error) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	current, err := m.store.Load()
	if err != nil {
		return fmt.Errorf("load active rescue state: %w", err)
	}
	// Recovery intentionally re-enables the authoritative data plane before the
	// rescue marker is removed. Persist an incomplete transition first so a
	// crash anywhere before Clear makes the next boot re-apply fail-open cleanup.
	current.CleanupComplete = false
	if err := m.store.SaveState(current); err != nil {
		return fmt.Errorf("mark rescue clear in progress: %w", err)
	}
	if recoverPending != nil {
		if err := recoverPending(); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cleanupErr := m.enter(cleanupCtx, current.Reason)
			cancel()
			if cleanupErr != nil {
				return fmt.Errorf("pending transaction recovery failed: %w; rescue cleanup also failed: %v", err, cleanupErr)
			}
			return fmt.Errorf("pending transaction recovery failed; rescue remains active: %w", err)
		}
	}
	if err := m.clear(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cleanupErr := m.enter(cleanupCtx, current.Reason)
		cancel()
		if cleanupErr != nil {
			return fmt.Errorf("could not clear rescue: %w; rescue cleanup also failed: %v", err, cleanupErr)
		}
		return fmt.Errorf("could not clear rescue; cleanup was re-applied: %w", err)
	}
	return nil
}

func (m *Manager) Status() (State, error) {
	return m.store.Load()
}

func (m *Manager) OwnerTag() string {
	return m.owners.Tag("rescue", "runtime")
}
