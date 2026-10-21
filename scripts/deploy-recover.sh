#!/bin/sh
set -eu

MODE=manual
case "${1:-}" in
  "") ;;
  --from-init) MODE=from-init ;;
  --boot-hook) MODE=boot-hook ;;
  *) echo "smartgate deploy recovery: unknown mode $1" >&2; exit 2 ;;
esac

DEPLOY_ROOT=${SMARTGATE_DEPLOY_ROOT:-/root/smartgate-deployments}
SYSTEM_ROOT=${SMARTGATE_SYSTEM_ROOT:-}
API_BASE=${SMARTGATE_API_BASE:-http://192.168.1.1:3000}
JOURNAL=$DEPLOY_ROOT/pending.json
STARTING_JOURNAL=$DEPLOY_ROOT/pending.starting.json
DEPLOY_LOCK=$DEPLOY_ROOT/lock
GATE=$SYSTEM_ROOT/tmp/smartgate/deploy-scheduler-disabled
BOOT_HOOK=$SYSTEM_ROOT/etc/rc.d/S01smartgate-deploy-recover
RECOVERED_RUNNING=$DEPLOY_ROOT/recovered-running
SMARTGATE_BIN=$SYSTEM_ROOT/usr/bin/smartgate
SMARTGATE_INIT=$SYSTEM_ROOT/etc/init.d/smartgate
FIREWALL_INIT=$SYSTEM_ROOT/etc/init.d/firewall
DNSMASQ_INIT=$SYSTEM_ROOT/etc/init.d/dnsmasq
SMARTGATE_ETC=$SYSTEM_ROOT/etc/smartgate
SMARTGATE_SPEC=$SMARTGATE_ETC/spec.json
SMARTGATE_APPLIED=$SMARTGATE_ETC/applied-state.json
SMARTGATE_TRANSACTIONS=$SMARTGATE_ETC/transactions
SMARTGATE_RUNTIME=$SYSTEM_ROOT/tmp/smartgate/runtime.json
LEGACY_INIT=
LEGACY_PROCESS=
LEGACY_CONFIG=
LEGACY_CONFIG_OWNER=
LEGACY_SERVICE_OWNER=
FIREWALL_HOOK=$SYSTEM_ROOT/etc/hotplug.d/firewall/95-smartgate
DNSMASQ_CONFIG=$SYSTEM_ROOT/etc/dnsmasq.d/smartgate.conf
FIREWALL_WRAPPER=$SYSTEM_ROOT/etc/firewall.smartgate-reconcile
V3_FABRIC_RECONCILE=$SMARTGATE_ETC/fabric-reconcile.sh
INSTALLED_RECOVERY=$SYSTEM_ROOT/usr/lib/smartgate/deploy-recover
WORKER_CONTROL=$SYSTEM_ROOT/usr/libexec/smartgate-worker-control
V3_WORKER_CONFIG_DIR=$SMARTGATE_ETC/workers
V3_WORKER_RUNTIME_DIR=$SYSTEM_ROOT/tmp/smartgate/workers
V3_FABRIC_TABLE_OWNER='owner=smartgate:fabric-mark-table:smartgate'
V3_FABRIC_ROUTE_PROTO=186
PROC_ROOT=${SMARTGATE_PROC_ROOT:-/proc}

load_legacy_metadata() {
  metadata_control=$1
  metadata=$(SMARTGATE_WORKER_ROOT="$SYSTEM_ROOT" "$metadata_control" legacy-metadata) || return 1
  set -- $metadata
  [ "$#" -eq 5 ] || return 1
  LEGACY_INIT=$SYSTEM_ROOT$1
  LEGACY_PROCESS=$2
  LEGACY_CONFIG=$SYSTEM_ROOT$3
  LEGACY_CONFIG_OWNER=$SYSTEM_ROOT$4
  LEGACY_SERVICE_OWNER=$SYSTEM_ROOT$5
}

# A router may run another client with the same binary name (for example a
# diagnostic or health-check client) alongside the V2 data plane. Identify the legacy
# process by the SmartGate-owned config it runs, not by the executable name
# alone. Keep recovery from rejecting a valid V2 restore because of an
# unrelated client.
legacy_process_pids() {
  for proc_cmdline in "$PROC_ROOT"/[0-9]*/cmdline; do
    [ -r "$proc_cmdline" ] || continue
    pid=${proc_cmdline%/cmdline}
    pid=${pid##*/}
    [ "$pid" != "$$" ] || continue
    kill -0 "$pid" 2>/dev/null || continue
    command_line="$(tr '\000' ' ' <"$proc_cmdline" 2>/dev/null || true)"
    case " $command_line " in
      *" $LEGACY_CONFIG "*) printf '%s\n' "$pid" ;;
    esac
  done
}
[ ! -f "$JOURNAL" ] || [ ! -f "$STARTING_JOURNAL" ] || {
  echo "smartgate deploy recovery: conflicting pending recovery markers" >&2
  exit 1
}
[ -f "$JOURNAL" ] || [ ! -f "$STARTING_JOURNAL" ] || mv "$STARTING_JOURNAL" "$JOURNAL"
[ -f "$JOURNAL" ] || exit 0

backup=$(jsonfilter -i "$JOURNAL" -e '@.backup' 2>/dev/null || true)
[ -n "$backup" ] && [ -f "$backup" ] || { echo "smartgate deploy recovery: backup is missing" >&2; exit 1; }
tar -tzf "$backup" >/dev/null 2>&1 || { echo "smartgate deploy recovery: backup is invalid" >&2; exit 1; }
work=$(jsonfilter -i "$JOURNAL" -e '@.work' 2>/dev/null || true)
[ -n "$work" ] && [ -x "$work/stage/worker-control" ] || { echo "smartgate deploy recovery: staged Worker control is missing" >&2; exit 1; }
load_legacy_metadata "$work/stage/worker-control" || { echo "smartgate deploy recovery: Worker control metadata is invalid" >&2; exit 1; }

restore=$DEPLOY_ROOT/recovery-$$
rm -rf "$restore"
mkdir -p "$restore"
trap 'rm -rf "$restore"' EXIT HUP INT TERM
tar -xzf "$backup" -C "$restore"
[ -f "$restore/usr/bin/smartgate" ]
[ -f "$restore/etc/init.d/smartgate" ]
[ -d "$restore/etc/smartgate" ]
legacy_directory=$(dirname "$LEGACY_CONFIG")
legacy_archive_directory=${legacy_directory#$SYSTEM_ROOT}
legacy_archive_directory=${legacy_archive_directory#/}
complete_legacy_directory=$(cat "$restore/legacy-directory-complete" 2>/dev/null || true)
if [ -n "$complete_legacy_directory" ]; then
  [ "$complete_legacy_directory" = "$legacy_archive_directory" ] &&
    [ -d "$restore/$legacy_archive_directory" ] || {
    echo "smartgate deploy recovery: complete legacy directory is invalid or missing" >&2
    exit 1
  }
fi

copy_if_present() {
  src=$1
  dst=$2
  if [ -e "$src" ]; then
    mkdir -p "$(dirname "$dst")"
    cp -a "$src" "$dst"
  fi
}

wait_api() {
  attempt=0
  while [ "$attempt" -lt 15 ]; do
    if wget -q -T 5 -O "$restore/healthz" "$API_BASE/healthz"; then
      return 0
    fi
    sleep 1
    attempt=$((attempt + 1))
  done
  return 1
}

validate_control_state() {
  [ -f "$SMARTGATE_SPEC" ] || return 1
  [ -f "$SMARTGATE_APPLIED" ] || return 1
  [ -f "$SMARTGATE_RUNTIME" ] || return 1
  spec_schema=$(jsonfilter -i "$SMARTGATE_SPEC" -e '@.schema_version' 2>/dev/null || true)
  applied_schema=$(jsonfilter -i "$SMARTGATE_APPLIED" -e '@.schema_version' 2>/dev/null || true)
  applied_epoch=$(jsonfilter -i "$SMARTGATE_APPLIED" -e '@.config_epoch' 2>/dev/null || true)
  runtime_epoch=$(jsonfilter -i "$SMARTGATE_RUNTIME" -e '@.config_epoch' 2>/dev/null || true)
  applied_topology=$(jsonfilter -i "$SMARTGATE_APPLIED" -e '@.topology_hash' 2>/dev/null || true)
  runtime_topology=$(jsonfilter -i "$SMARTGATE_RUNTIME" -e '@.topology_hash' 2>/dev/null || true)
  [ -n "$spec_schema" ] || return 1
  [ -n "$applied_schema" ] || return 1
  [ -n "$applied_epoch" ] || return 1
  [ -n "$runtime_epoch" ] || return 1
  [ "$runtime_epoch" = "$applied_epoch" ] || return 1
  [ "$runtime_topology" = "$applied_topology" ] || return 1
}

transactions_empty() {
  set -- "$SMARTGATE_TRANSACTIONS"/*.json
  [ ! -e "$1" ]
}

active_v3_workers() {
  jsonfilter -i "$SMARTGATE_APPLIED" -e '@.execution_graph.pool_bindings[*].target.worker' 2>/dev/null || true
}

configured_v3_workers() {
  jsonfilter -i "$SMARTGATE_APPLIED" -e '@.execution_graph.workers[*].id' 2>/dev/null || true
}

is_v3_schema() {
  schema=$1
  case "$schema" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$schema" -ge 2 ]
}

# Every Worker id V3 may have started. applied-state names none right after a
# startup reconcile (it persists control state only), so the pid files and
# active configs on disk are part of the inventory.
inventory_v3_workers() {
  {
    configured_v3_workers
    active_v3_workers
    for entry in "$V3_WORKER_RUNTIME_DIR"/*.pid "$V3_WORKER_CONFIG_DIR"/*.json; do
      [ -e "$entry" ] || continue
      entry=${entry##*/}
      printf '%s\n' "${entry%.*}"
    done
  } | sort -u
}

# Worker processes are the only processes on the box that run with a config
# under $V3_WORKER_CONFIG_DIR, so a cmdline scan finds them even after their
# pid file is gone.
v3_worker_pids() {
  for cmdline in "$PROC_ROOT"/[0-9]*/cmdline; do
    [ -r "$cmdline" ] || continue
    pid=${cmdline%/cmdline}
    pid=${pid##*/}
    [ "$pid" != "$$" ] || continue
    kill -0 "$pid" 2>/dev/null || continue
    case "$(tr '\000' ' ' <"$cmdline" 2>/dev/null)" in
      *" $V3_WORKER_CONFIG_DIR/"*) printf '%s\n' "$pid" ;;
    esac
  done
}

stop_v3_workers() {
  if [ -x "$WORKER_CONTROL" ]; then
    for worker_id in $(inventory_v3_workers); do
      case "$worker_id" in ''|null) continue ;; esac
      "$WORKER_CONTROL" stop "$worker_id" >/dev/null 2>&1 || true
    done
  fi
  # OpenWrt busybox sleep rejects fractional seconds; poll at 1s.
  for pid in $(v3_worker_pids); do kill "$pid" 2>/dev/null || true; done
  attempt=0
  while [ -n "$(v3_worker_pids)" ] && [ "$attempt" -lt 5 ]; do
    sleep 1
    attempt=$((attempt + 1))
  done
  for pid in $(v3_worker_pids); do kill -KILL "$pid" 2>/dev/null || true; done
  attempt=0
  while [ -n "$(v3_worker_pids)" ] && [ "$attempt" -lt 3 ]; do
    sleep 1
    attempt=$((attempt + 1))
  done
  [ -z "$(v3_worker_pids)" ]
}

