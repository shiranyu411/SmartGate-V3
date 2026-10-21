package singbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"smartgate/internal/netgate"
)

type fakeChecker struct {
	seenPath string
	log      string
	err      error
}

func (f *fakeChecker) Check(_ context.Context, path string) (string, error) {
	f.seenPath = path
	return f.log, f.err
}

func TestAdapterDryRunWritesCandidateAndChecks(t *testing.T) {
	checker := &fakeChecker{log: "ok"}
	adapter := Adapter{
		RuntimeDir: t.TempDir(),
		Checker:    checker,
	}
	candidate, err := adapter.DryRun(context.Background(), "op-1", BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate")))
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if !candidate.CheckOK || candidate.Path == "" || candidate.Size == 0 || candidate.ConfigSHA256 == "" || candidate.CreatedAt.IsZero() {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
	if checker.seenPath != candidate.Path {
		t.Fatalf("checker path = %q, want %q", checker.seenPath, candidate.Path)
	}
	b, err := os.ReadFile(candidate.Path)
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if len(b) != candidate.Size {
		t.Fatalf("candidate size mismatch: file=%d candidate=%d", len(b), candidate.Size)
	}
}

func TestRemoveCandidateRemovesOwnedDirectoryOnly(t *testing.T) {
	runtimeDir := t.TempDir()
	adapter := Adapter{RuntimeDir: runtimeDir, Checker: &fakeChecker{}}
	candidate, err := adapter.DryRun(context.Background(), "cleanup-op", BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate")))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(candidate.Path)
	if err := RemoveCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("candidate directory remains after cleanup: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCandidate(Candidate{OpID: "cleanup-op", Path: outside}); err == nil {
		t.Fatal("unowned candidate path was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unowned file was removed: %v", err)
	}
}

func TestAdapterDryRunRejectsOversizedCandidate(t *testing.T) {
	adapter := Adapter{
		RuntimeDir:        t.TempDir(),
		Checker:           &fakeChecker{},
		MaxCandidateBytes: 8,
	}
	if _, err := adapter.DryRun(context.Background(), "op-1", BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))); err == nil {
		t.Fatal("expected oversized candidate to fail")
	}
}

func TestAdapterDryRunRequiresChecker(t *testing.T) {
	adapter := Adapter{RuntimeDir: t.TempDir()}
	if _, err := adapter.DryRun(context.Background(), "op-1", BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))); err == nil {
		t.Fatal("expected missing checker to fail")
	}
}

func TestAdapterDryRunRejectsUnsafeOpID(t *testing.T) {
	adapter := Adapter{
		RuntimeDir: t.TempDir(),
		Checker:    &fakeChecker{},
	}
	for _, opID := range []string{"../escape", "nested/path", "has space", strings.Repeat("x", MaxOpIDLength+1)} {
		if _, err := adapter.DryRun(context.Background(), opID, BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate"))); err == nil {
			t.Fatalf("expected unsafe op id %q to fail", opID)
		}
	}
}

func TestAdapterDryRunBoundsCheckLog(t *testing.T) {
	secret := "super-secret-cookie"
	checker := &fakeChecker{
		log: strings.Repeat("x", DefaultMaxCheckOutputBytes) + secret + "/private/runtime/candidates/config.json",
		err: errors.New("check failed"),
	}
	adapter := Adapter{RuntimeDir: t.TempDir(), Checker: checker}
	candidate, err := adapter.DryRun(context.Background(), "large-check-log", BuildMinimalConfig(netgate.NewOwnerRegistry("smartgate")))
	if err == nil {
		t.Fatal("expected checker failure")
	}
	if len(candidate.CheckLog) > DefaultMaxCheckOutputBytes {
		t.Fatalf("check log exceeded hard limit: %d", len(candidate.CheckLog))
	}
	if !strings.HasSuffix(candidate.CheckLog, CheckOutputTruncatedMarker) {
		t.Fatalf("check log missing truncation marker: %q", candidate.CheckLog[len(candidate.CheckLog)-64:])
	}
	if strings.Contains(candidate.CheckLog, secret) || strings.Contains(candidate.CheckLog, "/private/runtime") {
		t.Fatalf("data beyond the check-log budget was retained: %q", candidate.CheckLog[len(candidate.CheckLog)-128:])
	}
}

func TestShellCheckerBoundsCombinedStdoutAndStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sing-box")
	body := "#!/bin/sh\nchunk=0123456789abcdef0123456789abcdef\ni=0\nwhile [ \"$i\" -lt 2048 ]; do\n  printf '%s' \"$chunk\"\n  printf '%s' \"$chunk\" >&2\n  i=$((i + 1))\ndone\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write checker: %v", err)
	}
	output, err := (ShellChecker{Binary: script}).Check(context.Background(), filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("expected shell checker failure")
	}
	if len(output) > DefaultMaxCheckOutputBytes {
		t.Fatalf("combined output exceeded hard limit: %d", len(output))
	}
	if !strings.HasSuffix(output, CheckOutputTruncatedMarker) {
		t.Fatalf("combined output missing truncation marker: %q", output[len(output)-64:])
	}
}
