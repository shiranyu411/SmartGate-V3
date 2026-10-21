package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeploySafeRejectsRecoveryMarkersWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		pending bool
	}{
		{name: "starting marker only"},
		{name: "both markers", pending: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			temp := t.TempDir()
			deployRoot := filepath.Join(temp, "deployments")
			write := func(path, body string, mode fs.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(deployRoot, "lock/owner"), "stale-owner\nstale-boot\nstale-start\n", 0o600)
			write(filepath.Join(deployRoot, "pending.starting.json"), `{"phase":"starting"}`, 0o600)
			write(filepath.Join(deployRoot, "deploy-recover"), "recovery-helper-sentinel\n", 0o755)
			write(filepath.Join(deployRoot, "payload-sentinel"), "payload-sentinel\n", 0o600)
			write(filepath.Join(deployRoot, "hook-sentinel"), "hook-sentinel\n", 0o600)
			if tt.pending {
				write(filepath.Join(deployRoot, "pending.json"), `{"phase":"pending"}`, 0o600)
			}

			before := snapshotTestTree(t, deployRoot)
			scriptPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", scriptPath)
			cmd.Env = append(os.Environ(),
				"SMARTGATE_DEPLOY_ROOT="+deployRoot,
				"SMARTGATE_RELEASE_ROOT="+filepath.Join(temp, "release"),
			)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("deploy-safe accepted %s", tt.name)
			}
			if !strings.Contains(string(output), "pending deployment recovery exists") {
				t.Fatalf("unexpected rejection for %s: %s", tt.name, output)
			}
			after := snapshotTestTree(t, deployRoot)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("deploy-safe mutated recovery materials\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// procd `status` exits 0 for a service that is enabled but has no running
// instance ("active with no instances"), which happens after a reboot in
// off/safe mode. The legacy baseline checks must use `running` instead.
func TestDeployScriptsProbeLegacyServiceWithProcdRunning(t *testing.T) {
	for _, script := range []string{"deploy-safe.sh", "deploy-recover.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "scripts", script))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, `"$LEGACY_INIT" status`) {
			t.Fatalf("%s: legacy service probe must not rely on `status` exit code", script)
		}
		if !strings.Contains(text, `"$LEGACY_INIT" running`) {
			t.Fatalf("%s: legacy service probe must use procd `running`", script)
		}
		if strings.Contains(text, `pidof "$LEGACY_PROCESS"`) {
			t.Fatalf("%s: legacy process probe must not treat every process with the same executable name as SmartGate-owned", script)
		}
		if !strings.Contains(text, "legacy_process_pids()") || !strings.Contains(text, `"$LEGACY_CONFIG"`) {
			t.Fatalf("%s: legacy process probe must identify the process by its owned config", script)
		}
	}
}

func TestDeploySafeLegacyProcessInventoryIgnoresUnrelatedSameNameClient(t *testing.T) {
	env := newDeploySafeFunctionEnv(t)
	legacy := env.spawnSleeper(t, "/usr/bin/sing-box\x00run\x00-c\x00/etc/sing-box/config.json\x00")
	bystander := env.spawnSleeper(t, "/usr/bin/sing-box\x00run\x00-c\x00/etc/proxy-health/client.json\x00")

	output, code := env.call(t, "legacy_process_pids")
	if code != 0 {
		t.Fatalf("legacy_process_pids exited %d:\n%s", code, output)
	}
	if !strings.Contains(output, fmt.Sprint(legacy.cmd.Process.Pid)) {
		t.Fatalf("legacy process was not found by its owned config:\n%s", output)
	}
	if strings.Contains(output, fmt.Sprint(bystander.cmd.Process.Pid)) {
		t.Fatalf("unrelated same-name client was incorrectly classified as legacy:\n%s", output)
	}
}

func TestDeploySafeQuiescesLegacyDataPlaneAfterDurableJournal(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	journal := strings.Index(text, `mv "$JOURNAL.tmp" "$JOURNAL"`)
	quiesce := strings.Index(text, `quiesce_legacy_data_plane "$old_schema"`)
	if journal < 0 || quiesce < 0 || quiesce <= journal {
		t.Fatal("legacy data-plane quiesce must follow the durable deployment journal")
	}
	if !strings.Contains(text, `SMARTGATE_RESCUE_EXECUTE=1 "$WORK/stage/smartgate" rescue`) {
		t.Fatal("legacy upgrade must use the staged owner-safe rescue cleanup")
	}
	if !strings.Contains(text, `[ "$schema" -ge 2 ]`) {
		t.Fatal("schema routing must use a numeric AppliedState boundary")
	}
	for _, marker := range []string{`[ ! -e "$LEGACY_CONFIG_OWNER" ]`, `[ ! -e "$LEGACY_SERVICE_OWNER" ]`} {
		if !strings.Contains(text, marker) {
			t.Fatalf("legacy quiesce must verify removal of %s", marker)
		}
	}
	// The V3 rescue plan has no action for the V2 core config marker, so the
	// quiesce step itself must retire it (found in the on-mode QEMU rehearsal:
	// otherwise every upgrade from a V2 that applied in `on` rolls back).
	retire := `rm -f "$LEGACY_CONFIG" "$LEGACY_CONFIG_OWNER"`
	if !strings.Contains(text, retire) || strings.Index(text, retire) > strings.Index(text, `[ ! -e "$LEGACY_CONFIG_OWNER" ]`) {
		t.Fatal("legacy quiesce must retire the owned V2 core config before verifying the marker is gone")
	}
	if !strings.Contains(text, `grep -q '^owner=smartgate:' "$LEGACY_CONFIG_OWNER"`) {
		t.Fatal("legacy config removal must be gated on the SmartGate owner marker")
	}
	serviceRetire := `rm -f "$LEGACY_SERVICE_OWNER"`
	if !strings.Contains(text, serviceRetire) || strings.Index(text, serviceRetire) > strings.Index(text, `[ ! -e "$LEGACY_SERVICE_OWNER" ]`) {
		t.Fatal("legacy quiesce must retire the owned V2 service marker before verifying it is gone")
	}
	if !strings.Contains(text, `grep -q '^owner=smartgate:' "$LEGACY_SERVICE_OWNER"`) {
		t.Fatal("legacy service marker removal must be gated on the SmartGate owner marker")
	}
	if !strings.Contains(text, `"$LEGACY_INIT" disable`) || !strings.Contains(text, `"$WORK/backup/legacy-enabled"`) {
		t.Fatal("legacy quiesce must disable the stock core service and record its previous enable state for rollback")
	}
	for _, script := range []string{"deploy-safe.sh", "deploy-recover.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "scripts", script))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `legacy-enabled`) || !strings.Contains(string(body), `"$LEGACY_INIT" enable`) {
			t.Fatalf("%s: rollback must re-enable the legacy core service when the backup says it was enabled", script)
		}
	}
}

