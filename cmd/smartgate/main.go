package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"smartgate/internal/auth"
	"smartgate/internal/backup"
	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/rescue"
	"smartgate/internal/runtime"
	"smartgate/internal/scheduler"
	"smartgate/internal/singbox"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/version"
	"smartgate/internal/worker"
	workerfakecore "smartgate/internal/worker/fakecore"
	workersingbox "smartgate/internal/worker/singbox"
	workerxray "smartgate/internal/worker/xray"
)

const (
	defaultDataDir    = "/etc/smartgate"
	defaultRuntimeDir = "/tmp/smartgate"
	defaultListen     = "192.168.1.1:3000"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-version", "--version", "version":
			fmt.Println(version.Line())
			return
		case "rescue":
			if err := runRescue(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "locks":
			fmt.Print(lockset.DescribeHierarchy())
			return
		}
	}

	if err := runServe(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("smartgate", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "persistent SmartGate data directory")
	runtimeDir := fs.String("runtime-dir", defaultRuntimeDir, "ephemeral SmartGate runtime directory")
	listen := fs.String("listen", defaultListen, "LAN-only listen address")
	bootstrapProxy := fs.String("bootstrap-proxy", provider.DefaultBootstrapProxyURL, "loopback HTTP proxy URL for via_bootstrap provider refreshes")
	clashAPI := fs.String("clash-api", singbox.DefaultClashAPIURL, "loopback Clash-compatible API URL for legacy selector control")
	clashSecret := fs.String("clash-secret", "/etc/sing-box/clash.secret", "Clash-compatible API bearer secret file")
	staticDir := fs.String("static-dir", "", "optional static frontend directory to serve for non-API paths")
	corsOrigins := fs.String("cors-origin", "", "comma-separated allowed CORS origins for separately served frontends")
	schedulerDisabled := fs.Bool("scheduler-disabled", false, "disable automatic scheduler polling (deployment/maintenance gate)")
	devFakeCore := fs.Bool("dev-fakecore", false, "register FakeCore no-op worker adapter on non-OpenWrt dev runtime")
	if err := fs.Parse(args); err != nil {
		return err
	}

	layout := state.Layout{
		DataDir:    *dataDir,
		RuntimeDir: *runtimeDir,
	}
	liveRuntime := openWrtRuntime()
	if liveRuntime && *devFakeCore {
		return fmt.Errorf("-dev-fakecore is only allowed on non-OpenWrt dev runtime")
	}
	netGateRunner := netgate.Runner(nil)
	if !liveRuntime {
		netGateRunner = noopRunner{}
	}
	knownAdapters := newWorkerAdapters(layout, netGateRunner, liveRuntime, *devFakeCore)
	// Rescue must be able to stop every Worker this build knows how to start,
	// whether or not its core is installed right now.
	cleanupTargets := workerCleanupTargetsForAdapters(knownAdapters)
	workerAdapters := knownAdapters
	if liveRuntime {
		installed, err := installedWorkerAdapters(knownAdapters, binaryInstalled)
		if err != nil {
			return err
		}
		workerAdapters = installed
		if err := probeLiveWorkerVersions(context.Background(), workerAdapters); err != nil {
			return err
		}
	}
	coreRegistry, err := newCoreRegistry(workerAdapters...)
	if err != nil {
		return err
	}
	configureSmartNodeEvaluators(coreRegistry)
	locks := lockset.New()
	boot, err := runtime.Bootstrap(context.Background(), layout, *listen, locks, cleanupTargets)
	if err != nil {
		return err
	}
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(boot.CommitLane, provider.NewLastGoodStore(layout)).WithStateStore(boot.Store), provider.HTTPFetcher{
		BootstrapProxyURL: *bootstrapProxy,
	})
	proxyControl := singbox.NewClashClient(
		*clashAPI,
		*clashSecret,
	)
	systemRunner := netgate.Runner(nil)
	if !liveRuntime {
		noop := noopRunner{}
		systemRunner = noop
		log.Printf("non-OpenWrt host detected; live apply uses runtime-local config and no-op system commands")
	}
	workerManager, fabricManager, err := newWorkerDataPlane(layout, boot.Owners, netGateRunner, liveRuntime, coreRegistry, workerAdapters...)
	if err != nil {
		return err
	}
	configTxn := commitlane.NewConfigTxn(boot.CommitLane, boot.Store, configapply.Engine{
		Owners:               boot.Owners,
		WorkerCleanupTargets: cleanupTargets,
		NetGateRunner:        netGateRunner,
		Workers:              workerManager,
		Fabric:               fabricManager,
	})
	selectorTxn := commitlane.NewCutoverTxn(boot.CommitLane, boot.Store, workerManager, fabricManager)
	recovery := commitlane.NewRecoveryOrchestrator(configTxn, selectorTxn).WithLegacyCore(legacyCoreID(workerAdapters))
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 45*time.Second)
	recoveryErr := recovery.Recover(recoveryCtx)
	cancelRecovery()
	if recoveryErr != nil {
		// A blocked startup reconcile must never take the daemon down: procd
		// would respawn it into the same failure and the LAN would lose both
		// the data plane and the management API. Degrade to rescue (fail-open
		// LAN, owner resources cleaned) and serve the API so the operator can
		// repair the topology and POST /api/runtime/rescue/clear.
		degradeToStartupRescue(boot, recoveryErr)
	}
	backupTxn := commitlane.NewBackupTxn(boot.CommitLane, boot.Store, state.NewSnapshotStore(layout), layout).WithConfigTxn(configTxn)
	schedulerRunner := scheduler.NewRunner(boot.Store, refresher).
		WithDelayTester(layout, runtime.NewSchedulerRuntimeStore(boot.Store, locks), runtime.NewSchedulerNodeDelayTester(runtime.Config{Store: boot.Store, Layout: layout, Locks: locks, Workers: workerManager, SelectorTxn: selectorTxn})).
		WithBackupCreator(schedulerBackupCreator{txn: backupTxn})
	providerTxn := commitlane.NewProviderTxn(boot.CommitLane, boot.Store, state.NewSnapshotStore(layout)).
		WithLastGoodStore(provider.NewLastGoodStore(layout))
	cfg := runtime.Config{
		ListenAddr:             *listen,
		StaticDir:              *staticDir,
		CORSOrigins:            splitCSV(*corsOrigins),
		Layout:                 layout,
		Locks:                  locks,
		CommitLane:             boot.CommitLane,
		Store:                  boot.Store,
		ProviderTxn:            providerTxn,
		PoolTxn:                commitlane.NewBusinessPoolTxn(boot.CommitLane, boot.Store, state.NewSnapshotStore(layout)),
		SmartNodeTxn:           commitlane.NewSmartNodeTxn(boot.CommitLane, boot.Store, state.NewSnapshotStore(layout)),
		BackupTxn:              backupTxn,
		SnapshotTxn:            commitlane.NewSnapshotTxn(boot.CommitLane, boot.Store, state.NewSnapshotStore(layout)).WithConfigTxn(configTxn),
		ConfigTxn:              configTxn,
		SelectorTxn:            selectorTxn,
		Recovery:               recovery,
		ProviderRef:            refresher,
		Scheduler:              schedulerRunner,
		SchedulerDisabled:      *schedulerDisabled,
		Rescue:                 boot.Rescue,
		Owners:                 boot.Owners,
		WorkerCleanupTargets:   cleanupTargets,
		NetGateRunner:          netGateRunner,
		SystemRunner:           systemRunner,
		ProxyControl:           proxyControl,
		Workers:                workerManager,
		Fabric:                 fabricManager,
		CoreRegistry:           coreRegistry,
		DataPlaneRescueReason:  "data_plane_unavailable",
		FabricHostVerification: liveRuntime,
		Status:                 boot.Status,
		Auth:                   auth.NewStore(layout.AuthPath()),
		AuthRequired:           true,
	}
	srv := runtime.NewServer(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), runtime.DefaultShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, runtime.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// legacyCoreID names the core every node ran on before SmartNodes existed, so
