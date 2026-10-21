package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
)

type BootstrapResult struct {
	Store      *state.Store
	CommitLane *commitlane.Manager
	Rescue     *rescue.Manager
	Owners     *netgate.OwnerRegistry
	Runtime    state.RuntimeState
	Spec       state.SpecState
	Status     Status
}

func Bootstrap(ctx context.Context, layout state.Layout, listenAddr string, locks *lockset.LockSet, workerCleanupTargets []netgate.WorkerCleanupTarget) (*BootstrapResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Preflight(layout, listenAddr); err != nil {
		return nil, err
	}
	if err := migrateLegacyRescueStatus(layout); err != nil {
		return nil, err
	}

	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	commitMgr := commitlane.New(locks, layout, owners)
	snapshots := state.NewSnapshotStore(layout)
	if err := commitlane.NewRecovery(commitMgr, store, snapshots).Recover(ctx); err != nil {
		return nil, err
	}

	if err := store.PersistDefaultCoreChoices(); err != nil {
		return nil, fmt.Errorf("persist default node core choices: %w", err)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		return nil, err
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		return nil, err
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		return nil, err
	}
	pendingConfigRecovery, err := hasPendingConfigRecovery(layout)
	if err != nil {
		return nil, err
	}
	if appliedState.SchemaVersion != 0 && appliedState.SpecCaptured && !pendingConfigRecovery {
		selection, err := store.LoadSelectionState()
		if err != nil {
			return nil, err
		}
		if selection.SchemaVersion == 0 {
			selection = state.SelectionStateForApplied(appliedState, appliedState.ProxySelection)
			if err := store.SaveSelectionState(selection); err != nil {
				return nil, err
			}
		} else if !state.SelectionStateMatchesApplied(selection, appliedState) {
			return nil, fmt.Errorf("durable selection does not match captured applied topology")
		}
		runtimeState = runtimeWithAppliedState(runtimeState, appliedState, selection.ProxySelection)
	}

	rescueMgr := rescue.NewWithCleanup(layout.RescueStatusPath(), commitMgr, owners, netgate.OwnerCleanup{
		Owners:               owners,
		WorkerCleanupTargets: workerCleanupTargets,
		Execute:              netgate.ShouldExecuteOpenWrtCleanup(),
	})

	active, err := rescueMgr.Active()
	if err != nil {
		return nil, err
	}
	if active {
		if err := rescueMgr.ResumeIncomplete(ctx); err != nil {
			return nil, fmt.Errorf("resume incomplete rescue cleanup: %w", err)
		}
		rs, err := rescueMgr.Status()
		if err != nil {
			return nil, err
		}
		runtimeState.RescueActive = true
		runtimeState.RescueReason = rs.Reason
		runtimeState.GatewayMode = state.GatewayModeSafe
	} else {
		runtimeState.RescueActive = false
		runtimeState.RescueReason = ""
	}
	if err := store.SaveRuntime(runtimeState); err != nil {
		return nil, err
	}

	return &BootstrapResult{
		Store:      store,
		CommitLane: commitMgr,
		Rescue:     rescueMgr,
		Owners:     owners,
		Runtime:    runtimeState,
		Spec:       spec,
		Status:     NewStatus(listenAddr, runtimeState),
	}, nil
}

func hasPendingConfigRecovery(layout state.Layout) (bool, error) {
	journals, err := state.NewTransactionStore(layout).List()
	if err != nil {
		return false, err
	}
	for _, journal := range journals {
		switch journal.Kind {
		case "config_apply", "snapshot_rollback", "backup_restore":
			return true, nil
		}
	}
	return false, nil
}

func migrateLegacyRescueStatus(layout state.Layout) error {
	legacy := filepath.Join(layout.RuntimeDir, "smartgate-rescue.status")
	current := layout.RescueStatusPath()
	if legacy == current {
		return nil
	}
	if _, err := os.Stat(current); err == nil {
		if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncBootstrapDir(filepath.Dir(legacy))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	tmp := current + ".migrate"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, current); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncBootstrapDir(filepath.Dir(current)); err != nil {
		return err
	}
	if err := os.Remove(legacy); err != nil {
		return err
	}
	return syncBootstrapDir(filepath.Dir(legacy))
}

func syncBootstrapDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func runtimeWithAppliedState(runtimeState state.RuntimeState, applied state.AppliedState, selections map[string]string) state.RuntimeState {
	runtimeState.ConfigEpoch = applied.ConfigEpoch
	runtimeState.TopologyHash = applied.TopologyHash
	runtimeState.GatewayMode = applied.GatewayMode
	runtimeState.LastAppliedAt = applied.LastAppliedAt
	runtimeState.LastRollbackAt = applied.LastRollbackAt
	runtimeState.LastRestoreAt = applied.LastRestoreAt
	runtimeState.ProxySelection = copySelections(selections)
	return runtimeState
}

func copySelections(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
