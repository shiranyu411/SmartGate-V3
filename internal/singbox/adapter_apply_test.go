package singbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type applyRunner struct {
	argv []string
}

func (r *applyRunner) Run(_ context.Context, argv []string) (string, error) {
	r.argv = append([]string(nil), argv...)
	return "restarted", nil
}

type applyReadyChecker struct {
	calls int
	err   error
}

func (c *applyReadyChecker) WaitReady(_ context.Context) (string, error) {
	c.calls++
	if c.err != nil {
		return "not ready", c.err
	}
	return "ready", nil
}

func TestApplyCandidateWritesConfigAndRestarts(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(candidate, []byte(`{"log":{"level":"info"}}`), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	runner := &applyRunner{}
	adapter := Adapter{
		ConfigPath:     filepath.Join(dir, "etc", "sing-box", "config.json"),
		RestartCommand: []string{"service", "sing-box", "restart"},
		Runner:         runner,
	}
	result, err := adapter.ApplyCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("apply candidate: %v", err)
	}
	if !result.Restarted || result.ConfigPath != adapter.ConfigPath {
		t.Fatalf("unexpected result: %+v", result)
	}
	body, err := os.ReadFile(adapter.ConfigPath)
	if err != nil {
		t.Fatalf("read applied config: %v", err)
	}
	if string(body) != `{"log":{"level":"info"}}` {
		t.Fatalf("unexpected applied config: %s", body)
	}
	if len(runner.argv) != 3 || runner.argv[0] != "service" {
		t.Fatalf("restart command not run: %+v", runner.argv)
	}
}

func TestApplyCandidateWaitsForReady(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(candidate, []byte(`{"log":{"level":"info"}}`), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	ready := &applyReadyChecker{}
	adapter := Adapter{
		ConfigPath:     filepath.Join(dir, "etc", "sing-box", "config.json"),
		RestartCommand: []string{"service", "sing-box", "restart"},
		Runner:         &applyRunner{},
		ReadyChecker:   ready,
	}
	result, err := adapter.ApplyCandidate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("apply candidate: %v", err)
	}
	if ready.calls != 1 || !result.Restarted || result.Log != "restarted\nready" {
		t.Fatalf("unexpected ready result calls=%d result=%+v", ready.calls, result)
	}
}

func TestApplyCandidateReturnsReadyError(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(candidate, []byte(`{"log":{"level":"info"}}`), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	adapter := Adapter{
		ConfigPath:     filepath.Join(dir, "etc", "sing-box", "config.json"),
		RestartCommand: []string{"service", "sing-box", "restart"},
		Runner:         &applyRunner{},
		ReadyChecker:   &applyReadyChecker{err: fmt.Errorf("not listening")},
	}
	result, err := adapter.ApplyCandidate(context.Background(), candidate)
	if err == nil {
		t.Fatal("expected ready error")
	}
	if result.Restarted || result.Log != "restarted\nnot ready" {
		t.Fatalf("unexpected error result: %+v", result)
	}
}

func TestRuntimeReadyCheckerDefaultPortsDoNotRequireBootstrapProxy(t *testing.T) {
	checker := RuntimeReadyChecker{}
	ports := checker.readyPorts()
	if len(ports) != 2 || ports[0] != "127.0.0.1:9090" || ports[1] != "127.0.0.1:1053" {
		t.Fatalf("unexpected default ready ports: %+v", ports)
	}
}

func TestRuntimeReadyCheckerHonorsExplicitPorts(t *testing.T) {
	checker := RuntimeReadyChecker{Ports: []string{"127.0.0.1:9090", "127.0.0.1:2080", "127.0.0.1:1053"}}
	ports := checker.readyPorts()
	if len(ports) != 3 || ports[1] != "127.0.0.1:2080" {
		t.Fatalf("explicit ready ports not preserved: %+v", ports)
	}
}

func TestBackupAndRestoreConfigRestoresPreviousConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "etc", "sing-box", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	runner := &applyRunner{}
	adapter := Adapter{
		RuntimeDir:     dir,
		ConfigPath:     configPath,
		RestartCommand: []string{"service", "sing-box", "restart"},
		StopCommand:    []string{"service", "sing-box", "stop"},
		Runner:         runner,
	}
	backup, err := adapter.BackupConfig("restore-test")
	if err != nil {
		t.Fatalf("backup config: %v", err)
	}
	if !backup.Existed || backup.BackupPath == "" {
		t.Fatalf("expected existing backup: %+v", backup)
	}
	if err := os.WriteFile(configPath, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	result, err := adapter.RestoreConfig(context.Background(), backup)
	if err != nil {
		t.Fatalf("restore config: %v", err)
	}
	if !result.Restarted {
		t.Fatalf("restore should restart sing-box: %+v", result)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if string(body) != `{"old":true}` {
		t.Fatalf("unexpected restored config: %s", body)
	}
}

func TestRestoreConfigRemovesConfigWhenNoPreviousConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "etc", "sing-box", "config.json")
	runner := &applyRunner{}
	adapter := Adapter{
		RuntimeDir:     dir,
		ConfigPath:     configPath,
		RestartCommand: []string{"service", "sing-box", "restart"},
		Runner:         runner,
	}
	backup, err := adapter.BackupConfig("restore-missing")
	if err != nil {
		t.Fatalf("backup missing config: %v", err)
	}
	if backup.Existed {
		t.Fatalf("backup should record missing config: %+v", backup)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	if _, err := adapter.RestoreConfig(context.Background(), backup); err != nil {
		t.Fatalf("restore missing config: %v", err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("restore should remove newly-created config, stat err=%v", err)
	}
	if len(runner.argv) == 0 || runner.argv[len(runner.argv)-1] != "stop" {
		t.Fatalf("restore missing config should stop sing-box, argv=%+v", runner.argv)
	}
}
