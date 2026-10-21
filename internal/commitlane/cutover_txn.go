package commitlane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/planner"
	"smartgate/internal/redact"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type CutoverSource struct {
	SmartNode  string `json:"smart_node"`
	Generation uint64 `json:"generation"`
}

type CutoverRequest struct {
	OpID                string
	PoolID              string
	TargetSmartNode     string
	Source              CutoverSource
	ExpectedConfigEpoch int64
	TargetPool          state.FailoverPoolState
}

type CutoverDrain struct {
	OldGeneration uint64    `json:"old_generation"`
	Remaining     int       `json:"remaining"`
	Deadline      time.Time `json:"deadline"`
	Status        string    `json:"status"`
}

const (
	drainRetryLimit                 = 3
	defaultDrainPollInterval        = 250 * time.Millisecond
	defaultDrainRetryBase           = 250 * time.Millisecond
	defaultDrainAttemptLimit        = 10 * time.Second
	defaultDrainFailedRetryInterval = 30 * time.Second
)

type drainTiming struct {
	pollInterval        time.Duration
	retryBase           time.Duration
	attemptLimit        time.Duration
	failedRetryInterval time.Duration
}

func defaultDrainTiming() drainTiming {
	return drainTiming{
		pollInterval:        defaultDrainPollInterval,
		retryBase:           defaultDrainRetryBase,
		attemptLimit:        defaultDrainAttemptLimit,
		failedRetryInterval: defaultDrainFailedRetryInterval,
	}
}

func (t *SelectorTxn) effectiveDrainTiming() drainTiming {
	timing := t.drainTiming
	defaults := defaultDrainTiming()
	if timing.pollInterval <= 0 {
		timing.pollInterval = defaults.pollInterval
	}
	if timing.retryBase <= 0 {
		timing.retryBase = defaults.retryBase
	}
	if timing.attemptLimit <= 0 {
		timing.attemptLimit = defaults.attemptLimit
	}
	if timing.failedRetryInterval <= 0 {
		timing.failedRetryInterval = defaults.failedRetryInterval
	}
	return timing
}

type CutoverResult struct {
	Result              string            `json:"result"`
	Binding             state.PoolBinding `json:"binding"`
	ClosedConnections   int               `json:"closed_connections,omitempty"`
	ResidualConnections int               `json:"residual_connections,omitempty"`
	Verified            bool              `json:"verified"`
	Draining            *CutoverDrain     `json:"draining,omitempty"`
}

type SelectionConflictError struct {
	Pool     string
	Expected CutoverSource
	Actual   CutoverSource
	Reason   string
}

func (err SelectionConflictError) Error() string {
	if err.Reason != "" {
		return fmt.Sprintf("selection conflict for pool %s: %s", err.Pool, err.Reason)
	}
	return fmt.Sprintf(
		"selection conflict for pool %s: live source is %s generation %d, expected %s generation %d",
		err.Pool, err.Actual.SmartNode, err.Actual.Generation, err.Expected.SmartNode, err.Expected.Generation,
	)
}

type RequiresConfigApplyError struct {
	Pool   string
	Target string
	Reason string
}

func (err RequiresConfigApplyError) Error() string {
	return fmt.Sprintf("pool %s target %s requires config apply: %s", err.Pool, err.Target, err.Reason)
}

type CutoverConflictError struct {
	Pool          string
	Reason        string
	OldGeneration uint64
}

func (err CutoverConflictError) Error() string {
	if err.OldGeneration != 0 {
		return fmt.Sprintf("pool %s cutover conflict: %s (generation %d)", err.Pool, err.Reason, err.OldGeneration)
	}
	return fmt.Sprintf("pool %s cutover conflict: %s", err.Pool, err.Reason)
}

type CutoverError struct {
	Pool       string
	Stage      string
	RolledBack bool
	Blocked    bool
	Cause      error
}

func (err *CutoverError) Error() string {
	if err == nil {
		return "cutover failed"
	}
	return fmt.Sprintf("cutover failed for pool %s at %s: %v", err.Pool, err.Stage, err.Cause)
}

func (err *CutoverError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (t *SelectorTxn) Apply(ctx context.Context, req SelectorTxnApplyRequest) error {
	if !t.graphConfigured() {
		return t.applyLegacy(ctx, req)
	}
	_, err := t.Cutover(ctx, CutoverRequest{
		OpID: req.OpID, PoolID: req.PoolID, TargetSmartNode: req.Target,
		Source: CutoverSource{SmartNode: req.Source}, ExpectedConfigEpoch: req.ExpectedConfigEpoch, TargetPool: req.TargetPool,
	})
	return err
}

// EnsureWorkerRunning starts an installed Worker that no Pool Binding
// currently references, so its SmartNodes can be probed. Health probes run
// outside the commit lane; the start itself is serialized here against
// applies and cutovers, which also start and stop Workers. Returns true when a
// start happened. A Pool on its no_candidate fallback depends on this: its
// candidates live on a Worker nothing binds, and without a running Worker the
// failover watchdog could never see them healthy again.
func (t *SelectorTxn) EnsureWorkerRunning(ctx context.Context, opID, workerID string) (bool, error) {
	if !t.graphConfigured() {
		return false, fmt.Errorf("cutover transaction is not configured")
	}
	if strings.TrimSpace(workerID) == "" {
		return false, fmt.Errorf("worker id is required")
	}
	if t.workers.IsRunning(workerID) {
		return false, nil
	}
	started := false
	_, err := t.commit.WithCommit(ctx, CommitRequest{OpID: opID, Kind: "worker_probe_start", StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.AcquireContext(ctx, level)
			if err != nil {
				return err
			}
			defer release()
		}
		applied, err := t.store.LoadAppliedState()
		if err != nil {
			return err
		}
		mode := state.EffectiveGatewayMode(applied.GatewayMode)
		if !applied.SpecCaptured || mode != state.GatewayModeOn && mode != state.GatewayModeGlobal {
			return fmt.Errorf("gateway mode %s runs no Workers", mode)
		}
		var startErr error
		started, startErr = t.workers.EnsureRunning(ctx, workerID)
		return startErr
	})
	return started, err
}

