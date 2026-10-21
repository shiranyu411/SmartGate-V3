package runtime

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployRecoverManualPathStartsDaemon(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		bootMarker bool
	}{
		{name: "manual"},
		{name: "from init", arg: "--from-init"},
		{name: "boot hook", arg: "--boot-hook", bootMarker: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runDeployRecoverFixture(t, tt.arg, tt.bootMarker, "")
		})
	}
}

func TestDeployRecoverKeepsMarkersUntilAllRecoveryGatesPass(t *testing.T) {
	for _, failure := range []string{"api", "transaction", "checksum", "legacy-directory"} {
		t.Run(failure, func(t *testing.T) {
			runDeployRecoverFixture(t, "", false, failure)
		})
	}
}

func TestDeployRecoverV3WithoutWorkersDoesNotRestartRetiredService(t *testing.T) {
	runDeployRecoverFixtureWithOptions(t, "", false, "", deployRecoverFixtureOptions{
		schema:  2,
		mode:    "on",
		workers: "",
	})
}

func TestDeployRecoverV3WorkerGateFailureKeepsMarkers(t *testing.T) {
	runDeployRecoverFixtureWithOptions(t, "", false, "worker", deployRecoverFixtureOptions{
		schema:           2,
		mode:             "on",
		workers:          "w_sb_1",
		workerStatusFail: true,
	})
}

func TestDeployRecoverRestoresCompleteLegacyDirectory(t *testing.T) {
	runDeployRecoverFixtureWithOptions(t, "", false, "", deployRecoverFixtureOptions{
		schema: 1, mode: "off", completeLegacy: true,
	})
}

func TestDeployRecoverRestoresV2RulesAfterFirewallReload(t *testing.T) {
	for _, mode := range []string{"", "--from-init", "--boot-hook"} {
		t.Run(mode, func(t *testing.T) {
			runDeployRecoverFixtureWithOptions(t, mode, mode == "--boot-hook", "", deployRecoverFixtureOptions{
				schema: 1, mode: "on", completeLegacy: true,
			})
		})
	}
}

func TestDeployRecoverV2FirewallFailureKeepsMarkers(t *testing.T) {
	runDeployRecoverFixtureWithOptions(t, "", false, "legacy-firewall", deployRecoverFixtureOptions{
		schema: 1, mode: "on", completeLegacy: true,
	})
}

type deployRecoverFixtureOptions struct {
	schema           int
	mode             string
	workers          string
	workerStatusFail bool
	completeLegacy   bool
}

func runDeployRecoverFixture(t *testing.T, modeArg string, expectBootMarker bool, failureMode string) {
	runDeployRecoverFixtureWithOptions(t, modeArg, expectBootMarker, failureMode, deployRecoverFixtureOptions{
		schema: 1,
		mode:   "off",
	})
}

