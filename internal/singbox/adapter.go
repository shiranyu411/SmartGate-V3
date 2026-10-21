package singbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"smartgate/internal/opid"
)

const DefaultMaxCandidateBytes = 1024 * 1024
const DefaultMaxCheckOutputBytes = 64 * 1024
const CheckOutputTruncatedMarker = "\n...[TRUNCATED]"
const MaxOpIDLength = opid.MaxLength
const DefaultConfigPath = "/etc/sing-box/config.json"

func durableReplaceFile(path string, body []byte, mode os.FileMode, tempSuffix string) (retErr error) {
	tmp := path + tempSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(body); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type Checker interface {
	Check(ctx context.Context, path string) (string, error)
}

type Runner interface {
	Run(ctx context.Context, argv []string) (string, error)
}

type ReadyChecker interface {
	WaitReady(ctx context.Context) (string, error)
}

type ShellChecker struct {
	Binary string
}

func (c ShellChecker) Check(ctx context.Context, path string) (string, error) {
	binary := c.Binary
	if binary == "" {
		binary = "sing-box"
	}
	output := newBoundedCommandOutput(DefaultMaxCheckOutputBytes)
	cmd := exec.CommandContext(ctx, binary, "check", "-c", path)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("sing-box check failed: %w", err)
	}
	return output.String(), nil
}

type boundedCommandOutput struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func newBoundedCommandOutput(limit int) *boundedCommandOutput {
	if limit < 0 {
		limit = 0
	}
	return &boundedCommandOutput{limit: limit, buf: make([]byte, 0, limit)}
}

func (b *boundedCommandOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		b.buf = append(b.buf, p[:keep]...)
	}
	if len(p) > remaining {
		b.truncated = true
	}
	// Report the whole write as consumed so os/exec keeps draining both pipes
	// without retaining output beyond the configured limit.
	return len(p), nil
}

func (b *boundedCommandOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return boundedCheckOutput(string(b.buf), b.limit, b.truncated)
}

func boundedCheckOutput(output string, limit int, alreadyTruncated ...bool) string {
	if limit <= 0 {
		return ""
	}
	truncated := len(output) > limit
	if len(alreadyTruncated) > 0 && alreadyTruncated[0] {
		truncated = true
	}
	if !truncated {
		return output
	}
	if limit <= len(CheckOutputTruncatedMarker) {
		return CheckOutputTruncatedMarker[:limit]
	}
	keep := limit - len(CheckOutputTruncatedMarker)
	if keep > len(output) {
		keep = len(output)
	}
	return output[:keep] + CheckOutputTruncatedMarker
}

type ShellRunner struct{}

func (ShellRunner) Run(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	output := newBoundedCommandOutput(DefaultMaxCheckOutputBytes)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("command %q failed: %w", argv[0], err)
	}
	return output.String(), nil
}

type Adapter struct {
	RuntimeDir        string
	Checker           Checker
	Runner            Runner
	ReadyChecker      ReadyChecker
	ConfigPath        string
	RestartCommand    []string
	StopCommand       []string
	MaxCandidateBytes int
}

func (a Adapter) maxCandidateBytes() int64 {
	limit := a.MaxCandidateBytes
	if limit <= 0 {
		limit = DefaultMaxCandidateBytes
	}
	return int64(limit)
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
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

type Candidate struct {
	OpID         string    `json:"op_id"`
	Path         string    `json:"path"`
	Size         int       `json:"size"`
	ConfigSHA256 string    `json:"config_sha256"`
	CreatedAt    time.Time `json:"created_at"`
	SpecHash     string    `json:"spec_hash,omitempty"`
	ConfigEpoch  int64     `json:"config_epoch,omitempty"`
	TopologyHash string    `json:"topology_hash,omitempty"`
	CheckLog     string    `json:"check_log,omitempty"`
	CheckOK      bool      `json:"check_ok"`
}

func (a Adapter) DryRun(ctx context.Context, opID string, cfg MinimalConfig) (Candidate, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Candidate{}, err
	}
	if a.Checker == nil {
		return Candidate{}, fmt.Errorf("checker is required")
	}
	if a.RuntimeDir == "" {
		return Candidate{}, fmt.Errorf("runtime dir is required")
	}
	maxBytes := a.MaxCandidateBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCandidateBytes
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Candidate{}, err
	}
	if len(b) > maxBytes {
		return Candidate{}, fmt.Errorf("candidate config too large: %d > %d", len(b), maxBytes)
	}
	sum := sha256.Sum256(b)
	candidateRoot := filepath.Join(a.RuntimeDir, "candidates")
	if err := os.MkdirAll(candidateRoot, 0o755); err != nil {
		return Candidate{}, err
	}
	candidateDir, err := os.MkdirTemp(candidateRoot, "singbox-"+safeOpID+"-")
	if err != nil {
		return Candidate{}, err
	}
	path := filepath.Join(candidateDir, "config.json")
	if err := durableReplaceFile(path, b, 0o600, ".tmp"); err != nil {
		return Candidate{}, err
	}
	log, err := a.Checker.Check(ctx, path)
	log = boundedCheckOutput(log, DefaultMaxCheckOutputBytes)
	candidate := Candidate{
		OpID:         safeOpID,
		Path:         path,
		Size:         len(b),
		CheckLog:     log,
		CheckOK:      err == nil,
		ConfigSHA256: hex.EncodeToString(sum[:]),
		CreatedAt:    time.Now().UTC(),
	}
	if err != nil {
		return candidate, err
	}
	return candidate, nil
}