func (t *SelectorTxn) Cutover(ctx context.Context, req CutoverRequest) (CutoverResult, error) {
	if !t.graphConfigured() {
		return CutoverResult{}, fmt.Errorf("cutover transaction is not configured")
	}
	if strings.TrimSpace(req.OpID) == "" || strings.TrimSpace(req.PoolID) == "" || strings.TrimSpace(req.TargetSmartNode) == "" {
		return CutoverResult{}, fmt.Errorf("op_id, pool and target_smart_node are required")
	}
	var result CutoverResult
	var immediateDrainDone chan struct{}
	_, err := t.commit.WithCommit(ctx, CommitRequest{OpID: req.OpID, Kind: selectorTransactionKind, StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.AcquireContext(ctx, level)
			if err != nil {
				return err
			}
			defer release()
		}
		sourceState, err := t.loadState()
		if err != nil {
			return err
		}
		if req.ExpectedConfigEpoch != 0 && sourceState.Applied.ConfigEpoch != req.ExpectedConfigEpoch {
			return SelectionConflictError{Pool: req.PoolID, Reason: fmt.Sprintf("config epoch is %d, expected %d", sourceState.Applied.ConfigEpoch, req.ExpectedConfigEpoch)}
		}
		pool, ok := businessPoolByID(sourceState.Applied.Spec, req.PoolID)
		if !ok {
			return fmt.Errorf("business pool %s is not in the applied topology", req.PoolID)
		}
		if pool.Policy.Cutover != state.CutoverStrict && pool.Policy.Cutover != state.CutoverGraceful {
			return fmt.Errorf("business pool %s has invalid cutover policy %q", req.PoolID, pool.Policy.Cutover)
		}
		currentSelections := copyPoolSelections(sourceState.Selection.Selections)
		currentGraph, err := buildCutoverGraph(sourceState.Applied, currentSelections, sourceState.Applied.ExecutionGraph, time.Now().UTC())
		if err != nil {
			return err
		}
		sourceBinding, ok := bindingForPool(currentGraph, req.PoolID)
		if !ok {
			return fmt.Errorf("pool %s has no applied Binding", req.PoolID)
		}
		actualSource := CutoverSource{SmartNode: bindingSelection(sourceBinding), Generation: sourceBinding.Generation}
		expectedSource := req.Source
		if expectedSource.SmartNode == "" {
			expectedSource.SmartNode = actualSource.SmartNode
		}
		if expectedSource.Generation == 0 {
			expectedSource.Generation = actualSource.Generation
		}
		if expectedSource != actualSource {
			return SelectionConflictError{Pool: req.PoolID, Expected: expectedSource, Actual: actualSource}
		}
		if normalizeSelectionTarget(req.TargetSmartNode) == normalizeSelectionTarget(actualSource.SmartNode) {
			result = CutoverResult{Result: "cutover_complete", Binding: sourceBinding, Verified: true}
			return nil
		}
		targetSelections, targetSelection, err := targetSelectionsForCutover(currentSelections, sourceState.Applied, pool, sourceBinding, req.TargetSmartNode)
		if err != nil {
			return err
		}
		targetGraph, err := buildCutoverGraph(sourceState.Applied, targetSelections, currentGraph, time.Now().UTC())
		if err != nil {
			return err
		}
		targetBinding, ok := bindingForPool(targetGraph, req.PoolID)
		if !ok || targetBinding.Generation != sourceBinding.Generation+1 {
			return fmt.Errorf("pool %s target Binding did not advance Generation", req.PoolID)
		}
		if err := validateOtherBindingsUnchanged(currentGraph, targetGraph, req.PoolID); err != nil {
			return err
		}
		if err := t.tracker.Start(req.PoolID, sourceBinding.Generation, targetBinding.Generation); err != nil {
			return err
		}
		handedToDrain := false
		defer func() {
			if !handedToDrain {
				t.tracker.Finish(req.PoolID)
			}
		}()

		targetState := cutoverTargetState(sourceState, req, pool, targetSelection, targetBinding)
		sourceBindingCopy := sourceBinding
		targetBindingCopy := targetBinding
		journal := state.TransactionJournal{
			OpID: req.OpID, Kind: selectorTransactionKind, Phase: state.TransactionPrepared,
			Source: sourceState, Target: targetState, PoolID: req.PoolID, CutoverPolicy: pool.Policy.Cutover,
			SourceBinding: &sourceBindingCopy, TargetBinding: &targetBindingCopy,
		}
		journals := state.NewTransactionStore(t.commit.Layout())
		if err := journals.Save(journal); err != nil {
			return err
		}
		journal.Phase = state.TransactionApplying
		if err := journals.Save(journal); err != nil {
			return err
		}

		var targetPath *worker.BindingPath
		startedTargetWorker := false
		if targetBinding.Target.Kind == "node" {
			path, err := t.workers.ResolvePath(targetBinding.Target.Worker, req.PoolID, planner.BindingLane(targetBinding.Generation), targetBinding.Target.Executor)
			if err != nil {
				return t.failBeforeFabric(ctx, journals, journal, currentGraph, startedTargetWorker, "resolve_target", RequiresConfigApplyError{Pool: req.PoolID, Target: req.TargetSmartNode, Reason: err.Error()})
			}
			targetPath = &path
			startedTargetWorker, err = t.workers.EnsureRunning(ctx, path.Worker.ID)
			if err != nil {
				return t.failBeforeFabric(ctx, journals, journal, currentGraph, startedTargetWorker, "start_target_worker", err)
			}
			t.tracker.Stage(req.PoolID, "target_preflight")
			if err := t.workers.SelectPath(ctx, path); err != nil {
				return t.failBeforeFabric(ctx, journals, journal, currentGraph, startedTargetWorker, "select_target_executor", err)
			}
			if err := t.workers.VerifyPath(ctx, path); err != nil {
				return t.failBeforeFabric(ctx, journals, journal, currentGraph, startedTargetWorker, "target_preflight", err)
			}
		}

		t.tracker.Stage(req.PoolID, "fabric_switch")
		preparedFabric, err := t.fabric.PrepareCutover(ctx, suffixedOpID(req.OpID, "-fabric"), req.PoolID, sourceState.Applied.Spec, targetGraph.Fabric)
		if err != nil {
			return t.failBeforeFabric(ctx, journals, journal, currentGraph, startedTargetWorker, "prepare_fabric", err)
		}
		if _, err := t.fabric.Apply(ctx, preparedFabric); err != nil {
			return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "fabric_switch", err)
		}
		t.workers.UpdateBindings(targetGraph.PoolBindings)

		sourceDispatch, _ := dispatchForPoolGraph(currentGraph.Fabric, req.PoolID)
		var sourcePath *worker.BindingPath
		if sourceBinding.Target.Kind == "node" {
			path, err := t.workers.ResolvePath(sourceBinding.Target.Worker, req.PoolID, planner.BindingLane(sourceBinding.Generation), sourceBinding.Target.Executor)
			if err != nil {
				return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "resolve_source", err)
			}
			sourcePath = &path
		}

		switch pool.Policy.Cutover {
		case state.CutoverStrict:
			t.tracker.Stage(req.PoolID, "close_old_binding")
			closed, residual, err := t.closeBinding(ctx, sourceDispatch, sourcePath)
			if err != nil {
				return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "close_old_binding", err)
			}
			if residual != 0 {
				return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "residual_connections", fmt.Errorf("old Binding has %d residual connections", residual))
			}
			t.tracker.Stage(req.PoolID, "verify_new_path")
			if targetPath != nil {
				if err := t.workers.VerifyPath(ctx, *targetPath); err != nil {
					return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "verify_new_path", err)
				}
			}
			if err := t.commitCutover(journals, &journal, targetState, nil); err != nil {
				return err
			}
			if err := t.workers.StopUnbound(context.WithoutCancel(ctx)); err != nil {
				return &CutoverError{Pool: req.PoolID, Stage: "stop_unbound_worker", Cause: err}
			}
			result = CutoverResult{Result: "cutover_complete", Binding: targetBinding, ClosedConnections: closed, Verified: true}
		case state.CutoverGraceful:
			remaining, err := t.countBindingConnections(ctx, sourceDispatch, sourcePath)
			if err != nil {
				return t.compensateCutover(ctx, journals, journal, currentGraph, targetGraph, startedTargetWorker, "start_drain", err)
			}
			deadline := time.Now().UTC().Add(time.Duration(pool.Policy.GracefulDrainSeconds) * time.Second)
			journal.DrainDeadline = &deadline
			journal.Target = targetState
			drain := DrainProgress{
				Pool: req.PoolID, OldGeneration: sourceBinding.Generation, Remaining: remaining, Deadline: deadline,
				Status: state.CutoverDrainStatusDraining, Binding: sourceBinding, Dispatch: sourceDispatch, Path: sourcePath,
			}
			if err := t.commitCutover(journals, &journal, targetState, &drain); err != nil {
				return err
			}
			t.tracker.BeginDrain(drain)
			handedToDrain = true
			if remaining == 0 {
				immediateDrainDone = make(chan struct{})
				t.startDrain(drain, func() {
					defer close(immediateDrainDone)
					t.drainBinding(drain)
				})
			} else {
				t.startDrain(drain, func() { t.drainBinding(drain) })
			}
			result = CutoverResult{
				Result: "committed", Binding: targetBinding, Verified: true,
				Draining: &CutoverDrain{
					OldGeneration: sourceBinding.Generation, Remaining: remaining, Deadline: deadline,
					Status: state.CutoverDrainStatusDraining,
				},
			}
		}
		return nil
	})
	if err == nil && immediateDrainDone != nil {
		select {
		case <-immediateDrainDone:
		case <-time.After(2 * time.Second):
		}
	}
	return result, err
}