func runDeployRecoverFixtureWithOptions(t *testing.T, modeArg string, expectBootMarker bool, failureMode string, options deployRecoverFixtureOptions) {
	t.Helper()
	temp := t.TempDir()
	rootFS := filepath.Join(temp, "rootfs")
	deployRoot := filepath.Join(temp, "deployments")
	backupRoot := filepath.Join(temp, "backup-root")
	workRoot := filepath.Join(temp, "work")
	fakeBin := filepath.Join(temp, "bin")
	logPath := filepath.Join(temp, "commands.log")
	daemonMarker := filepath.Join(temp, "daemon-running")
	legacyMarker := filepath.Join(temp, "legacy-running")
	legacyRules := filepath.Join(temp, "legacy-rules")

	writeExecutable := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A restored V3 daemon re-applies its own Gateway Fabric on start; the
	// fabric marker models the owner nft table.
	initStub := `#!/bin/sh
echo "smartgate $*" >>"$SMARTGATE_TEST_LOG"
if [ "${1:-}" = start ]; then
  [ ! -f "$SMARTGATE_DEPLOY_ROOT/pending.json" ] || { echo recursive-start >>"$SMARTGATE_TEST_LOG"; exit 97; }
  : >"$SMARTGATE_TEST_DAEMON"
  mkdir -p "$SMARTGATE_SYSTEM_ROOT/tmp/smartgate"
  printf '%s\n' '{"config_epoch":7,"topology_hash":"topology-old"}' >"$SMARTGATE_SYSTEM_ROOT/tmp/smartgate/runtime.json"
  [ "$SMARTGATE_TEST_SCHEMA" -lt 2 ] || : >"$SMARTGATE_TEST_FABRIC_TABLE"
fi
exit 0
`
	writeExecutable(filepath.Join(rootFS, "etc/init.d/smartgate"), initStub)
	writeExecutable(filepath.Join(backupRoot, "etc/init.d/smartgate"), initStub)
	writeExecutable(filepath.Join(backupRoot, "usr/bin/smartgate"), `#!/bin/sh
echo "binary $*" >>"$SMARTGATE_TEST_LOG"
[ "${1:-}" != version ] || printf 'smartgate 1.0 commit=old-commit\n'
exit 0
`)
	writeFile(filepath.Join(backupRoot, "etc/smartgate/version.json"), `{"commit":"old-commit"}`)
	writeFile(filepath.Join(backupRoot, "etc/config/firewall"), "baseline-firewall\n")
	writeExecutable(filepath.Join(backupRoot, "etc/firewall.smartgate-reconcile"), "#!/bin/sh\n# SmartGate V2 baseline bridge\n")
	writeFile(filepath.Join(rootFS, "etc/config/firewall"), "candidate-firewall\n")
	writeExecutable(filepath.Join(rootFS, "etc/firewall.smartgate-reconcile"), "#!/bin/sh\n# SmartGate fw4 lifecycle bridge\n")
	payloadHash := sha256.Sum256([]byte(`{"commit":"old-commit"}`))
	if failureMode == "checksum" {
		payloadHash = sha256.Sum256([]byte("mismatched-payload"))
	}
	writeFile(filepath.Join(workRoot, "payload.sha256"), fmt.Sprintf("%x  etc/smartgate/version.json\n", payloadHash))
	writeFile(filepath.Join(backupRoot, "etc/smartgate/spec.json"), fmt.Sprintf(`{"schema_version":%d}`, options.schema))
	appliedState := fmt.Sprintf(`{"schema_version":%d,"config_epoch":7,"topology_hash":"topology-old","gateway_mode":%q}`, options.schema, options.mode)
	if options.schema >= 2 {
		appliedState = fmt.Sprintf(`{"schema_version":%d,"config_epoch":7,"topology_hash":"topology-old","gateway_mode":%q,"execution_graph":{"pool_bindings":[{"target":{"worker":%q}}]}}`, options.schema, options.mode, options.workers)
	}
	writeFile(filepath.Join(backupRoot, "etc/smartgate/applied-state.json"), appliedState)
	if failureMode == "transaction" {
		writeFile(filepath.Join(backupRoot, "etc/smartgate/transactions/selector-pending.json"), `{"kind":"selector_apply","phase":"applying"}`)
	}
	writeFile(filepath.Join(backupRoot, "smartgate-enabled"), "yes\n")
	writeFile(filepath.Join(backupRoot, "old-mode"), options.mode+"\n")
	writeFile(filepath.Join(backupRoot, "old-schema"), fmt.Sprintf("%d\n", options.schema))
	writeExecutable(filepath.Join(workRoot, "stage/worker-control"), `#!/bin/sh
case "${1:-}" in
  legacy-metadata)
    printf '%s\n' \
      '/etc/init.d/sing-box' \
      'sing-box' \
      '/etc/sing-box/config.json' \
      '/etc/sing-box/config.json.owner' \
      '/var/run/smartgate-singbox.owner'
    ;;
  *) exit 2 ;;
esac
`)
	// The staged candidate binary: its rescue plan is the authoritative
	// inventory of owner-tagged host resources during recovery.
	writeExecutable(filepath.Join(workRoot, "stage/smartgate"), `#!/bin/sh
echo "staged-smartgate $* execute=${SMARTGATE_RESCUE_EXECUTE:-unset}" >>"$SMARTGATE_TEST_LOG"
exit 0
`)
	// The failed candidate left its Gateway Fabric table behind (B23).
	fabricMarker := filepath.Join(temp, "fabric-table")
	writeFile(fabricMarker, "present\n")
	if options.schema >= 2 {
		writeExecutable(filepath.Join(backupRoot, "usr/libexec/smartgate-worker-control"), `#!/bin/sh
echo "worker-control $*" >>"$SMARTGATE_TEST_LOG"
case "${1:-}" in
  stop) exit 0 ;;
  status) [ "${SMARTGATE_TEST_WORKER_FAIL:-0}" != 1 ] ;;
  *) exit 2 ;;
esac
`)
		writeExecutable(filepath.Join(backupRoot, "etc/smartgate/fabric-reconcile.sh"), `#!/bin/sh
printf '%s\n' 'owner=smartgate:firewall-hook:fabric-reconcile'
`)
		writeExecutable(filepath.Join(backupRoot, "etc/hotplug.d/firewall/95-smartgate"), `#!/bin/sh
# owner=smartgate:firewall-hook:fabric-reconcile
exit 0
`)
		writeFile(filepath.Join(backupRoot, "etc/dnsmasq.d/smartgate.conf"), "# owner=smartgate:dnsmasq:smartgate-upstream\nserver=127.0.0.1#1053\n")
	} else if options.mode == "on" {
		writeFile(legacyRules, "present before firewall reload\n")
		writeFile(filepath.Join(backupRoot, "etc/dnsmasq.d/smartgate.conf"), "server=127.0.0.1#1053\n")
		writeExecutable(filepath.Join(backupRoot, "etc/hotplug.d/firewall/95-smartgate"), `#!/bin/sh
# owner=smartgate:firewall-hook:fw4-reconcile
echo "legacy-firewall replay" >>"$SMARTGATE_TEST_LOG"
[ "${SMARTGATE_TEST_LEGACY_HOOK_FAIL:-0}" != 1 ] || exit 1
: >"$SMARTGATE_TEST_LEGACY_RULES"
`)
	}
	for _, service := range []string{"firewall", "dnsmasq"} {
		writeExecutable(filepath.Join(rootFS, "etc/init.d", service), `#!/bin/sh
echo "service $(basename "$0") $*" >>"$SMARTGATE_TEST_LOG"
if [ "$(basename "$0")" = firewall ] && [ "${1:-}" = reload ]; then
  rm -f "$SMARTGATE_TEST_LEGACY_RULES"
fi
exit 0
`)
	}
	// The legacy service starts stopped and acquires TUN/DNS on restart.
	legacyInitStub := `#!/bin/sh
echo "service sing-box $*" >>"$SMARTGATE_TEST_LOG"
case "${1:-}" in
  status|running) [ -f "$SMARTGATE_TEST_LEGACY_RUNNING" ] ;;
  start|restart) : >"$SMARTGATE_TEST_LEGACY_RUNNING" ;;
  stop) rm -f "$SMARTGATE_TEST_LEGACY_RUNNING" ;;
  *) exit 0 ;;
esac
`
	writeExecutable(filepath.Join(rootFS, "etc/init.d/sing-box"), legacyInitStub)
	if options.completeLegacy {
		writeFile(filepath.Join(backupRoot, "legacy-directory-complete"), "etc/sing-box\n")
		writeFile(filepath.Join(backupRoot, "etc/sing-box/rule-set/local.srs"), "local-rules-fixture")
		writeFile(filepath.Join(backupRoot, "etc/config/sing-box"), "legacy-uci-fixture")
		writeExecutable(filepath.Join(backupRoot, "etc/init.d/sing-box"), legacyInitStub+"# restored fixture\n")
		if err := os.Symlink("local.srs", filepath.Join(backupRoot, "etc/sing-box/rule-set/alias.srs")); err != nil {
			t.Fatal(err)
		}
		writeFile(filepath.Join(rootFS, "etc/sing-box/stale-candidate.json"), "{}")
	}
	if failureMode == "legacy-directory" {
		writeFile(filepath.Join(backupRoot, "legacy-directory-complete"), "etc/unrelated\n")
	}

	backupPath := filepath.Join(deployRoot, "rollback.tgz")
	if err := os.MkdirAll(deployRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "-czf", backupPath, "-C", backupRoot, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create recovery fixture: %v: %s", err, output)
	}
	writeFile(filepath.Join(deployRoot, "pending.json"), `{"phase":"payload_installed","backup":"`+backupPath+`","work":"`+workRoot+`"}`)
	writeFile(filepath.Join(deployRoot, "lock/owner"), "stale\n")
	writeFile(filepath.Join(rootFS, "etc/rc.d/S01smartgate-deploy-recover"), "hook\n")
	writeFile(filepath.Join(rootFS, "tmp/smartgate/deploy-scheduler-disabled"), "gate\n")

	writeExecutable(filepath.Join(fakeBin, "jsonfilter"), `#!/bin/sh
echo "jsonfilter $*" >>"$SMARTGATE_TEST_LOG"
case "$*" in
  *pending.json*@.backup*) printf '%s\n' "$SMARTGATE_TEST_BACKUP" ;;
  *pending.json*@.work*) printf '%s\n' "$SMARTGATE_TEST_WORK" ;;
  *version.json*) printf '%s\n' old-commit ;;
	*spec.json*schema_version*) printf '%s\n' "$SMARTGATE_TEST_SCHEMA" ;;
	*applied-state.json*schema_version*) printf '%s\n' "$SMARTGATE_TEST_SCHEMA" ;;
	*pool_bindings*target.worker*) printf '%s\n' "$SMARTGATE_TEST_WORKERS" ;;
	*execution_graph.workers*id*) printf '%s\n' "$SMARTGATE_TEST_WORKERS" ;;
	*applied-state.json*config_epoch*) printf '%s\n' 7 ;;
	*runtime.json*config_epoch*) printf '%s\n' 7 ;;
	*applied-state.json*topology_hash*) printf '%s\n' topology-old ;;
	*runtime.json*topology_hash*) printf '%s\n' topology-old ;;
esac
`)
	writeExecutable(filepath.Join(fakeBin, "wget"), `#!/bin/sh
echo "wget $*" >>"$SMARTGATE_TEST_LOG"
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) output=$2; shift 2 ;;
    http://*) url=$1; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  */healthz)
    [ "${SMARTGATE_TEST_API_FAIL:-0}" != 1 ] || exit 1
    printf 'ok\n' >"$output"
    ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(filepath.Join(fakeBin, "pidof"), `#!/bin/sh
echo "pidof $*" >>"$SMARTGATE_TEST_LOG"
case "${1:-}" in
  smartgate) [ -f "$SMARTGATE_TEST_DAEMON" ] ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(filepath.Join(fakeBin, "ip"), `#!/bin/sh
echo "ip $*" >>"$SMARTGATE_TEST_LOG"
case "$*" in
  "link show dev tun-sb") [ -f "$SMARTGATE_TEST_LEGACY_RUNNING" ]; exit $? ;;
esac
exit 1
`)
	writeExecutable(filepath.Join(fakeBin, "nft"), `#!/bin/sh
echo "nft $*" >>"$SMARTGATE_TEST_LOG"
case "$*" in
  "list table inet smartgate")
    [ -f "$SMARTGATE_TEST_FABRIC_TABLE" ] || exit 1
    printf 'table inet smartgate {\n\tcomment "owner=smartgate:fabric-mark-table:smartgate"\n}\n'
    ;;
  "delete table inet smartgate")
    [ -f "$SMARTGATE_TEST_FABRIC_TABLE" ] || exit 1
    rm -f "$SMARTGATE_TEST_FABRIC_TABLE"
    ;;
  *"list ruleset"*)
    if [ -f "$SMARTGATE_TEST_LEGACY_RULES" ]; then
      printf '%s\n' \
        'owner=smartgate:dns-hijack:lan-ipv4-53' \
        'owner=smartgate:dot-block:lan-ipv4-853'
    fi
    [ -f "$SMARTGATE_TEST_FABRIC_TABLE" ] || exit 0
    printf '%s\n' \
      'owner=smartgate:fabric-mark-table:smartgate' \
      'owner=smartgate:dns-hijack:lan-dualstack-53' \
      'owner=smartgate:dot-block:lan-dualstack-853'
    ;;
esac
exit 0
`)
	writeExecutable(filepath.Join(fakeBin, "netstat"), `#!/bin/sh
echo "netstat $*" >>"$SMARTGATE_TEST_LOG"
printf '%s\n' 'tcp 0 0 :::1053 :::* LISTEN'
`)
	writeExecutable(filepath.Join(fakeBin, "rm"), `#!/bin/sh
echo "rm $*" >>"$SMARTGATE_TEST_LOG"
for arg in "$@"; do
  if [ "$arg" = "$SMARTGATE_DEPLOY_ROOT/pending.json" ] && [ "${SMARTGATE_TEST_EXPECT_BOOT_GUARD:-0}" = 1 ]; then
    [ -f "$SMARTGATE_DEPLOY_ROOT/recovered-running" ] || { echo missing-boot-guard >>"$SMARTGATE_TEST_LOG"; exit 96; }
  fi
done
exec /bin/rm "$@"
`)
	for _, command := range []string{"sleep", "sync"} {
		writeExecutable(filepath.Join(fakeBin, command), `#!/bin/sh
exit 0
`)
	}

	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "deploy-recover.sh"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{scriptPath}
	if modeArg != "" {
		args = append(args, modeArg)
	}
	recoverCmd := exec.Command("sh", args...)
	recoverCmd.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SMARTGATE_DEPLOY_ROOT="+deployRoot,
		"SMARTGATE_SYSTEM_ROOT="+rootFS,
		"SMARTGATE_API_BASE=http://127.0.0.1:3000",
		"SMARTGATE_TEST_BACKUP="+backupPath,
		"SMARTGATE_TEST_WORK="+workRoot,
		"SMARTGATE_TEST_LOG="+logPath,
		"SMARTGATE_TEST_DAEMON="+daemonMarker,
		"SMARTGATE_TEST_LEGACY_RUNNING="+legacyMarker,
		"SMARTGATE_TEST_LEGACY_RULES="+legacyRules,
		"SMARTGATE_TEST_SCHEMA="+fmt.Sprintf("%d", options.schema),
		"SMARTGATE_TEST_WORKERS="+options.workers,
		"SMARTGATE_TEST_FABRIC_TABLE="+fabricMarker,
	)
	if options.workerStatusFail {
		recoverCmd.Env = append(recoverCmd.Env, "SMARTGATE_TEST_WORKER_FAIL=1")
	}
	if failureMode == "api" {
		recoverCmd.Env = append(recoverCmd.Env, "SMARTGATE_TEST_API_FAIL=1")
	}
	if failureMode == "legacy-firewall" {
		recoverCmd.Env = append(recoverCmd.Env, "SMARTGATE_TEST_LEGACY_HOOK_FAIL=1")
	}
	if expectBootMarker {
		recoverCmd.Env = append(recoverCmd.Env, "SMARTGATE_TEST_EXPECT_BOOT_GUARD=1")
	}
	output, runErr := recoverCmd.CombinedOutput()
	if failureMode != "" {
		if runErr == nil {
			t.Fatalf("recovery unexpectedly passed the %s gate", failureMode)
		}
		for _, marker := range []string{
			filepath.Join(deployRoot, "pending.json"),
			filepath.Join(rootFS, "etc/rc.d/S01smartgate-deploy-recover"),
			filepath.Join(rootFS, "tmp/smartgate/deploy-scheduler-disabled"),
			filepath.Join(deployRoot, "lock/owner"),
		} {
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("recovery failure removed marker %s: %v\n%s", marker, err, output)
			}
		}
		if _, err := os.Stat(filepath.Join(deployRoot, "pending.starting.json")); !os.IsNotExist(err) {
			t.Fatalf("recovery failure did not restore canonical journal name: %v", err)
		}
		if failureMode == "checksum" || failureMode == "legacy-directory" {
			if _, err := os.Stat(daemonMarker); !os.IsNotExist(err) {
				t.Fatalf("invalid recovery payload started the daemon: %v", err)
			}
		}
		if failureMode == "legacy-directory" {
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), "smartgate stop") || strings.Contains(string(log), "staged-smartgate rescue") {
				t.Fatalf("invalid legacy directory changed services:\n%s", log)
			}
		}
		return
	}
	if runErr != nil {
		t.Fatalf("manual recovery failed: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "restored payload checksums verified") {
		t.Fatalf("restored payload was not checksum-verified:\n%s", output)
	}
	for path, expected := range map[string]string{"etc/config/firewall": "baseline-firewall\n", "etc/firewall.smartgate-reconcile": "#!/bin/sh\n# SmartGate V2 baseline bridge\n"} {
		content, err := os.ReadFile(filepath.Join(rootFS, path))
		if err != nil || string(content) != expected {
			t.Fatalf("firewall lifecycle restore lost %s: %v", path, err)
		}
	}
	if options.completeLegacy {
		for path, expected := range map[string]string{
			"etc/sing-box/rule-set/local.srs": "local-rules-fixture",
			"etc/config/sing-box":             "legacy-uci-fixture",
			"etc/init.d/sing-box":             legacyInitStub + "# restored fixture\n",
		} {
			content, err := os.ReadFile(filepath.Join(rootFS, path))
			if err != nil || string(content) != expected {
				t.Fatalf("complete legacy restore lost %s: %v", path, err)
			}
		}
		if _, err := os.Stat(filepath.Join(rootFS, "etc/sing-box/stale-candidate.json")); !os.IsNotExist(err) {
			t.Fatalf("complete legacy restore retained stale candidate file: %v", err)
		}
		link, err := os.Readlink(filepath.Join(rootFS, "etc/sing-box/rule-set/alias.srs"))
		if err != nil || link != "local.srs" {
			t.Fatalf("complete legacy restore lost rule-set symlink: %q: %v", link, err)
		}
	}
	if _, err := os.Stat(daemonMarker); err != nil {
		t.Fatalf("restored daemon was not started: %v", err)
	}
	if _, err := os.Stat(filepath.Join(deployRoot, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("journal was not removed after all gates: %v", err)
	}
	if _, err := os.Stat(filepath.Join(deployRoot, "pending.starting.json")); !os.IsNotExist(err) {
		t.Fatalf("temporary start marker was not finalized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootFS, "etc/rc.d/S01smartgate-deploy-recover")); !os.IsNotExist(err) {
		t.Fatalf("boot hook was not removed after all gates: %v", err)
	}
	_, markerErr := os.Stat(filepath.Join(deployRoot, "recovered-running"))
	if expectBootMarker && markerErr != nil {
		t.Fatalf("boot recovery did not leave the one-shot double-start guard: %v", markerErr)
	}
	if !expectBootMarker && !os.IsNotExist(markerErr) {
		t.Fatalf("non-boot recovery unexpectedly left a double-start guard: %v", markerErr)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	startAt := strings.Index(log, "smartgate start")
	healthAt := strings.Index(log, "http://127.0.0.1:3000/healthz")
	versionAt := strings.Index(log, "binary version")
	controlAt := strings.Index(log, "runtime.json -e @.config_epoch")
	if options.schema >= 2 {
		dataPlaneAt := strings.Index(log, "netstat -ln")
		if startAt < 0 || healthAt <= startAt || versionAt <= healthAt || controlAt <= versionAt || dataPlaneAt <= controlAt {
			t.Fatalf("V3 recovery gates ran out of order:\n%s", log)
		}
	} else {
		dataPlaneAt := strings.LastIndex(log, "service sing-box running")
		if startAt < 0 || healthAt <= startAt || versionAt <= healthAt || controlAt <= versionAt || dataPlaneAt <= controlAt {
			t.Fatalf("V2 recovery gates ran out of order:\n%s", log)
		}
	}
	if options.schema == 1 && options.mode == "on" {
		reloadAt := strings.Index(log, "service firewall reload")
		replayAt := strings.Index(log, "legacy-firewall replay")
		dataPlaneAt := strings.LastIndex(log, "service sing-box running")
		if reloadAt < 0 || replayAt <= reloadAt || replayAt <= startAt || dataPlaneAt <= replayAt {
			t.Fatalf("V2 firewall rules must be replayed after reload/startup and before the data-plane gate:\n%s", log)
		}
	} else if strings.Contains(log, "legacy-firewall replay") {
		t.Fatalf("V3 and disabled gateways must not replay legacy firewall rules:\n%s", log)
	}
	if got := strings.Count(log, "smartgate start"); got != 1 {
		t.Fatalf("recovery started the daemon %d times, want exactly once:\n%s", got, log)
	}
	// B23: the candidate's Gateway Fabric and Workers must be retired from the
	// live inventory before the pid files they are found through are deleted.
	deleteTableAt := strings.Index(log, "nft delete table inet smartgate")
	removeRuntimeAt := strings.Index(log, "rm -rf "+filepath.Join(rootFS, "tmp/smartgate/workers"))
	if deleteTableAt < 0 || removeRuntimeAt < 0 || deleteTableAt > removeRuntimeAt || deleteTableAt > startAt {
		t.Fatalf("recovery must delete the leftover Gateway Fabric table before deleting Worker inventory and starting the restored daemon:\n%s", log)
	}
	stagedRescue := "staged-smartgate rescue -data-dir"
	if modeArg == "--boot-hook" {
		if strings.Contains(log, stagedRescue) {
			t.Fatalf("boot recovery must not run the staged rescue (nothing is live and it would start services early):\n%s", log)
		}
	} else {
		rescueAt := strings.Index(log, stagedRescue)
		if rescueAt < 0 || rescueAt > deleteTableAt {
			t.Fatalf("live recovery must run the staged rescue plan before the host sweep:\n%s", log)
		}
		if !strings.Contains(log[rescueAt:], "execute=1") {
			t.Fatalf("staged rescue must be told to execute its cleanup plan:\n%s", log)
		}
	}
	if strings.Contains(log, "recursive-start") {
		t.Fatalf("restored init observed the pending journal and recursed:\n%s", log)
	}
	if strings.Contains(log, "missing-boot-guard") {
		t.Fatalf("boot recovery removed its last journal before persisting the double-start guard:\n%s", log)
	}
	if options.schema >= 2 && strings.Contains(log, legacyRuntimeFixture.ServiceRestartOutput) {
		t.Fatalf("V3 recovery unexpectedly restarted the legacy sing-box service:\n%s", log)
	}
}

func TestDeployRecoverInitPathDoesNotDoubleStart(t *testing.T) {
	for _, markerName := range []string{"pending.json", "pending.starting.json"} {
		t.Run(markerName, func(t *testing.T) {
			temp := t.TempDir()
			journal := filepath.Join(temp, "pending.json")
			starting := filepath.Join(temp, "pending.starting.json")
			helper := filepath.Join(temp, "deploy-recover")
			logPath := filepath.Join(temp, "init.log")
			marker := journal
			if markerName == "pending.starting.json" {
				marker = starting
			}
			if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			helperBody := `#!/bin/sh
echo "recover $*" >>"$SMARTGATE_TEST_LOG"
exit 0
`
			if err := os.WriteFile(helper, []byte(helperBody), 0o755); err != nil {
				t.Fatal(err)
			}
			initPath, err := filepath.Abs(filepath.Join("..", "..", "configs", "init.d", "smartgate"))
			if err != nil {
				t.Fatal(err)
			}
			command := `. "$SMARTGATE_TEST_INIT"; start_service`
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(os.Environ(),
				"SMARTGATE_TEST_INIT="+initPath,
				"SMARTGATE_TEST_LOG="+logPath,
				"SMARTGATE_DEPLOY_JOURNAL="+journal,
				"SMARTGATE_DEPLOY_STARTING_JOURNAL="+starting,
				"SMARTGATE_DEPLOY_RECOVER="+helper,
				"SMARTGATE_DEPLOY_LOCK="+filepath.Join(temp, "missing-owner"),
				"SMARTGATE_DEPLOY_RECOVERED_RUNNING="+filepath.Join(temp, "missing-running"),
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("init recovery path failed: %v\n%s", err, output)
			}
			logBytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			log := string(logBytes)
			if got := strings.Count(log, "recover --from-init"); got != 1 {
				t.Fatalf("init path invoked recovery helper %d times, want once: %q", got, log)
			}
		})
	}
}

func TestDeployRecoverPersistsBootGuardBeforeRemovingJournal(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deploy-recover.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	guard := strings.LastIndex(text, `>"$RECOVERED_RUNNING"`)
	removeJournal := strings.LastIndex(text, `rm -f "$JOURNAL"`)
	if guard < 0 || removeJournal < 0 || guard >= removeJournal {
		t.Fatalf("boot double-start guard must be durable before the last recovery marker is removed")
	}
}
