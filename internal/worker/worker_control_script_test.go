package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWorkerControlScriptLifecycleIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCore := filepath.Join(root, "fake-core")
	if err := os.WriteFile(fakeCore, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "smartgate-worker-control"))
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"SMARTGATE_WORKER_CONFIG_DIR="+configDir,
		"SMARTGATE_WORKER_RUNTIME_DIR="+runtimeDir,
		"SMARTGATE_SINGBOX_BIN="+fakeCore,
		"SMARTGATE_XRAY_BIN="+fakeCore,
	)
	run := func(wantOK bool, args ...string) string {
		t.Helper()
		command := exec.Command("sh", append([]string{script}, args...)...)
		command.Env = env
		output, err := command.CombinedOutput()
		if wantOK && err != nil {
			t.Fatalf("worker control %v failed: %v: %s", args, err, output)
		}
		if !wantOK && err == nil {
			t.Fatalf("worker control %v unexpectedly succeeded: %s", args, output)
		}
		return strings.TrimSpace(string(output))
	}
	t.Cleanup(func() { run(true, "stop", "w_sb_1") })

	if output := run(false, "restart", "w_sb_1"); !strings.Contains(output, "no active config") {
		t.Fatalf("missing config rejection was unclear: %q", output)
	}
	if err := os.WriteFile(filepath.Join(configDir, "w_sb_1.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(true, "restart", "w_sb_1")
	run(true, "status", "w_sb_1")
	run(true, "start", "w_sb_1")
	run(true, "stop", "w_sb_1")
	run(true, "stop", "w_sb_1")
	run(false, "status", "w_sb_1")
}

// OpenWrt's busybox sleep rejects fractional seconds; under the script's
// set -e that aborted every stop/restart of a running Worker (B24). Emulate
// that sleep and drive the lifecycle through the wait loops.
func TestWorkerControlScriptSurvivesBusyboxSleep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	runtimeDir := filepath.Join(root, "runtime")
	fakeBin := filepath.Join(root, "bin")
	for _, dir := range []string{configDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not found")
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte("#!/bin/sh\ncase \"${1:-}\" in *.*) echo \"sleep: invalid number '$1'\" >&2; exit 1 ;; esac\nexec "+realSleep+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A core that ignores SIGTERM for a moment forces the stop loop to wait.
	fakeCore := filepath.Join(root, "fake-core")
	if err := os.WriteFile(fakeCore, []byte("#!/bin/sh\ntrap 'sleep 1; exit 0' TERM INT\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "w_sb_1.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "smartgate-worker-control"))
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SMARTGATE_WORKER_CONFIG_DIR="+configDir,
		"SMARTGATE_WORKER_RUNTIME_DIR="+runtimeDir,
		"SMARTGATE_SINGBOX_BIN="+fakeCore,
		"SMARTGATE_XRAY_BIN="+fakeCore,
	)
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("sh", append([]string{script}, args...)...)
		command.Env = env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("worker control %v failed under busybox-style sleep: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	t.Cleanup(func() {
		command := exec.Command("sh", script, "stop", "w_sb_1")
		command.Env = env
		_ = command.Run()
	})
	run("start", "w_sb_1")
	run("status", "w_sb_1")
	if output := run("restart", "w_sb_1"); !strings.Contains(output, "running") {
		t.Fatalf("restart of a running Worker did not report running: %q", output)
	}
	run("status", "w_sb_1")
	if output := run("stop", "w_sb_1"); !strings.Contains(output, "stopped") {
		t.Fatalf("stop of a running Worker did not report stopped: %q", output)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "w_sb_1.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file survived stop: %v", err)
	}
}

