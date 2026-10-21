package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"smartgate/internal/auth"
	"smartgate/internal/commitlane"
	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/rescue"
	"smartgate/internal/scheduler"
	"smartgate/internal/selection"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

var (
	ErrServerClosed        = http.ErrServerClosed
	DefaultShutdownTimeout = 5 * time.Second
)

type Config struct {
	ListenAddr            string
	StaticDir             string
	CORSOrigins           []string
	Layout                state.Layout
	Locks                 *lockset.LockSet
	CommitLane            *commitlane.Manager
	Store                 *state.Store
	ProviderTxn           *commitlane.ProviderTxn
	PoolTxn               *commitlane.BusinessPoolTxn
	SmartNodeTxn          *commitlane.SmartNodeTxn
	BackupTxn             *commitlane.BackupTxn
	SnapshotTxn           *commitlane.SnapshotTxn
	ConfigTxn             *commitlane.ConfigTxn
	SelectorTxn           *commitlane.SelectorTxn
	Recovery              *commitlane.RecoveryOrchestrator
	ProviderRef           *provider.RefreshService
	Scheduler             *scheduler.Runner
	SchedulerDisabled     bool
	Rescue                *rescue.Manager
	Owners                *netgate.OwnerRegistry
	WorkerCleanupTargets  []netgate.WorkerCleanupTarget
	NetGateRunner         netgate.Runner
	SystemRunner          netgate.Runner
	Workers               *worker.Manager
	Fabric                *fabric.Manager
	CoreRegistry          *coredesc.Registry
	DataPlaneRescueReason string
	// FabricHostVerification makes the data-plane supervisor verify the owned
	// nft table, policy routes, hook and dnsmasq override on every poll and
	// reassert them when they drift. Enable only on a live OpenWrt runtime.
	FabricHostVerification bool
	ProxyControl           selection.ProxyController
	Status                 Status
	Auth                   *auth.Store
	AuthRequired           bool
}

type Server struct {
	cfg             Config
	http            *http.Server
	scheduler       *schedulerDaemon
	planeSupervisor *dataPlaneSupervisor
}