// migrated bindings default to it. Only the sing-box adapter carries that
// history; other adapters return an empty id and the migration falls back to
// capability order.
func legacyCoreID(adapters []worker.Adapter) smartnode.CoreID {
	for _, adapter := range adapters {
		if legacy, ok := adapter.(*workersingbox.Adapter); ok {
			return legacy.Descriptor().ID
		}
	}
	return ""
}

func degradeToStartupRescue(boot *runtime.BootstrapResult, cause error) {
	log.Printf("startup recovery failed; entering rescue so the management API stays reachable: %v", cause)
	rescueCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := boot.Rescue.Enter(rescueCtx, rescue.ReasonStartupRecovery); err != nil {
		log.Printf("enter rescue after startup recovery failure: %v", err)
	}
	runtimeState, err := boot.Store.LoadRuntime()
	if err != nil {
		log.Printf("record startup recovery failure: %v", err)
		return
	}
	runtimeState.RecoveryError = cause.Error()
	runtimeState.RescueActive = true
	runtimeState.RescueReason = rescue.ReasonStartupRecovery
	runtimeState.GatewayMode = state.GatewayModeSafe
	if err := boot.Store.SaveRuntime(runtimeState); err != nil {
		log.Printf("record startup recovery failure: %v", err)
	}
	boot.Status = runtime.NewStatus(boot.Status.ListenAddr, runtimeState)
}

