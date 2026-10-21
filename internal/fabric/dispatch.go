package fabric

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/state"
)

const (
	NftFamily               = "inet"
	NftTable                = "smartgate"
	FabricReconcilePath     = "/etc/smartgate/fabric-reconcile.sh"
	FabricFirewallHookPath  = "/etc/hotplug.d/firewall/95-smartgate"
	FabricDNSMasqConfigPath = "/etc/dnsmasq.d/smartgate.conf"
	FabricRouteProtocol     = 186
)

type DispatchPlanOptions struct {
	DNSListenPort  int
	ManagementPort int
	LANDevice      string
}

func BuildDispatchDryRunPlan(opID string, owners *netgate.OwnerRegistry, graph state.FabricGraph, options DispatchPlanOptions) (netgate.DryRunPlan, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return netgate.DryRunPlan{}, err
	}
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	if err := validateDispatchGraph(graph, owners); err != nil {
		return netgate.DryRunPlan{}, err
	}
	if options.DNSListenPort == 0 {
		_, rawPort, err := net.SplitHostPort(graph.DNS.Listen)
		if err != nil {
			return netgate.DryRunPlan{}, fmt.Errorf("parse fabric DNS listen address: %w", err)
		}
		options.DNSListenPort, err = strconv.Atoi(rawPort)
		if err != nil || options.DNSListenPort < 1 || options.DNSListenPort > 65535 {
			return netgate.DryRunPlan{}, fmt.Errorf("invalid fabric DNS listen port %q", rawPort)
		}
	}
	if options.ManagementPort == 0 {
		options.ManagementPort = 3000
	}
	if options.LANDevice != "" && !validInterfaceName(options.LANDevice) {
		return netgate.DryRunPlan{}, fmt.Errorf("invalid LAN device %q", options.LANDevice)
	}

	resources := dispatchResources(owners, graph)
	plan := netgate.DryRunPlan{OpID: safeOpID, DryRunOnly: true, Resources: resources}
	tableResource := requireResource(resources, netgate.KindFabricTable, NftTable)
	reconcileScript, err := buildReconcileScript(owners, graph, options)
	if err != nil {
		return netgate.DryRunPlan{}, err
	}
	plan.Steps = append(plan.Steps, netgate.DryRunStep{
		Name:     "install and apply Gateway Fabric reconcile script",
		Resource: tableResource,
		Commands: []netgate.PlannedCommand{
			{Argv: []string{
				"sh", "-c", "set -eu; mkdir -p \"$(dirname \"$1\")\"; tmp=\"$1.tmp.$$\"; trap 'rm -f \"$tmp\"' EXIT; printf '%s\\n' \"$3\" >\"$tmp\"; grep -F \"$2\" \"$tmp\" >/dev/null; chmod 0755 \"$tmp\"; mv \"$tmp\" \"$1\"; \"$1\"",
				"sh", FabricReconcilePath, tableResource.Comment, reconcileScript,
			}},
			{Argv: []string{"sh", "-c", netgate.FirewallPersistenceInstallScript, "sh", tableResource.Comment}},
		},
	})
	for _, resource := range resources {
		if resource.Kind == netgate.KindFabricTable {
			continue
		}
		command, err := verificationCommand(resource, graph, safeOpID)
		if err != nil {
			return netgate.DryRunPlan{}, err
		}
		plan.Steps = append(plan.Steps, netgate.DryRunStep{
			Name: "verify " + resource.Kind + " " + resource.ID, Resource: resource, Commands: []netgate.PlannedCommand{command},
		})
	}
	return plan, nil
}

func BuildDispatchApplyPlan(opID string, owners *netgate.OwnerRegistry, graph state.FabricGraph, options DispatchPlanOptions) (netgate.DryRunPlan, error) {
	plan, err := BuildDispatchDryRunPlan(opID, owners, graph, options)
	if err != nil {
		return netgate.DryRunPlan{}, err
	}
	plan.DryRunOnly = false
	return plan, nil
}