func NewServer(cfg Config) *Server {
	if cfg.ConfigTxn == nil {
		cfg.ConfigTxn = configTxnForConfig(cfg)
	}
	if cfg.SelectorTxn == nil && cfg.CommitLane != nil && cfg.Store != nil {
		if cfg.Workers != nil && cfg.Fabric != nil {
			cfg.SelectorTxn = commitlane.NewCutoverTxn(cfg.CommitLane, cfg.Store, cfg.Workers, cfg.Fabric)
		} else if cfg.ProxyControl != nil {
			cfg.SelectorTxn = commitlane.NewSelectorTxn(cfg.CommitLane, cfg.Store, cfg.ProxyControl)
		}
	}
	if cfg.SmartNodeTxn == nil && cfg.CommitLane != nil && cfg.Store != nil {
		cfg.SmartNodeTxn = commitlane.NewSmartNodeTxn(cfg.CommitLane, cfg.Store, state.NewSnapshotStore(cfg.Layout))
	}
	if cfg.Recovery == nil && cfg.ConfigTxn != nil && cfg.SelectorTxn != nil {
		cfg.Recovery = commitlane.NewRecoveryOrchestrator(cfg.ConfigTxn, cfg.SelectorTxn)
	}
	planeSupervisor := newDataPlaneSupervisor(cfg)
	if cfg.Workers != nil && cfg.Store != nil && cfg.SelectorTxn != nil {
		cfg.Workers.WithFailureHandler(func(_ context.Context, failure worker.Failure) {
			planeSupervisor.HandleWorkerFailureAsync(failure)
		})
	}
	if cfg.BackupTxn != nil {
		cfg.BackupTxn.WithConfigTxn(cfg.ConfigTxn)
	}
	if cfg.SnapshotTxn != nil {
		cfg.SnapshotTxn.WithConfigTxn(cfg.ConfigTxn)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/runtime/locks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(lockset.DescribeHierarchy()))
	})
	if cfg.StaticDir != "" {
		registerStaticHandler(mux, cfg.StaticDir)
	}
	handler := http.Handler(mux)
	if cfg.Auth != nil && cfg.AuthRequired {
		handler = authMiddleware(handler, cfg.Auth)
	}
	handler = corsMiddleware(handler, cfg.CORSOrigins)
	handler = requestBodyLimitMiddleware(handler)
	server := &Server{
		cfg:             cfg,
		planeSupervisor: planeSupervisor,
		http: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	if cfg.Scheduler != nil {
		cfg.Scheduler.WithServiceRestarter(NewSchedulerServiceRestarter(cfg))
		cfg.Scheduler.WithRouterRebooter(NewSchedulerRouterRebooter(cfg))
		if cfg.Store != nil {
			cfg.Scheduler.WithFailoverRunner(NewSchedulerFailoverRunner(cfg))
		}
	}
	if cfg.Store != nil && cfg.Scheduler != nil && !cfg.SchedulerDisabled {
		if cfg.ConfigTxn != nil {
			cfg.Scheduler.WithConfigApplier(NewSchedulerConfigApplier(cfg, server.currentStatus))
		}
		server.scheduler = newSchedulerDaemon(cfg.Store, cfg.Locks, cfg.Scheduler)
	}
	if cfg.Auth != nil {
		registerAuthHandlers(mux, cfg.Auth)
	}
	owners := cfg.Owners
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	registerHandlers(mux, server.currentStatus, cfg.Rescue, cfg.Recovery)
	registerEventHandlers(mux, server.currentStatus)
	registerCoreHandlers(mux, cfg.CoreRegistry)
	registerSystemHandlers(mux, cfg, server.currentStatus)
	registerWiFiHandlers(mux, cfg)
	registerConnectionHandlersWithConfig(mux, connectionsAPI{
		store: cfg.Store, workers: cfg.Workers,
		fabric: cfg.Fabric, selector: cfg.SelectorTxn, locks: cfg.Locks, layout: cfg.Layout,
	})
	if cfg.Store != nil {
		registerConfigHandlers(mux, cfg, server.currentStatus)
		registerDNSHandlers(mux, cfg.Store, cfg.Locks)
	}
	if cfg.Workers != nil {
		registerWorkerHandlers(mux, cfg.Workers)
	}
	registerOpenWrtHandlers(mux, owners, cfg.WorkerCleanupTargets)
	registerRouteHandlers(mux, cfg.Store, cfg.Locks)
	if cfg.Store != nil {
		registerNodeHandlers(mux, cfg.Store, cfg.Layout, cfg.Locks, cfg.ProxyControl, cfg.Workers, cfg.SelectorTxn, cfg.SmartNodeTxn, cfg.Scheduler)
		registerRulesHandlers(mux, cfg.Store, cfg.Locks)
		registerRuleSetHandlers(mux, cfg.Store, cfg.Locks, cfg.CoreRegistry)
	}
	if cfg.Store != nil && cfg.ProviderTxn != nil {
		registerProviderHandlers(mux, cfg)
	}
	if cfg.Store != nil && cfg.PoolTxn != nil {
		registerBusinessPoolHandlers(mux, cfg.Store, cfg.PoolTxn, cfg.Locks)
	}
	if cfg.BackupTxn != nil {
		registerBackupHandlers(mux, cfg.Layout, cfg.BackupTxn)
	}
	if cfg.SnapshotTxn != nil {
		registerSnapshotHandlers(mux, cfg.Layout, cfg.SnapshotTxn)
	}
	if cfg.Scheduler != nil || cfg.Store != nil {
		registerSchedulerHandlers(mux, cfg.Store, cfg.Locks, cfg.Scheduler)
	}
	return server
}