func splitCSV(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

type noopRunner struct{}

func (noopRunner) Run(_ context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "smartgate dev no-op: empty command", nil
	}
	return "smartgate dev no-op: " + strings.Join(argv, " "), nil
}

type noopConfigChecker struct{}

func (noopConfigChecker) Check(context.Context, string) (string, error) {
	return "smartgate dev no-op config check", nil
}

const workerControlCommand = "/usr/libexec/smartgate-worker-control"

func newWorkerAdapters(layout state.Layout, runner netgate.Runner, live bool, includeFakeCore bool) []worker.Adapter {
	activeDir := filepath.Join(layout.DataDir, "workers")
	if !live {
		activeDir = filepath.Join(layout.RuntimeDir, "dev-workers")
	}
	commands := func(action string) func(string) []string {
		return func(workerID string) []string { return []string{workerControlCommand, action, workerID} }
	}
	singChecker := worker.ConfigChecker(noopConfigChecker{})
	xrayChecker := worker.ConfigChecker(noopConfigChecker{})
	if live {
		singChecker = worker.ShellConfigChecker{Binary: "/usr/bin/sing-box", Args: func(path string) []string { return []string{"check", "-c", path} }}
		xrayChecker = worker.ShellConfigChecker{Binary: "/usr/bin/xray", Args: func(path string) []string { return []string{"run", "-test", "-config", path} }}
	}
	newRuntime := func(checker worker.ConfigChecker) *worker.FileRuntime {
		return &worker.FileRuntime{
			RuntimeDir: filepath.Join(layout.RuntimeDir, "worker-runtime"), Checker: checker, Runner: runner,
			ConfigPath:   func(workerID string) string { return filepath.Join(activeDir, workerID+".json") },
			StartCommand: commands("restart"), StopCommand: commands("stop"), StatusCommand: commands("status"),
			SkipReadiness: !live,
		}
	}
	adapters := []worker.Adapter{
		&workersingbox.Adapter{Runtime: newRuntime(singChecker)},
		&workerxray.Adapter{Runtime: newRuntime(xrayChecker), Binary: "/usr/bin/xray"},
	}
	if includeFakeCore {
		adapters = append(adapters, &workerfakecore.Adapter{Runtime: newRuntime(worker.ConfigChecker(noopConfigChecker{}))})
	}
	return adapters
}

// liveCoreBinary names the executable each adapter drives on a live OpenWrt
// host. Cores are discovered, not assumed: a SmartNode can only select a core
// whose binary is installed, and a missing optional core must not keep the
// daemon from starting.
func liveCoreBinary(adapter worker.Adapter) string {
	switch adapter := adapter.(type) {
	case *workersingbox.Adapter:
		return "/usr/bin/sing-box"
	case *workerxray.Adapter:
		if adapter.Binary != "" {
			return adapter.Binary
		}
		return "/usr/bin/xray"
	}
	return ""
}

