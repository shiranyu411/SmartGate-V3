package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type NodeProbeTarget struct {
	Node   smartnode.SmartNode
	Source smartnode.IdentitySource
}

// NodeProbeBatch runs at most one private process per selected core, with a
// dedicated loopback SOCKS route for every node. It never installs live state.
type NodeProbeBatch struct {
	inbounds  map[string]state.WorkerInbound
	processes map[smartnode.CoreID]*nodeProbeProcess
	cores     map[string]smartnode.CoreID
	cancel    context.CancelFunc
	directory string
	once      sync.Once
}

type nodeProbeProcess struct {
	command *exec.Cmd
	exited  chan struct{}
	err     error
}

func (batch *NodeProbeBatch) Close() {
	if batch == nil {
		return
	}
	batch.once.Do(func() {
		batch.cancel()
		for _, process := range batch.processes {
			_ = process.command.Process.Kill()
		}
		for _, process := range batch.processes {
			<-process.exited
		}
		_ = os.RemoveAll(batch.directory)
		<-nodeProbeSlot
	})
}

func (batch *NodeProbeBatch) Delay(ctx context.Context, nodeID, testURL string, timeout time.Duration) (DelayResult, error) {
	return batch.delay(ctx, nodeID, testURL, timeout, true)
}

func (batch *NodeProbeBatch) DelayLatency(ctx context.Context, nodeID, testURL string, timeout time.Duration) (DelayResult, error) {
	return batch.delay(ctx, nodeID, testURL, timeout, false)
}

func (batch *NodeProbeBatch) delay(ctx context.Context, nodeID, testURL string, timeout time.Duration, requireSuccess bool) (DelayResult, error) {
	result := DelayResult{URL: testURL}
	inbound, ok := batch.inbounds[nodeID]
	if !ok {
		return result, fmt.Errorf("%w: node has no batch route", ErrNodeProbeUnavailable)
	}
	process := batch.processes[batch.cores[nodeID]]
	select {
	case <-process.exited:
		return result, fmt.Errorf("%w: batch core exited: %v", ErrNodeProbeUnavailable, process.err)
	default:
	}
	started := time.Now()
	err := probeSOCKSRoute(ctx, inbound, testURL, timeout, requireSuccess)
	result.DelayMS = int(time.Since(started).Milliseconds())
	if err != nil {
		select {
		case <-process.exited:
			return result, fmt.Errorf("%w: batch core exited: %v", ErrNodeProbeUnavailable, process.err)
		default:
		}
	}
	return result, err
}

func (manager *Manager) SupportsNodeProbeBatch(core smartnode.CoreID) bool {
	adapter := manager.adapters[core]
	if _, ok := adapter.(ConfigCompiler); !ok {
		return false
	}
	descriptor := adapter.Descriptor()
	return descriptor.Process.Binary != "" && descriptor.Process.StartArgs != nil && descriptor.Process.CheckArgs != nil
}

func (manager *Manager) StartNodeProbeBatch(ctx context.Context, targets []NodeProbeTarget) (_ *NodeProbeBatch, err error) {
	select {
	case nodeProbeSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, ctx.Err())
	}
	runCtx, cancel := context.WithCancel(ctx)
	batch := &NodeProbeBatch{inbounds: map[string]state.WorkerInbound{}, processes: map[smartnode.CoreID]*nodeProbeProcess{}, cores: map[string]smartnode.CoreID{}, cancel: cancel}
	defer func() {
		if err != nil {
			batch.Close()
		}
	}()
	batch.directory, err = os.MkdirTemp("", "smartgate-node-probe-")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, err)
	}
	groups := map[smartnode.CoreID][]NodeProbeTarget{}
	for _, target := range targets {
		if _, exists := batch.cores[target.Node.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate node", ErrNodeProbeUnavailable)
		}
		batch.cores[target.Node.ID] = target.Node.CoreChoice
		groups[target.Node.CoreChoice] = append(groups[target.Node.CoreChoice], target)
	}
	cores := make([]smartnode.CoreID, 0, len(groups))
	for core := range groups {
		cores = append(cores, core)
	}
	sort.Slice(cores, func(i, j int) bool { return cores[i] < cores[j] })
	for _, core := range cores {
		if err = manager.startNodeProbeCore(runCtx, batch, core, groups[core]); err != nil {
			return nil, err
		}
	}
	return batch, nil
}