func TestDeploySafeCleansWorkWhenStagingFailsBeforeJournal(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	condition := `if [ "$SUCCESS" -ne 1 ] && [ "$BACKUP_CREATED" -eq 0 ] && recovery_markers_absent; then`
	backupCleanup := `rm -f "$BACKUP.tmp" "$BACKUP" 2>/dev/null || true`
	cleanup := `rm -rf "$WORK" 2>/dev/null || true`
	if !strings.Contains(text, condition) || !strings.Contains(text, backupCleanup) || !strings.Contains(text, cleanup) {
		t.Fatal("pre-journal deployment failures must remove the non-durable work directory")
	}
	if !strings.Contains(text, "else\n      rm -f \"$GATE\"") {
		t.Fatal("pre-journal deployment failures must clear the scheduler gate")
	}
	if strings.Index(text, cleanup) > strings.Index(text, `trap cleanup EXIT HUP INT TERM`) {
		t.Fatal("work cleanup must run inside the deployment exit trap")
	}
}

// B23: the on-mode QEMU rehearsal left the candidate's Worker running and the
// Gateway Fabric table in place after an automatic rollback, because the
// rollback derived the Worker list from applied-state (empty after a startup
// reconcile) and deleted the pid files before looking again. Rollback must
// retire the candidate data plane from the live inventory before it removes
// anything that inventory depends on.
func TestDeploySafeRollbackRetiresCandidateDataPlaneBeforeDeletingInventory(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	restore := shellFunctionBody(t, text, "restore_backup")
	stop := strings.Index(restore, `/etc/init.d/smartgate stop`)
	wait := strings.Index(restore, `wait_daemon_stopped`)
	retire := strings.Index(restore, `retire_v3_data_plane`)
	removeRuntime := strings.Index(restore, `rm -rf /tmp/smartgate/workers /tmp/smartgate/worker-runtime`)
	removePayload := strings.Index(restore, `rm -rf /usr/bin/smartgate`)
	if stop < 0 || wait < 0 || retire < 0 || removeRuntime < 0 || removePayload < 0 {
		t.Fatalf("restore_backup is missing a rollback step:\n%s", restore)
	}
	if !(stop < wait && wait < retire && retire < removeRuntime && removeRuntime < removePayload) {
		t.Fatalf("rollback must stop the daemon, wait for it, retire the data plane, then delete inventory:\n%s", restore)
	}
	retireBody := shellFunctionBody(t, text, "retire_v3_data_plane")
	for _, required := range []string{
		`SMARTGATE_RESCUE_EXECUTE=1 "$WORK/stage/smartgate" rescue`,
		`stop_v3_workers ||`,
		`retire_v3_fabric_host_state ||`,
	} {
		if !strings.Contains(retireBody, required) {
			t.Fatalf("retire_v3_data_plane must contain %q:\n%s", required, retireBody)
		}
	}
	stopBody := shellFunctionBody(t, text, "stop_v3_workers")
	for _, required := range []string{`inventory_v3_workers`, `v3_worker_pids`, `kill -KILL`} {
		if !strings.Contains(stopBody, required) {
			t.Fatalf("stop_v3_workers must use the live inventory (%q missing):\n%s", required, stopBody)
		}
	}
	fabricBody := shellFunctionBody(t, text, "retire_v3_fabric_host_state")
	if !strings.Contains(fabricBody, `nft delete table inet smartgate`) || !strings.Contains(fabricBody, `retire_v3_policy_routes`) {
		t.Fatalf("fabric retirement must delete the owner table and policy routes:\n%s", fabricBody)
	}
	cleanup := shellFunctionBody(t, text, "cleanup")
	diagnostics := strings.Index(cleanup, `capture_failure_diagnostics`)
	rollback := strings.Index(cleanup, `restore_backup ||`)
	if diagnostics < 0 || rollback < 0 || diagnostics > rollback {
		t.Fatalf("failure diagnostics must be captured before the automatic rollback mutates the host:\n%s", cleanup)
	}
	// Both scripts carry their own copy of the inventory helpers (deploy-recover
	// ships standalone); keep them identical.
	recoverBody, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-recover.sh"))
	if err != nil {
		t.Fatal(err)
	}
	recoverText := strings.ReplaceAll(string(recoverBody), `"$WORKER_CONTROL"`, `"$V3_WORKER_CONTROL"`)
	for _, name := range []string{"inventory_v3_workers", "v3_worker_pids", "stop_v3_workers", "retire_v3_fabric_host_state"} {
		if got, want := shellFunctionBody(t, recoverText, name), shellFunctionBody(t, text, name); got != want {
			t.Fatalf("deploy-recover.sh %s() drifted from deploy-safe.sh:\n--- deploy-safe\n%s\n--- deploy-recover\n%s", name, want, got)
		}
	}
}