func (t *SelectorTxn) graphConfigured() bool {
	return t != nil && t.commit != nil && t.store != nil && t.workers != nil && t.fabric != nil && t.tracker != nil
}

func (t *SelectorTxn) Progress() ([]CutoverProgress, []DrainProgress) {
	if t == nil || t.tracker == nil {
		return nil, nil
	}
	return t.tracker.Snapshot()
}

func (t *SelectorTxn) commitCutover(journals *state.TransactionStore, journal *state.TransactionJournal, target state.TransactionState, drain *DrainProgress) error {
	journal.Target = target
	journal.Phase = state.TransactionCommitted
	if err := journals.Save(*journal); err != nil {
		return err
	}
	if err := t.persistState(target); err != nil {
		return &CutoverError{Pool: journal.PoolID, Stage: "persist_committed", Cause: fmt.Errorf("startup recovery required: %w", err)}
	}
	if drain != nil {
		if err := t.saveDrain(*drain); err != nil {
			return &CutoverError{Pool: journal.PoolID, Stage: "persist_drain", Cause: fmt.Errorf("startup recovery required: %w", err)}
		}
	}
	if err := journals.Remove(journal.OpID); err != nil {
		return &CutoverError{Pool: journal.PoolID, Stage: "remove_committed_journal", Cause: err}
	}
	return nil
}