type ApplyResult struct {
	ConfigPath string `json:"config_path"`
	Restarted  bool   `json:"restarted"`
	Log        string `json:"log,omitempty"`
}

type ConfigBackup struct {
	ConfigPath string `json:"config_path"`
	BackupPath string `json:"backup_path,omitempty"`
	Existed    bool   `json:"existed"`
}

func (a Adapter) ApplyCandidate(ctx context.Context, candidatePath string) (ApplyResult, error) {
	if candidatePath == "" {
		return ApplyResult{}, fmt.Errorf("candidate path is required")
	}
	configPath := a.ConfigPath
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	body, err := readBoundedFile(candidatePath, a.maxCandidateBytes())
	if err != nil {
		return ApplyResult{}, err
	}
	if err := verifyCandidateFile(candidatePath, body); err != nil {
		return ApplyResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return ApplyResult{}, err
	}
	if err := durableReplaceFile(configPath, body, 0o600, ".smartgate-tmp"); err != nil {
		return ApplyResult{}, err
	}
	command := a.RestartCommand
	if len(command) == 0 {
		command = []string{"/etc/init.d/sing-box", "restart"}
	}
	runner := a.Runner
	if runner == nil {
		runner = ShellRunner{}
	}
	log, err := runner.Run(ctx, command)
	if err != nil {
		return ApplyResult{ConfigPath: configPath, Log: log}, err
	}
	if a.ReadyChecker != nil {
		readyLog, err := a.ReadyChecker.WaitReady(ctx)
		if readyLog != "" {
			if log != "" {
				log += "\n"
			}
			log += readyLog
		}
		if err != nil {
			return ApplyResult{ConfigPath: configPath, Log: log}, err
		}
	}
	return ApplyResult{ConfigPath: configPath, Restarted: true, Log: log}, nil
}

func VerifyCandidate(candidate Candidate) error {
	if candidate.Path == "" {
		return fmt.Errorf("candidate path is required")
	}
	body, err := readBoundedFile(candidate.Path, DefaultMaxCandidateBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("candidate config no longer exists; run dry-run again: %w", err)
		}
		return err
	}
	if candidate.Size > 0 && len(body) != candidate.Size {
		return fmt.Errorf("candidate config size mismatch: file=%d record=%d; run dry-run again", len(body), candidate.Size)
	}
	if candidate.ConfigSHA256 != "" {
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != candidate.ConfigSHA256 {
			return fmt.Errorf("candidate config hash mismatch; run dry-run again")
		}
	}
	return verifyCandidateFile(candidate.Path, body)
}

// RemoveCandidate removes the candidate's owned temporary directory. The
// layout checks deliberately fail closed so a corrupted dry-run record cannot
// turn cleanup into an arbitrary RemoveAll.
func RemoveCandidate(candidate Candidate) error {
	if candidate.Path == "" {
		return nil
	}
	safeOpID, err := opid.Clean(candidate.OpID)
	if err != nil {
		return err
	}
	path := filepath.Clean(candidate.Path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("candidate cleanup rejected relative path %q", path)
	}
	if filepath.Base(path) != "config.json" {
		return fmt.Errorf("candidate cleanup rejected unexpected file %q", path)
	}
	dir := filepath.Dir(path)
	if !strings.HasPrefix(filepath.Base(dir), "singbox-"+safeOpID+"-") || filepath.Base(filepath.Dir(dir)) != "candidates" {
		return fmt.Errorf("candidate cleanup rejected unowned directory %q", dir)
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("candidate cleanup rejected non-directory %q", dir)
	}
	return os.RemoveAll(dir)
}

func verifyCandidateFile(path string, body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("candidate config is empty: %s", path)
	}
	return nil
}

