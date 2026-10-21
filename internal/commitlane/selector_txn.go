package commitlane

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/selection"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

const selectorTransactionKind = "selector_apply"

type cutoverDrainStore interface {
	List() ([]state.CutoverDrainState, error)
	Save(state.CutoverDrainState) error
	Remove(string) error
}

type SelectorTxn struct {
	commit      *Manager
	store       *state.Store
	control     selection.ProxyController
	workers     *worker.Manager
	fabric      *fabric.Manager
	tracker     *CutoverTracker
	drains      cutoverDrainStore
	drainMu     sync.Mutex
	running     map[string]bool
	scheduled   map[string]bool
	drainTiming drainTiming
}

type SelectorTxnApplyRequest struct {
	OpID                string
	PoolID              string
	Selector            string
	Target              string
	Source              string
	TargetPool          state.FailoverPoolState
	ExpectedConfigEpoch int64
}

func NewSelectorTxn(commit *Manager, store *state.Store, control selection.ProxyController) *SelectorTxn {
	return &SelectorTxn{commit: commit, store: store, control: control}
}

func NewCutoverTxn(commit *Manager, store *state.Store, workers *worker.Manager, fabricManager *fabric.Manager) *SelectorTxn {
	var drains cutoverDrainStore
	if commit != nil {
		drains = state.NewCutoverDrainStore(commit.Layout())
	}
	return &SelectorTxn{
		commit: commit, store: store, workers: workers, fabric: fabricManager,
		tracker: NewCutoverTracker(), drains: drains, running: map[string]bool{}, scheduled: map[string]bool{},
		drainTiming: defaultDrainTiming(),
	}
}

func (t *SelectorTxn) applyLegacy(ctx context.Context, req SelectorTxnApplyRequest) error {
	if t == nil || t.commit == nil || t.store == nil || t.control == nil {
		return fmt.Errorf("selector transaction is not configured")
	}
	if req.PoolID == "" || req.Selector == "" || req.Target == "" {
		return fmt.Errorf("pool, selector and target are required")
	}
	_, err := t.commit.WithCommit(ctx, CommitRequest{OpID: req.OpID, Kind: selectorTransactionKind, StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		source, err := t.loadState()
		if err != nil {
			return err
		}
		if req.ExpectedConfigEpoch != 0 && source.Applied.ConfigEpoch != req.ExpectedConfigEpoch {
			return fmt.Errorf("selector topology changed: config epoch is %d, expected %d", source.Applied.ConfigEpoch, req.ExpectedConfigEpoch)
		}
		reader, ok := t.control.(selection.ProxySelectionReader)
		if !ok {
			return fmt.Errorf("selector transaction cannot verify live source for pool %s", req.PoolID)
		}
		sourceTarget, err := reader.Selected(ctx, req.Selector)
		if err != nil {
			return fmt.Errorf("verify live selector source for pool %s: %w", req.PoolID, err)
		}
		if req.Source != "" && req.Source != sourceTarget {
			return fmt.Errorf("selector source changed for pool %s: live target is %s, expected %s", req.PoolID, sourceTarget, req.Source)
		}
		if source.Runtime.ProxySelection == nil {
			source.Runtime.ProxySelection = map[string]string{}
		}
		source.Runtime.ProxySelection[req.PoolID] = sourceTarget
		setTransactionSelections(&source, source.Runtime.ProxySelection)
		source.Failover = cloneFailoverState(source.Failover)
		sourcePool := source.Failover.Pools[req.PoolID]
		sourcePool.PoolID = req.PoolID
		sourcePool.Current = sourceTarget
		source.Failover.Pools[req.PoolID] = sourcePool
		target := cloneSelectorState(source)
		target.Runtime.ProxySelection[req.PoolID] = req.Target
		setTransactionSelections(&target, target.Runtime.ProxySelection)
		target.Failover = cloneFailoverState(source.Failover)
		target.Failover.Pools[req.PoolID] = req.TargetPool
		journal := state.TransactionJournal{OpID: req.OpID, Kind: selectorTransactionKind, Phase: state.TransactionPrepared, Source: source, Target: target, PoolID: req.PoolID, Selector: req.Selector}
		journals := state.NewTransactionStore(t.commit.Layout())
		if err := journals.Save(journal); err != nil {
			return err
		}
		journal.Phase = state.TransactionApplying
		if err := journals.Save(journal); err != nil {
			return err
		}
		if err := t.control.Select(ctx, req.Selector, req.Target); err != nil {
			return t.compensate(ctx, journals, journal, sourceTarget, err)
		}
		if err := t.persistState(target); err != nil {
			return t.compensate(ctx, journals, journal, sourceTarget, err)
		}
		// Best-effort sticky cleanup: existing TUN/browser sockets keep the old
		// exit until closed. Immediately rebind this pool selector and schedule
		// residual sweeps for sockets still chained through the old node
		// (never close-all). Failures must not roll back select.
		if sourceTarget != req.Target && req.Selector != "" {
			selection.ScheduleSelectorConnectionRebind(t.control, req.Selector, sourceTarget)
		}
		journal.Phase = state.TransactionCommitted
		if err := journals.Save(journal); err != nil {
			return err
		}
		return journals.Remove(req.OpID)
	})
	return err
}

func (t *SelectorTxn) Recover(ctx context.Context) error               { return t.recover(ctx, false) }
func (t *SelectorTxn) RecoverForRescueClear(ctx context.Context) error { return t.recover(ctx, true) }