func (t *SelectorTxn) failBeforeFabric(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, sourceGraph state.ExecutionGraph, startedTargetWorker bool, stage string, cause error) error {
	if startedTargetWorker {
		t.workers.UpdateBindings(sourceGraph.PoolBindings)
		_ = t.workers.StopUnbound(context.WithoutCancel(ctx))
	}
	if err := t.persistState(journal.Source); err != nil {
		journal.Phase = state.TransactionRecoveryRequired
		journal.LastError = fmt.Sprintf("%v; restore source state: %v", cause, err)
		_ = journals.Save(journal)
		return &CutoverError{Pool: journal.PoolID, Stage: stage, RolledBack: false, Cause: fmt.Errorf("%v; restore source state: %w", cause, err)}
	}
	if err := journals.Remove(journal.OpID); err != nil {
		return &CutoverError{Pool: journal.PoolID, Stage: stage, RolledBack: true, Cause: fmt.Errorf("%v; remove journal: %w", cause, err)}
	}
	return &CutoverError{Pool: journal.PoolID, Stage: stage, RolledBack: true, Cause: cause}
}

func (t *SelectorTxn) compensateCutover(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, sourceGraph, targetGraph state.ExecutionGraph, startedTargetWorker bool, stage string, cause error) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	prepared, prepareErr := t.fabric.PrepareCompensation(recoveryCtx, suffixedOpID(journal.OpID, "-rollback"), journal.PoolID, journal.Source.Applied.Spec, sourceGraph.Fabric)
	if prepareErr == nil {
		_, prepareErr = t.fabric.Apply(recoveryCtx, prepared)
	}
	if prepareErr == nil {
		t.workers.UpdateBindings(sourceGraph.PoolBindings)
		if startedTargetWorker {
			prepareErr = t.workers.StopUnbound(recoveryCtx)
		}
	}
	if prepareErr == nil {
		prepareErr = t.persistState(journal.Source)
	}
	if prepareErr == nil {
		prepareErr = journals.Remove(journal.OpID)
	}
	if prepareErr == nil {
		return &CutoverError{Pool: journal.PoolID, Stage: stage, RolledBack: true, Cause: cause}
	}
	journal.Phase = state.TransactionRecoveryRequired
	journal.LastError = fmt.Sprintf("%v; compensation: %v", cause, prepareErr)
	_ = journals.Save(journal)
	blocked := t.blockFailedCutover(recoveryCtx, journals, journal, targetGraph)
	return &CutoverError{
		Pool: journal.PoolID, Stage: stage, RolledBack: false, Blocked: blocked,
		Cause: fmt.Errorf("%v; compensation failed: %w", cause, prepareErr),
	}
}

