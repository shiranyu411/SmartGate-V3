package netgate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"smartgate/internal/opid"
)

const (
	dnsmasqSmartGatePath = "/etc/dnsmasq.d/smartgate.conf"
	firewallHookPath     = "/etc/hotplug.d/firewall/95-smartgate"
	fabricReconcilePath  = "/etc/smartgate/fabric-reconcile.sh"
	workerControlPath    = "/usr/libexec/smartgate-worker-control"
	workerRuntimeDir     = "/tmp/smartgate/workers"
	maxCommandOutput     = 64 << 10
	commandTruncated     = "\n...[TRUNCATED]"
)

type PlannedCommand struct {
	Argv []string `json:"argv"`
}

type DryRunStep struct {
	Name        string           `json:"name"`
	Resource    OwnedResource    `json:"resource"`
	Commands    []PlannedCommand `json:"commands"`
	Destructive bool             `json:"destructive"`
}

type DryRunPlan struct {
	OpID       string          `json:"op_id"`
	DryRunOnly bool            `json:"dry_run_only"`
	Resources  []OwnedResource `json:"resources"`
	Steps      []DryRunStep    `json:"steps"`
}

type Runner interface {
	Run(ctx context.Context, argv []string) (string, error)
}

type ShellRunner struct{}

type ApplyResult struct {
	OpID  string   `json:"op_id"`
	Logs  []string `json:"logs,omitempty"`
	Steps int      `json:"steps"`
}

func (ShellRunner) Run(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	output := &boundedCommandOutput{limit: maxCommandOutput}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("command %q failed: %w", argv[0], err)
	}
	return output.String(), nil
}

type boundedCommandOutput struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
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
	return len(p), nil
}

func (b *boundedCommandOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return string(b.buf)
	}
	if b.limit <= len(commandTruncated) {
		return commandTruncated[:b.limit]
	}
	keep := b.limit - len(commandTruncated)
	if keep > len(b.buf) {
		keep = len(b.buf)
	}
	return string(b.buf[:keep]) + commandTruncated
}

