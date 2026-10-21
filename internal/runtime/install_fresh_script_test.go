package runtime

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type freshInstallFixture struct {
	t          *testing.T
	rootFS     string
	release    string
	deployRoot string
	fakeBin    string
	logPath    string
	daemonPath string
}

func TestInstallFreshBootstrapPassesHealthAndAuthGates(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	output, err := fixture.runInstall()
	if err != nil {
		t.Fatalf("fresh install failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "installation passed health and auth-setup gates") {
		t.Fatalf("missing success result: %s", output)
	}
	for _, dir := range []string{fixture.target("/etc/smartgate"), fixture.target("/tmp/smartgate")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("fresh install state directory %s mode = %o, want 700", dir, got)
		}
	}

	for _, path := range []string{
		fixture.target("/usr/bin/smartgate"),
		fixture.target("/etc/init.d/smartgate"),
		fixture.target("/www/smartgate/index.html"),
		fixture.target("/etc/smartgate/version.json"),
		fixture.target("/usr/lib/smartgate/deploy-recover"),
		fixture.target("/usr/libexec/smartgate-worker-control"),
		fixture.target("/etc/rc.d/S99smartgate"),
		fixture.target("/etc/rc.d/K10smartgate"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("installed payload missing %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(fixture.deployRoot, "fresh-pending.json"),
		filepath.Join(fixture.deployRoot, "install-fresh-recover"),
		fixture.target("/etc/rc.d/S01smartgate-install-recover"),
		fixture.target("/tmp/smartgate/deploy-scheduler-disabled"),
		filepath.Join(fixture.deployRoot, "lock"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("transaction residue remains at %s: %v", path, err)
		}
	}

	log := fixture.readLog()
	enableAt := strings.Index(log, "init enable")
	startAt := strings.Index(log, "init start")
	firstHealthAt := strings.Index(log, "/healthz")
	firstAuthAt := strings.Index(log, "/api/auth/check")
	restartAt := strings.Index(log, "init restart")
	lastAuthAt := strings.LastIndex(log, "/api/auth/check")
	if enableAt < 0 || startAt <= enableAt || firstHealthAt <= startAt || firstAuthAt <= firstHealthAt || restartAt <= firstAuthAt || lastAuthAt <= restartAt {
		t.Fatalf("fresh install gates ran out of order:\n%s", log)
	}
}

func TestInstallFreshFailureRemovesCreatedPayload(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	output, err := fixture.runInstall("SMARTGATE_TEST_AUTH_READY=0")
	if err == nil {
		t.Fatalf("fresh install unexpectedly passed auth gate:\n%s", output)
	}
	if !strings.Contains(output, "removing fresh payload") {
		t.Fatalf("cleanup was not reported: %s", output)
	}
	for _, path := range fixture.managedTargetPaths() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed install left managed payload at %s: %v", path, err)
		}
	}
	log := fixture.readLog()
	if !strings.Contains(log, "init stop") || !strings.Contains(log, "init disable") {
		t.Fatalf("failed install did not stop and disable service:\n%s", log)
	}
}

func TestInstallFreshRecoveryRemovesAtomicAndStagingResidue(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	staleWork := filepath.Join(fixture.deployRoot, "fresh-work-stale-123")
	writeFreshTestFile(t, filepath.Join(staleWork, "stage/smartgate"), "stale\n", 0o600)
	for _, path := range []string{
		fixture.target("/usr/bin/smartgate.new"),
		fixture.target("/etc/init.d/smartgate.new"),
		fixture.target("/www/smartgate.new/index.html"),
		fixture.target("/etc/smartgate/version.json.new"),
		fixture.target("/usr/lib/smartgate/deploy-recover.new"),
		fixture.target("/usr/libexec/smartgate-worker-control.new"),
		fixture.target("/etc/rc.d/S01smartgate-install-recover.new"),
		fixture.target("/etc/rc.d/K10smartgate"),
	} {
		writeFreshTestFile(t, path, "stale\n", 0o600)
	}
	copyFreshTestFile(t, filepath.Join(fixture.release, "scripts/install-fresh.sh"), filepath.Join(fixture.deployRoot, "install-fresh-recover"), 0o755)
	writeFreshTestFile(t, filepath.Join(fixture.deployRoot, "fresh-pending.json"), fmt.Sprintf("{\"kind\":\"fresh_install\",\"phase\":\"prepared\",\"work\":%q}\n", staleWork), 0o600)

	output, err := fixture.runInstall()
	if err != nil {
		t.Fatalf("retry after atomic residue failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(staleWork); !os.IsNotExist(err) {
		t.Fatalf("stale work directory remains: %v", err)
	}
	for _, path := range fixture.managedTargetPaths() {
		if strings.HasSuffix(path, ".new") {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("atomic residue remains at %s: %v", path, err)
			}
		}
	}
}