func (t *SelectorTxn) blockFailedCutover(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, previous state.ExecutionGraph) bool {
	selections := copyPoolSelections(journal.Target.Selection.Selections)
	selection := selections[journal.PoolID]
	selection.SmartNode = ""
	selection.Target = state.NoCandidateBlock
	selection.Generation++
	selection.BoundEpoch = journal.Target.Applied.ConfigEpoch
	selection.BoundTopologyHash = journal.Target.Applied.TopologyHash
	selections[journal.PoolID] = selection
	graph, err := buildCutoverGraph(journal.Target.Applied, selections, previous, time.Now().UTC())
	if err != nil {
		return false
	}
	prepared, err := t.fabric.PrepareCompensation(ctx, suffixedOpID(journal.OpID, "-block"), journal.PoolID, journal.Target.Applied.Spec, graph.Fabric)
	if err != nil {
		return false
	}
	if _, err := t.fabric.Apply(ctx, prepared); err != nil {
		return false
	}
	blockedBinding, ok := bindingForPool(graph, journal.PoolID)
	if !ok {
		return false
	}
	blockedState := cloneSelectorState(journal.Target)
	blockedState.Selection.Selections = selections
	blockedState.Selection.ProxySelection[journal.PoolID] = "Block"
	blockedState.Runtime.ProxySelection[journal.PoolID] = "Block"
	blockedState.Failover = cloneFailoverState(blockedState.Failover)
	pool := blockedState.Failover.Pools[journal.PoolID]
	pool.PoolID = journal.PoolID
	pool.Previous = pool.Current
	pool.Current = "Block"
	pool.Terminal = true
	pool.LastKind = "fail_closed"
	pool.LastReason = "cutover_compensation_failed"
	now := time.Now().UTC()
	pool.LastChangedAt = &now
	blockedState.Failover.Pools[journal.PoolID] = pool
	t.workers.UpdateBindings(graph.PoolBindings)
	_ = t.workers.StopUnbound(ctx)
	journal.Target = blockedState
	journal.TargetBinding = &blockedBinding
	journal.Phase = state.TransactionCommitted
	journal.LastError = "cutover compensation failed; pool forced to Block"
	if err := journals.Save(journal); err != nil {
		return false
	}
	if err := t.persistState(blockedState); err != nil {
		return false
	}
	return journals.Remove(journal.OpID) == nil
}

func (t *SelectorTxn) countBindingConnections(ctx context.Context, dispatch state.PoolDispatchGraph, path *worker.BindingPath) (int, error) {
	kernelCount, err := t.fabric.CountConnections(ctx, dispatch)
	if err != nil {
		return 0, err
	}
	workerCount := 0
	if path != nil {
		workerCount, err = t.workers.CountPathConnections(ctx, *path)
		if errors.Is(err, worker.ErrConnectionCountUnsupported) {
			err = nil
		}
		if err != nil && t.workers.IsReady(path.Worker.ID) {
			return 0, err
		}
	}
	if workerCount > kernelCount {
		return workerCount, nil
	}
	return kernelCount, nil
}

func (t *SelectorTxn) closeBinding(ctx context.Context, dispatch state.PoolDispatchGraph, path *worker.BindingPath) (int, int, error) {
	workerClosed := 0
	if path != nil {
		closed, err := t.workers.ClosePathConnections(ctx, *path)
		workerClosed = closed
		if err != nil && t.workers.IsReady(path.Worker.ID) {
			return workerClosed, 0, err
		}
	}
	kernelClosed, err := t.fabric.CloseConnections(ctx, dispatch)
	if err != nil {
		return maxInt(workerClosed, kernelClosed), 0, err
	}
	remaining, err := t.countBindingConnections(ctx, dispatch, path)
	return maxInt(workerClosed, kernelClosed), remaining, err
}

func (t *SelectorTxn) drainBinding(drain DrainProgress) {
	ticker := time.NewTicker(t.effectiveDrainTiming().pollInterval)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		remaining, err := t.countBindingConnections(ctx, drain.Dispatch, drain.Path)
		cancel()
		if err == nil {
			drain.Remaining = remaining
			drain.Status = state.CutoverDrainStatusDraining
			drain.LastError = ""
			if !t.updateDrain(drain) {
				t.scheduleDrainRetryAfterPersistFailure(drain)
				return
			}
			if remaining == 0 {
				t.completeDrain(drain, false)
				return
			}
		} else {
			drain.LastError = sanitizeDrainError(err)
			if !t.updateDrain(drain) {
				t.scheduleDrainRetryAfterPersistFailure(drain)
				return
			}
		}
		if !time.Now().Before(drain.Deadline) {
			t.completeDrain(drain, true)
			return
		}
		<-ticker.C
	}
}

func (t *SelectorTxn) startDrain(drain DrainProgress, run func()) bool {
	t.drainMu.Lock()
	if t.running == nil {
		t.running = map[string]bool{}
	}
	if t.running[drain.Pool] {
		t.drainMu.Unlock()
		return false
	}
	if t.scheduled != nil {
		delete(t.scheduled, drain.Pool)
	}
	t.running[drain.Pool] = true
	t.drainMu.Unlock()
	go func() {
		defer func() {
			t.drainMu.Lock()
			delete(t.running, drain.Pool)
			t.drainMu.Unlock()
		}()
		run()
	}()
	return true
}

func (t *SelectorTxn) completeDrain(drain DrainProgress, force bool) {
	timing := t.effectiveDrainTiming()
	startAttempt := drain.RetryCount
	if startAttempt < 0 || startAttempt >= drainRetryLimit {
		startAttempt = 0
	}
	for attempt := startAttempt; attempt < drainRetryLimit; attempt++ {
		if attempt > 0 {
			drain.Status = state.CutoverDrainStatusRetrying
			drain.RetryCount = attempt
			if !t.updateDrain(drain) {
				t.scheduleDrainRetryAfterPersistFailure(drain)
				return
			}
			time.Sleep(drainRetryDelay(attempt, timing.retryBase))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timing.attemptLimit)
		err := t.finishDrainAttempt(ctx, drain, force)
		cancel()
		if err == nil {
			return
		}
		drain.Status = state.CutoverDrainStatusRetrying
		drain.LastError = sanitizeDrainError(err)
		drain.RetryCount = attempt + 1
		if !t.updateDrain(drain) {
			t.scheduleDrainRetryAfterPersistFailure(drain)
			return
		}
	}
	drain.Status = state.CutoverDrainStatusFailed
	drain.RetryCount = drainRetryLimit
	t.updateDrain(drain)
	t.scheduleDrainRetry(drain)
}

