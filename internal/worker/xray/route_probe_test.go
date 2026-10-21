package xray

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/worker"
)

type probeAPIConfig struct {
	Inbounds []struct {
		Tag      string `json:"tag"`
		Listen   string `json:"listen"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	} `json:"inbounds"`
	Routing struct {
		Rules []struct {
			Tag      string   `json:"ruleTag"`
			Inbounds []string `json:"inboundTag"`
			Outbound string   `json:"outboundTag"`
		} `json:"rules"`
	} `json:"routing"`
}

type probeAPIRunner struct {
	mu               sync.Mutex
	commands         []string
	rules            map[string]string
	inbounds         map[string]net.Listener
	files            []string
	failCommand      string
	cancelOnInbound  context.CancelFunc
	cleanupCancelled bool
	httpStatus       int
}

func newProbeAPIRunner(t *testing.T) *probeAPIRunner {
	t.Helper()
	runner := &probeAPIRunner{
		rules: map[string]string{}, inbounds: map[string]net.Listener{}, httpStatus: http.StatusNoContent,
	}
	t.Cleanup(func() {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		for _, listener := range runner.inbounds {
			_ = listener.Close()
		}
	})
	return runner
}

func (runner *probeAPIRunner) Run(ctx context.Context, command []string) (string, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(command) < 4 || command[1] != "api" {
		return "", fmt.Errorf("unexpected probe command")
	}
	action := command[2]
	runner.commands = append(runner.commands, action)
	if action == "rmi" || action == "rmrules" {
		runner.cleanupCancelled = runner.cleanupCancelled || ctx.Err() != nil
		tag := command[len(command)-1]
		if action == runner.failCommand {
			return "", fmt.Errorf("injected %s failure", action)
		}
		if action == "rmi" {
			if listener := runner.inbounds[tag]; listener != nil {
				_ = listener.Close()
				delete(runner.inbounds, tag)
			}
		} else {
			delete(runner.rules, tag)
		}
		return "", nil
	}
	if action != "adrules" && action != "adi" {
		return "", fmt.Errorf("probe attempted to change business routing: %s", action)
	}
	file := command[len(command)-1]
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("probe config is missing or not private")
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var config probeAPIConfig
	if err := json.Unmarshal(body, &config); err != nil {
		return "", err
	}
	runner.files = append(runner.files, file)
	if len(config.Inbounds) != 1 || len(config.Routing.Rules) != 1 {
		return "", fmt.Errorf("probe config changes more than its own resources")
	}
	inbound, rule := config.Inbounds[0], config.Routing.Rules[0]
	if inbound.Listen != "127.0.0.1" || inbound.Protocol != "socks" ||
		rule.Tag != inbound.Tag || !reflect.DeepEqual(rule.Inbounds, []string{inbound.Tag}) {
		return "", fmt.Errorf("probe is not a loopback executor-pinned route")
	}
	if action == "adrules" {
		if !containsString(command, "--append") {
			return "", fmt.Errorf("probe would replace existing rules")
		}
		runner.rules[rule.Tag] = rule.Outbound
	} else {
		if runner.rules[rule.Tag] != rule.Outbound {
			return "", fmt.Errorf("probe listener precedes its routing rule")
		}
		listener, err := net.Listen("tcp4", net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)))
		if err != nil {
			return "", err
		}
		runner.inbounds[inbound.Tag] = listener
		status := runner.httpStatus
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go serveProbeSOCKS(conn, status)
			}
		}()
		if runner.cancelOnInbound != nil {
			runner.cancelOnInbound()
			return "", ctx.Err()
		}
	}
	if action == runner.failCommand {
		return "", fmt.Errorf("injected %s failure after commit", action)
	}
	return "", nil
}

func serveProbeSOCKS(conn net.Conn, status int) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:2]); err != nil || header[0] != 5 {
		return
	}
	if _, err := io.CopyN(io.Discard, reader, int64(header[1])); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	if _, err := io.ReadFull(reader, header[:]); err != nil || header[1] != 1 {
		return
	}
	length := 0
	switch header[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		size, err := reader.ReadByte()
		if err != nil {
			return
		}
		length = int(size)
	default:
		return
	}
	if _, err := io.CopyN(io.Discard, reader, int64(length+2)); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	_ = request.Body.Close()
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
}

func probePath() worker.BindingPath {
	request := xrayRequest()
	route := request.Worker.Routes[0]
	route.Candidates = append(route.Candidates, "business-executor")
	route.Selected = "business-executor"
	request.Worker.Routes[0] = route
	return worker.BindingPath{Worker: request.Worker, Route: route, Executor: request.Executors[0]}
}

func assertProbeCleaned(t *testing.T, runner *probeAPIRunner, directory string) {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.rules) != 0 || len(runner.inbounds) != 0 || runner.cleanupCancelled {
		t.Fatalf("probe resources leaked or cleanup reused a canceled context: rules=%v inbounds=%d canceled=%v",
			runner.rules, len(runner.inbounds), runner.cleanupCancelled)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe files leaked: entries=%v err=%v", entries, err)
	}
}

func TestDelayRoutePinsUnselectedExecutorAndCleansUp(t *testing.T) {
	runner := newProbeAPIRunner(t)
	directory := t.TempDir()
	adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner, RuntimeDir: directory}}
	path := probePath()
	result, err := adapter.DelayRoute(context.Background(), path, "http://probe.invalid/check", time.Second)
	if err != nil || result.URL != "http://probe.invalid/check" {
		t.Fatalf("probe failed: result=%+v err=%v", result, err)
	}
	if path.Route.Selected != "business-executor" {
		t.Fatal("probe changed the selected executor")
	}
	if !reflect.DeepEqual(runner.commands, []string{"adrules", "adi", "rmi", "rmrules"}) {
		t.Fatalf("unexpected probe operations: %v", runner.commands)
	}
	assertProbeCleaned(t, runner, directory)
}

func TestDelayRouteCleansUpFailuresAndCancellation(t *testing.T) {
	for _, failure := range []string{"adrules", "adi", "http", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			runner := newProbeAPIRunner(t)
			directory := t.TempDir()
			adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner, RuntimeDir: directory}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "http":
				runner.httpStatus = http.StatusBadGateway
			case "cancel":
				runner.cancelOnInbound = cancel
			default:
				runner.failCommand = failure
			}
			if _, err := adapter.DelayRoute(ctx, probePath(), "http://probe.invalid/check", time.Second); err == nil {
				t.Fatal("injected failure was not reported")
			} else if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", err)
			}
			assertProbeCleaned(t, runner, directory)
		})
	}
}

func TestDelayRouteReportsCleanupFailure(t *testing.T) {
	runner := newProbeAPIRunner(t)
	runner.failCommand = "rmrules"
	adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner, RuntimeDir: t.TempDir()}}
	if _, err := adapter.DelayRoute(context.Background(), probePath(), "http://probe.invalid/check", time.Second); err == nil || !strings.Contains(err.Error(), "remove Xray probe rule") {
		t.Fatalf("cleanup failure was hidden: %v", err)
	}
}

func TestDelayRouteConcurrentProbesRemainIsolated(t *testing.T) {
	runner := newProbeAPIRunner(t)
	directory := t.TempDir()
	adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner, RuntimeDir: directory}}
	errors := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := adapter.DelayRoute(context.Background(), probePath(), "http://probe.invalid/check", 3*time.Second)
			errors <- err
		}()
	}
	for range 8 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	assertProbeCleaned(t, runner, directory)
}

func TestDelayRouteRejectsForeignExecutorAndNonLoopbackControl(t *testing.T) {
	for _, invalid := range []string{"executor", "control"} {
		t.Run(invalid, func(t *testing.T) {
			runner := newProbeAPIRunner(t)
			adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner, RuntimeDir: t.TempDir()}}
			path := probePath()
			if invalid == "executor" {
				path.Executor.ID = "not-a-route-candidate"
			} else {
				path.Worker.Control.Listen = "0.0.0.0"
			}
			if _, err := adapter.DelayRoute(context.Background(), path, "", 0); err == nil {
				t.Fatal("unsafe probe path accepted")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("rejected probe reached the control API: %v", runner.commands)
			}
		})
	}
}
