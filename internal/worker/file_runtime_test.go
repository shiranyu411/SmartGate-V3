package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"smartgate/internal/netgate"
	"smartgate/internal/smartnode"
)

type checkerFunc func(context.Context, string) (string, error)

func (function checkerFunc) Check(ctx context.Context, path string) (string, error) {
	return function(ctx, path)
}

type fileRunner struct {
	commands [][]string
}

func (runner *fileRunner) Run(_ context.Context, argv []string) (string, error) {
	runner.commands = append(runner.commands, append([]string(nil), argv...))
	return "ok", nil
}

func TestFileRuntimeCandidateIsBoundedVerifiedAndRedacted(t *testing.T) {
	runtimeDir := t.TempDir()
	workerID := "worker-a"
	secret := "uuid-super-secret"
	runtime := &FileRuntime{
		Core: smartnode.CoreID("core-a"), RuntimeDir: runtimeDir,
		Checker: checkerFunc(func(_ context.Context, path string) (string, error) {
			body, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return "invalid credential " + secret + " in " + string(body[:1]), errors.New("invalid")
		}),
	}
	candidate, err := runtime.Prepare(context.Background(), "candidate-redact", workerID, CandidateConfig{
		Value: map[string]any{"credential": secret}, SensitiveValues: []string{secret},
	})
	if err == nil {
		t.Fatal("expected checker failure")
	}
	if strings.Contains(candidate.CheckLog, secret) || !strings.Contains(candidate.CheckLog, "[REDACTED]") {
		t.Fatalf("check log was not redacted: %q", candidate.CheckLog)
	}
	if candidate.CheckOK {
		t.Fatal("failed candidate marked checked")
	}
}

func TestFileRuntimeActivateBackupRestoreAndCleanup(t *testing.T) {
	runtimeDir := t.TempDir()
	workerID := "worker-a"
	configPath := filepath.Join(t.TempDir(), "core-a", "config.json")
	runner := &fileRunner{}
	runtime := &FileRuntime{
		Core: smartnode.CoreID("core-a"), RuntimeDir: runtimeDir,
		Checker: checkerFunc(func(context.Context, string) (string, error) { return "checked", nil }),
		Runner:  netgate.Runner(runner), ConfigPath: func(string) string { return configPath },
		StartCommand:  func(string) []string { return []string{"service", "restart"} },
		StopCommand:   func(string) []string { return []string{"service", "stop"} },
		StatusCommand: func(string) []string { return []string{"service", "status"} },
		SkipReadiness: true,
	}
	candidate, err := runtime.Prepare(context.Background(), "file-runtime", workerID, CandidateConfig{Value: map[string]any{"log": map[string]any{"level": "warning"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Verify(candidate); err != nil {
		t.Fatal(err)
	}
	activation, err := runtime.Activate(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !activation.Restarted {
		t.Fatalf("worker was not restarted: %+v", activation)
	}
	backup, err := runtime.Backup("backup-worker", workerID)
	if err != nil || !backup.Existed {
		t.Fatalf("backup=%+v err=%v", backup, err)
	}
	if err := os.WriteFile(configPath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	backup.WasActive = true
	if _, err := runtime.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RemoveCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(candidate.Path)); !os.IsNotExist(err) {
		t.Fatalf("candidate directory still exists: %v", err)
	}
}
