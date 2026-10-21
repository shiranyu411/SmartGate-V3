package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/smartnode"
)

const (
	DefaultMaxCandidateBytes = 1024 * 1024
	DefaultMaxCheckOutput    = 64 * 1024
	checkTruncatedMarker     = "\n...[TRUNCATED]"
)

type ConfigChecker interface {
	Check(context.Context, string) (string, error)
}

type ShellConfigChecker struct {
	Binary string
	Args   func(string) []string
}

func (checker ShellConfigChecker) Check(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(checker.Binary) == "" {
		return "", fmt.Errorf("config checker binary is required")
	}
	args := []string{"check", "-c", path}
	if checker.Args != nil {
		args = checker.Args(path)
	}
	output := newBoundedOutput(DefaultMaxCheckOutput)
	command := exec.CommandContext(ctx, checker.Binary, args...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("%s config check failed: %w", filepath.Base(checker.Binary), err)
	}
	return output.String(), nil
}

type CandidateConfig struct {
	Value           any
	SensitiveValues []string
	ReadyPorts      []string
}

type FileRuntime struct {
	Core              smartnode.CoreID
	RuntimeDir        string
	Checker           ConfigChecker
	Runner            netgate.Runner
	ConfigPath        func(string) string
	StartCommand      func(string) []string
	StopCommand       func(string) []string
	StatusCommand     func(string) []string
	MaxCandidateBytes int
	ReadyTimeout      time.Duration
	ReadyPollInterval time.Duration
	SkipReadiness     bool

	mu           sync.RWMutex
	activePorts  map[string][]string
	activeConfig map[string]string
	installed    map[string][]string
}

func (runtime *FileRuntime) Prepare(ctx context.Context, opID, workerID string, config CandidateConfig) (Candidate, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Candidate{}, err
	}
	if strings.TrimSpace(workerID) == "" {
		return Candidate{}, fmt.Errorf("worker id is required")
	}
	if runtime.Checker == nil {
		return Candidate{}, fmt.Errorf("worker config checker is required")
	}
	if strings.TrimSpace(runtime.RuntimeDir) == "" {
		return Candidate{}, fmt.Errorf("worker runtime dir is required")
	}
	body, err := json.MarshalIndent(config.Value, "", "  ")
	if err != nil {
		return Candidate{}, err
	}
	maxBytes := runtime.maxCandidateBytes()
	if len(body) > maxBytes {
		return Candidate{}, fmt.Errorf("worker candidate config too large: %d > %d", len(body), maxBytes)
	}
	candidateRoot := filepath.Join(runtime.RuntimeDir, "candidates", workerID)
	if err := os.MkdirAll(candidateRoot, 0o700); err != nil {
		return Candidate{}, err
	}
	candidateDir, err := os.MkdirTemp(candidateRoot, safeOpID+"-")
	if err != nil {
		return Candidate{}, err
	}
	path := filepath.Join(candidateDir, "config.json")
	if err := durableReplace(path, body, 0o600); err != nil {
		_ = os.RemoveAll(candidateDir)
		return Candidate{}, err
	}
	checkLog, checkErr := runtime.Checker.Check(ctx, path)
	checkLog = redactOutput(boundedText(checkLog, DefaultMaxCheckOutput), config.SensitiveValues)
	sum := sha256.Sum256(body)
	candidate := Candidate{
		OpID: safeOpID, WorkerID: workerID, Core: runtime.Core, Path: path, Size: len(body),
		ConfigSHA256: hex.EncodeToString(sum[:]), CheckLog: checkLog, CheckOK: checkErr == nil,
		CreatedAt: time.Now().UTC(), ReadyPorts: append([]string(nil), config.ReadyPorts...),
	}
	if checkErr != nil {
		return candidate, checkErr
	}
	return candidate, nil
}

func (runtime *FileRuntime) Verify(candidate Candidate) error {
	if candidate.WorkerID == "" || candidate.Path == "" {
		return fmt.Errorf("worker candidate identity is incomplete")
	}
	if candidate.Core != runtime.Core {
		return fmt.Errorf("worker candidate core %s does not match adapter %s", candidate.Core, runtime.Core)
	}
	if !candidate.CheckOK {
		return fmt.Errorf("worker candidate was not validated")
	}
	body, err := readBounded(candidate.Path, int64(runtime.maxCandidateBytes()))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("worker candidate no longer exists; run dry-run again: %w", err)
		}
		return err
	}
	if candidate.Size != 0 && candidate.Size != len(body) {
		return fmt.Errorf("worker candidate size changed; run dry-run again")
	}
	if candidate.ConfigSHA256 != "" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != candidate.ConfigSHA256 {
			return fmt.Errorf("worker candidate hash changed; run dry-run again")
		}
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("worker candidate is invalid JSON: %w", err)
	}
	return nil
}