type RuntimeReadyChecker struct {
	ServiceCommand  []string
	ClashAPIURL     string
	ClashSecret     string
	ClashSecretPath string
	HTTPClient      *http.Client
	PollInterval    time.Duration
	Timeout         time.Duration
	Ports           []string
}

func (c RuntimeReadyChecker) WaitReady(ctx context.Context) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	poll := c.PollInterval
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		if err := c.checkOnce(deadlineCtx); err == nil {
			return "sing-box ready", nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(poll)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			return "", fmt.Errorf("sing-box not ready after %s: %w", timeout, lastErr)
		case <-timer.C:
		}
	}
}

func (c RuntimeReadyChecker) checkOnce(ctx context.Context) error {
	command := c.ServiceCommand
	if len(command) == 0 {
		command = []string{"/etc/init.d/sing-box", "status"}
	}
	output := newBoundedCommandOutput(DefaultMaxCheckOutputBytes)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("service status failed: %s: %w", output.String(), err)
	}
	for _, addr := range c.readyPorts() {
		conn, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("port %s not listening: %w", addr, err)
		}
		_ = conn.Close()
	}
	baseURL := c.ClashAPIURL
	if baseURL == "" {
		baseURL = DefaultClashAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/proxies", nil)
	if err != nil {
		return err
	}
	if secret := c.clashSecret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 1 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("clash api not ready: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("clash api status %d", resp.StatusCode)
	}
	return nil
}

func (c RuntimeReadyChecker) readyPorts() []string {
	if len(c.Ports) > 0 {
		return c.Ports
	}
	return []string{"127.0.0.1:9090", "127.0.0.1:1053"}
}

func (c RuntimeReadyChecker) clashSecret() string {
	if c.ClashSecret != "" {
		return c.ClashSecret
	}
	if c.ClashSecretPath == "" {
		return ""
	}
	body, err := readBoundedFile(c.ClashSecretPath, 8<<10)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func (a Adapter) BackupConfig(opID string) (ConfigBackup, error) {
	configPath := a.ConfigPath
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return ConfigBackup{}, err
	}
	backup := ConfigBackup{ConfigPath: configPath}
	body, err := readBoundedFile(configPath, a.maxCandidateBytes())
	if err != nil {
		if os.IsNotExist(err) {
			return backup, nil
		}
		return ConfigBackup{}, err
	}
	runtimeDir := a.RuntimeDir
	if runtimeDir == "" {
		runtimeDir = filepath.Dir(configPath)
	}
	backupRoot := filepath.Join(runtimeDir, "live-backups")
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return ConfigBackup{}, err
	}
	backupPath := filepath.Join(backupRoot, "singbox-"+safeOpID+".json")
	if err := durableReplaceFile(backupPath, body, 0o600, ".tmp"); err != nil {
		return ConfigBackup{}, err
	}
	backup.BackupPath = backupPath
	backup.Existed = true
	return backup, nil
}

func (a Adapter) RestoreConfig(ctx context.Context, backup ConfigBackup) (ApplyResult, error) {
	configPath := backup.ConfigPath
	if configPath == "" {
		configPath = a.ConfigPath
	}
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return ApplyResult{}, err
	}
	if backup.Existed {
		body, err := readBoundedFile(backup.BackupPath, a.maxCandidateBytes())
		if err != nil {
			return ApplyResult{}, err
		}
		if err := durableReplaceFile(configPath, body, 0o600, ".smartgate-restore-tmp"); err != nil {
			return ApplyResult{}, err
		}
	} else if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return ApplyResult{}, err
	} else if err == nil {
		if err := syncDir(filepath.Dir(configPath)); err != nil {
			return ApplyResult{}, err
		}
	}
	command := a.RestartCommand
	if !backup.Existed {
		command = a.StopCommand
	}
	if len(command) == 0 {
		if backup.Existed {
			command = []string{"/etc/init.d/sing-box", "restart"}
		} else {
			command = []string{"/etc/init.d/sing-box", "stop"}
		}
	}
	runner := a.Runner
	if runner == nil {
		runner = ShellRunner{}
	}
	log, err := runner.Run(ctx, command)
	if err != nil {
		return ApplyResult{ConfigPath: configPath, Log: log}, err
	}
	if backup.Existed && a.ReadyChecker != nil {
		readyLog, err := a.ReadyChecker.WaitReady(ctx)
		if readyLog != "" {
			if log != "" {
				log += "\n"
			}
			log += readyLog
		}
		if err != nil {
			return ApplyResult{ConfigPath: configPath, Log: log}, err
		}
	}
	return ApplyResult{ConfigPath: configPath, Restarted: true, Log: log}, nil
}