func (t *SelectorTxn) retryFailedDrain(drain DrainProgress) (DrainProgress, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), t.effectiveDrainTiming().attemptLimit)
	err := t.finishDrainAttempt(ctx, drain, true)
	cancel()
	if err == nil {
		return drain, false
	}
	drain.Status = state.CutoverDrainStatusFailed
	drain.LastError = sanitizeDrainError(err)
	drain.RetryCount = drainRetryLimit
	// Keep the durable state failed between background attempts. If the error is
	// unchanged, CutoverDrainStore.Save becomes a read-only no-op instead of
	// writing retrying/failed transitions to flash on every retry interval.
	t.updateDrain(drain)
	return drain, true
}

func (t *SelectorTxn) scheduleDrainRetryAfterPersistFailure(drain DrainProgress) {
	drain.Status = state.CutoverDrainStatusFailed
	drain.RetryCount = drainRetryLimit
	t.scheduleDrainRetry(drain)
}

func (t *SelectorTxn) scheduleDrainRetry(drain DrainProgress) {
	if t == nil || !t.graphConfigured() {
		return
	}
	delay := t.effectiveDrainTiming().failedRetryInterval
	t.drainMu.Lock()
	if t.running == nil {
		t.running = map[string]bool{}
	}
	if t.scheduled == nil {
		t.scheduled = map[string]bool{}
	}
	if t.scheduled[drain.Pool] {
		t.drainMu.Unlock()
		return
	}
	t.scheduled[drain.Pool] = true
	t.drainMu.Unlock()

	go func() {
		for {
			timer := time.NewTimer(delay)
			<-timer.C

			t.drainMu.Lock()
			if !t.scheduled[drain.Pool] {
				t.drainMu.Unlock()
				return
			}
			if t.running[drain.Pool] {
				t.drainMu.Unlock()
				continue
			}
			delete(t.scheduled, drain.Pool)
			t.running[drain.Pool] = true
			t.drainMu.Unlock()

			// The durable state stays failed to avoid flash churn, while Runtime
			// exposes the active background attempt from the in-memory tracker.
			t.tracker.UpdateDrain(
				drain.Pool, drain.Remaining, state.CutoverDrainStatusRetrying,
				drain.LastError, drain.RetryCount,
			)
			next, retry := t.retryFailedDrain(drain)
			t.drainMu.Lock()
			delete(t.running, drain.Pool)
			t.drainMu.Unlock()
			if retry {
				t.scheduleDrainRetry(next)
			}
			return
		}
	}()
}

func drainRetryDelay(attempt int, base time.Duration) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(attempt) * base
}

func (t *SelectorTxn) updateDrain(drain DrainProgress) bool {
	if drain.Status == "" {
		drain.Status = state.CutoverDrainStatusDraining
	}
	if err := t.saveDrain(drain); err != nil {
		drain.Status = state.CutoverDrainStatusFailed
		drain.LastError = sanitizeDrainError(fmt.Errorf("persist drain: %w", err))
		t.tracker.UpdateDrain(drain.Pool, drain.Remaining, drain.Status, drain.LastError, drain.RetryCount)
		return false
	}
	t.tracker.UpdateDrain(drain.Pool, drain.Remaining, drain.Status, drain.LastError, drain.RetryCount)
	return true
}

func (t *SelectorTxn) saveDrain(drain DrainProgress) error {
	if t.drains == nil {
		return fmt.Errorf("cutover drain store is not configured")
	}
	return t.drains.Save(state.CutoverDrainState{
		Pool: drain.Pool, OldGeneration: drain.OldGeneration, Remaining: drain.Remaining,
		Deadline: drain.Deadline, Status: drain.Status, LastError: drain.LastError, RetryCount: drain.RetryCount,
		Binding: drain.Binding, Dispatch: drain.Dispatch,
	})
}

func (t *SelectorTxn) finishDrain(ctx context.Context, poolID string, dispatch state.PoolDispatchGraph, path *worker.BindingPath, force bool) error {
	opID := fmt.Sprintf("drain-%d", time.Now().UnixNano())
	_, err := t.commit.WithCommit(ctx, CommitRequest{OpID: opID, Kind: "cutover_drain", StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.AcquireContext(ctx, level)
			if err != nil {
				return err
			}
			defer release()
		}
		if force {
			if _, _, err := t.closeBinding(ctx, dispatch, path); err != nil {
				return err
			}
		}
		return t.workers.StopUnbound(ctx)
	})
	return err
}

func (t *SelectorTxn) finishDrainAttempt(ctx context.Context, drain DrainProgress, force bool) error {
	if err := t.finishDrain(ctx, drain.Pool, drain.Dispatch, drain.Path, force); err != nil {
		return err
	}
	if t.drains != nil {
		if err := t.drains.Remove(drain.Pool); err != nil {
			return fmt.Errorf("remove durable drain: %w", err)
		}
	}
	t.tracker.FinishDrain(drain.Pool)
	return nil
}

func sanitizeDrainError(err error) string {
	if err == nil {
		return ""
	}
	message := redact.MessageWithPaths(err.Error())
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}