func (runtime *FileRuntime) Activate(ctx context.Context, candidate Candidate) (Activation, error) {
	if err := runtime.Verify(candidate); err != nil {
		return Activation{}, err
	}
	// A dry-run may produce the same bytes that are already installed. Keep a
	// healthy process in place and only refresh the in-memory readiness ledger.
	if configPath, pathErr := runtime.configPath(candidate.WorkerID); pathErr == nil {
		if body, readErr := readBounded(configPath, int64(runtime.maxCandidateBytes())); readErr == nil {
			sum := sha256.Sum256(body)
			if candidate.ConfigSHA256 != "" && hex.EncodeToString(sum[:]) == candidate.ConfigSHA256 {
				if probeErr := runtime.probeReady(ctx, candidate.WorkerID, candidate.ReadyPorts); probeErr == nil {
					runtime.mu.Lock()
					if runtime.activePorts == nil {
						runtime.activePorts = map[string][]string{}
					}
					if runtime.activeConfig == nil {
						runtime.activeConfig = map[string]string{}
					}
					runtime.activePorts[candidate.WorkerID] = append([]string(nil), candidate.ReadyPorts...)
					runtime.activeConfig[candidate.WorkerID] = configPath
					runtime.mu.Unlock()
					return Activation{WorkerID: candidate.WorkerID, ConfigPath: configPath, Restarted: false}, nil
				}
			}
		}
	}
	activation, err := runtime.Install(ctx, candidate)
	if err != nil {
		return activation, err
	}
	return runtime.startInstalled(ctx, candidate.WorkerID, candidate.ReadyPorts, activation.ConfigPath)
}

func (runtime *FileRuntime) Install(_ context.Context, candidate Candidate) (Activation, error) {
	if err := runtime.Verify(candidate); err != nil {
		return Activation{}, err
	}
	configPath, err := runtime.configPath(candidate.WorkerID)
	if err != nil {
		return Activation{}, err
	}
	body, err := readBounded(candidate.Path, int64(runtime.maxCandidateBytes()))
	if err != nil {
		return Activation{}, err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return Activation{}, err
	}
	if err := durableReplace(configPath, body, 0o600); err != nil {
		return Activation{}, err
	}
	runtime.mu.Lock()
	if runtime.installed == nil {
		runtime.installed = map[string][]string{}
	}
	runtime.installed[candidate.WorkerID] = append([]string(nil), candidate.ReadyPorts...)
	runtime.mu.Unlock()
	return Activation{WorkerID: candidate.WorkerID, ConfigPath: configPath}, nil
}

func (runtime *FileRuntime) StartInstalled(ctx context.Context, workerID string, readyPorts []string) (Activation, error) {
	configPath, err := runtime.configPath(workerID)
	if err != nil {
		return Activation{}, err
	}
	if _, err := readBounded(configPath, int64(runtime.maxCandidateBytes())); err != nil {
		return Activation{}, err
	}
	if len(readyPorts) == 0 {
		runtime.mu.RLock()
		readyPorts = append([]string(nil), runtime.installed[workerID]...)
		runtime.mu.RUnlock()
	}
	return runtime.startInstalled(ctx, workerID, readyPorts, configPath)
}

func (runtime *FileRuntime) startInstalled(ctx context.Context, workerID string, readyPorts []string, configPath string) (Activation, error) {
	command, err := runtime.command(runtime.StartCommand, workerID, "start")
	if err != nil {
		return Activation{}, err
	}
	log, err := runtime.runner().Run(ctx, command)
	activation := Activation{WorkerID: workerID, ConfigPath: configPath, Log: boundedText(log, DefaultMaxCheckOutput)}
	if err != nil {
		return activation, err
	}
	runtime.mu.Lock()
	if runtime.activePorts == nil {
		runtime.activePorts = map[string][]string{}
	}
	if runtime.activeConfig == nil {
		runtime.activeConfig = map[string]string{}
	}
	runtime.activePorts[workerID] = append([]string(nil), readyPorts...)
	runtime.activeConfig[workerID] = configPath
	runtime.mu.Unlock()
	if !runtime.SkipReadiness {
		if err := runtime.waitReady(ctx, workerID, readyPorts); err != nil {
			return activation, err
		}
	}
	activation.Restarted = true
	return activation, nil
}