func dispatchResources(owners *netgate.OwnerRegistry, graph state.FabricGraph) []netgate.OwnedResource {
	resources := []netgate.OwnedResource{ownedResource(owners, netgate.KindFabricTable, NftTable)}
	targets := make([]string, 0, len(graph.DNS.PoolSets))
	for target := range graph.DNS.PoolSets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		resources = append(resources, ownedResource(owners, netgate.KindFabricPoolSet, target))
	}
	for _, rule := range graph.Rules {
		if rule.Kind == RuleGeoIP && rule.IPSets != nil {
			resources = append(resources, ownedResource(owners, netgate.KindFabricGeoSet, rule.ID))
		}
	}
	for _, dispatch := range graph.Dispatch {
		resources = append(resources,
			ownedResource(owners, netgate.KindFabricDispatch, dispatch.Pool),
			ownedResource(owners, netgate.KindFabricRoute, dispatch.Pool),
		)
	}
	resources = append(resources,
		ownedResource(owners, netgate.KindDNSHijack, "lan-dualstack-53"),
		ownedResource(owners, netgate.KindDoTBlock, "lan-dualstack-853"),
		ownedResource(owners, netgate.KindDNSMasq, "smartgate-upstream"),
		ownedResource(owners, netgate.KindFirewallHook, "fabric-reconcile"),
	)
	return resources
}

func buildReconcileScript(owners *netgate.OwnerRegistry, graph state.FabricGraph, options DispatchPlanOptions) (string, error) {
	nftScript, err := buildNftScript(owners, graph, options)
	if err != nil {
		return "", err
	}
	tableOwner := owners.Comment(netgate.KindFabricTable, NftTable)
	hookOwner := owners.Comment(netgate.KindFirewallHook, "fabric-reconcile")
	dnsmasqOwner := owners.Comment(netgate.KindDNSMasq, "smartgate-upstream")
	lanDevice := options.LANDevice
	if lanDevice == "" {
		lanDevice = "auto"
	}

	var routes strings.Builder
	for _, dispatch := range graph.Dispatch {
		if !strings.HasPrefix(dispatch.Target, "w_") {
			continue
		}
		owner := owners.Comment(netgate.KindFabricRoute, dispatch.Pool)
		priority := 11000 + dispatch.Table - 1000
		fmt.Fprintf(&routes, "smartgate_route %s %s %d %d '%s'\n", formatMark(dispatch.Mark), formatMark(dispatch.MarkMask), dispatch.Table, priority, owner)
	}

	return fmt.Sprintf(`#!/bin/sh
set -eu

fabric_owner='%s'
hook_owner='%s'
dnsmasq_owner='%s'
configured_lan='%s'
lock=/var/lock/smartgate-fabric.lock
table_file=/tmp/smartgate-fabric.$$.nft
next_file=/tmp/smartgate-fabric-next.$$.nft

smartgate_release_lock() {
  rm -f "$lock/owner" "$table_file" "$next_file"
  rmdir "$lock" 2>/dev/null || true
}

smartgate_acquire_lock() {
  count=0
  deadline=$(( $(date +%%s) + 10 ))
  while ! mkdir "$lock" 2>/dev/null; do
    count=$((count+1))
    if test "$count" -gt 5; then
      read lock_pid lock_start <"$lock/owner" 2>/dev/null || true
      current_start=$(awk '{print $22}' "/proc/$lock_pid/stat" 2>/dev/null || true)
      if test -z "${lock_pid:-}" || test -z "${lock_start:-}" || test "$current_start" != "$lock_start"; then
        rm -rf "$lock"
        continue
      fi
    fi
    test "$(date +%%s)" -lt "$deadline" || return 1
    # busybox sleep may reject fractional seconds; never let that abort set -e
    sleep 0.1 2>/dev/null || sleep 1
  done
  self_start=$(awk '{print $22}' /proc/$$/stat)
  echo "$$ $self_start" >"$lock/owner"
}

smartgate_lan_device() {
  if test "$configured_lan" != auto; then
    printf '%%s\n' "$configured_lan"
    return
  fi
  device=$(ubus call network.interface.lan status 2>/dev/null | jsonfilter -e '@.l3_device' 2>/dev/null || true)
  test -n "$device" || device=br-lan
  printf '%%s\n' "$device"
}

smartgate_route() {
  mark=$1
  mask=$2
  table=$3
  priority=$4
  owner=$5
  case "$mark:$mask:$table:$priority" in *[!0-9a-fx:]*) return 1;; esac
  printf '%%s\n' "$owner" >/dev/null
  while ip -4 rule del pref "$priority" fwmark "$mark/$mask" table "$table" 2>/dev/null; do :; done
  while ip -6 rule del pref "$priority" fwmark "$mark/$mask" table "$table" 2>/dev/null; do :; done
  ip -4 route replace local 0.0.0.0/0 dev lo table "$table" proto %d
  ip -6 route replace local ::/0 dev lo table "$table" proto %d
  ip -4 rule add pref "$priority" fwmark "$mark/$mask" table "$table"
  ip -6 rule add pref "$priority" fwmark "$mark/$mask" table "$table"
}

smartgate_acquire_lock
trap smartgate_release_lock EXIT INT TERM
lan_device=$(smartgate_lan_device)
case "$lan_device" in ''|*[!A-Za-z0-9_.:@-]*) exit 1;; esac
cat >"$table_file" <<'SMARTGATE_NFT'
%s
SMARTGATE_NFT
sed "s|__SMARTGATE_LAN_DEVICE__|$lan_device|g" "$table_file" >"$next_file"
grep -F "$fabric_owner" "$next_file" >/dev/null
if nft list table %s %s >/dev/null 2>&1; then
  nft list table %s %s | grep -F "$fabric_owner" >/dev/null
  { printf 'delete table %s %s\n'; cat "$next_file"; } | nft -f -
else
  nft -f "$next_file"
fi
%s
mkdir -p "$(dirname '%s')"
printf '%%s\n' '#!/bin/sh' '# %s' "test -x '%s' && '%s'" >'%s'
chmod 0755 '%s'
mkdir -p "$(dirname '%s')"
printf '%%s\n' '# %s' 'no-resolv' 'server=127.0.0.1#%d' >'%s'
%s
/etc/init.d/dnsmasq reload
`, tableOwner, hookOwner, dnsmasqOwner, lanDevice, FabricRouteProtocol, FabricRouteProtocol,
		nftScript, NftFamily, NftTable, NftFamily, NftTable, NftFamily, NftTable, routes.String(),
		FabricFirewallHookPath, hookOwner, FabricReconcilePath, FabricReconcilePath, FabricFirewallHookPath,
		FabricFirewallHookPath, FabricDNSMasqConfigPath, dnsmasqOwner, options.DNSListenPort, FabricDNSMasqConfigPath, netgate.DNSMasqRuntimeInstallScript), nil
}