func (t *SelectorTxn) recoverCutovers(ctx context.Context, allowDuringRescue bool) error {
	if t == nil || t.commit == nil || t.store == nil {
		return fmt.Errorf("cutover transaction is not configured")
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
			if active, _ := rescueFlagActive(t.commit.Layout()); active && journal.Phase != state.TransactionCommitted {
				continue
			}
		}
		journal.Source, err = normalizeTransactionLastGood(journal.Source)
		if err != nil {
			return err
		}
		journal.Target, err = normalizeTransactionLastGood(journal.Target)
		if err != nil {
			return err
		}
		next := journal.Source
		if journal.Phase == state.TransactionCommitted {
			next = journal.Target
		}
		if err := t.persistState(next); err != nil {
			journal.Phase = state.TransactionRecoveryRequired
			journal.LastError = err.Error()
			_ = journals.Save(journal)
			return err
		}
		if journal.Phase == state.TransactionCommitted && journal.CutoverPolicy == state.CutoverGraceful {
			if err := t.recoverCommittedDrain(journal); err != nil {
				journal.LastError = sanitizeDrainError(err)
				_ = journals.Save(journal)
				return err
			}
		}
		if err := journals.Remove(journal.OpID); err != nil {
			return err
		}
	}
	if t.tracker != nil {
		t.tracker.Clear()
	}
	if t.drains == nil {
		return fmt.Errorf("cutover drain store is not configured")
	}
	drains, err := t.drains.List()
	if err != nil {
		return err
	}
	for _, persisted := range drains {
		drain, err := t.restoreDrain(persisted)
		if err != nil {
			persisted.Status = state.CutoverDrainStatusFailed
			persisted.LastError = sanitizeDrainError(err)
			if saveErr := t.drains.Save(persisted); saveErr != nil {
				return fmt.Errorf("restore drain %s: %v; persist failure: %w", persisted.Pool, err, saveErr)
			}
			drain = DrainProgress{
				Pool: persisted.Pool, OldGeneration: persisted.OldGeneration, Remaining: persisted.Remaining,
				Deadline: persisted.Deadline, Status: persisted.Status, LastError: persisted.LastError,
				RetryCount: persisted.RetryCount, Binding: persisted.Binding, Dispatch: persisted.Dispatch,
			}
		}
		t.tracker.BeginDrain(drain)
	}
	return nil
}

// ResumeDrains runs after startup data-plane reconciliation has restored the
// worker and Fabric managers. Durable failed drains remain visible and blocked
// until this retry succeeds.
func (t *SelectorTxn) ResumeDrains() {
	if !t.graphConfigured() {
		return
	}
	_, drains := t.tracker.Snapshot()
	for _, drain := range drains {
		drain := drain
		t.startDrain(drain, func() { t.completeDrain(drain, true) })
	}
}

func (t *SelectorTxn) recoverCommittedDrain(journal state.TransactionJournal) error {
	if journal.SourceBinding == nil || journal.DrainDeadline == nil {
		return fmt.Errorf("committed graceful cutover %s has incomplete drain metadata", journal.OpID)
	}
	dispatch, ok := dispatchForPoolGraph(journal.Source.Applied.ExecutionGraph.Fabric, journal.PoolID)
	if !ok {
		return fmt.Errorf("committed graceful cutover %s has no source dispatch", journal.OpID)
	}
	remaining := 0
	if persisted, err := t.drains.List(); err != nil {
		return err
	} else {
		for _, item := range persisted {
			if item.Pool == journal.PoolID {
				return nil
			}
		}
	}
	return t.drains.Save(state.CutoverDrainState{
		Pool: journal.PoolID, OldGeneration: journal.SourceBinding.Generation,
		Remaining: remaining, Deadline: *journal.DrainDeadline,
		Status:    state.CutoverDrainStatusFailed,
		LastError: "drain state recovered from committed transaction journal",
		Binding:   *journal.SourceBinding, Dispatch: dispatch,
	})
}

func (t *SelectorTxn) restoreDrain(persisted state.CutoverDrainState) (DrainProgress, error) {
	drain := DrainProgress{
		Pool: persisted.Pool, OldGeneration: persisted.OldGeneration, Remaining: persisted.Remaining,
		Deadline: persisted.Deadline, Status: persisted.Status, LastError: persisted.LastError,
		RetryCount: persisted.RetryCount, Binding: persisted.Binding, Dispatch: persisted.Dispatch,
	}
	if persisted.Binding.Target.Kind != "node" {
		return drain, nil
	}
	path, err := t.workers.ResolvePath(
		persisted.Binding.Target.Worker, persisted.Pool, planner.BindingLane(persisted.OldGeneration), persisted.Binding.Target.Executor,
	)
	if err != nil {
		return drain, fmt.Errorf("resolve persisted drain path: %w", err)
	}
	drain.Path = &path
	return drain, nil
}

func buildCutoverGraph(applied state.AppliedState, selections map[string]state.PoolSelection, previous state.ExecutionGraph, now time.Time) (state.ExecutionGraph, error) {
	plan := planner.BuildExecutionGraph(applied.Spec, selections, previous, now)
	if plan.ReadyToApply {
		return plan.Graph, nil
	}
	issues := make([]string, 0)
	for _, issue := range plan.Issues {
		if issue.Blocking {
			issues = append(issues, issue.Code+": "+issue.Message)
		}
	}
	return state.ExecutionGraph{}, fmt.Errorf("build cutover graph: %s", strings.Join(issues, "; "))
}