func binaryInstalled(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// installedWorkerAdapters keeps the adapters whose core binary exists. At least
// one execution core must be present; the skipped ones are logged so an
// operator sees why a core is absent from /api/cores.
func installedWorkerAdapters(adapters []worker.Adapter, installed func(string) bool) ([]worker.Adapter, error) {
	kept := make([]worker.Adapter, 0, len(adapters))
	var missing []string
	for _, adapter := range adapters {
		binary := liveCoreBinary(adapter)
		if binary != "" && !installed(binary) {
			missing = append(missing, fmt.Sprintf("%s (%s)", adapter.Descriptor().ID, binary))
			continue
		}
		kept = append(kept, adapter)
	}
	for _, entry := range missing {
		log.Printf("execution core %s is not installed; SmartNodes cannot select it until the package is added", entry)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no execution core is installed (%s)", strings.Join(missing, ", "))
	}
	return kept, nil
}

func probeLiveWorkerVersions(ctx context.Context, adapters []worker.Adapter) error {
	for _, adapter := range adapters {
		switch adapter := adapter.(type) {
		case *workersingbox.Adapter:
			version, err := adapter.ProbeVersion(ctx)
			if err != nil {
				return fmt.Errorf("probe sing-box version: %w", err)
			}
			adapter.Version = version
		case *workerxray.Adapter:
			version, err := adapter.ProbeVersion(ctx)
			if err != nil {
				return fmt.Errorf("probe Xray version: %w", err)
			}
			adapter.Version = version
		}
	}
	return nil
}

func newWorkerDataPlane(layout state.Layout, owners *netgate.OwnerRegistry, runner netgate.Runner, live bool, registry *coredesc.Registry, adapters ...worker.Adapter) (*worker.Manager, *fabric.Manager, error) {
	configureSmartNodeEvaluators(registry)
	planner.ConfigureCoreRegistry(registry)
	manager, err := worker.NewManager(adapters...)
	if err != nil {
		return nil, nil, err
	}
	fabricManager := fabric.NewManager(owners, runner)
	ruleSets := fabric.NewRuleSetStore(layout)
	ruleSets.Decompiler = fabric.ShellRuleSetDecompiler{Binary: "/usr/bin/sing-box"}
	ruleSets.ProxyAddress = fabricManager.RuleSetProxyAddress
	fabricManager.WithRuleSetStore(ruleSets)
	if !live {
		fabricManager.WithConnectionTracker(fabric.NoopConnectionTracker{})
	}
	return manager, fabricManager, nil
}

func newCoreRegistry(adapters ...worker.Adapter) (*coredesc.Registry, error) {
	registry := coredesc.NewRegistry()
	for _, adapter := range adapters {
		if err := registry.Register(adapter.Descriptor()); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func workerCleanupTargets(registry *coredesc.Registry) []netgate.WorkerCleanupTarget {
	return cleanupTargetsForDescriptors(registry.List())
}

// workerCleanupTargetsForAdapters covers every Worker this build can start,
// including cores that are not installed on the host: a rescue must still be
// able to stop a Worker left behind by an earlier install.
func workerCleanupTargetsForAdapters(adapters []worker.Adapter) []netgate.WorkerCleanupTarget {
	descriptors := make([]coredesc.Descriptor, 0, len(adapters))
	for _, adapter := range adapters {
		descriptors = append(descriptors, adapter.Descriptor())
	}
	return cleanupTargetsForDescriptors(descriptors)
}

func cleanupTargetsForDescriptors(descriptors []coredesc.Descriptor) []netgate.WorkerCleanupTarget {
	targets := make([]netgate.WorkerCleanupTarget, 0, len(descriptors))
	for _, descriptor := range descriptors {
		targets = append(targets, netgate.WorkerCleanupTarget{
			ID:          descriptor.WorkerID(1),
			DisplayName: descriptor.DisplayName,
		})
	}
	return targets
}

func configureSmartNodeEvaluators(registry *coredesc.Registry) {
	descriptors := registry.List()
	defaultCore := (&workersingbox.Adapter{}).Descriptor().ID
	evaluators := make([]smartnode.CoreEvaluator, 0, len(descriptors))
	for _, descriptor := range descriptors {
		evaluators = append(evaluators, smartnode.CoreEvaluator{
			Core: descriptor.ID, Default: descriptor.ID == defaultCore, Evaluate: descriptor.Capability.Evaluator,
		})
	}
	smartnode.ConfigureCoreEvaluators(evaluators)
}

func openWrtRuntime() bool {
	for _, path := range []string{"/etc/openwrt_release", "/etc/board.json"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	body, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return false
	}
	contents := strings.ToLower(string(body))
	return strings.Contains(contents, "openwrt") || strings.Contains(contents, "immortalwrt")
}

type schedulerBackupCreator struct {
	txn *commitlane.BackupTxn
}

const scheduledBackupRetention = 14

func (c schedulerBackupCreator) Create(ctx context.Context, opID, kind string) (scheduler.BackupResult, error) {
	record, err := c.txn.Create(ctx, opID, kind)
	if err != nil {
		return scheduler.BackupResult{}, err
	}
	result := scheduler.BackupResult{
		ID:        record.ID,
		Path:      record.Path,
		Size:      record.Size,
		SHA256:    record.SHA256,
		CreatedAt: record.CreatedAt,
	}
	if _, err := backup.PruneScheduled(c.txn.Layout(), scheduledBackupRetention); err != nil {
		return result, fmt.Errorf("backup %s created but scheduled retention failed: %w", record.ID, err)
	}
	return result, nil
}

func runRescue(args []string) error {
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		args = append(args[1:], args[0])
	}
	fs := flag.NewFlagSet("smartgate rescue", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "persistent SmartGate data directory")
	runtimeDir := fs.String("runtime-dir", defaultRuntimeDir, "ephemeral SmartGate runtime directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := "enter"
	if fs.NArg() > 0 {
		action = fs.Arg(fs.NArg() - 1)
	}

	layout := state.Layout{
		DataDir:    *dataDir,
		RuntimeDir: *runtimeDir,
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	liveRuntime := openWrtRuntime()
	netRunner := netgate.Runner(netgate.ShellRunner{})
	if !liveRuntime {
		netRunner = noopRunner{}
	}
	knownAdapters := newWorkerAdapters(layout, netRunner, liveRuntime, false)
	cleanupTargets := workerCleanupTargetsForAdapters(knownAdapters)
	workerAdapters := knownAdapters
	if liveRuntime {
		installed, err := installedWorkerAdapters(knownAdapters, binaryInstalled)
		if err != nil {
			return err
		}
		workerAdapters = installed
	}
	coreRegistry, err := newCoreRegistry(workerAdapters...)
	if err != nil {
		return err
	}
	locks := lockset.New()
	boot, err := runtime.Bootstrap(context.Background(), layout, defaultListen, locks, cleanupTargets)
	if err != nil {
		return err
	}
	switch action {
	case "enter":
		if err := boot.Rescue.Enter(context.Background(), rescue.ReasonManual); err != nil {
			return err
		}
		fmt.Printf("rescue mode entered: %s\n", filepath.Clean(layout.RescueStatusPath()))
	case "status":
		active, err := boot.Rescue.Active()
		if err != nil {
			return err
		}
		if !active {
			fmt.Println("rescue inactive")
			return nil
		}
		status, err := boot.Rescue.Status()
		if err != nil {
			return err
		}
		fmt.Printf("rescue active: reason=%s entered_at=%s\n", status.Reason, status.EnteredAt.Format(time.RFC3339))
	case "clear":
		workerManager, fabricManager, err := newWorkerDataPlane(layout, boot.Owners, netRunner, liveRuntime, coreRegistry, workerAdapters...)
		if err != nil {
			return err
		}
		configTxn := commitlane.NewConfigTxn(boot.CommitLane, boot.Store, configapply.Engine{
			Owners:               boot.Owners,
			WorkerCleanupTargets: cleanupTargets,
			NetGateRunner:        netRunner,
			Workers:              workerManager,
			Fabric:               fabricManager,
		})
		selectorTxn := commitlane.NewCutoverTxn(boot.CommitLane, boot.Store, workerManager, fabricManager)
		recovery := commitlane.NewRecoveryOrchestrator(configTxn, selectorTxn)
		clearCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := boot.Rescue.ClearWithRecovery(clearCtx, func() error {
			recoveryCtx, recoveryCancel := context.WithTimeout(clearCtx, 30*time.Second)
			defer recoveryCancel()
			return recovery.RecoverForRescueClear(recoveryCtx)
		}); err != nil {
			return err
		}
		fmt.Printf("rescue mode cleared: %s\n", filepath.Clean(layout.RescueStatusPath()))
	default:
		return fmt.Errorf("unknown rescue action %q", action)
	}
	return nil
}