# Mirrors the fabric route cleanup of the V3 rescue plan: SmartGate policy
# rules live in pref 11001-15095 with a 0x5??????? mark and point at tables
# 1001-5095 whose routes carry the SmartGate route protocol.
retire_v3_policy_routes() {
  rules=$DEPLOY_ROOT/recovery-policy-rules-$$
  for family in -4 -6; do
    ip "$family" rule show 2>/dev/null | awk '$1 ~ /^[0-9]+:$/ { mark=""; table=""; for (i=1; i<=NF; i++) { if ($i=="fwmark") mark=$(i+1); if ($i=="lookup") table=$(i+1) } if (mark!="" && table!="") { sub(/:$/, "", $1); print $1, mark, table } }' >"$rules"
    while read -r priority mark table; do
      case "$priority:$table" in *[!0-9:]*) continue ;; esac
      [ "$priority" -ge 11001 ] && [ "$priority" -le 15095 ] || continue
      [ "$table" -ge 1001 ] && [ "$table" -le 5095 ] || continue
      case "$mark" in 0x5???????|0x5???????/0xffffffff) ;; *) continue ;; esac
      ip "$family" rule del pref "$priority" fwmark "$mark" table "$table" || { rm -f "$rules"; return 1; }
      ip "$family" route flush table "$table" proto "$V3_FABRIC_ROUTE_PROTO" 2>/dev/null || true
    done <"$rules"
    ip "$family" route show table all proto "$V3_FABRIC_ROUTE_PROTO" 2>/dev/null | awk '{ for (i=1; i<=NF; i++) if ($i=="table") print $(i+1) }' | sort -u >"$rules"
    while read -r table; do
      case "$table" in ''|*[!0-9]*) continue ;; esac
      [ "$table" -ge 1001 ] && [ "$table" -le 5095 ] || continue
      ip "$family" route flush table "$table" proto "$V3_FABRIC_ROUTE_PROTO" 2>/dev/null || true
    done <"$rules"
  done
  rm -f "$rules"
  for family in -4 -6; do
    ! ip "$family" rule show 2>/dev/null | grep -E 'fwmark 0x5[0-9a-f]{7}' >/dev/null || return 1
  done
}

