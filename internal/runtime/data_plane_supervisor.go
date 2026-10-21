package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

const (
	defaultDataPlaneSupervisorTimeout     = 45 * time.Second
	defaultFabricSupervisorPollInterval   = 5 * time.Second
	defaultFabricSupervisorConnectTimeout = 2 * time.Second
)

type dataPlaneSupervisor struct {
	store        *state.Store
	commit       *commitlane.Manager
	workers      *worker.Manager
	worker       *workerFailureController
	fabric       *fabric.Manager
	reconciler   planeSelectionReconciler
	rescuer      *rescue.Manager
	rescueReason string
	interval     time.Duration
	initialized  bool
	restartWait  bool
	// verifyHost enables the read-only host-state check (owner nft table,
	// policy routes, hook, dnsmasq override) on every poll. Only meaningful on
	// a live OpenWrt runtime; dev runtimes use a no-op command runner.
	verifyHost      bool
	reassertWait    bool
	reasserts       int
	workerRestarter failedWorkerRestarter
	workerRestarts  map[string]time.Time
}

type failedWorkerRestarter interface {
	RestartFailed(context.Context, string) (bool, error)
}

type planeSelectionReconciler interface {
	ReconcileLiveSelectionsLocked(context.Context) error
}

func newDataPlaneSupervisor(cfg Config) *dataPlaneSupervisor {
	supervisor := &dataPlaneSupervisor{
		store: cfg.Store, commit: cfg.CommitLane, workers: cfg.Workers, worker: newWorkerFailureController(cfg),
		fabric: cfg.Fabric, reconciler: cfg.Recovery, rescuer: cfg.Rescue, rescueReason: cfg.DataPlaneRescueReason,
		interval: defaultFabricSupervisorPollInterval, verifyHost: cfg.FabricHostVerification,
	}
	if cfg.Workers != nil {
		supervisor.workerRestarter = cfg.Workers
	}
	return supervisor
}

func (supervisor *dataPlaneSupervisor) Start(ctx context.Context) func() {
	runCtx, cancel := context.WithCancel(ctx)
	go supervisor.monitorFabric(runCtx)
	return cancel
}

func (supervisor *dataPlaneSupervisor) monitorFabric(ctx context.Context) {
	ticker := time.NewTicker(supervisor.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := supervisor.pollFabric(ctx); err != nil {
				var failure *fabricRestartFailure
				if !errors.As(err, &failure) {
					log.Printf("fabric data-plane supervisor poll failed: %v", err)
					continue
				}
				handleCtx, cancel := context.WithTimeout(context.Background(), defaultDataPlaneSupervisorTimeout)
				err = supervisor.HandleFabricFailure(handleCtx, err)
				cancel()
				log.Printf("fabric data-plane supervisor failed: %v", err)
			}
		}
	}
}

type fabricRestartFailure struct{ err error }

func (failure *fabricRestartFailure) Error() string {
	return "restart Gateway Fabric: " + failure.err.Error()
}
func (failure *fabricRestartFailure) Unwrap() error { return failure.err }

func (supervisor *dataPlaneSupervisor) pollFabric(ctx context.Context) error {
	if supervisor == nil {
		return fmt.Errorf("data-plane supervisor is not configured")
	}
	if supervisor.store == nil {
		return fmt.Errorf("data-plane supervisor store is required")
	}
	if supervisor.commit == nil {
		return fmt.Errorf("data-plane supervisor CommitLane is required")
	}
	if supervisor.fabric == nil {
		return fmt.Errorf("data-plane supervisor Fabric manager is required")
	}
	if supervisor.rescuer != nil {
		active, err := supervisor.rescuer.Active()
		if err != nil {
			return err
		}
		if active {
			return nil
		}
	}
	var pollErr error
	_, err := supervisor.commit.WithCommit(ctx, commitlane.CommitRequest{
		OpID: fmt.Sprintf("data-plane-supervisor-%d", time.Now().UnixNano()), Kind: "watchdog", StartedAt: time.Now().UTC(),
	}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		pollErr = supervisor.pollFabricLocked(ctx)
		return pollErr
	})
	if err != nil {
		return err
	}
	return pollErr
}

func (supervisor *dataPlaneSupervisor) pollFabricLocked(ctx context.Context) error {
	applied, err := supervisor.store.LoadAppliedState()
	if err != nil {
		return err
	}
	mode := state.EffectiveGatewayMode(applied.GatewayMode)
	if !applied.SpecCaptured || mode != state.GatewayModeOn && mode != state.GatewayModeGlobal {
		return supervisor.stopUnexpectedDataPlane(ctx, mode)
	}
	current, ok := supervisor.fabric.Current()
	if !ok {
		return supervisor.recoverFabric(ctx, applied, fmt.Errorf("Gateway Fabric has no active plane"))
	}
	if err := probeFabricTCP(ctx, current.Graph.DNS.Listen); err != nil {
		return supervisor.recoverFabric(ctx, applied, err)
	}
	if supervisor.verifyHost {
		if err := supervisor.verifyFabricHost(ctx); err != nil {
			return err
		}
	}
	if !supervisor.initialized || supervisor.restartWait {
		if supervisor.reconciler == nil {
			return fmt.Errorf("data-plane selection reconciler is required")
		}
		if err := supervisor.reconciler.ReconcileLiveSelectionsLocked(ctx); err != nil {
			return err
		}
	}
	supervisor.initialized = true
	supervisor.restartWait = false
	return supervisor.observeGateway(mode, true)
}