func buildNftScript(owners *netgate.OwnerRegistry, graph state.FabricGraph, options DispatchPlanOptions) (string, error) {
	dispatchByPool := make(map[string]state.PoolDispatchGraph, len(graph.Dispatch))
	for _, dispatch := range graph.Dispatch {
		dispatchByPool[dispatch.Pool] = dispatch
	}
	if _, ok := dispatchByPool[graph.FallbackPool]; !ok {
		return "", fmt.Errorf("fallback pool %s has no dispatch", graph.FallbackPool)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "table %s %s {\n", NftFamily, NftTable)
	fmt.Fprintf(&builder, "  comment %s\n", nftQuote(owners.Comment(netgate.KindFabricTable, NftTable)))

	targets := make([]string, 0, len(graph.DNS.PoolSets))
	for target := range graph.DNS.PoolSets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		names := graph.DNS.PoolSets[target]
		owner := nftQuote(owners.Comment(netgate.KindFabricPoolSet, target))
		fmt.Fprintf(&builder, "  set %s { type ipv4_addr; flags timeout; comment %s; }\n", names.IPv4, owner)
		fmt.Fprintf(&builder, "  set %s { type ipv6_addr; flags timeout; comment %s; }\n", names.IPv6, owner)
	}
	for _, rule := range sortedFabricRules(graph.Rules) {
		if rule.Kind != RuleGeoIP || rule.IPSets == nil {
			continue
		}
		fmt.Fprintf(&builder, "  include %s\n", nftQuote(rule.IPSets.DefinitionPath))
	}

	fmt.Fprintf(&builder, "  chain dns_hijack {\n    type nat hook prerouting priority dstnat; policy accept;\n")
	if graph.DNS.RedirectLAN53 {
		owner := nftQuote(owners.Comment(netgate.KindDNSHijack, "lan-dualstack-53"))
		fmt.Fprintf(&builder, "    iifname %s udp dport 53 redirect to :%d comment %s\n", nftQuote("__SMARTGATE_LAN_DEVICE__"), options.DNSListenPort, owner)
		fmt.Fprintf(&builder, "    iifname %s tcp dport 53 redirect to :%d comment %s\n", nftQuote("__SMARTGATE_LAN_DEVICE__"), options.DNSListenPort, owner)
	}
	fmt.Fprintf(&builder, "  }\n")

	fmt.Fprintf(&builder, "  chain dispatch {\n")
	for _, dispatch := range graph.Dispatch {
		if !strings.HasPrefix(dispatch.Target, "w_") {
			continue
		}
		owner := nftQuote(owners.Comment(netgate.KindFabricDispatch, dispatch.Pool))
		fmt.Fprintf(&builder, "    meta mark %s meta l4proto tcp tproxy to :%d accept comment %s\n", formatMark(dispatch.Mark), dispatch.Port, owner)
		fmt.Fprintf(&builder, "    meta mark %s meta l4proto udp tproxy to :%d accept comment %s\n", formatMark(dispatch.Mark), dispatch.Port, owner)
	}
	fmt.Fprintf(&builder, "    meta mark & 0xf0000000 == 0x50000000 drop comment %s\n", nftQuote(owners.Comment(netgate.KindFabricTable, NftTable)))
	fmt.Fprintf(&builder, "  }\n")

	fmt.Fprintf(&builder, "  chain prerouting {\n    type filter hook prerouting priority mangle; policy accept;\n")
	tableOwner := nftQuote(owners.Comment(netgate.KindFabricTable, NftTable))
	fmt.Fprintf(&builder, "    iifname != %s return comment %s\n", nftQuote("__SMARTGATE_LAN_DEVICE__"), tableOwner)
	fmt.Fprintf(&builder, "    fib daddr type local return comment %s\n", tableOwner)
	fmt.Fprintf(&builder, "    meta l4proto != { tcp, udp } return comment %s\n", tableOwner)
	if !graph.Takeover.IPv4 {
		fmt.Fprintf(&builder, "    meta nfproto ipv4 return comment %s\n", tableOwner)
	}
	if !graph.Takeover.IPv6 {
		fmt.Fprintf(&builder, "    meta nfproto ipv6 return comment %s\n", tableOwner)
	}
	if !graph.Takeover.TCP {
		fmt.Fprintf(&builder, "    meta l4proto tcp return comment %s\n", tableOwner)
	}
	if !graph.Takeover.UDP {
		fmt.Fprintf(&builder, "    meta l4proto udp return comment %s\n", tableOwner)
	}
	fmt.Fprintf(&builder, "    ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 255.255.255.255/32 } return comment %s\n", tableOwner)
	fmt.Fprintf(&builder, "    ip6 daddr { ::/128, ::1/128, fc00::/7, fe80::/10, ff00::/8 } return comment %s\n", tableOwner)
	fmt.Fprintf(&builder, "    udp dport { 53, 67, 68, 5353, 1900 } return comment %s\n", tableOwner)
	fmt.Fprintf(&builder, "    tcp dport { 53, %d } return comment %s\n", options.ManagementPort, tableOwner)
	if graph.DNS.BlockDoT853 {
		owner := nftQuote(owners.Comment(netgate.KindDoTBlock, "lan-dualstack-853"))
		fmt.Fprintf(&builder, "    tcp dport 853 reject comment %s\n", owner)
		fmt.Fprintf(&builder, "    udp dport 853 reject comment %s\n", owner)
	}
	fmt.Fprintf(&builder, "    ct state established,related ct mark & 0xf0000000 == 0x50000000 meta mark set ct mark jump dispatch comment %s\n", tableOwner)

	for _, rule := range sortedFabricRules(graph.Rules) {
		switch rule.Kind {
		case RuleIPCIDR:
			family := "ip"
			if strings.Contains(rule.Value, ":") {
				family = "ip6"
			}
			writeClassificationRule(&builder, family+" daddr "+rule.Value, rule.Target, dispatchByPool, owners, rule.ID)
		case RuleGeoIP:
			if rule.IPSets == nil {
				return "", fmt.Errorf("GeoIP rule %s has no nft set names", rule.ID)
			}
			writeClassificationRule(&builder, "ip daddr @"+rule.IPSets.IPv4, rule.Target, dispatchByPool, owners, rule.ID)
			writeClassificationRule(&builder, "ip6 daddr @"+rule.IPSets.IPv6, rule.Target, dispatchByPool, owners, rule.ID)
		}
	}
	for _, target := range orderedSetTargets(targets, graph.FallbackPool) {
		names := graph.DNS.PoolSets[target]
		writeClassificationRule(&builder, "ip daddr @"+names.IPv4, target, dispatchByPool, owners, "dns-set")
		writeClassificationRule(&builder, "ip6 daddr @"+names.IPv6, target, dispatchByPool, owners, "dns-set")
	}
	writeClassificationRule(&builder, "", graph.FallbackPool, dispatchByPool, owners, "fallback")
	fmt.Fprintf(&builder, "  }\n")

	ports := workerPorts(graph.Dispatch)
	fmt.Fprintf(&builder, "  chain ingress_guard {\n    type filter hook input priority filter - 5; policy accept;\n")
	if graph.DNS.RedirectLAN53 {
		owner := nftQuote(owners.Comment(netgate.KindDNSHijack, "lan-dualstack-53"))
		fmt.Fprintf(&builder, "    iifname != \"lo\" iifname != %s tcp dport %d drop comment %s\n", nftQuote("__SMARTGATE_LAN_DEVICE__"), options.DNSListenPort, owner)
		fmt.Fprintf(&builder, "    iifname != \"lo\" iifname != %s udp dport %d drop comment %s\n", nftQuote("__SMARTGATE_LAN_DEVICE__"), options.DNSListenPort, owner)
	}
	if len(ports.tcp) > 0 {
		fmt.Fprintf(&builder, "    iifname != \"lo\" tcp dport { %s } drop comment %s\n", joinInts(ports.tcp), tableOwner)
	}
	if len(ports.udp) > 0 {
		fmt.Fprintf(&builder, "    iifname != \"lo\" udp dport { %s } drop comment %s\n", joinInts(ports.udp), tableOwner)
	}
	fmt.Fprintf(&builder, "  }\n}\n")
	return builder.String(), nil
}