func (manager *Manager) startNodeProbeCore(ctx context.Context, batch *NodeProbeBatch, core smartnode.CoreID, targets []NodeProbeTarget) error {
	adapter := manager.adapters[core]
	compiler, ok := adapter.(ConfigCompiler)
	if !ok {
		return fmt.Errorf("%w: core %s has no isolated probe compiler", ErrNodeProbeUnavailable, core)
	}
	descriptor := adapter.Descriptor()
	if descriptor.Process.Binary == "" || descriptor.Process.StartArgs == nil || descriptor.Process.CheckArgs == nil {
		return fmt.Errorf("%w: core %s has no process commands", ErrNodeProbeUnavailable, core)
	}
	var reservations []net.Listener
	defer func() {
		for _, listener := range reservations {
			_ = listener.Close()
		}
	}()
	reserve := func() (int, error) {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		reservations = append(reservations, listener)
		return listener.Addr().(*net.TCPAddr).Port, nil
	}
	controlPort, err := reserve()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, err)
	}
	request := CompileRequest{
		Worker:     state.WorkerTarget{ID: "node_probe", Core: core, Control: state.WorkerControl{Kind: descriptor.Control.Kind, Listen: "127.0.0.1", Port: controlPort}},
		Identities: map[string]smartnode.IdentitySource{}, Generation: time.Now().UTC(),
	}
	for i, target := range targets {
		port, err := reserve()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, err)
		}
		executorID, routeID := fmt.Sprintf("probe_node_%d", i), fmt.Sprintf("probe_route_%d", i)
		inbound := state.WorkerInbound{ID: fmt.Sprintf("probe_socks_%d", i), Kind: "socks", Pool: routeID, Route: routeID, Listen: "127.0.0.1", Port: port, Network: "tcp"}
		request.Worker.Routes = append(request.Worker.Routes, state.WorkerRoute{ID: routeID, Pool: routeID, Selector: routeID, Candidates: []string{executorID}, Selected: executorID})
		request.Worker.Inbounds = append(request.Worker.Inbounds, inbound)
		request.Executors = append(request.Executors, state.NodeExecutor{ID: executorID, SmartNode: target.Node.ID, Revision: target.Node.Revision, Core: core, Worker: "node_probe"})
		request.Identities[target.Node.ID] = target.Source
		batch.inbounds[target.Node.ID] = inbound
	}
	config, err := compiler.Compile(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, err)
	}
	probeRuntime := FileRuntime{Core: core, RuntimeDir: batch.directory, Checker: ShellConfigChecker{Binary: descriptor.Process.Binary, Args: descriptor.Process.CheckArgs}}
	candidate, err := probeRuntime.Prepare(ctx, "node-probe-"+string(core), "node_probe", config)
	if err != nil {
		return fmt.Errorf("%w: %v: %s", ErrNodeProbeUnavailable, err, boundedText(candidate.CheckLog, 4096))
	}
	for _, listener := range reservations {
		_ = listener.Close()
	}
	output := newBoundedOutput(DefaultMaxCheckOutput)
	command := exec.CommandContext(ctx, descriptor.Process.Binary, descriptor.Process.StartArgs(candidate.Path)...)
	configureNodeProbeProcess(command)
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrNodeProbeUnavailable, err)
	}
	process := &nodeProbeProcess{command: command, exited: make(chan struct{})}
	batch.processes[core] = process
	go func() { process.err = command.Wait(); close(process.exited) }()
	setupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	pending := append([]state.WorkerInbound(nil), request.Worker.Inbounds...)
	for len(pending) > 0 {
		select {
		case <-process.exited:
			log := redactOutput(boundedText(output.String(), 4096), config.SensitiveValues)
			return fmt.Errorf("%w: core exited: %v: %s", ErrNodeProbeUnavailable, process.err, log)
		case <-setupCtx.Done():
			return fmt.Errorf("%w: waiting for loopback SOCKS: %v", ErrNodeProbeUnavailable, setupCtx.Err())
		case <-poll.C:
			next := pending[:0]
			for _, inbound := range pending {
				address := net.JoinHostPort(inbound.Listen, fmt.Sprint(inbound.Port))
				connection, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(setupCtx, "tcp", address)
				if err != nil {
					next = append(next, inbound)
				} else {
					_ = connection.Close()
				}
			}
			pending = next
		}
	}
	return nil
}