// Cores are optional individually: the router ships only the legacy core, so
// verify-prerequisites must pass with one installed core and fail with none.
func TestWorkerControlScriptPrerequisitesAcceptSingleInstalledCore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	root := t.TempDir()
	fakeCore := filepath.Join(root, "fake-core")
	if err := os.WriteFile(fakeCore, []byte("#!/bin/sh\ncase \"${1:-}\" in version) echo 'fake version 1.12.0' ;; esac\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "smartgate-worker-control"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(envPairs ...string) (string, error) {
		t.Helper()
		command := exec.Command("sh", script, "verify-prerequisites")
		command.Env = append(os.Environ(), envPairs...)
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
	missing := filepath.Join(root, "not-installed")
	if output, err := run("SMARTGATE_SINGBOX_BIN="+fakeCore, "SMARTGATE_XRAY_BIN="+missing); err != nil {
		t.Fatalf("one installed core must satisfy prerequisites: %v: %s", err, output)
	} else if !strings.Contains(output, "not installed") {
		t.Fatalf("missing optional core must be reported: %q", output)
	}
	if output, err := run("SMARTGATE_SINGBOX_BIN="+missing, "SMARTGATE_XRAY_BIN="+fakeCore); err != nil {
		t.Fatalf("the other core alone must also satisfy prerequisites: %v: %s", err, output)
	}
	if output, err := run("SMARTGATE_SINGBOX_BIN="+missing, "SMARTGATE_XRAY_BIN="+missing); err == nil || !strings.Contains(output, "no execution core") {
		t.Fatalf("no installed core must fail: err=%v output=%q", err, output)
	}
}

func TestWorkerControlScriptRejectsUnknownWorker(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "smartgate-worker-control"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", script, "start", "user-worker")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "usage:") {
		t.Fatalf("unknown worker was accepted: err=%v output=%s", err, output)
	}
}

func TestWorkerControlStatusDoesNotEraseStartingProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	runtimeDir := filepath.Join(root, "runtime")
	fakeBin := filepath.Join(root, "bin")
	for _, dir := range []string{configDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fakeCore := filepath.Join(root, "fake-core")
	if err := os.WriteFile(fakeCore, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "w_sb_1.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Hold the daemon between writing its PID and exec, matching BusyBox's
	// background launcher window. Polling status must not lose its identity.
	launcher := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) pidfile=$2; shift 2 ;;
    -x) binary=$2; shift 2 ;;
    --) shift; break ;;
    *) shift ;;
  esac
done
export SMARTGATE_TEST_BINARY="$binary"
sh -c 'echo $$ > "$1"; touch "$2/starting"; while [ ! -e "$2/release" ]; do sleep 0.1; done; exec "$SMARTGATE_TEST_BINARY" run -c "$SMARTGATE_TEST_CONFIG"' launcher "$pidfile" "$SMARTGATE_TEST_LAUNCH_DIR" </dev/null >/dev/null 2>&1 &
`
	if err := os.WriteFile(filepath.Join(fakeBin, "start-stop-daemon"), []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "smartgate-worker-control"))
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SMARTGATE_TEST_LAUNCH_DIR="+root,
		"SMARTGATE_TEST_CONFIG="+filepath.Join(configDir, "w_sb_1.json"),
		"SMARTGATE_WORKER_CONFIG_DIR="+configDir,
		"SMARTGATE_WORKER_RUNTIME_DIR="+runtimeDir,
		"SMARTGATE_SINGBOX_BIN="+fakeCore,
	)
	command := func(action string) *exec.Cmd {
		cmd := exec.Command("sh", script, action, "w_sb_1")
		cmd.Env = env
		return cmd
	}
	started := make(chan struct{})
	var startErr error
	go func() {
		startErr = command("start").Run()
		close(started)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0o600)
		<-started
		_ = command("stop").Run()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "starting")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher did not reach pre-exec window")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command("status").Run(); err == nil {
		t.Fatal("pre-exec launcher must not be reported as the running core")
	}
	for _, name := range []string{"w_sb_1.pid", "w_sb_1.owner"} {
		if _, err := os.Stat(filepath.Join(runtimeDir, name)); err != nil {
			t.Fatalf("status erased starting worker identity %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-started
	if startErr != nil {
		t.Fatalf("worker did not finish starting: %v", startErr)
	}
	if err := command("status").Run(); err != nil {
		t.Fatalf("started worker became untracked: %v", err)
	}
}