func writeClassificationRule(builder *strings.Builder, expression, target string, dispatches map[string]state.PoolDispatchGraph, owners *netgate.OwnerRegistry, source string) {
	prefix := "    "
	if expression != "" {
		prefix += expression + " "
	}
	switch target {
	case TargetDirect:
		fmt.Fprintf(builder, "%sreturn comment %s\n", prefix, nftQuote(owners.Comment(netgate.KindFabricDispatch, "direct")))
	case TargetBlock:
		fmt.Fprintf(builder, "%sreject comment %s\n", prefix, nftQuote(owners.Comment(netgate.KindFabricDispatch, "block")))
	default:
		dispatch, ok := dispatches[target]
		if !ok {
			fmt.Fprintf(builder, "%sdrop comment %s\n", prefix, nftQuote(owners.Comment(netgate.KindFabricDispatch, "unbound-"+source)))
			return
		}
		if dispatch.Target == TargetDirect {
			fmt.Fprintf(builder, "%sreturn comment %s\n", prefix, nftQuote(owners.Comment(netgate.KindFabricDispatch, dispatch.Pool)))
			return
		}
		if dispatch.Target == TargetBlock || dispatch.Port == 0 {
			fmt.Fprintf(builder, "%sreject comment %s\n", prefix, nftQuote(owners.Comment(netgate.KindFabricDispatch, dispatch.Pool)))
			return
		}
		fmt.Fprintf(builder, "%smeta mark set %s ct mark set %s jump dispatch comment %s\n", prefix, formatMark(dispatch.Mark), formatMark(dispatch.Mark), nftQuote(owners.Comment(netgate.KindFabricDispatch, dispatch.Pool)))
	}
}