func OpenWrtDryRunPlan(opID string, owners *OwnerRegistry) (DryRunPlan, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return DryRunPlan{}, err
	}
	resourcePlan := RuntimePlan(safeOpID, owners)
	plan := DryRunPlan{
		OpID:       safeOpID,
		DryRunOnly: true,
		Resources:  resourcePlan.Resources,
	}
	for _, spec := range []struct {
		name     string
		kind     string
		id       string
		commands func(OwnedResource) []PlannedCommand
	}{
		{"tag TUN interface", KindTunInterface, "tun-sb", tunTagCommands},
		{"install fw4 reconcile hook", KindFirewallHook, "fw4-reconcile", firewallHookInstallCommands},
		{"verify LAN DNS 53 hijack", KindDNSHijack, "lan-ipv4-53", dnsHijackVerifyCommands},
		{"verify LAN DoT 853 block", KindDoTBlock, "lan-ipv4-853", dotBlockVerifyCommands},
		{"install dnsmasq override", KindDNSMasq, "wan-cpe-upstream", dnsmasqInstallCommands},
	} {
		step, err := dryRunStep(resourcePlan, spec.name, spec.kind, spec.id, false, spec.commands)
		if err != nil {
			return DryRunPlan{}, err
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, nil
}

func OpenWrtApplyPlan(opID string, owners *OwnerRegistry) (DryRunPlan, error) {
	plan, err := OpenWrtDryRunPlan(opID, owners)
	if err != nil {
		return DryRunPlan{}, err
	}
	plan.DryRunOnly = false
	return plan, nil
}

func ExecutePlan(ctx context.Context, plan DryRunPlan, runner Runner) (ApplyResult, error) {
	if plan.DryRunOnly {
		return ApplyResult{}, fmt.Errorf("refusing to execute dry-run-only plan %q", plan.OpID)
	}
	if runner == nil {
		runner = ShellRunner{}
	}
	result := ApplyResult{OpID: plan.OpID}
	for _, step := range plan.Steps {
		if !step.Resource.IsOwnerTagged() {
			return result, fmt.Errorf("step %q targets unowned resource %s/%s", step.Name, step.Resource.Kind, step.Resource.ID)
		}
		for _, command := range step.Commands {
			if !plannedCommandContains(command, step.Resource.Comment) {
				return result, fmt.Errorf("step %q has command without owner match %q", step.Name, step.Resource.Comment)
			}
			log, err := runner.Run(ctx, command.Argv)
			result.Steps++
			if log != "" {
				result.Logs = append(result.Logs, log)
			}
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func OpenWrtRescueCleanupDryRunPlan(opID string, owners *OwnerRegistry, workers []WorkerCleanupTarget) (DryRunPlan, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return DryRunPlan{}, err
	}
	resourcePlan := RescuePlan(safeOpID, owners, workers)
	plan := DryRunPlan{
		OpID:       safeOpID,
		DryRunOnly: true,
		Resources:  resourcePlan.Resources,
	}
	type stepSpec struct {
		name     string
		kind     string
		id       string
		commands func(OwnedResource) []PlannedCommand
	}
	specs := []stepSpec{
		{"remove Gateway Fabric fw4 reconcile hook", KindFirewallHook, "fabric-reconcile", optionalOwnedFileCleanupCommands(firewallHookPath)},
		{"remove fw4 reconcile hook", KindFirewallHook, "fw4-reconcile", firewallHookCleanupCommands},
		{"restore Gateway Fabric dnsmasq override", KindDNSMasq, "smartgate-upstream", optionalOwnedDNSMasqCleanupCommands},
		{"delete owner-tagged Gateway Fabric", KindFabricTable, "smartgate", fabricCleanupCommands},
		{"delete owner-tagged DNS 53 hijack", KindDNSHijack, "lan-ipv4-53", dnsHijackCleanupCommands},
		{"delete owner-tagged DoT 853 block", KindDoTBlock, "lan-ipv4-853", nftCleanupCommands("forward_lan")},
		{"restore owner-tagged dnsmasq override", KindDNSMasq, "wan-cpe-upstream", dnsmasqRestoreCommands},
		{"delete SmartGate policy routes", KindFabricRoute, "protocol-186", fabricRouteCleanupCommands},
	}
	for _, worker := range workers {
		specs = append(specs, stepSpec{
			name:     fmt.Sprintf("stop %s Worker", worker.DisplayName),
			kind:     KindWorkerService,
			id:       worker.ID,
			commands: workerServiceStopCommands,
		})
	}
	specs = append(specs,
		stepSpec{"delete owner-tagged TUN interface", KindTunInterface, "tun-sb", tunCleanupCommands},
	)
	for _, spec := range specs {
		step, err := dryRunStep(resourcePlan, spec.name, spec.kind, spec.id, true, spec.commands)
		if err != nil {
			return DryRunPlan{}, err
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, nil
}

func OpenWrtRescueCleanupApplyPlan(opID string, owners *OwnerRegistry, workers []WorkerCleanupTarget) (DryRunPlan, error) {
	plan, err := OpenWrtRescueCleanupDryRunPlan(opID, owners, workers)
	if err != nil {
		return DryRunPlan{}, err
	}
	plan.DryRunOnly = false
	return plan, nil
}

func CleanupPlanFromDryRun(plan DryRunPlan) (CleanupPlan, error) {
	actions := make([]CleanupAction, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		if !step.Resource.IsOwnerTagged() {
			return CleanupPlan{}, fmt.Errorf("cleanup step %q targets unowned resource %s/%s", step.Name, step.Resource.Kind, step.Resource.ID)
		}
		if len(step.Commands) == 0 {
			return CleanupPlan{}, fmt.Errorf("cleanup step %q has no commands", step.Name)
		}
		for _, command := range step.Commands {
			if !plannedCommandContains(command, step.Resource.Comment) {
				return CleanupPlan{}, fmt.Errorf("cleanup step %q has command without owner match %q", step.Name, step.Resource.Comment)
			}
		}
		actions = append(actions, CleanupAction{
			Kind:     step.Resource.Kind,
			ID:       step.Resource.ID,
			OwnerTag: step.Resource.OwnerTag,
			Comment:  step.Resource.Comment,
			Commands: step.Commands,
		})
	}
	return CleanupPlan{OpID: plan.OpID, Actions: actions}, nil
}

func dryRunStep(plan Plan, name, kind, id string, destructive bool, commands func(OwnedResource) []PlannedCommand) (DryRunStep, error) {
	resource, err := plan.RequireOwned(kind, id)
	if err != nil {
		return DryRunStep{}, err
	}
	return DryRunStep{
		Name:        name,
		Resource:    resource,
		Commands:    commands(resource),
		Destructive: destructive,
	}, nil
}

func cmd(argv ...string) PlannedCommand {
	return PlannedCommand{Argv: argv}
}

func plannedCommandContains(command PlannedCommand, needle string) bool {
	for _, arg := range command.Argv {
		if arg == needle {
			return true
		}
	}
	return false
}

func workerServiceStopCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "printf '%s\\n' \"$3\" >/dev/null; if test -x \"$1\"; then \"$1\" stop \"$2\"; elif test -e \"$4/$2.owner\" || test -e \"$4/$2.pid\"; then exit 1; fi", "sh", workerControlPath, resource.ID, resource.Comment, workerRuntimeDir),
	}
}

func tunTagCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "ip link show dev \"$1\" >/dev/null 2>&1 && ip link set dev \"$1\" alias \"$2\" && ip -d link show dev \"$1\" | grep -F \"$2\" >/dev/null", "sh", resource.ID, resource.Comment),
	}
}

func tunCleanupCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "if ip link show dev \"$1\" >/dev/null 2>&1; then ip -d link show dev \"$1\" | grep -F \"$2\" >/dev/null || exit 1; ip link delete dev \"$1\"; fi; ! ip link show dev \"$1\" >/dev/null 2>&1", "sh", resource.ID, resource.Comment),
	}
}

func dnsHijackVerifyCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "count=0; for chain in dstnat_lan dstnat; do if nft list chain inet fw4 \"$chain\" >/dev/null 2>&1; then found=$(nft list chain inet fw4 \"$chain\" | grep -F -c \"$1\"); count=$((count+found)); fi; done; test \"$count\" -eq 2", "sh", resource.Comment),
	}
}

func dotBlockVerifyCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "test \"$(nft list chain inet fw4 forward_lan | grep -F -c \"$1\")\" -eq 2", "sh", resource.Comment),
	}
}

func nftCleanupCommands(chain string) func(OwnedResource) []PlannedCommand {
	return func(resource OwnedResource) []PlannedCommand {
		return []PlannedCommand{
			cmd("sh", "-c", "handles=$(nft -a list chain inet fw4 \"$1\" | awk -v owner=\"$2\" 'index($0, owner) {print $NF}'); for handle in $handles; do nft delete rule inet fw4 \"$1\" handle \"$handle\" || exit 1; done; ! nft list chain inet fw4 \"$1\" | grep -F \"$2\" >/dev/null", "sh", chain, resource.Comment),
		}
	}
}

func dnsHijackCleanupCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "for chain in dstnat_lan dstnat; do if nft list chain inet fw4 \"$chain\" >/dev/null 2>&1; then handles=$(nft -a list chain inet fw4 \"$chain\" | awk -v owner=\"$1\" 'index($0, owner) {print $NF}'); for handle in $handles; do nft delete rule inet fw4 \"$chain\" handle \"$handle\" || exit 1; done; ! nft list chain inet fw4 \"$chain\" | grep -F \"$1\" >/dev/null || exit 1; fi; done", "sh", resource.Comment),
	}
}

func dnsmasqInstallCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && printf '%s\\n' \"$2\" 'server=127.0.0.1#1053' > \"$1\" && /etc/init.d/dnsmasq reload", "sh", dnsmasqSmartGatePath, resource.Comment),
	}
}

func dnsmasqRestoreCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "if test -f \"$1\"; then grep -F \"$2\" \"$1\" >/dev/null || exit 1; rm -f \"$1\"; fi; test ! -e \"$1\" && /etc/init.d/dnsmasq reload", "sh", dnsmasqSmartGatePath, resource.Comment),
	}
}

func optionalOwnedDNSMasqCleanupCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{cmd("sh", "-c", DNSMasqRuntimeCleanupScript, "sh", dnsmasqSmartGatePath, resource.Comment)}
}

func optionalOwnedFileCleanupCommands(path string) func(OwnedResource) []PlannedCommand {
	return func(resource OwnedResource) []PlannedCommand {
		return []PlannedCommand{
			cmd("sh", "-c", "if test -f \"$1\" && grep -F \"$2\" \"$1\" >/dev/null; then rm -f \"$1\"; fi", "sh", path, resource.Comment),
		}
	}
}

func fabricCleanupCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "if test -f \"$1\"; then grep -F \"$2\" \"$1\" >/dev/null || exit 1; rm -f \"$1\"; fi", "sh", fabricReconcilePath, resource.Comment),
		cmd("sh", "-c", "tmp=/tmp/smartgate-rescue-fabric.$$.nft; trap 'rm -f \"$tmp\"' EXIT; if nft list table inet smartgate >\"$tmp\" 2>/dev/null; then grep -F \"$1\" \"$tmp\" >/dev/null || exit 1; nft delete table inet smartgate; fi; ! nft list table inet smartgate >/dev/null 2>&1", "sh", resource.Comment),
	}
}

func fabricRouteCleanupCommands(resource OwnedResource) []PlannedCommand {
	const script = `printf '%s\n' "$1" >/dev/null
for family in -4 -6; do
  rules=/tmp/smartgate-rescue-rules.$$
  routes=/tmp/smartgate-rescue-routes.$$
  trap 'rm -f "$rules" "$routes"' EXIT
  ip "$family" rule show 2>/dev/null | awk '$1 ~ /^[0-9]+:$/ { mark=""; table=""; for (i=1; i<=NF; i++) { if ($i=="fwmark") mark=$(i+1); if ($i=="lookup") table=$(i+1) } if (mark!="" && table!="") { sub(/:$/, "", $1); print $1, mark, table } }' >"$rules"
  while read -r priority mark table; do
    case "$priority:$table" in *[!0-9:]*) continue;; esac
    test "$priority" -ge 11001 && test "$priority" -le 15095 || continue
    test "$table" -ge 1001 && test "$table" -le 5095 || continue
    case "$mark" in 0x5???????|0x5???????/0xffffffff) ;; *) continue;; esac
    ip "$family" rule del pref "$priority" fwmark "$mark" table "$table" || exit 1
    ip "$family" route flush table "$table" proto 186 2>/dev/null || true
  done <"$rules"
  ip "$family" route show table all proto 186 2>/dev/null | awk '{ for (i=1; i<=NF; i++) if ($i=="table") print $(i+1) }' | sort -u >"$routes"
  while read -r table; do
    case "$table" in ''|*[!0-9]*) continue;; esac
    test "$table" -ge 1001 && test "$table" -le 5095 || continue
    ip "$family" route flush table "$table" proto 186 2>/dev/null || true
  done <"$routes"
  rm -f "$rules" "$routes"
  trap - EXIT
done`
	return []PlannedCommand{cmd("sh", "-c", script, "sh", resource.Comment)}
}

func firewallHookInstallCommands(resource OwnedResource) []PlannedCommand {
	script := firewallReconcileScript(resource)
	return []PlannedCommand{
		cmd("sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && printf '%s\\n' \"$3\" > \"$1\" && chmod 0755 \"$1\" && \"$1\"", "sh", firewallHookPath, resource.Comment, script),
	}
}

func firewallHookCleanupCommands(resource OwnedResource) []PlannedCommand {
	return []PlannedCommand{
		cmd("sh", "-c", "lock=$3; count=0; deadline=$(( $(date +%s) + 10 )); while ! mkdir \"$lock\" 2>/dev/null; do count=$((count+1)); if test \"$count\" -gt 5; then read lock_pid lock_start <\"$lock/owner\" 2>/dev/null || true; current_start=$(awk '{print $22}' \"/proc/$lock_pid/stat\" 2>/dev/null || true); if test -z \"$lock_pid\" || test -z \"$lock_start\" || test \"$current_start\" != \"$lock_start\"; then rm -rf \"$lock\"; continue; fi; fi; test \"$(date +%s)\" -lt \"$deadline\" || exit 1; sleep 0.1 2>/dev/null || sleep 1; done; self_start=$(awk '{print $22}' /proc/$$/stat); echo \"$$ $self_start\" >\"$lock/owner\"; trap 'rm -f \"$lock/owner\"; rmdir \"$lock\"' EXIT; if test -f \"$1\"; then grep -F \"$2\" \"$1\" >/dev/null || exit 1; rm -f \"$1\"; fi; test ! -e \"$1\"", "sh", firewallHookPath, resource.Comment, "/var/lock/smartgate-fw4.lock"),
	}
}