func (t *SelectorTxn) recover(ctx context.Context, allowDuringRescue bool) error {
	if t.graphConfigured() {
		return t.recoverCutovers(ctx, allowDuringRescue)
	}
	if t == nil || t.commit == nil || t.store == nil || t.control == nil {
		return fmt.Errorf("selector transaction is not configured")
	}
	journals := state.NewTransactionStore(t.commit.Layout())
	items, err := journals.List()
	if err != nil {
		return err
	}
	for _, journal := range items {
		if journal.Kind != selectorTransactionKind {
			continue
		}
		if !allowDuringRescue {
			if active, _ := rescueFlagActive(t.commit.Layout()); active {
				continue
			}
		}
		if err := t.recoverOne(ctx, journals, journal); err != nil {
			return err
		}
	}
	return nil
}

func (t *SelectorTxn) recoverOne(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal) error {
	var err error
	journal.Source, err = normalizeTransactionLastGood(journal.Source)
	if err != nil {
		return err
	}
	journal.Target, err = normalizeTransactionLastGood(journal.Target)
	if err != nil {
		return err
	}
	_, err = t.commit.WithCommit(ctx, CommitRequest{OpID: journal.OpID + "-recover", Kind: "recovery", StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		next := journal.Source
		if journal.Phase == state.TransactionCommitted {
			next = journal.Target
		}
		target := next.Runtime.ProxySelection[journal.PoolID]
		if journal.Selector == "" || target == "" {
			return fmt.Errorf("selector recovery metadata is incomplete for %s", journal.OpID)
		}
		if err := t.control.Select(ctx, journal.Selector, target); err != nil {
			if journal.Phase != state.TransactionCommitted {
				journal.Phase = state.TransactionRecoveryRequired
			}
			journal.LastError = err.Error()
			_ = journals.Save(journal)
			return err
		}
		setTransactionSelections(&next, next.Runtime.ProxySelection)
		if err := t.persistState(next); err != nil {
			if journal.Phase != state.TransactionCommitted {
				journal.Phase = state.TransactionRecoveryRequired
			}
			journal.LastError = err.Error()
			_ = journals.Save(journal)
			return err
		}
		return journals.Remove(journal.OpID)
	})
	return err
}

func (t *SelectorTxn) compensate(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, sourceTarget string, cause error) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := t.control.Select(recoveryCtx, journal.Selector, sourceTarget); err != nil {
		journal.Phase = state.TransactionRecoveryRequired
		journal.LastError = fmt.Sprintf("%v; selector rollback: %v", cause, err)
		_ = journals.Save(journal)
		return fmt.Errorf("%v; selector rollback: %w", cause, err)
	}
	if err := t.persistState(journal.Source); err != nil {
		journal.Phase = state.TransactionRecoveryRequired
		journal.LastError = fmt.Sprintf("%v; state rollback: %v", cause, err)
		_ = journals.Save(journal)
		return fmt.Errorf("%v; state rollback: %w", cause, err)
	}
	if err := journals.Remove(journal.OpID); err != nil {
		return fmt.Errorf("%v; remove compensated journal: %w", cause, err)
	}
	return cause
}

func (t *SelectorTxn) loadState() (state.TransactionState, error) {
	applied, err := t.store.LoadAppliedState()
	if err != nil {
		return state.TransactionState{}, err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return state.TransactionState{}, fmt.Errorf("applied topology is not captured")
	}
	selection, err := t.store.LoadSelectionForApplied(applied)
	if err != nil {
		return state.TransactionState{}, err
	}
	runtimeState, err := t.store.LoadRuntime()
	if err != nil {
		return state.TransactionState{}, err
	}
	// Prefer durable SelectionState over RuntimeDir so a recreated runtime
	// does not journal an incomplete source selection map.
	runtimeState.ProxySelection = copyStringMap(selection.ProxySelection)
	runtimeState.ConfigEpoch = applied.ConfigEpoch
	runtimeState.TopologyHash = applied.TopologyHash
	failoverState, err := state.NewFailoverStateStore(t.commit.Layout()).Load()
	if err != nil {
		return state.TransactionState{}, err
	}
	return normalizeTransactionLastGood(state.TransactionState{
		Applied:           applied,
		Runtime:           runtimeState,
		SelectionCaptured: true,
		Selection:         selection,
		FailoverCaptured:  true,
		Failover:          failoverState,
	})
}

func (t *SelectorTxn) persistState(next state.TransactionState) error {
	if err := t.store.SaveRuntime(next.Runtime); err != nil {
		return err
	}
	selection := next.Selection
	if !next.SelectionCaptured {
		selection = state.SelectionStateForApplied(next.Applied, next.Runtime.ProxySelection)
	}
	if !state.SelectionStateMatchesApplied(selection, next.Applied) || !selectionMapsEqual(selection.ProxySelection, next.Runtime.ProxySelection) {
		return fmt.Errorf("selector transaction selection does not match applied topology")
	}
	if err := t.store.SaveSelectionState(selection); err != nil {
		return err
	}
	return state.NewFailoverStateStore(t.commit.Layout()).Save(next.Failover)
}

func cloneSelectorState(source state.TransactionState) state.TransactionState {
	next := source
	next.Runtime.ProxySelection = copyStringMap(source.Runtime.ProxySelection)
	next.Selection = copySelectionState(source.Selection)
	return next
}

func cloneFailoverState(source state.FailoverState) state.FailoverState {
	next := state.NormalizeFailoverState(source)
	next.Locks = make(map[string]state.FailoverLock, len(source.Locks))
	for key, value := range source.Locks {
		next.Locks[key] = value
	}
	next.Pools = make(map[string]state.FailoverPoolState, len(source.Pools))
	for key, value := range source.Pools {
		next.Pools[key] = value
	}
	return next
}

func rescueFlagActive(layout state.Layout) (bool, error) {
	_, err := os.Stat(layout.RescueStatusPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
