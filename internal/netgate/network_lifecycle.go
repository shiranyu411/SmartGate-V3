package netgate

// The persistent fw4 bridge stays inert when off/rescue removes the owned hook.
// It replaces the V2 bridge on migration without committing unrelated UCI edits.
const FirewallPersistenceInstallScript = `set -eu
if test -x /etc/firewall.smartgate-reconcile &&
 grep -F 'SmartGate fw4 lifecycle bridge' /etc/firewall.smartgate-reconcile >/dev/null &&
 test "$(uci -q get firewall.smartgate_reconcile.enabled)" = 1 &&
 test "$(uci -q get firewall.smartgate_reconcile.type)" = script &&
 test "$(uci -q get firewall.smartgate_reconcile.path)" = /etc/firewall.smartgate-reconcile; then
 printf '%s\n' "$1" >/dev/null
 exit 0
fi
test -z "$(uci changes firewall)" || { echo 'pending firewall edits' >&2; exit 1; }
wrapper=/etc/firewall.smartgate-reconcile
if test -e "$wrapper" && ! grep -Eq 'SmartGate V2|SmartGate fw4 lifecycle bridge' "$wrapper"; then
 echo 'unrecognized firewall wrapper' >&2; exit 1
fi
existing=$(uci -q get firewall.smartgate_reconcile.path || true)
test -z "$existing" || test "$existing" = "$wrapper" || exit 1
cat > "$wrapper" <<'HOOK'
#!/bin/sh
# SmartGate fw4 lifecycle bridge
hook=/etc/hotplug.d/firewall/95-smartgate
[ -x "$hook" ] || exit 0
grep -F 'owner=smartgate:' "$hook" >/dev/null || exit 1
exec "$hook"
HOOK
chmod 0755 "$wrapper"
uci set firewall.smartgate_reconcile=include
uci set firewall.smartgate_reconcile.type=script
uci set firewall.smartgate_reconcile.path="$wrapper"
uci set firewall.smartgate_reconcile.fw4_compatible=1
uci set firewall.smartgate_reconcile.enabled=1
uci commit firewall
printf '%s\n' "$1" >/dev/null
`

// OpenWrt's default confdir is per-instance under /tmp; /etc/dnsmasq.d is not
// loaded automatically. Follow the generated configuration without changing
// DHCP UCI or importing unrelated files from /etc/dnsmasq.d.
const DNSMasqRuntimeInstallScript = `source=/etc/dnsmasq.d/smartgate.conf
loaded=0
for config in /var/etc/dnsmasq.conf.*; do
 test -f "$config" || continue
 dirs=$(sed -n 's/^conf-dir=//p' "$config")
 for dir in $dirs; do
  dir=${dir%%,*}
  case "$dir" in /*) ;; *) exit 1;; esac
  destination="$dir/smartgate.conf"
  if test "$destination" != "$source"; then
   if test -e "$destination" && ! grep -F 'owner=smartgate:dnsmasq:' "$destination" >/dev/null; then
    echo 'unowned dnsmasq override' >&2; exit 1
   fi
   mkdir -p "$dir"
   cp "$source" "$destination"
  fi
  loaded=1
 done
 dnsmasq --test -C "$config" >/dev/null 2>&1
done
test "$loaded" = 1 || { echo 'dnsmasq has no loaded confdir' >&2; exit 1; }
`

const DNSMasqRuntimeVerifyScript = `set -eu
grep -F "$2" "$1" >/dev/null
loaded=0
for config in /var/etc/dnsmasq.conf.*; do
 test -f "$config" || continue
 dirs=$(sed -n 's/^conf-dir=//p' "$config")
 for dir in $dirs; do
  dir=${dir%%,*}
  case "$dir" in /*) ;; *) exit 1;; esac
  grep -F "$2" "$dir/smartgate.conf" >/dev/null
  cmp -s "$1" "$dir/smartgate.conf"
  loaded=1
 done
done
test "$loaded" = 1
`

const FirewallPersistenceVerifyScript = `test "$(uci -q get firewall.smartgate_reconcile.enabled)" = 1 &&
test "$(uci -q get firewall.smartgate_reconcile.type)" = script &&
test "$(uci -q get firewall.smartgate_reconcile.path)" = /etc/firewall.smartgate-reconcile &&
test -x /etc/firewall.smartgate-reconcile &&
test -x /etc/hotplug.d/firewall/95-smartgate &&
grep -F 'owner=smartgate:' /etc/hotplug.d/firewall/95-smartgate >/dev/null
`

const NetworkPersistenceVerifyScript = DNSMasqRuntimeVerifyScript + "\n" + FirewallPersistenceVerifyScript

const DNSMasqRuntimeCleanupScript = `set -eu
removed=0
for config in /var/etc/dnsmasq.conf.*; do
 test -f "$config" || continue
 dirs=$(sed -n 's/^conf-dir=//p' "$config")
 for dir in $dirs; do
  dir=${dir%%,*}
  case "$dir" in /*) ;; *) exit 1;; esac
  destination="$dir/smartgate.conf"
  if test -f "$destination" && grep -F "$2" "$destination" >/dev/null; then
   rm -f "$destination"
   removed=1
  fi
 done
done
if test -f "$1" && grep -F "$2" "$1" >/dev/null; then rm -f "$1"; removed=1; fi
test "$removed" = 0 || /etc/init.d/dnsmasq reload
`