// A V2 node reference the legacy migration cannot resolve leaves its pool on
// the no_candidate fallback; every later gate still passes because there is no
// Worker to check. deploy-safe must treat it as a failed gate before the
// controlled reconcile so the automatic rollback returns V2.
func TestDeploySafeFailsGateOnUnresolvedLegacyMigration(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	gated := strings.Index(text, `wait_api gated || fail`)
	check := strings.Index(text, `legacy_migration_resolved "$WORK/runtime-gated" || fail`)
	dryRun := strings.Index(text, `api_post /api/config/dryrun`)
	if gated < 0 || check < 0 || dryRun < 0 || !(gated < check && check < dryRun) {
		t.Fatalf("unresolved-migration gate must sit between the health gate and the controlled dry-run:\ngated=%d check=%d dryrun=%d", gated, check, dryRun)
	}
	env := newDeploySafeFunctionEnv(t)
	cases := []struct {
		name    string
		runtime string
		wantRC  int
	}{
		{"no migration report", `{"ok":true,"data":{"phase":"running","gateway_mode":"on"}}`, 0},
		{"all resolved", `{"ok":true,"data":{"legacy_migration":{"migrated_at":"2026-09-03T10:15:06Z","resolved":1,"unresolved":[],"warnings":[]}}}`, 0},
		{"null unresolved", `{"ok":true,"data":{"legacy_migration":{"resolved":1,"unresolved":null}}}`, 0},
		{"one unresolved", `{"ok":true,"data":{"legacy_migration":{"resolved":0,"unresolved":[{"scope":"pool","pool":"general","legacy_id":"p:0123456789abcdef"}],"warnings":[]}}}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(env.temp, "runtime-gated")
			if err := os.WriteFile(path, []byte(tc.runtime), 0o600); err != nil {
				t.Fatal(err)
			}
			output, code := env.call(t, "legacy_migration_resolved", path)
			if code != tc.wantRC {
				t.Fatalf("legacy_migration_resolved returned %d, want %d:\n%s", code, tc.wantRC, output)
			}
		})
	}
}

func TestDeploySafeStopsWorkersByProcessInventory(t *testing.T) {
	env := newDeploySafeFunctionEnv(t)
	worker := env.spawnSleeper(t, "/usr/bin/fake-core\x00run\x00-c\x00/etc/smartgate/workers/w_test_1.json\x00")
	bystander := env.spawnSleeper(t, "/usr/bin/fake-core\x00run\x00-c\x00/root/backup/etc/smartgate/workers/w_test_1.json\x00")
	// A dead pid that still has a proc entry must be ignored rather than block
	// the sweep forever.
	env.writeProcEntry(t, 2147483000, "/usr/bin/fake-core\x00run\x00-c\x00/etc/smartgate/workers/w_test_9.json\x00")

	output, code := env.call(t, "stop_v3_workers")
	if code != 0 {
		t.Fatalf("stop_v3_workers exited %d:\n%s", code, output)
	}
	if !worker.exited(3 * time.Second) {
		t.Fatalf("Worker process with an active config was not stopped:\n%s", output)
	}
	if bystander.exited(200 * time.Millisecond) {
		t.Fatalf("process without an active Worker config was killed:\n%s", output)
	}
}

func TestDeploySafeRetiresFabricHostState(t *testing.T) {
	t.Run("owner table and policy route are removed", func(t *testing.T) {
		env := newDeploySafeFunctionEnv(t)
		env.installFabricFakes(t, true)
		output, code := env.call(t, "retire_v3_fabric_host_state")
		if code != 0 {
			t.Fatalf("retire_v3_fabric_host_state exited %d:\n%s", code, output)
		}
		log := env.commandLog(t)
		for _, required := range []string{
			"nft delete table inet smartgate",
			"ip -4 rule del pref 11254 fwmark 0x50fe0001 table 1254",
			"ip -4 route flush table 1254 proto 186",
		} {
			if !strings.Contains(log, required) {
				t.Fatalf("expected %q in command log:\n%s", required, log)
			}
		}
		if _, err := os.Stat(env.fabricMarker); !os.IsNotExist(err) {
			t.Fatalf("owner table survived retirement: %v", err)
		}
	})
	t.Run("foreign table is left alone", func(t *testing.T) {
		env := newDeploySafeFunctionEnv(t)
		env.installFabricFakes(t, false)
		output, code := env.call(t, "retire_v3_fabric_host_state")
		if code == 0 {
			t.Fatalf("retire_v3_fabric_host_state must fail on a table without the SmartGate owner comment:\n%s", output)
		}
		if strings.Contains(env.commandLog(t), "nft delete table") {
			t.Fatalf("a table without the owner comment must never be deleted:\n%s", env.commandLog(t))
		}
	})
}

type deploySafeFunctionEnv struct {
	temp         string
	procRoot     string
	fakeBin      string
	logPath      string
	fabricMarker string
	routeMarker  string
}

func newDeploySafeFunctionEnv(t *testing.T) *deploySafeFunctionEnv {
	t.Helper()
	temp := t.TempDir()
	env := &deploySafeFunctionEnv{
		temp:         temp,
		procRoot:     filepath.Join(temp, "proc"),
		fakeBin:      filepath.Join(temp, "bin"),
		logPath:      filepath.Join(temp, "commands.log"),
		fabricMarker: filepath.Join(temp, "fabric-table"),
		routeMarker:  filepath.Join(temp, "policy-route"),
	}
	for _, dir := range []string{env.procRoot, env.fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

type sleeperProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *sleeperProcess) exited(within time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(within):
		return false
	}
}

func (env *deploySafeFunctionEnv) spawnSleeper(t *testing.T, cmdline string) *sleeperProcess {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	env.writeProcEntry(t, cmd.Process.Pid, cmdline)
	return &sleeperProcess{cmd: cmd, done: done}
}

func (env *deploySafeFunctionEnv) writeProcEntry(t *testing.T, pid int, cmdline string) {
	t.Helper()
	dir := filepath.Join(env.procRoot, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (env *deploySafeFunctionEnv) installFabricFakes(t *testing.T, ownerTagged bool) {
	t.Helper()
	comment := `comment "owner=smartgate:fabric-mark-table:smartgate"`
	if !ownerTagged {
		comment = `comment "owner=someone-else"`
	}
	if err := os.WriteFile(env.fabricMarker, []byte("present\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.routeMarker, []byte("present\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(env.fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("nft", `#!/bin/sh
echo "nft $*" >>"$SMARTGATE_TEST_LOG"
case "$*" in
  "list table inet smartgate")
    [ -f "$SMARTGATE_TEST_FABRIC_TABLE" ] || exit 1
    printf 'table inet smartgate {\n\t%s\n}\n' '`+comment+`'
    ;;
  "delete table inet smartgate") rm -f "$SMARTGATE_TEST_FABRIC_TABLE" ;;
esac
exit 0
`)
	writeExecutable("ip", `#!/bin/sh
echo "ip $*" >>"$SMARTGATE_TEST_LOG"
case "$*" in
  "-4 rule show")
    [ -f "$SMARTGATE_TEST_POLICY_ROUTE" ] && printf '0:\tfrom all lookup local\n11254:\tfrom all fwmark 0x50fe0001 lookup 1254\n32766:\tfrom all lookup main\n'
    ;;
  "-4 rule del pref 11254 fwmark 0x50fe0001 table 1254") rm -f "$SMARTGATE_TEST_POLICY_ROUTE" ;;
  "-4 route show table all proto 186") printf 'local default dev lo table 1254 proto 186 scope host\n' ;;