func (runtime *FileRuntime) Backup(opID, workerID string) (Backup, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Backup{}, err
	}
	configPath, err := runtime.configPath(workerID)
	if err != nil {
		return Backup{}, err
	}
	backup := Backup{WorkerID: workerID, Core: runtime.Core}
	runtime.mu.RLock()
	backup.ReadyPorts = append([]string(nil), runtime.installed[workerID]...)
	if len(backup.ReadyPorts) == 0 {
		backup.ReadyPorts = append([]string(nil), runtime.activePorts[workerID]...)
	}
	runtime.mu.RUnlock()
	body, err := readBounded(configPath, int64(runtime.maxCandidateBytes()))
	if err != nil {
		if os.IsNotExist(err) {
			return backup, nil
		}
		return Backup{}, err
	}
	backupRoot := filepath.Join(runtime.RuntimeDir, "live-backups", workerID)
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return Backup{}, err
	}
	backup.Path = filepath.Join(backupRoot, safeOpID+".json")
	if err := durableReplace(backup.Path, body, 0o600); err != nil {
		return Backup{}, err
	}
	backup.Existed = true
	return backup, nil
}

func (runtime *FileRuntime) Restore(ctx context.Context, backup Backup) (Activation, error) {
	if backup.Core != runtime.Core {
		return Activation{}, fmt.Errorf("worker backup core %s does not match adapter %s", backup.Core, runtime.Core)
	}
	configPath, err := runtime.configPath(backup.WorkerID)
	if err != nil {
		return Activation{}, err
	}
	if !backup.Existed {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			return Activation{}, err
		}
		if backup.WasActive {
			return Activation{}, fmt.Errorf("active worker %s has no backup config", backup.WorkerID)
		}
		if err := runtime.Stop(ctx, backup.WorkerID); err != nil {
			return Activation{}, err
		}
		return Activation{WorkerID: backup.WorkerID, ConfigPath: configPath}, nil
	}
	body, err := readBounded(backup.Path, int64(runtime.maxCandidateBytes()))
	if err != nil {
		return Activation{}, err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return Activation{}, err
	}
	if err := durableReplace(configPath, body, 0o600); err != nil {
		return Activation{}, err
	}
	runtime.mu.Lock()
	if runtime.installed == nil {
		runtime.installed = map[string][]string{}
	}
	runtime.installed[backup.WorkerID] = append([]string(nil), backup.ReadyPorts...)
	runtime.mu.Unlock()
	if !backup.WasActive {
		return Activation{WorkerID: backup.WorkerID, ConfigPath: configPath}, nil
	}
	return runtime.startInstalled(ctx, backup.WorkerID, backup.ReadyPorts, configPath)
}

func (runtime *FileRuntime) Stop(ctx context.Context, workerID string) error {
	command, err := runtime.command(runtime.StopCommand, workerID, "stop")
	if err != nil {
		return err
	}
	if _, err := runtime.runner().Run(ctx, command); err != nil {
		return err
	}
	runtime.mu.Lock()
	delete(runtime.activePorts, workerID)
	delete(runtime.activeConfig, workerID)
	runtime.mu.Unlock()
	return nil
}

func (runtime *FileRuntime) Probe(ctx context.Context, workerID string) error {
	runtime.mu.RLock()
	ports := append([]string(nil), runtime.activePorts[workerID]...)
	runtime.mu.RUnlock()
	return runtime.probeReady(ctx, workerID, ports)
}

func (runtime *FileRuntime) probeReady(ctx context.Context, workerID string, ports []string) error {
	command, err := runtime.command(runtime.StatusCommand, workerID, "status")
	if err != nil {
		return err
	}
	if _, err := runtime.runner().Run(ctx, command); err != nil {
		return err
	}
	for _, address := range ports {
		connection, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("worker %s port %s is not ready: %w", workerID, address, err)
		}
		_ = connection.Close()
	}
	return nil
}