func TestInstallFreshRejectsTamperedReleaseBeforeTargetMutation(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.release, "smartgate"), []byte("tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := fixture.runInstall()
	if err == nil {
		t.Fatalf("tampered release unexpectedly installed:\n%s", output)
	}
	if !strings.Contains(output, "release payload hash verification failed") {
		t.Fatalf("unexpected tamper rejection: %s", output)
	}
	for _, path := range fixture.managedTargetPaths() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("tamper rejection mutated target at %s: %v", path, err)
		}
	}
}

func TestInstallEntryRejectsUnjournaledPartialInstallation(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	writeFreshTestExecutable(t, fixture.target("/usr/bin/smartgate"), "#!/bin/sh\nexit 0\n")
	before := snapshotTestTree(t, fixture.rootFS)
	output, err := fixture.runInstall()
	if err == nil {
		t.Fatalf("partial installation unexpectedly accepted:\n%s", output)
	}
	if !strings.Contains(output, "partial SmartGate installation detected") {
		t.Fatalf("unexpected partial-install rejection: %s", output)
	}
	after := snapshotTestTree(t, fixture.rootFS)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("partial-install rejection mutated target\nbefore=%v\nafter=%v", before, after)
	}
}

func TestInstallEntryRecoversInterruptedFreshInstallThenRetries(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	interruptedWork := filepath.Join(fixture.deployRoot, "fresh-work-interrupted")
	writeFreshTestFile(t, filepath.Join(interruptedWork, "stage/smartgate"), "stale\n", 0o600)
	for _, pair := range [][2]string{
		{filepath.Join(fixture.release, "smartgate"), fixture.target("/usr/bin/smartgate")},
		{filepath.Join(fixture.release, "configs/init.d/smartgate"), fixture.target("/etc/init.d/smartgate")},
		{filepath.Join(fixture.release, "scripts/deploy-recover.sh"), fixture.target("/usr/lib/smartgate/deploy-recover")},
		{filepath.Join(fixture.release, "libexec/smartgate-worker-control"), fixture.target("/usr/libexec/smartgate-worker-control")},
	} {
		copyFreshTestFile(t, pair[0], pair[1], 0o755)
	}
	copyFreshTestFile(t, filepath.Join(fixture.release, "www/smartgate/index.html"), fixture.target("/www/smartgate/index.html"), 0o644)
	copyFreshTestFile(t, filepath.Join(fixture.release, "version.json"), fixture.target("/etc/smartgate/version.json"), 0o600)
	copyFreshTestFile(t, filepath.Join(fixture.release, "scripts/install-fresh.sh"), filepath.Join(fixture.deployRoot, "install-fresh-recover"), 0o755)
	writeFreshTestFile(t, filepath.Join(fixture.deployRoot, "fresh-pending.json"), fmt.Sprintf("{\"kind\":\"fresh_install\",\"phase\":\"payload_installed\",\"work\":%q}\n", interruptedWork), 0o600)
	writeFreshTestFile(t, filepath.Join(fixture.deployRoot, "lock/owner"), "stale\nstale\nstale\n", 0o600)
	writeFreshTestFile(t, fixture.target("/etc/rc.d/S01smartgate-install-recover"), "stale hook\n", 0o755)
	writeFreshTestFile(t, fixture.target("/etc/rc.d/S99smartgate"), "stale enabled link\n", 0o755)
	writeFreshTestFile(t, fixture.daemonPath, "running\n", 0o600)

	output, err := fixture.runInstall()
	if err != nil {
		t.Fatalf("retry after interrupted install failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "incomplete fresh installation removed") || !strings.Contains(output, "installation passed health and auth-setup gates") {
		t.Fatalf("recovery/retry result missing:\n%s", output)
	}
	if _, err := os.Stat(fixture.target("/usr/bin/smartgate")); err != nil {
		t.Fatalf("retry did not install payload: %v", err)
	}
}

func TestInstallEntryRoutesHealthyExistingInstallToDeploySafe(t *testing.T) {
	fixture := newFreshInstallFixture(t)
	writeFreshTestExecutable(t, fixture.target("/usr/bin/smartgate"), "#!/bin/sh\nexit 0\n")
	writeFreshTestExecutable(t, fixture.target("/etc/init.d/smartgate"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(fixture.target("/etc/smartgate"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := fixture.runInstall()
	if err != nil {
		t.Fatalf("existing installation did not route to deploy-safe: %v\n%s", err, output)
	}
	if !strings.Contains(fixture.readLog(), "deploy-safe") {
		t.Fatalf("deploy-safe was not invoked; output=%s", output)
	}
}

func newFreshInstallFixture(t *testing.T) *freshInstallFixture {
	t.Helper()
	temp := t.TempDir()
	fixture := &freshInstallFixture{
		t:          t,
		rootFS:     filepath.Join(temp, "rootfs"),
		release:    filepath.Join(temp, "release"),
		deployRoot: filepath.Join(temp, "rootfs/root/smartgate-deployments"),
		fakeBin:    filepath.Join(temp, "bin"),
		logPath:    filepath.Join(temp, "commands.log"),
		daemonPath: filepath.Join(temp, "daemon-running"),
	}

	writeFreshTestFile(t, fixture.target("/etc/openwrt_release"), "DISTRIB_ID='OpenWrt'\n", 0o644)
	writeFreshTestExecutable(t, fixture.target("/sbin/procd"), "#!/bin/sh\nexit 0\n")
	writeFreshTestExecutable(t, fixture.target("/usr/bin/sing-box"), "#!/bin/sh\nprintf 'sing-box version 1.13.14\\n'\n")
	writeFreshTestExecutable(t, fixture.target("/usr/bin/xray"), "#!/bin/sh\nprintf 'Xray 26.3.27\\n'\n")

	copyFreshTestFile(t, freshRepoPath(t, "scripts/install.sh"), filepath.Join(fixture.release, "scripts/install.sh"), 0o755)
	copyFreshTestFile(t, freshRepoPath(t, "scripts/install-fresh.sh"), filepath.Join(fixture.release, "scripts/install-fresh.sh"), 0o755)
	writeFreshTestExecutable(t, filepath.Join(fixture.release, "scripts/deploy-safe.sh"), `#!/bin/sh
echo deploy-safe >>"$SMARTGATE_TEST_LOG"
exit 0
`)
	writeFreshTestExecutable(t, filepath.Join(fixture.release, "scripts/deploy-recover.sh"), "#!/bin/sh\nexit 0\n")
	writeFreshTestExecutable(t, filepath.Join(fixture.release, "smartgate"), `#!/bin/sh
[ "${1:-}" = locks ] || exit 1
printf 'locks ok\n'
`)
	writeFreshTestExecutable(t, filepath.Join(fixture.release, "configs/init.d/smartgate"), `#!/bin/sh
echo "init $*" >>"$SMARTGATE_TEST_LOG"
case "${1:-}" in
  enable)
    mkdir -p "$SMARTGATE_TARGET_ROOT/etc/rc.d"
    ln -sf "$SMARTGATE_TARGET_ROOT/etc/init.d/smartgate" "$SMARTGATE_TARGET_ROOT/etc/rc.d/S99smartgate"
    ln -sf "$SMARTGATE_TARGET_ROOT/etc/init.d/smartgate" "$SMARTGATE_TARGET_ROOT/etc/rc.d/K10smartgate"
    ;;
  disable)
    rm -f "$SMARTGATE_TARGET_ROOT/etc/rc.d/S99smartgate" "$SMARTGATE_TARGET_ROOT/etc/rc.d/K10smartgate"
    ;;
  start|restart)
    : >"$SMARTGATE_TEST_DAEMON"
    ;;
  stop)
    rm -f "$SMARTGATE_TEST_DAEMON"
    ;;
esac
exit 0
`)
	writeFreshTestExecutable(t, filepath.Join(fixture.release, "libexec/smartgate-worker-control"), "#!/bin/sh\nexit 0\n")
	writeFreshTestFile(t, filepath.Join(fixture.release, "www/smartgate/index.html"), "<!doctype html><title>SmartGate</title>\n", 0o644)
	writeFreshTestFile(t, filepath.Join(fixture.release, "version.json"), `{"version":"test","commit":"fresh-test","sing_box_min":"1.12"}`+"\n", 0o644)

	writeFreshTestExecutable(t, filepath.Join(fixture.fakeBin, "wget"), `#!/bin/sh
echo "wget $*" >>"$SMARTGATE_TEST_LOG"
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) output=$2; shift 2 ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  */healthz) printf 'ok\n' >"$output" ;;
  */api/auth/check)
    if [ "${SMARTGATE_TEST_AUTH_READY:-1}" = 1 ]; then
      printf '%s\n' '{"ok":true,"data":{"needs_setup":true,"authenticated":false}}' >"$output"
    else
      printf '%s\n' '{"ok":true,"data":{"needs_setup":false,"authenticated":false}}' >"$output"
    fi
    ;;
  *) exit 1 ;;
esac
`)
	for _, commandName := range []string{"sleep", "sync"} {
		writeFreshTestExecutable(t, filepath.Join(fixture.fakeBin, commandName), "#!/bin/sh\nexit 0\n")
	}
	writeFreshManifest(t, fixture.release)
	return fixture
}

func (fixture *freshInstallFixture) runInstall(extraEnv ...string) (string, error) {
	fixture.t.Helper()
	command := exec.Command("sh", filepath.Join(fixture.release, "scripts/install.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fixture.fakeBin+":"+os.Getenv("PATH"),
		"SMARTGATE_TARGET_ROOT="+fixture.rootFS,
		"SMARTGATE_RELEASE_ROOT="+fixture.release,
		"SMARTGATE_DEPLOY_ROOT="+fixture.deployRoot,
		"SMARTGATE_API_BASE=http://127.0.0.1:3000",
		"SMARTGATE_INSTALL_API_ATTEMPTS=2",
		"SMARTGATE_TEST_LOG="+fixture.logPath,
		"SMARTGATE_TEST_DAEMON="+fixture.daemonPath,
	)
	command.Env = append(command.Env, extraEnv...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (fixture *freshInstallFixture) target(path string) string {
	return filepath.Join(fixture.rootFS, filepath.FromSlash(strings.TrimPrefix(path, "/")))
}

func (fixture *freshInstallFixture) managedTargetPaths() []string {
	return []string{
		fixture.target("/usr/bin/smartgate"),
		fixture.target("/etc/init.d/smartgate"),
		fixture.target("/www/smartgate"),
		fixture.target("/etc/smartgate"),
		fixture.target("/tmp/smartgate"),
		fixture.target("/usr/lib/smartgate/deploy-recover"),
		fixture.target("/usr/libexec/smartgate-worker-control"),
		fixture.target("/usr/bin/smartgate.new"),
		fixture.target("/etc/init.d/smartgate.new"),
		fixture.target("/www/smartgate.new"),
		fixture.target("/etc/smartgate/version.json.new"),
		fixture.target("/usr/lib/smartgate/deploy-recover.new"),
		fixture.target("/usr/libexec/smartgate-worker-control.new"),
		fixture.target("/etc/rc.d/S99smartgate"),
		fixture.target("/etc/rc.d/K10smartgate"),
		fixture.target("/etc/rc.d/S01smartgate-install-recover"),
		fixture.target("/etc/rc.d/S01smartgate-install-recover.new"),
		filepath.Join(fixture.deployRoot, "fresh-pending.json"),
		filepath.Join(fixture.deployRoot, "install-fresh-recover"),
		filepath.Join(fixture.deployRoot, "lock"),
	}
}

func (fixture *freshInstallFixture) readLog() string {
	fixture.t.Helper()
	content, err := os.ReadFile(fixture.logPath)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		fixture.t.Fatal(err)
	}
	return string(content)
}

func freshRepoPath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(filepath.Join("..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func writeFreshTestExecutable(t *testing.T, path, body string) {
	t.Helper()
	writeFreshTestFile(t, path, body, 0o755)
}

func writeFreshTestFile(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func copyFreshTestFile(t *testing.T, source, destination string, mode fs.FileMode) {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writeFreshTestFile(t, destination, string(content), mode)
}

func writeFreshManifest(t *testing.T, root string) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() == "SHA256SUMS" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var manifest strings.Builder
	for _, relative := range paths {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		fmt.Fprintf(&manifest, "%x  %s\n", digest, relative)
	}
	writeFreshTestFile(t, filepath.Join(root, "SHA256SUMS"), manifest.String(), 0o644)
}