retire_v3_fabric_host_state() {
  if nft list table inet smartgate >/dev/null 2>&1; then
    nft list table inet smartgate | grep -F "$V3_FABRIC_TABLE_OWNER" >/dev/null || return 1
    nft delete table inet smartgate || return 1
  fi
  retire_v3_policy_routes || return 1
  ! nft list table inet smartgate >/dev/null 2>&1
}

# Recovery must leave nothing of the failed candidate behind: a surviving
# Worker keeps the ports the restored release needs and a leftover Gateway
# Fabric keeps hijacking LAN traffic. The staged binary's rescue plan knows
# the complete owner-tagged inventory; run it against scratch state dirs on a
# live system, then sweep the host directly. At boot nothing of the candidate
# is running and the rescue's dnsmasq reload would start services early, so
# the boot hook relies on the sweep alone.
retire_v3_data_plane() {
  if [ "$MODE" != boot-hook ] && [ -x "$work/stage/smartgate" ]; then
    rescue_data=$DEPLOY_ROOT/recovery-rescue-data-$$
    rescue_runtime=$DEPLOY_ROOT/recovery-rescue-runtime-$$
    rm -rf "$rescue_data" "$rescue_runtime"
    mkdir -p "$rescue_data" "$rescue_runtime"
    SMARTGATE_RESCUE_EXECUTE=1 "$work/stage/smartgate" rescue \
      -data-dir "$rescue_data" -runtime-dir "$rescue_runtime" enter >"$work/recovery-rescue.log" 2>&1 ||
      echo "smartgate deploy recovery: staged rescue cleanup failed; see $work/recovery-rescue.log" >&2
    rm -rf "$rescue_data" "$rescue_runtime"
  fi
  retired=0
  stop_v3_workers || { echo "smartgate deploy recovery: V3 Worker processes survived cleanup" >&2; retired=1; }
  retire_v3_fabric_host_state || { echo "smartgate deploy recovery: V3 Gateway Fabric host state survived cleanup" >&2; retired=1; }
  return "$retired"
}