func (supervisor *dataPlaneSupervisor) stopUnexpectedDataPlane(ctx context.Context, mode string) error {
	statuses := []worker.Status(nil)
	if supervisor.workers != nil {
		statuses = supervisor.workers.Statuses()
		for _, status := range statuses {
			if status.State == worker.StateStarting || status.State == worker.StateRunning || status.State == worker.StateDraining {
				if err := supervisor.workers.StopAll(ctx); err != nil {
					return err
				}
				break
			}
		}
	}
	if _, active := supervisor.fabric.Current(); active {
		if err := supervisor.fabric.Stop(ctx); err != nil {
			return err
		}
	}
	supervisor.initialized = false
	supervisor.restartWait = false
	supervisor.reassertWait = false
	if err := supervisor.observeGateway(mode, false); err != nil {
		return err
	}
	for _, status := range statuses {
		if err := supervisor.observeWorker(status.ID, false); err != nil {
			return err
		}
	}
	return nil
}

func (supervisor *dataPlaneSupervisor) recoverFabric(ctx context.Context, applied state.AppliedState, cause error) error {
	if supervisor.restartWait {
		return &fabricRestartFailure{err: cause}
	}
	prepared, active := supervisor.fabric.Current()
	if active {
		if err := supervisor.fabric.Stop(ctx); err != nil {
			return &fabricRestartFailure{err: err}
		}
	} else {
		var err error
		prepared, err = supervisor.fabric.Prepare(ctx, fmt.Sprintf("fabric-supervisor-%d", time.Now().UnixNano()), applied.Spec, applied.ExecutionGraph.Fabric)
		if err != nil {
			return &fabricRestartFailure{err: err}
		}
	}
	if _, err := supervisor.fabric.Apply(ctx, prepared); err != nil {
		return &fabricRestartFailure{err: err}
	}
	supervisor.initialized = false
	supervisor.restartWait = true
	supervisor.reassertWait = false
	if err := supervisor.observeGateway("", false); err != nil {
		return err
	}
	log.Printf("fabric data-plane supervisor restarted Gateway Fabric after: %v", cause)
	return nil
}

// verifyFabricHost re-asserts the owned host state when it has drifted while
// the embedded DNS is still answering (firewall stop/flush, manual nft delete).
// A drift that survives one reassert is a confirmed failure and goes to rescue
// through the same path as a failed Fabric restart.
func (supervisor *dataPlaneSupervisor) verifyFabricHost(ctx context.Context) error {
	err := supervisor.fabric.Verify(ctx)
	if err == nil {
		supervisor.reassertWait = false
		return nil
	}
	if supervisor.reassertWait {
		return &fabricRestartFailure{err: fmt.Errorf("Gateway Fabric host state still incomplete after reassert: %w", err)}
	}
	if _, reassertErr := supervisor.fabric.Reassert(ctx); reassertErr != nil {
		return &fabricRestartFailure{err: fmt.Errorf("reassert Gateway Fabric host state after %v: %w", err, reassertErr)}
	}
	supervisor.reassertWait = true
	supervisor.reasserts++
	log.Printf("fabric data-plane supervisor reasserted Gateway Fabric host state after: %v", err)
	return nil
}

func probeFabricTCP(ctx context.Context, listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("parse Gateway Fabric DNS listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() == nil {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	} else if host == "" {
		host = "127.0.0.1"
	}
	dialer := net.Dialer{Timeout: defaultFabricSupervisorConnectTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("probe Gateway Fabric DNS: %w", err)
	}
	return connection.Close()
}

func (supervisor *dataPlaneSupervisor) HandleFabricFailure(ctx context.Context, failure error) error {
	if supervisor == nil {
		return fmt.Errorf("data-plane supervisor is not configured")
	}
	var errs []error
	if failure != nil {
		errs = append(errs, failure)
	}
	supervisor.initialized = false
	supervisor.restartWait = false
	if err := supervisor.enterRescue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("enter rescue: %w", err))
		return errors.Join(errs...)
	}
	if err := supervisor.observeGateway(state.GatewayModeSafe, false); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (supervisor *dataPlaneSupervisor) HandleWorkerFailure(ctx context.Context, failure worker.Failure) error {
	if supervisor == nil {
		return fmt.Errorf("data-plane supervisor is not configured")
	}
	if recovered, err := supervisor.recoverWorker(ctx, failure); recovered {
		return err
	} else if err != nil {
		log.Printf("worker %s restart failed; applying failure policy: %v", failure.WorkerID, err)
	}
	var errs []error
	if err := supervisor.observeWorker(failure.WorkerID, false); err != nil {
		errs = append(errs, err)
	}
	if supervisor.worker == nil {
		errs = append(errs, fmt.Errorf("worker failure controller is not configured"))
	} else if err := supervisor.worker.Handle(ctx, failure); err != nil {
		errs = append(errs, fmt.Errorf("worker failure recovery: %w", err))
	}
	if len(errs) == 0 {
		return nil
	}
	if err := supervisor.enterRescue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("enter rescue: %w", err))
		return errors.Join(errs...)
	}
	if err := supervisor.observeGateway(state.GatewayModeSafe, false); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Try one bounded process restart per minute before changing any Pool Binding.