func verificationCommand(resource netgate.OwnedResource, graph state.FabricGraph, opID string) (netgate.PlannedCommand, error) {
	switch resource.Kind {
	case netgate.KindFabricPoolSet, netgate.KindFabricGeoSet, netgate.KindFabricDispatch, netgate.KindDNSHijack, netgate.KindDoTBlock, netgate.KindFabricTable:
		cache := "/tmp/smartgate-verify-" + opID + ".nft"
		return netgate.PlannedCommand{Argv: []string{"sh", "-c", "set -eu; cache=\"$4\"; if test ! -s \"$cache\"; then nft -t list table \"$1\" \"$2\" >\"$cache\"; fi; grep -F \"$3\" \"$cache\" >/dev/null", "sh", NftFamily, NftTable, resource.Comment, cache}}, nil
	case netgate.KindFabricRoute:
		var dispatch state.PoolDispatchGraph
		for _, item := range graph.Dispatch {
			if item.Pool == resource.ID {
				dispatch = item
				break
			}
		}
		if !strings.HasPrefix(dispatch.Target, "w_") {
			return netgate.PlannedCommand{Argv: []string{"sh", "-c", "printf '%s\\n' \"$1\" >/dev/null", "sh", resource.Comment}}, nil
		}
		return netgate.PlannedCommand{Argv: []string{"sh", "-c", "ip -4 rule show | grep -E \"$1\" >/dev/null && ip -6 rule show | grep -E \"$1\" >/dev/null && printf '%s\\n' \"$2\" >/dev/null", "sh", routeRulePattern(dispatch).String(), resource.Comment}}, nil
	case netgate.KindDNSMasq:
		return netgate.PlannedCommand{Argv: []string{"sh", "-c", netgate.DNSMasqRuntimeVerifyScript, "sh", FabricDNSMasqConfigPath, resource.Comment}}, nil
	case netgate.KindFirewallHook:
		return netgate.PlannedCommand{Argv: []string{"sh", "-c", "test -x \"$1\" && grep -F \"$2\" \"$1\" >/dev/null &&\n" + netgate.FirewallPersistenceVerifyScript, "sh", FabricFirewallHookPath, resource.Comment}}, nil
	default:
		return netgate.PlannedCommand{}, fmt.Errorf("unsupported fabric resource kind %s", resource.Kind)
	}
}