func (runtime *FileRuntime) RemoveCandidate(candidate Candidate) error {
	if candidate.Path == "" {
		return nil
	}
	if _, err := opid.Clean(candidate.OpID); err != nil {
		return err
	}
	root := filepath.Clean(filepath.Join(runtime.RuntimeDir, "candidates", candidate.WorkerID))
	path := filepath.Clean(candidate.Path)
	dir := filepath.Dir(path)
	if !filepath.IsAbs(path) || dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) || filepath.Base(path) != "config.json" {
		return fmt.Errorf("refusing unsafe worker candidate cleanup path %q", path)
	}
	return os.RemoveAll(dir)
}

func (runtime *FileRuntime) maxCandidateBytes() int {
	if runtime.MaxCandidateBytes > 0 {
		return runtime.MaxCandidateBytes
	}
	return DefaultMaxCandidateBytes
}

func (runtime *FileRuntime) configPath(workerID string) (string, error) {
	if strings.TrimSpace(workerID) == "" {
		return "", fmt.Errorf("worker id is required")
	}
	if runtime.ConfigPath == nil {
		return "", fmt.Errorf("worker config path resolver is required")
	}
	path := filepath.Clean(runtime.ConfigPath(workerID))
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("worker config path must be absolute")
	}
	return path, nil
}

func (runtime *FileRuntime) command(builder func(string) []string, workerID, action string) ([]string, error) {
	if builder == nil {
		return nil, fmt.Errorf("worker %s command is required", action)
	}
	command := builder(workerID)
	if len(command) == 0 {
		return nil, fmt.Errorf("worker %s command is empty", action)
	}
	return append([]string(nil), command...), nil
}

func (runtime *FileRuntime) runner() netgate.Runner {
	if runtime.Runner != nil {
		return runtime.Runner
	}
	return netgate.ShellRunner{}
}

func (runtime *FileRuntime) waitReady(ctx context.Context, workerID string, ports []string) error {
	timeout := runtime.ReadyTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	interval := runtime.ReadyPollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		if err := runtime.Probe(deadlineCtx, workerID); err == nil {
			allReady := true
			for _, address := range ports {
				connection, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(deadlineCtx, "tcp", address)
				if err != nil {
					lastErr = err
					allReady = false
					break
				}
				_ = connection.Close()
			}
			if allReady {
				return nil
			}
		} else {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			return fmt.Errorf("worker %s not ready after %s: %w", workerID, timeout, lastErr)
		case <-timer.C:
		}
	}
}

func durableReplace(path string, body []byte, mode os.FileMode) (returnErr error) {
	temporary := path + ".smartgate-tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			_ = file.Close()
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readBounded(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file %s exceeds %d bytes", filepath.Base(path), maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("file %s exceeds %d bytes", filepath.Base(path), maxBytes)
	}
	return body, nil
}

type boundedOutput struct {
	mu        sync.Mutex
	limit     int
	buffer    bytes.Buffer
	truncated bool
}

func newBoundedOutput(limit int) *boundedOutput {
	return &boundedOutput{limit: limit}
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.limit - output.buffer.Len()
	if remaining > 0 {
		keep := len(value)
		if keep > remaining {
			keep = remaining
		}
		_, _ = output.buffer.Write(value[:keep])
	}
	if len(value) > remaining {
		output.truncated = true
	}
	return len(value), nil
}

func (output *boundedOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return boundedTextWithFlag(output.buffer.String(), output.limit, output.truncated)
}

func boundedText(value string, limit int) string {
	return boundedTextWithFlag(value, limit, len(value) > limit)
}

func boundedTextWithFlag(value string, limit int, truncated bool) string {
	if limit <= 0 {
		return ""
	}
	if len(value) > limit {
		value = value[:limit]
		truncated = true
	}
	if !truncated {
		return value
	}
	if limit <= len(checkTruncatedMarker) {
		return checkTruncatedMarker[:limit]
	}
	keep := limit - len(checkTruncatedMarker)
	if keep > len(value) {
		keep = len(value)
	}
	return value[:keep] + checkTruncatedMarker
}

func redactOutput(output string, sensitive []string) string {
	redacted := output
	for _, value := range sensitive {
		value = strings.TrimSpace(value)
		if len(value) < 4 {
			continue
		}
		redacted = strings.ReplaceAll(redacted, value, "[REDACTED]")
	}
	return redacted
}