// Repeated crashes and failed restarts still use the explicit failure policies.
func (supervisor *dataPlaneSupervisor) recoverWorker(ctx context.Context, failure worker.Failure) (bool, error) {
	if supervisor.workerRestarter == nil || supervisor.commit == nil || supervisor.store == nil || supervisor.reconciler == nil {
		return false, nil
	}
	handled := false
	_, err := supervisor.commit.WithCommit(ctx, commitlane.CommitRequest{
		OpID: fmt.Sprintf("worker-restart-%d", time.Now().UnixNano()), Kind: "worker_restart", StartedAt: time.Now().UTC(),
	}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.AcquireContext(ctx, level)
			if err != nil {
				return err
			}
			defer release()
		}
		applied, err := supervisor.store.LoadAppliedState()
		if err != nil {
			return err
		}
		mode := state.EffectiveGatewayMode(applied.GatewayMode)
		if !applied.SpecCaptured || mode != state.GatewayModeOn && mode != state.GatewayModeGlobal {
			handled = true
			return nil
		}
		if supervisor.rescuer != nil {
			active, err := supervisor.rescuer.Active()
			if err != nil {
				return err
			}
			if active {
				handled = true
				return nil
			}
		}
		if supervisor.workerRestarts == nil {
			supervisor.workerRestarts = map[string]time.Time{}
		}
		now := time.Now().UTC()
		if now.Sub(supervisor.workerRestarts[failure.WorkerID]) < time.Minute {
			return nil
		}
		restartCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		restarted, err := supervisor.workerRestarter.RestartFailed(restartCtx, failure.WorkerID)
		if restarted || err != nil {
			supervisor.workerRestarts[failure.WorkerID] = now
		}
		if err != nil {
			return err
		}
		// A queued exit may already have been superseded by an apply/stop.
		if !restarted {
			handled = true
			return nil
		}
		if err := supervisor.reconciler.ReconcileLiveSelectionsLocked(restartCtx); err != nil {
			return err
		}
		if err := supervisor.observeWorker(failure.WorkerID, true); err != nil {
			return err
		}
		handled = true
		return nil
	})
	return handled, err
}

func (supervisor *dataPlaneSupervisor) HandleWorkerFailureAsync(failure worker.Failure) {
	supervisor.handleAsync("worker data-plane supervisor", func(ctx context.Context) error {
		return supervisor.HandleWorkerFailure(ctx, failure)
	})
}

func (supervisor *dataPlaneSupervisor) handleAsync(label string, run func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultDataPlaneSupervisorTimeout)
		defer cancel()
		if err := run(ctx); err != nil {
			log.Printf("%s failed: %v", label, err)
		}
	}()
}

func (supervisor *dataPlaneSupervisor) enterRescue(ctx context.Context) error {
	if supervisor.rescuer == nil {
		return fmt.Errorf("rescue manager is required")
	}
	if supervisor.rescueReason == "" {
		return fmt.Errorf("data-plane rescue reason is required")
	}
	return supervisor.rescuer.Enter(ctx, supervisor.rescueReason)
}

func (supervisor *dataPlaneSupervisor) observeGateway(mode string, running bool) error {
	if supervisor.store == nil {
		return fmt.Errorf("runtime store is required")
	}
	observation, err := supervisor.store.LoadDataPlaneObservation()
	if err != nil {
		return err
	}
	observation.RunningGatewayMode = state.EffectiveGatewayMode(mode)
	if observation.Planes == nil {
		observation.Planes = map[string]state.DataPlaneComponentObservation{}
	}
	observation.Planes[state.DataPlaneGateway] = state.DataPlaneComponentObservation{Running: running}
	observation.ObservedAt = time.Now().UTC()
	return supervisor.store.SaveDataPlaneObservation(observation)
}

func (supervisor *dataPlaneSupervisor) observeWorker(workerID string, running bool) error {
	if workerID == "" {
		return nil
	}
	if supervisor.store == nil {
		return fmt.Errorf("runtime store is required")
	}
	observation, err := supervisor.store.LoadDataPlaneObservation()
	if err != nil {
		return err
	}
	if observation.Workers == nil {
		observation.Workers = map[string]state.DataPlaneComponentObservation{}
	}
	observation.Workers[workerID] = state.DataPlaneComponentObservation{Running: running}
	observation.ObservedAt = time.Now().UTC()
	return supervisor.store.SaveDataPlaneObservation(observation)
}