validate_v3_workers() {
  [ -x "$WORKER_CONTROL" ] || return 1
  mode=$1
  case "$mode" in
    off|safe)
      for worker_id in $(configured_v3_workers) $(active_v3_workers); do
        case "$worker_id" in ''|null) continue ;; esac
        if "$WORKER_CONTROL" status "$worker_id" >/dev/null 2>&1; then
          return 1
        fi
      done
      [ -z "$(v3_worker_pids)" ] || return 1
      ;;
    *)
      for worker_id in $(active_v3_workers); do
        case "$worker_id" in
          ''|null) continue ;;
        esac
        "$WORKER_CONTROL" status "$worker_id" >/dev/null 2>&1 || return 1
      done
      ;;
  esac
}

validate_data_plane() {
  mode=$1
  schema=$2
  if is_v3_schema "$schema"; then
    case "$mode" in
      off|safe)
        validate_v3_workers "$mode" || return 1
        ! netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
        ! nft list table inet smartgate >/dev/null 2>&1 || return 1
        ! nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-dualstack-53' >/dev/null || return 1
        ! nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-dualstack-853' >/dev/null || return 1
        [ ! -e "$V3_FABRIC_RECONCILE" ] || return 1
        [ ! -e "$FIREWALL_HOOK" ] || return 1
        [ ! -e "$DNSMASQ_CONFIG" ] || return 1
        ;;
      *)
        validate_v3_workers "$mode" || return 1
        netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
        nft list table inet smartgate >/dev/null 2>&1 || return 1
        nft list ruleset | grep -F 'owner=smartgate:fabric-mark-table:smartgate' >/dev/null || return 1
        nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-dualstack-53' >/dev/null || return 1
        nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-dualstack-853' >/dev/null || return 1
        [ -x "$V3_FABRIC_RECONCILE" ] || return 1
        grep -F 'owner=smartgate:firewall-hook:fabric-reconcile' "$V3_FABRIC_RECONCILE" >/dev/null || return 1
        [ -x "$FIREWALL_HOOK" ] || return 1
        grep -F 'owner=smartgate:firewall-hook:fabric-reconcile' "$FIREWALL_HOOK" >/dev/null || return 1
        grep -F 'owner=smartgate:dnsmasq:smartgate-upstream' "$DNSMASQ_CONFIG" >/dev/null || return 1
        ;;
    esac
    return 0
  fi
  # procd `status` exits 0 for "active with no instances"; use `running`.
  case "$mode" in
    off|safe)
      ! "$LEGACY_INIT" running >/dev/null 2>&1 || return 1
      [ -z "$(legacy_process_pids)" ] || return 1
      ! ip link show dev tun-sb >/dev/null 2>&1 || return 1
      ! nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || return 1
      ! nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-ipv4-853' >/dev/null || return 1
      ;;
    *)
      "$LEGACY_INIT" running >/dev/null 2>&1 || return 1
      ip link show dev tun-sb >/dev/null 2>&1 || return 1
      netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
      nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || return 1
      nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-ipv4-853' >/dev/null || return 1
      [ -f "$DNSMASQ_CONFIG" ] || return 1
      ;;
  esac
}