esac
exit 0
`)
}

func (env *deploySafeFunctionEnv) call(t *testing.T, function string, args ...string) (string, int) {
	t.Helper()
	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(env.temp, "call.sh")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
SMARTGATE_DEPLOY_SAFE_FUNCTIONS_ONLY=1
export SMARTGATE_DEPLOY_SAFE_FUNCTIONS_ONLY
. "$SMARTGATE_TEST_SCRIPT"
LEGACY_CONFIG=${SMARTGATE_TEST_LEGACY_CONFIG:-$LEGACY_CONFIG}
export LEGACY_CONFIG
mkdir -p "$WORK"
set +e
"$@"
echo "rc=$?"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{wrapper, function}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+env.fakeBin+":"+os.Getenv("PATH"),
		"SMARTGATE_TEST_SCRIPT="+scriptPath,
		"SMARTGATE_DEPLOY_ROOT="+filepath.Join(env.temp, "deployments"),
		"SMARTGATE_RELEASE_ROOT="+filepath.Join(env.temp, "release"),
		"SMARTGATE_PROC_ROOT="+env.procRoot,
		"SMARTGATE_TEST_LOG="+env.logPath,
		"SMARTGATE_TEST_FABRIC_TABLE="+env.fabricMarker,
		"SMARTGATE_TEST_POLICY_ROUTE="+env.routeMarker,
		"SMARTGATE_TEST_LEGACY_CONFIG=/etc/sing-box/config.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("function wrapper failed: %v\n%s", err, output)
	}
	text := string(output)
	marker := strings.LastIndex(text, "rc=")
	if marker < 0 {
		t.Fatalf("wrapper did not report a return code:\n%s", text)
	}
	code, err := strconv.Atoi(strings.TrimSpace(text[marker+len("rc="):]))
	if err != nil {
		t.Fatalf("unparseable return code in %q: %v", text[marker:], err)
	}
	return text, code
}

func (env *deploySafeFunctionEnv) commandLog(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(env.logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(body)
}

// shellFunctionBody returns the text of a `name() {` ... `}` definition whose
// closing brace sits at column zero, matching the style of the deploy scripts.
func shellFunctionBody(t *testing.T, text, name string) string {
	t.Helper()
	start := strings.Index(text, "\n"+name+"() {\n")
	if start < 0 {
		t.Fatalf("function %s() is not defined", name)
	}
	rest := text[start+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("function %s() has no closing brace", name)
	}
	return rest[:end+3]
}

func TestDeploySafeTightensLegacyStatePermissions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-safe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"secure_state_permissions()",
		"chmod 0700 \"$STATE_DIR\"",
		"find \"$STATE_DIR\" -type f",
		"-name '*.tar.gz'",
		"-exec chmod 0600 {} +",
		"secure_state_permissions || fail \"could not tighten SmartGate state permissions\"",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("deploy-safe does not enforce state permission requirement %q", required)
		}
	}
}

func snapshotTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[rel] = "dir:" + info.Mode().String()
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = info.Mode().String() + ":" + string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