func (s *Server) ListenAndServe() error {
	if s.cfg.Workers != nil {
		stop := s.cfg.Workers.StartSupervisor(context.Background())
		defer stop()
	}
	if s.planeSupervisor != nil {
		stop := s.planeSupervisor.Start(context.Background())
		defer stop()
	}
	if s.scheduler != nil {
		stop := s.scheduler.Start(context.Background())
		defer stop()
	}
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) currentStatus() Status {
	status := s.cfg.Status
	if status.Phase == "" {
		status = NewStatus(s.cfg.ListenAddr, state.RuntimeState{})
	}
	if s.cfg.Store == nil {
		return status
	}
	runtimeState, err := s.cfg.Store.LoadRuntime()
	if err != nil {
		return status
	}
	// RuntimeDir may be empty after reboot before bootstrap rewrite. Prefer
	// applied topology metadata for status fields so operators do not see
	// epoch/hash 0 while AppliedState is already captured.
	if runtimeState.ConfigEpoch == 0 || runtimeState.TopologyHash == "" || runtimeState.GatewayMode == "" {
		if applied, appliedErr := s.cfg.Store.LoadAppliedState(); appliedErr == nil && applied.SchemaVersion != 0 && applied.SpecCaptured {
			if runtimeState.ConfigEpoch == 0 {
				runtimeState.ConfigEpoch = applied.ConfigEpoch
			}
			if runtimeState.TopologyHash == "" {
				runtimeState.TopologyHash = applied.TopologyHash
			}
			if runtimeState.GatewayMode == "" {
				runtimeState.GatewayMode = applied.GatewayMode
			}
		}
	}
	if s.cfg.Rescue != nil {
		active, err := s.cfg.Rescue.Active()
		if err == nil {
			runtimeState.RescueActive = active
			if active {
				rs, err := s.cfg.Rescue.Status()
				if err == nil {
					runtimeState.RescueReason = rs.Reason
				}
			} else {
				runtimeState.RescueReason = ""
			}
		}
	}
	status = NewStatus(s.cfg.ListenAddr, runtimeState)
	if report, ok, err := s.cfg.Store.LoadLegacyMigrationReport(); err == nil && ok {
		status.LegacyMigration = legacyMigrationStatus(report)
	}
	if s.cfg.Workers != nil {
		for _, workerStatus := range s.cfg.Workers.Statuses() {
			status.Workers = append(status.Workers, WorkerSummary{
				ID: workerStatus.ID, Core: workerStatus.Core, State: workerStatus.State,
				Pools: append([]string(nil), workerStatus.Pools...),
			})
		}
	}
	if topology, topologyErr := s.cfg.Store.LoadAppliedTopology(); topologyErr == nil {
		if graph, graphErr := rebuildLiveExecutionGraph(topology, time.Now().UTC()); graphErr == nil {
			status.Bindings = append([]state.PoolBinding(nil), graph.PoolBindings...)
		}
	}
	if s.cfg.SelectorTxn != nil {
		cutovers, drains := s.cfg.SelectorTxn.Progress()
		for _, progress := range cutovers {
			status.CutoversInFlight = append(status.CutoversInFlight, CutoverProgress{Pool: progress.Pool, Stage: progress.Stage})
		}
		for _, progress := range drains {
			deadline := progress.Deadline
			status.Drains = append(status.Drains, DrainProgress{
				Pool: progress.Pool, OldGeneration: progress.OldGeneration, Remaining: progress.Remaining, Deadline: &deadline,
				Status: progress.Status, LastError: progress.LastError, RetryCount: progress.RetryCount,
			})
		}
	}
	if runtimeState.RescueActive {
		status.GatewayMode = state.GatewayModeSafe
	}
	return status
}

func ValidateLANOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		return errors.New("listen host cannot be empty")
	}
	if host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("refusing non-LAN bind address %q", addr)
	}
	if strings.HasPrefix(host, "127.") || host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("listen host %q is not an IP or localhost", host)
	}
	if !ip.IsPrivate() {
		return fmt.Errorf("listen host %q is not private", host)
	}
	return nil
}