"$SMARTGATE_INIT" stop >/dev/null 2>&1 || true
attempt=0
while pidof smartgate >/dev/null 2>&1 && [ "$attempt" -lt 20 ]; do
  sleep 1
  attempt=$((attempt + 1))
done
pidof smartgate >/dev/null 2>&1 && { echo "smartgate deploy recovery: current daemon did not stop" >&2; exit 1; }
# Retire the candidate data plane before its pid files and configs are deleted;
# the Worker inventory needs them. Leftovers are reported here and caught by
# the data-plane gate below.
retire_v3_data_plane || echo "smartgate deploy recovery: WARNING: V3 data plane was not fully retired" >&2
rm -rf "$SYSTEM_ROOT/tmp/smartgate/workers" "$SYSTEM_ROOT/tmp/smartgate/worker-runtime"
rm -rf "$SMARTGATE_BIN" "$SMARTGATE_INIT" "$SYSTEM_ROOT/www/smartgate" "$SMARTGATE_ETC" \
  "$LEGACY_CONFIG" "$LEGACY_CONFIG_OWNER" "$LEGACY_SERVICE_OWNER" "$FIREWALL_HOOK" "$DNSMASQ_CONFIG" "$INSTALLED_RECOVERY" "$WORKER_CONTROL"
copy_if_present "$restore/usr/bin/smartgate" "$SMARTGATE_BIN"
copy_if_present "$restore/etc/init.d/smartgate" "$SMARTGATE_INIT"
copy_if_present "$restore/www/smartgate" "$SYSTEM_ROOT/www/smartgate"
copy_if_present "$restore/etc/smartgate" "$SMARTGATE_ETC"
if [ -n "$complete_legacy_directory" ]; then
  rm -rf "$legacy_directory"
  copy_if_present "$restore/$legacy_archive_directory" "$legacy_directory"
  copy_if_present "$restore${LEGACY_INIT#$SYSTEM_ROOT}" "$LEGACY_INIT"
  legacy_uci="/etc/config/$(basename "$LEGACY_INIT")"
  copy_if_present "$restore$legacy_uci" "$SYSTEM_ROOT$legacy_uci"