func targetSelectionsForCutover(current map[string]state.PoolSelection, applied state.AppliedState, pool state.BusinessPoolSpec, source state.PoolBinding, target string) (map[string]state.PoolSelection, state.PoolSelection, error) {
	target = normalizeSelectionTarget(target)
	selection := current[pool.ID]
	selection.Generation = source.Generation + 1
	selection.BoundEpoch = applied.ConfigEpoch
	selection.BoundTopologyHash = applied.TopologyHash
	switch target {
	case "Direct":
		selection.SmartNode = ""
		selection.Target = state.NoCandidateDirect
	case "Block":
		selection.SmartNode = ""
		selection.Target = state.NoCandidateBlock
	default:
		if !containsCutoverString(pool.NodeIDs, target) {
			return nil, state.PoolSelection{}, fmt.Errorf("SmartNode %s is not a member of pool %s", target, pool.ID)
		}
		selection.SmartNode = target
		selection.Target = "node"
	}
	next := copyPoolSelections(current)
	next[pool.ID] = selection
	return next, selection, nil
}

func cutoverTargetState(source state.TransactionState, req CutoverRequest, pool state.BusinessPoolSpec, selection state.PoolSelection, binding state.PoolBinding) state.TransactionState {
	target := cloneSelectorState(source)
	target.SelectionCaptured = true
	if target.Selection.ProxySelection == nil {
		target.Selection.ProxySelection = map[string]string{}
	}
	if target.Selection.Selections == nil {
		target.Selection.Selections = map[string]state.PoolSelection{}
	}
	selected := bindingSelection(binding)
	target.Selection.ProxySelection[req.PoolID] = selected
	target.Selection.Selections[req.PoolID] = selection
	target.Runtime.ProxySelection[req.PoolID] = selected
	target.Failover = cloneFailoverState(source.Failover)
	targetPool := req.TargetPool
	if targetPool.PoolID == "" {
		targetPool = target.Failover.Pools[req.PoolID]
		targetPool.PoolID = req.PoolID
		targetPool.Role = pool.Role
		targetPool.Previous = targetPool.Current
		targetPool.Current = selected
		targetPool.LastKind = "manual"
		targetPool.LastReason = "manual_select"
		now := time.Now().UTC()
		targetPool.LastChangedAt = &now
	}
	targetPool.Current = selected
	targetPool.Terminal = selected == "Block"
	target.Failover.Pools[req.PoolID] = targetPool
	return target
}

func validateOtherBindingsUnchanged(source, target state.ExecutionGraph, poolID string) error {
	if len(source.PoolBindings) != len(target.PoolBindings) {
		return fmt.Errorf("cutover changes the number of Pool Bindings")
	}
	for _, sourceBinding := range source.PoolBindings {
		if sourceBinding.Pool == poolID {
			continue
		}
		targetBinding, ok := bindingForPool(target, sourceBinding.Pool)
		if !ok || sourceBinding.ID != targetBinding.ID || sourceBinding.Generation != targetBinding.Generation || sourceBinding.Target != targetBinding.Target || !sourceBinding.Since.Equal(targetBinding.Since) {
			return fmt.Errorf("cutover for pool %s changes pool %s Binding", poolID, sourceBinding.Pool)
		}
	}
	return nil
}

func businessPoolByID(spec state.SpecState, poolID string) (state.BusinessPoolSpec, bool) {
	for _, pool := range state.EffectiveBusinessPools(spec.BusinessPools) {
		if pool.ID == poolID {
			return pool, true
		}
	}
	return state.BusinessPoolSpec{}, false
}

func bindingForPool(graph state.ExecutionGraph, poolID string) (state.PoolBinding, bool) {
	for _, binding := range graph.PoolBindings {
		if binding.Pool == poolID {
			return binding, true
		}
	}
	return state.PoolBinding{}, false
}

func dispatchForPoolGraph(graph state.FabricGraph, poolID string) (state.PoolDispatchGraph, bool) {
	for _, dispatch := range graph.Dispatch {
		if dispatch.Pool == poolID {
			return dispatch, true
		}
	}
	return state.PoolDispatchGraph{}, false
}

func bindingSelection(binding state.PoolBinding) string {
	switch binding.Target.Kind {
	case state.NoCandidateDirect:
		return "Direct"
	case state.NoCandidateBlock:
		return "Block"
	default:
		return binding.Target.SmartNode
	}
}

func normalizeSelectionTarget(target string) string {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "direct":
		return "Direct"
	case "block":
		return "Block"
	default:
		return strings.TrimSpace(target)
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func containsCutoverString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cutoverErrorDetails(err error) (stage string, rolledBack, blocked bool) {
	var cutoverErr *CutoverError
	if errors.As(err, &cutoverErr) {
		return cutoverErr.Stage, cutoverErr.RolledBack, cutoverErr.Blocked
	}
	return "", false, false
}

func cutoverJournalExists(layout state.Layout, opID string) bool {
	_, err := state.NewTransactionStore(layout).Load(opID)
	return err == nil || !os.IsNotExist(err)
}
