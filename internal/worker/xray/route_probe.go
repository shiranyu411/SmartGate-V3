package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"smartgate/internal/state"
	"smartgate/internal/worker"
)

func (adapter *Adapter) DelayRoute(ctx context.Context, path worker.BindingPath, testURL string, timeout time.Duration) (result worker.DelayResult, err error) {
	if path.Worker.Core != adapter.Core() || path.Executor.Core != adapter.Core() ||
		path.Executor.Worker != path.Worker.ID || path.Executor.ID == "" ||
		!containsString(path.Route.Candidates, path.Executor.ID) {
		return result, fmt.Errorf("Xray probe requires an executor from the requested Worker route")
	}
	address, err := xrayControlAddress(path.Worker)
	if err != nil {
		return result, err
	}
	if testURL == "" {
		testURL = worker.DefaultRouteProbeURL
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	result.URL = testURL
	// Slow router control calls must not consume the HTTP measurement budget.
	probeCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	listener, err := (&net.ListenConfig{}).Listen(probeCtx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return result, err
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	runtimeDir := os.TempDir()
	if adapter.Runtime != nil && adapter.Runtime.RuntimeDir != "" {
		runtimeDir = adapter.Runtime.RuntimeDir
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return result, err
	}
	directory, err := os.MkdirTemp(runtimeDir, "smartgate-xray-probe-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(directory)
	tag := filepath.Base(directory)
	inbound := state.WorkerInbound{ID: tag, Kind: "socks", Listen: "127.0.0.1", Port: port, Network: "tcp"}
	compiled, err := compileXrayInbound(inbound)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]any{
		"inbounds": []map[string]any{compiled},
		"routing": map[string]any{"rules": []map[string]any{{
			"type": "field", "ruleTag": tag, "inboundTag": []string{tag}, "outboundTag": path.Executor.ID,
		}}},
	})
	if err != nil {
		return result, err
	}
	configPath := filepath.Join(directory, "probe.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		return result, err
	}
	if err := listener.Close(); err != nil {
		return result, err
	}

	// Probe a pinned outbound without overriding any live Pool balancer. An
	// interrupted API call may already have committed, so clean up attempts too.
	ruleAttempted, inboundAttempted := false, false
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if inboundAttempted {
			if _, cleanupErr := adapter.runner().Run(cleanupCtx, []string{
				adapter.binary(), "api", "rmi", "--server=" + address, "--timeout=2", tag,
			}); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove Xray probe inbound: %w", cleanupErr))
			}
		}
		if ruleAttempted {
			if _, cleanupErr := adapter.runner().Run(cleanupCtx, []string{
				adapter.binary(), "api", "rmrules", "--server=" + address, "--timeout=2", tag,
			}); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove Xray probe rule: %w", cleanupErr))
			}
		}
	}()
	ruleAttempted = true
	if _, err = adapter.runner().Run(probeCtx, []string{
		adapter.binary(), "api", "adrules", "--server=" + address, "--append", configPath,
	}); err != nil {
		return result, fmt.Errorf("add Xray probe rule: %w", err)
	}
	inboundAttempted = true
	if _, err = adapter.runner().Run(probeCtx, []string{
		adapter.binary(), "api", "adi", "--server=" + address, configPath,
	}); err != nil {
		return result, fmt.Errorf("add Xray probe inbound: %w", err)
	}
	started := time.Now()
	err = worker.VerifySOCKSRoute(probeCtx, inbound, testURL, timeout)
	result.DelayMS = int(time.Since(started).Milliseconds())
	return result, err
}