// routeRulePattern matches the `ip rule show` line for a dispatch's policy
// route. iproute2 (and busybox) omit the mask when it is 0xffffffff, so a full
// mask is optional in the pattern; any other mask is always printed.
func routeRulePattern(dispatch state.PoolDispatchGraph) *regexp.Regexp {
	mark := regexp.QuoteMeta(formatMark(dispatch.Mark))
	mask := regexp.QuoteMeta(formatMark(dispatch.MarkMask))
	table := strconv.Itoa(dispatch.Table)
	if dispatch.MarkMask == 0xffffffff {
		return regexp.MustCompile(`fwmark ` + mark + `(/` + mask + `)? lookup ` + table + `\b`)
	}
	return regexp.MustCompile(`fwmark ` + mark + `/` + mask + ` lookup ` + table + `\b`)
}

// VerifyDispatch is the read-only counterpart of the dispatch plan: it lists
// the owner table once (terse, without set elements), reads the policy rules
// once per family, and checks the hook and dnsmasq files. It never mutates the
// host, so the supervisor can run it every poll and only reassert on drift.
func VerifyDispatch(ctx context.Context, runner netgate.Runner, plan netgate.DryRunPlan, graph state.FabricGraph) error {
	if runner == nil {
		runner = netgate.ShellRunner{}
	}
	table, err := runner.Run(ctx, []string{"nft", "-t", "list", "table", NftFamily, NftTable})
	if err != nil {
		return fmt.Errorf("owner table %s %s is not loaded: %w", NftFamily, NftTable, err)
	}
	var missing []string
	needRoutes := false
	for _, resource := range plan.Resources {
		switch resource.Kind {
		case netgate.KindFabricTable, netgate.KindFabricPoolSet, netgate.KindFabricGeoSet, netgate.KindFabricDispatch, netgate.KindDNSHijack, netgate.KindDoTBlock:
			if !strings.Contains(table, resource.Comment) {
				missing = append(missing, resource.Kind+":"+resource.ID)
			}
		case netgate.KindFabricRoute:
			needRoutes = true
		case netgate.KindDNSMasq:
			if _, err := runner.Run(ctx, []string{"sh", "-c", netgate.DNSMasqRuntimeVerifyScript, "sh", FabricDNSMasqConfigPath, resource.Comment}); err != nil {
				missing = append(missing, resource.Kind+":"+resource.ID)
			}
		case netgate.KindFirewallHook:
			if _, err := runner.Run(ctx, []string{"sh", "-c", "test -x \"$1\" && grep -F \"$2\" \"$1\" >/dev/null &&\n" + netgate.FirewallPersistenceVerifyScript, "sh", FabricFirewallHookPath, resource.Comment}); err != nil {
				missing = append(missing, resource.Kind+":"+resource.ID)
			}
		}
	}
	if needRoutes {
		for _, family := range []string{"-4", "-6"} {
			rules, err := runner.Run(ctx, []string{"ip", family, "rule", "show"})
			if err != nil {
				return fmt.Errorf("list ip %s policy rules: %w", family, err)
			}
			for _, dispatch := range graph.Dispatch {
				if !strings.HasPrefix(dispatch.Target, "w_") {
					continue
				}
				if !routeRulePattern(dispatch).MatchString(rules) {
					missing = append(missing, fmt.Sprintf("%s:%s(ip %s)", netgate.KindFabricRoute, dispatch.Pool, family))
				}
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Gateway Fabric host state is incomplete: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateDispatchGraph(graph state.FabricGraph, owners *netgate.OwnerRegistry) error {
	if !graph.Takeover.Enabled {
		return fmt.Errorf("fabric takeover is disabled")
	}
	if graph.FallbackPool == "" {
		return fmt.Errorf("fabric fallback pool is required")
	}
	if len(graph.Dispatch) == 0 {
		return fmt.Errorf("fabric dispatch is empty")
	}
	marks := map[uint32]string{}
	tables := map[int]string{}
	ports := map[int]string{}
	pools := map[string]struct{}{}
	for _, dispatch := range graph.Dispatch {
		if dispatch.Pool == "" || dispatch.Binding == "" || dispatch.Generation == 0 {
			return fmt.Errorf("fabric dispatch has incomplete binding metadata")
		}
		if _, exists := pools[dispatch.Pool]; exists {
			return fmt.Errorf("duplicate fabric dispatch for pool %s", dispatch.Pool)
		}
		pools[dispatch.Pool] = struct{}{}
		if previous := marks[dispatch.Mark]; previous != "" {
			return fmt.Errorf("fabric mark collision between %s and %s", previous, dispatch.Pool)
		}
		marks[dispatch.Mark] = dispatch.Pool
		if previous := tables[dispatch.Table]; previous != "" {
			return fmt.Errorf("fabric route table collision between %s and %s", previous, dispatch.Pool)
		}
		tables[dispatch.Table] = dispatch.Pool
		if strings.HasPrefix(dispatch.Target, "w_") {
			if dispatch.Port < 1 || dispatch.Port > 65535 || dispatch.DNSPort < 1 || dispatch.DNSPort > 65535 {
				return fmt.Errorf("fabric dispatch %s has invalid worker ports", dispatch.Pool)
			}
			for _, port := range []int{dispatch.Port, dispatch.DNSPort} {
				if previous := ports[port]; previous != "" {
					return fmt.Errorf("fabric port collision between %s and %s", previous, dispatch.Pool)
				}
				ports[port] = dispatch.Pool
			}
		}
	}
	if _, ok := pools[graph.FallbackPool]; !ok {
		return fmt.Errorf("fallback pool %s is not dispatched", graph.FallbackPool)
	}
	for target, names := range graph.DNS.PoolSets {
		if !validNftIdentifier(names.IPv4) || !validNftIdentifier(names.IPv6) {
			return fmt.Errorf("target %s has invalid nft set names", target)
		}
	}
	seenGeoSets := map[string]string{}
	resolvedRuleSets := map[string]state.RuleSetSnapshotRef{}
	for _, ref := range graph.RuleSets {
		if ref.ID == "" {
			return fmt.Errorf("fabric rule-set reference has no id")
		}
		if _, err := validateRuleSetReference(ref); err != nil {
			return fmt.Errorf("fabric rule-set reference %s is invalid: %w", ref.ID, err)
		}
		if _, exists := resolvedRuleSets[ref.ID]; exists {
			return fmt.Errorf("duplicate fabric rule-set reference %s", ref.ID)
		}
		resolvedRuleSets[ref.ID] = ref
	}
	for _, rule := range graph.Rules {
		if rule.Kind != RuleGeoIP {
			continue
		}
		ref, ok := resolvedRuleSets[rule.Value]
		if !ok {
			return fmt.Errorf("GeoIP rule %s has no resolved snapshot", rule.ID)
		}
		if rule.IPSets == nil || !validNftIdentifier(rule.IPSets.IPv4) || !validNftIdentifier(rule.IPSets.IPv6) {
			return fmt.Errorf("GeoIP rule %s has invalid nft set names", rule.ID)
		}
		if !filepath.IsAbs(rule.IPSets.DefinitionPath) || filepath.Clean(rule.IPSets.DefinitionPath) != rule.IPSets.DefinitionPath || !validSHA256Hex(rule.IPSets.DefinitionSHA256) {
			return fmt.Errorf("GeoIP rule %s has invalid nft definition", rule.ID)
		}
		if err := validateGeoIPDefinition(owners, rule, ref); err != nil {
			return fmt.Errorf("GeoIP rule %s has invalid nft definition: %w", rule.ID, err)
		}
		for _, name := range []string{rule.IPSets.IPv4, rule.IPSets.IPv6} {
			if previous := seenGeoSets[name]; previous != "" {
				return fmt.Errorf("GeoIP set collision between %s and %s", previous, rule.ID)
			}
			seenGeoSets[name] = rule.ID
		}
	}
	for _, target := range []string{TargetDirect, TargetBlock, graph.FallbackPool} {
		if _, ok := graph.DNS.PoolSets[target]; !ok {
			return fmt.Errorf("target %s has no DNS nft sets", target)
		}
	}
	return nil
}

func validateGeoIPDefinition(owners *netgate.OwnerRegistry, rule state.FabricRuleGraph, ref state.RuleSetSnapshotRef) error {
	cacheRoot, err := validateRuleSetReference(ref)
	if err != nil {
		return err
	}
	nftDir := filepath.Join(cacheRoot, "nft")
	expectedPath := filepath.Join(nftDir, rule.IPSets.DefinitionSHA256+".nft")
	if rule.IPSets.DefinitionPath != expectedPath {
		return fmt.Errorf("nft definition path does not match its digest")
	}
	if err := validateManagedRuleSetFile(nftDir, rule.IPSets.DefinitionPath, rule.IPSets.DefinitionSHA256+".nft", defaultRuleSetNFTLimit); err != nil {
		return err
	}
	body, err := readRuleSetFile(rule.IPSets.DefinitionPath, defaultRuleSetNFTLimit)
	if err != nil {
		return err
	}
	if sha256Hex(body) != rule.IPSets.DefinitionSHA256 {
		return fmt.Errorf("nft definition hash mismatch")
	}
	matcher, err := LoadRuleSetMatcher(ref, defaultRuleSetJSONLimit)
	if err != nil {
		return err
	}
	ipv4, ipv6 := splitRuleSetPrefixes(matcher.IPCIDR)
	var expected strings.Builder
	owner := nftQuote(owners.Comment(netgate.KindFabricGeoSet, rule.ID))
	writeStaticPrefixSet(&expected, rule.IPSets.IPv4, "ipv4_addr", ipv4, owner)
	writeStaticPrefixSet(&expected, rule.IPSets.IPv6, "ipv6_addr", ipv6, owner)
	expectedBody := []byte(expected.String())
	if sha256Hex(expectedBody) != rule.IPSets.DefinitionSHA256 || !bytes.Equal(body, expectedBody) {
		return fmt.Errorf("nft definition does not match the verified GeoIP matcher")
	}
	return nil
}

func splitRuleSetPrefixes(values []string) ([]string, []string) {
	ipv4 := make([]string, 0, len(values))
	ipv6 := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Contains(value, ":") {
			ipv6 = append(ipv6, value)
		} else {
			ipv4 = append(ipv4, value)
		}
	}
	return ipv4, ipv6
}

func writeStaticPrefixSet(builder *strings.Builder, name, addressType string, prefixes []string, owner string) {
	fmt.Fprintf(builder, "  set %s { type %s; flags interval; comment %s;", name, addressType, owner)
	if len(prefixes) > 0 {
		fmt.Fprintf(builder, " elements = { %s };", strings.Join(prefixes, ", "))
	}
	fmt.Fprintf(builder, " }\n")
}

func sortedFabricRules(rules []state.FabricRuleGraph) []state.FabricRuleGraph {
	out := append([]state.FabricRuleGraph(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func orderedSetTargets(targets []string, fallback string) []string {
	ordered := make([]string, 0, len(targets))
	for _, reserved := range []string{TargetBlock, TargetDirect} {
		for _, target := range targets {
			if target == reserved {
				ordered = append(ordered, target)
			}
		}
	}
	for _, target := range targets {
		if target == TargetBlock || target == TargetDirect || target == fallback {
			continue
		}
		ordered = append(ordered, target)
	}
	for _, target := range targets {
		if target == fallback {
			ordered = append(ordered, target)
		}
	}
	return ordered
}

type portLists struct {
	tcp []int
	udp []int
}

func workerPorts(dispatches []state.PoolDispatchGraph) portLists {
	seenTCP := map[int]struct{}{}
	seenUDP := map[int]struct{}{}
	for _, dispatch := range dispatches {
		if !strings.HasPrefix(dispatch.Target, "w_") {
			continue
		}
		seenTCP[dispatch.Port] = struct{}{}
		seenTCP[dispatch.DNSPort] = struct{}{}
		seenUDP[dispatch.Port] = struct{}{}
	}
	return portLists{tcp: sortedInts(seenTCP), udp: sortedInts(seenUDP)}
}

func sortedInts(values map[int]struct{}) []int {
	out := make([]int, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func joinInts(values []int) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strconv.Itoa(value))
	}
	return strings.Join(out, ", ")
}

func formatMark(mark uint32) string {
	return fmt.Sprintf("0x%08x", mark)
}

func nftQuote(value string) string {
	return strconv.Quote(value)
}

func ownedResource(owners *netgate.OwnerRegistry, kind, id string) netgate.OwnedResource {
	return netgate.OwnedResource{Kind: kind, ID: id, OwnerTag: owners.Tag(kind, id), Comment: owners.Comment(kind, id)}
}

func requireResource(resources []netgate.OwnedResource, kind, id string) netgate.OwnedResource {
	for _, resource := range resources {
		if resource.Kind == kind && resource.ID == id {
			return resource
		}
	}
	panic("required fabric resource is missing")
}

func validInterfaceName(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.:@-", char) {
			continue
		}
		return false
	}
	return true
}