func firewallReconcileScript(resource OwnedResource) string {
	dnsOwner := strings.Replace(resource.Comment, "firewall-hook:fw4-reconcile", "dns-hijack:lan-ipv4-53", 1)
	dotOwner := strings.Replace(resource.Comment, "firewall-hook:fw4-reconcile", "dot-block:lan-ipv4-853", 1)
	return fmt.Sprintf(`#!/bin/sh
(
hook_owner='%s'
dns_owner='%s'
dot_owner='%s'
hook_path='%s'
lock=/var/lock/smartgate-fw4.lock

smartgate_acquire_lock() {
  count=0
  deadline=$(( $(date +%%s) + 10 ))
  while ! mkdir "$lock" 2>/dev/null; do
    count=$((count+1))
    if test "$count" -gt 5; then
      read lock_pid lock_start <"$lock/owner" 2>/dev/null || true
      current_start=$(awk '{print $22}' "/proc/$lock_pid/stat" 2>/dev/null || true)
      if test -z "$lock_pid" || test -z "$lock_start" || test "$current_start" != "$lock_start"; then
        rm -rf "$lock"
        continue
      fi
    fi
    test "$(date +%%s)" -lt "$deadline" || return 1
    sleep 0.1 2>/dev/null || sleep 1
  done
  self_start=$(awk '{print $22}' /proc/$$/stat)
  echo "$$ $self_start" >"$lock/owner"
}

smartgate_release_lock() {
  rm -f "$lock/owner"
  rmdir "$lock"
}

smartgate_reconcile() {
  smartgate_acquire_lock || return 1
  if ! test -f "$hook_path"; then
    smartgate_release_lock
    return 0
  fi
  for chain in dstnat_lan dstnat; do
    if nft list chain inet fw4 "$chain" >/dev/null 2>&1; then
      handles=$(nft -a list chain inet fw4 "$chain" | awk -v owner="$dns_owner" 'index($0, owner) {print $NF}')
      for handle in $handles; do nft delete rule inet fw4 "$chain" handle "$handle" || { smartgate_release_lock; return 1; }; done
    fi
  done
  handles=$(nft -a list chain inet fw4 forward_lan | awk -v owner="$dot_owner" 'index($0, owner) {print $NF}')
  for handle in $handles; do nft delete rule inet fw4 forward_lan handle "$handle" || { smartgate_release_lock; return 1; }; done
  if nft list chain inet fw4 dstnat_lan >/dev/null 2>&1; then
    nft add rule inet fw4 dstnat_lan udp dport 53 redirect to :1053 comment "\"$dns_owner\"" || { smartgate_release_lock; return 1; }
    nft add rule inet fw4 dstnat_lan tcp dport 53 redirect to :1053 comment "\"$dns_owner\"" || { smartgate_release_lock; return 1; }
    dns_chain=dstnat_lan
  else
    lan_device=$(ubus call network.interface.lan status 2>/dev/null | jsonfilter -e '@.l3_device' 2>/dev/null || true)
    test -n "$lan_device" || lan_device=br-lan
    nft add rule inet fw4 dstnat iifname "$lan_device" udp dport 53 redirect to :1053 comment "\"$dns_owner\"" || { smartgate_release_lock; return 1; }
    nft add rule inet fw4 dstnat iifname "$lan_device" tcp dport 53 redirect to :1053 comment "\"$dns_owner\"" || { smartgate_release_lock; return 1; }
    dns_chain=dstnat
  fi
  nft add rule inet fw4 forward_lan tcp dport 853 reject comment "\"$dot_owner\"" || { smartgate_release_lock; return 1; }
  nft add rule inet fw4 forward_lan udp dport 853 reject comment "\"$dot_owner\"" || { smartgate_release_lock; return 1; }
  test "$(nft list chain inet fw4 "$dns_chain" | grep -F -c "$dns_owner")" -eq 2 || { smartgate_release_lock; return 1; }
  test "$(nft list chain inet fw4 forward_lan | grep -F -c "$dot_owner")" -eq 2 || { smartgate_release_lock; return 1; }
  smartgate_release_lock
}

smartgate_reconcile
)
`, resource.Comment, dnsOwner, dotOwner, firewallHookPath)
}