fi
copy_if_present "$restore${LEGACY_CONFIG#$SYSTEM_ROOT}" "$LEGACY_CONFIG"
copy_if_present "$restore${LEGACY_CONFIG_OWNER#$SYSTEM_ROOT}" "$LEGACY_CONFIG_OWNER"
copy_if_present "$restore${LEGACY_SERVICE_OWNER#$SYSTEM_ROOT}" "$LEGACY_SERVICE_OWNER"
copy_if_present "$restore/etc/hotplug.d/firewall/95-smartgate" "$FIREWALL_HOOK"
copy_if_present "$restore/etc/dnsmasq.d/smartgate.conf" "$DNSMASQ_CONFIG"
if [ -f "$FIREWALL_WRAPPER" ] && grep -Eq 'SmartGate V2|SmartGate fw4 lifecycle bridge' "$FIREWALL_WRAPPER"; then
  rm -f "$FIREWALL_WRAPPER"
fi
copy_if_present "$restore/etc/firewall.smartgate-reconcile" "$FIREWALL_WRAPPER"
copy_if_present "$restore/etc/config/firewall" "$SYSTEM_ROOT/etc/config/firewall"
copy_if_present "$restore/usr/lib/smartgate/deploy-recover" "$INSTALLED_RECOVERY"
copy_if_present "$restore/usr/libexec/smartgate-worker-control" "$WORKER_CONTROL"
chmod 0755 "$SMARTGATE_BIN" "$SMARTGATE_INIT"
[ ! -e "$WORKER_CONTROL" ] || chmod 0755 "$WORKER_CONTROL"
if [ -f "$work/payload.sha256" ]; then
  (cd "${SYSTEM_ROOT:-/}" && sha256sum -c "$work/payload.sha256") >/dev/null || {
    echo "smartgate deploy recovery: restored payload checksum verification failed" >&2
    exit 1
  }
  echo "smartgate deploy recovery: restored payload checksums verified" >&2
fi
"$SMARTGATE_BIN" locks >/dev/null

old_enabled=$(cat "$restore/smartgate-enabled" 2>/dev/null || echo no)
if [ "$old_enabled" = yes ]; then "$SMARTGATE_INIT" enable; else "$SMARTGATE_INIT" disable; fi
"$FIREWALL_INIT" reload >/dev/null 2>&1
"$DNSMASQ_INIT" reload >/dev/null 2>&1
old_schema=$(cat "$restore/old-schema" 2>/dev/null || true)
[ -n "$old_schema" ] || old_schema=$(jsonfilter -i "$SMARTGATE_APPLIED" -e '@.schema_version' 2>/dev/null || true)
case "$old_schema" in
  ''|*[!0-9]*) old_schema=1 ;;
esac
old_mode=$(cat "$restore/old-mode" 2>/dev/null || echo off)
if is_v3_schema "$old_schema"; then
  stop_v3_workers
else
  if [ "$(cat "$restore/legacy-enabled" 2>/dev/null || echo no)" = yes ]; then
    "$LEGACY_INIT" enable >/dev/null 2>&1 || true
  fi
  case "$old_mode" in
    off|safe)
      "$LEGACY_INIT" stop >/dev/null 2>&1 || true
      ;;
    *)
      "$LEGACY_INIT" restart >/dev/null 2>&1
      attempt=0
      while [ "$attempt" -lt 15 ]; do
        "$LEGACY_INIT" running >/dev/null 2>&1 && ip link show dev tun-sb >/dev/null 2>&1 && netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null && break
        sleep 1; attempt=$((attempt + 1))
      done
      ;;
  esac
fi

SMARTGATE_DEPLOY_RECOVERY_ACTIVE=1
export SMARTGATE_DEPLOY_RECOVERY_ACTIVE
# Older rollback targets do not know the recovery-active guard. Hide the
# journal only for the init start call so they cannot recursively invoke this
# helper; pending.starting.json is itself a durable recovery marker and is
# restored on the next invocation after a crash.
mv "$JOURNAL" "$STARTING_JOURNAL"
sync
if ! "$SMARTGATE_INIT" start >/dev/null 2>&1; then
  mv "$STARTING_JOURNAL" "$JOURNAL"
  sync
  echo "smartgate deploy recovery: restored daemon failed to start" >&2
  exit 1
fi
mv "$STARTING_JOURNAL" "$JOURNAL"
sync
pidof smartgate >/dev/null 2>&1 || { echo "smartgate deploy recovery: restored daemon did not start" >&2; exit 1; }
wait_api || { echo "smartgate deploy recovery: restored daemon API is unhealthy" >&2; exit 1; }
expected_commit=$(jsonfilter -i "$SMARTGATE_ETC/version.json" -e '@.commit' 2>/dev/null || true)
running_version=$("$SMARTGATE_BIN" version 2>/dev/null || true)
if [ -n "$expected_commit" ]; then
  echo "$running_version" | grep -F "commit=$expected_commit" >/dev/null || {
    echo "smartgate deploy recovery: restored daemon version mismatch" >&2
    exit 1
  }
fi
validate_control_state || { echo "smartgate deploy recovery: restored control state is inconsistent" >&2; exit 1; }
transactions_empty || { echo "smartgate deploy recovery: durable transaction journal remains" >&2; exit 1; }
# fw4 reload does not invoke the V2 hotplug hook. Replay the restored, owned
# hook after startup so its DNS/DoT rules exist before accepting recovery.
if ! is_v3_schema "$old_schema"; then
  case "$old_mode" in
    off|safe) ;;
    *)
      [ -x "$FIREWALL_HOOK" ] &&
        grep -F 'owner=smartgate:firewall-hook:fw4-reconcile' "$FIREWALL_HOOK" >/dev/null &&
        "$FIREWALL_HOOK" >"$work/recovery-firewall.log" 2>&1 || {
        echo "smartgate deploy recovery: restored V2 firewall hook failed" >&2
        exit 1
      }
      ;;
  esac
fi
validate_data_plane "$old_mode" "$old_schema" || { echo "smartgate deploy recovery: restored data plane is unhealthy" >&2; exit 1; }

rm -f "$GATE"
sync
[ "$MODE" != boot-hook ] || : >"$RECOVERED_RUNNING"
sync
rm -f "$JOURNAL"
rm -f "$BOOT_HOOK"
rm -f "$DEPLOY_LOCK/owner"
rmdir "$DEPLOY_LOCK" 2>/dev/null || true
sync
echo "smartgate deploy recovery: restored $backup" >&2
