#!/bin/sh
set -eu

# Transactional OpenWrt deployment with quiesced backup, controlled reconcile,
# health gates, and automatic rollback. Run from an extracted release.

ROOT=${SMARTGATE_RELEASE_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
NEW_BINARY=${SMARTGATE_BINARY:-$ROOT/smartgate}
NEW_INIT=${SMARTGATE_INIT_SCRIPT:-$ROOT/configs/init.d/smartgate}
NEW_WWW=${SMARTGATE_WWW_SRC:-$ROOT/www/smartgate}
NEW_VERSION=${SMARTGATE_VERSION_SRC:-$ROOT/version.json}
NEW_RECOVERY=${SMARTGATE_RECOVERY_SRC:-$ROOT/scripts/deploy-recover.sh}
NEW_WORKER_CONTROL=${SMARTGATE_WORKER_CONTROL_SRC:-$ROOT/libexec/smartgate-worker-control}
NEW_MANIFEST=${SMARTGATE_MANIFEST:-$ROOT/SHA256SUMS}
API_BASE=${SMARTGATE_API_BASE:-http://192.168.1.1:3000}
AUTH_HEADER=${SMARTGATE_AUTH_HEADER:-}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
DEPLOY_ROOT=${SMARTGATE_DEPLOY_ROOT:-/root/smartgate-deployments}
WORK=$DEPLOY_ROOT/work-$STAMP-$$
BACKUP=$DEPLOY_ROOT/before-$STAMP.tar.gz
JOURNAL=$DEPLOY_ROOT/pending.json
STARTING_JOURNAL=$DEPLOY_ROOT/pending.starting.json
DEPLOY_LOCK=$DEPLOY_ROOT/lock
LOCK_OWNER=$DEPLOY_LOCK/owner
GATE=/tmp/smartgate/deploy-scheduler-disabled
APPLIED_STATE=/etc/smartgate/applied-state.json
STATE_DIR=/etc/smartgate
RUNTIME_DIR=/tmp/smartgate
V3_FABRIC_RECONCILE=/etc/smartgate/fabric-reconcile.sh
V3_WORKER_CONTROL=/usr/libexec/smartgate-worker-control
V3_WORKER_CONFIG_DIR=/etc/smartgate/workers
V3_WORKER_RUNTIME_DIR=/tmp/smartgate/workers
V3_FABRIC_TABLE_OWNER='owner=smartgate:fabric-mark-table:smartgate'
V3_FABRIC_ROUTE_PROTO=186
PROC_ROOT=${SMARTGATE_PROC_ROOT:-/proc}
LEGACY_INIT=
LEGACY_PROCESS=
LEGACY_CONFIG=
LEGACY_CONFIG_OWNER=
LEGACY_SERVICE_OWNER=
SUCCESS=0
STOPPED=0
BACKUP_CREATED=0
LOCK_ACQUIRED=0
PRESERVE_LOCK=0

fail() { echo "deploy-safe: $*" >&2; exit 1; }

recovery_markers_absent() {
  [ ! -e "$JOURNAL" ] && [ ! -e "$STARTING_JOURNAL" ]
}

fail_pending_recovery() {
  [ "$LOCK_ACQUIRED" -ne 1 ] || PRESERVE_LOCK=1
  fail "pending deployment recovery exists; run $DEPLOY_ROOT/deploy-recover first"
}

copy_if_present() {
  src=$1
  dst=$2
  if [ -e "$src" ]; then
    mkdir -p "$(dirname "$dst")"
    cp -a "$src" "$dst"
  fi
}

load_legacy_metadata() {
  metadata_control=$1
  metadata=$("$metadata_control" legacy-metadata) || return 1
  set -- $metadata
  [ "$#" -eq 5 ] || return 1
  LEGACY_INIT=$1
  LEGACY_PROCESS=$2
  LEGACY_CONFIG=$3
  LEGACY_CONFIG_OWNER=$4
  LEGACY_SERVICE_OWNER=$5
}

# A router may run another client with the same binary name (for example a
# diagnostic or health-check client) alongside the V2 data plane. Identify the legacy
# process by the SmartGate-owned config it runs, not by the executable name
# alone. The old process-name check mistook unrelated clients
# for the V2 data plane and made an otherwise safe upgrade roll back.
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

# uclient-fetch (OpenWrt wget) prints "HTTP error <status>" only without -q
# and never writes a non-2xx body, so each call keeps its own diagnostics in
# <output>.log; api_failure surfaces the last line when a gate fails.
api_get() {
  path=$1
  output=$2
  wget -T 5 --header="$AUTH_HEADER" -O "$output" "$API_BASE$path" >"$output.log" 2>&1
}

api_post() {
  path=$1
  body=$2
  output=$3
  wget -T 45 --header="$AUTH_HEADER" --header='Content-Type: application/json' --post-data="$body" -O "$output" "$API_BASE$path" >"$output.log" 2>&1
}

api_failure() {
  tail -n 1 "$1.log" 2>/dev/null || true
}

transactions_empty() {
  set -- /etc/smartgate/transactions/*.json
  [ ! -e "$1" ]
}

applied_schema() {
  schema=$(jsonfilter -i "$APPLIED_STATE" -e '@.schema_version' 2>/dev/null || true)
  case "$schema" in
    ''|*[!0-9]*) echo 1 ;;
    *) echo "$schema" ;;
  esac
}

is_v3_schema() {
  schema=$1
  case "$schema" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$schema" -ge 2 ]
}

is_v3_state() {
  is_v3_schema "$(applied_schema)"
}

active_v3_workers() {
  jsonfilter -i "$APPLIED_STATE" -e '@.execution_graph.pool_bindings[*].target.worker' 2>/dev/null || true
}

configured_v3_workers() {
  jsonfilter -i "$APPLIED_STATE" -e '@.execution_graph.workers[*].id' 2>/dev/null || true
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
  if [ -x "$V3_WORKER_CONTROL" ]; then
    for worker_id in $(inventory_v3_workers); do
      case "$worker_id" in ''|null) continue ;; esac
      "$V3_WORKER_CONTROL" stop "$worker_id" >/dev/null 2>&1 || true
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

wait_daemon_stopped() {
  attempt=0
  while pidof smartgate >/dev/null 2>&1 && [ "$attempt" -lt 20 ]; do
    sleep 1
    attempt=$((attempt + 1))
  done
  ! pidof smartgate >/dev/null 2>&1
}

# Mirrors the fabric route cleanup of the V3 rescue plan: SmartGate policy
# rules live in pref 11001-15095 with a 0x5??????? mark and point at tables
# 1001-5095 whose routes carry the SmartGate route protocol.
retire_v3_policy_routes() {
  rules=$WORK/rollback-policy-rules
  for family in -4 -6; do
    ip "$family" rule show 2>/dev/null | awk '$1 ~ /^[0-9]+:$/ { mark=""; table=""; for (i=1; i<=NF; i++) { if ($i=="fwmark") mark=$(i+1); if ($i=="lookup") table=$(i+1) } if (mark!="" && table!="") { sub(/:$/, "", $1); print $1, mark, table } }' >"$rules"
    while read -r priority mark table; do
      case "$priority:$table" in *[!0-9:]*) continue ;; esac
      [ "$priority" -ge 11001 ] && [ "$priority" -le 15095 ] || continue
      [ "$table" -ge 1001 ] && [ "$table" -le 5095 ] || continue
      case "$mark" in 0x5???????|0x5???????/0xffffffff) ;; *) continue ;; esac
      ip "$family" rule del pref "$priority" fwmark "$mark" table "$table" || return 1
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

# Rollback must leave nothing of the failed candidate behind: a surviving
# Worker keeps the ports the restored release needs and a leftover Gateway
# Fabric keeps hijacking LAN traffic. The staged binary's rescue plan knows
# the complete owner-tagged inventory, so run it against scratch state dirs
# (as legacy quiesce does), then sweep the host directly in case the staged
# binary or a partial rescue left something running.
retire_v3_data_plane() {
  if [ -x "$WORK/stage/smartgate" ]; then
    rescue_data=$WORK/rollback-rescue-data
    rescue_runtime=$WORK/rollback-rescue-runtime
    rm -rf "$rescue_data" "$rescue_runtime"
    mkdir -p "$rescue_data" "$rescue_runtime"
    SMARTGATE_RESCUE_EXECUTE=1 "$WORK/stage/smartgate" rescue \
      -data-dir "$rescue_data" -runtime-dir "$rescue_runtime" enter >"$WORK/rollback-rescue.log" 2>&1 ||
      echo "deploy-safe: staged rescue cleanup failed during rollback; see $WORK/rollback-rescue.log" >&2
    rm -rf "$rescue_data" "$rescue_runtime"
  fi
  retired=0
  stop_v3_workers || { echo "deploy-safe: V3 Worker processes survived rollback cleanup" >&2; retired=1; }
  retire_v3_fabric_host_state || { echo "deploy-safe: V3 Gateway Fabric host state survived rollback cleanup" >&2; retired=1; }
  return "$retired"
}

capture_failure_diagnostics() {
  [ -d "$WORK" ] || return 0
  {
    echo "## captured_at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "## daemon pids"; pidof smartgate 2>/dev/null || echo none
    echo "## procd service"; ubus call service list '{"name":"smartgate"}' 2>&1 || true
    echo "## listeners"; netstat -ln 2>/dev/null || true
    echo "## worker processes"
    for pid in $(v3_worker_pids); do
      tr '\000' ' ' <"$PROC_ROOT/$pid/cmdline" 2>/dev/null || true
      echo "[pid $pid]"
    done
    echo "## processes"; ps w 2>/dev/null || true
    echo "## nft tables"; nft list tables 2>&1 || true
    echo "## ip rule"; ip rule 2>&1 || true
    echo "## transactions"; ls -la /etc/smartgate/transactions 2>&1 || true
    echo "## logread tail"; logread 2>/dev/null | tail -n 300 || true
  } >"$WORK/failure-diagnostics.txt" 2>&1 || true
}

secure_state_permissions() {
  [ -d "$STATE_DIR" ] || return 0
  chmod 0700 "$STATE_DIR" 2>/dev/null || return 1
  [ ! -d "$RUNTIME_DIR" ] || chmod 0700 "$RUNTIME_DIR" 2>/dev/null || return 1
  for dir in "$STATE_DIR/snapshots" "$STATE_DIR/backups" "$STATE_DIR/lastgood" "$STATE_DIR/transactions" "$STATE_DIR/rule-sets" "$RUNTIME_DIR/rule-sets" "$STATE_DIR/workers"; do
    [ -d "$dir" ] || continue
    chmod 0700 "$dir" 2>/dev/null || return 1
  done
  find "$STATE_DIR" -type f \( -name '*.json' -o -name '*.status' -o -name '*.tmp' -o -name '*.tar.gz' \) -exec chmod 0600 {} + 2>/dev/null || return 1
  [ ! -d "$RUNTIME_DIR" ] || find "$RUNTIME_DIR" -type f \( -name '*.json' -o -name '*.status' -o -name '*.tmp' \) -exec chmod 0600 {} + 2>/dev/null || return 1
  return 0
}

validate_v3_workers() {
  [ -x "$V3_WORKER_CONTROL" ] || return 1
  mode=$1
  case "$mode" in
    off|safe)
      for worker_id in $(configured_v3_workers) $(active_v3_workers); do
        case "$worker_id" in ''|null) continue ;; esac
        if "$V3_WORKER_CONTROL" status "$worker_id" >/dev/null 2>&1; then
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
        "$V3_WORKER_CONTROL" status "$worker_id" >/dev/null 2>&1 || return 1
      done
      ;;
  esac
}

# legacy_migration_resolved passes when the runtime status has no
# legacy_migration report (nothing to migrate) or its unresolved list is empty.
legacy_migration_resolved() {
  grep -F '"legacy_migration"' "$1" >/dev/null 2>&1 || return 0
  grep -F '"unresolved":[]' "$1" >/dev/null 2>&1 && return 0
  grep -F '"unresolved":null' "$1" >/dev/null 2>&1
}

legacy_migration_summary() {
  sed -nE 's/.*"legacy_migration":(\{[^}]*\}).*/\1/p' "$1" 2>/dev/null | head -c 300
}

wait_api() {
  suffix=$1
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    attempt=$((attempt + 1))
    if api_get /healthz "$WORK/healthz-$suffix" &&
       api_get /api/version "$WORK/version-$suffix" &&
       api_get /api/runtime/status "$WORK/runtime-$suffix" &&
       api_get /api/config/state "$WORK/config-$suffix"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

restore_owner_rules() {
  dns_owner='owner=smartgate:dns-hijack:lan-ipv4-53'
  dot_owner='owner=smartgate:dot-block:lan-ipv4-853'
  if nft list chain inet fw4 dstnat_lan >/dev/null 2>&1; then
    nft add rule inet fw4 dstnat_lan udp dport 53 redirect to :1053 comment "\"$dns_owner\"" || return 1
    nft add rule inet fw4 dstnat_lan tcp dport 53 redirect to :1053 comment "\"$dns_owner\"" || return 1
  else
    lan_device=$(ubus call network.interface.lan status 2>/dev/null | jsonfilter -e '@.l3_device' 2>/dev/null || true)
    [ -n "$lan_device" ] || lan_device=br-lan
    nft add rule inet fw4 dstnat iifname "$lan_device" udp dport 53 redirect to :1053 comment "\"$dns_owner\"" || return 1
    nft add rule inet fw4 dstnat iifname "$lan_device" tcp dport 53 redirect to :1053 comment "\"$dns_owner\"" || return 1
  fi
  nft add rule inet fw4 forward_lan tcp dport 853 reject comment "\"$dot_owner\"" || return 1
  nft add rule inet fw4 forward_lan udp dport 853 reject comment "\"$dot_owner\"" || return 1
}

restore_v3_fabric() {
  [ -x "$V3_FABRIC_RECONCILE" ] || return 1
  "$V3_FABRIC_RECONCILE" >/dev/null 2>&1
}

quiesce_legacy_data_plane() {
  old_schema=$1
  is_v3_schema "$old_schema" && return 0

  "$LEGACY_INIT" stop >/dev/null 2>&1 || return 1
  attempt=0
  while [ -n "$(legacy_process_pids)" ] && [ "$attempt" -lt 20 ]; do
    sleep 1
    attempt=$((attempt + 1))
  done
  [ -z "$(legacy_process_pids)" ] || return 1

  rescue_data=$WORK/legacy-quiesce-data
  rescue_runtime=$WORK/legacy-quiesce-runtime
  rescue_log=$WORK/legacy-quiesce.log
  rm -rf "$rescue_data" "$rescue_runtime"
  mkdir -p "$rescue_data" "$rescue_runtime"
  if ! SMARTGATE_RESCUE_EXECUTE=1 "$WORK/stage/smartgate" rescue \
    -data-dir "$rescue_data" -runtime-dir "$rescue_runtime" enter >"$rescue_log" 2>&1; then
    cat "$rescue_log" >&2
    return 1
  fi
  # The V3 rescue plan knows nothing about the V2 core config file. Retire it
  # here, but only when the V2 owner marker proves SmartGate wrote it, and keep
  # the stock service from resurrecting it at boot. The backup holds both files
  # and legacy-enabled, so rollback restores them.
  if [ -f "$LEGACY_CONFIG_OWNER" ] && grep -q '^owner=smartgate:' "$LEGACY_CONFIG_OWNER"; then
    rm -f "$LEGACY_CONFIG" "$LEGACY_CONFIG_OWNER" || return 1
  fi
  if [ -f "$LEGACY_SERVICE_OWNER" ] && grep -q '^owner=smartgate:' "$LEGACY_SERVICE_OWNER"; then
    rm -f "$LEGACY_SERVICE_OWNER" || return 1
  fi
  "$LEGACY_INIT" disable >/dev/null 2>&1 || true
  [ -z "$(legacy_process_pids)" ] || return 1
  ! netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
  ! ip link show dev tun-sb >/dev/null 2>&1 || return 1
  ! nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || return 1
  ! nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-ipv4-853' >/dev/null || return 1
  [ ! -e "$LEGACY_CONFIG_OWNER" ] || return 1
  [ ! -e "$LEGACY_SERVICE_OWNER" ] || return 1
  [ ! -e /etc/hotplug.d/firewall/95-smartgate ] || return 1
  [ ! -e /etc/dnsmasq.d/smartgate.conf ] || return 1
  rm -f "$rescue_data/smartgate-rescue.status" "$rescue_runtime/smartgate-rescue.status"
  rm -rf "$rescue_data" "$rescue_runtime"
}

validate_data_plane() {
  mode=${1:-$(jsonfilter -i "$APPLIED_STATE" -e '@.gateway_mode' 2>/dev/null || echo off)}
  if is_v3_state; then
    case "$mode" in
      off|safe)
        validate_v3_workers "$mode" || return 1
        ! netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
        ! nft list table inet smartgate >/dev/null 2>&1 || return 1
        ! nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-dualstack-53' >/dev/null || return 1
        ! nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-dualstack-853' >/dev/null || return 1
        [ ! -e "$V3_FABRIC_RECONCILE" ] || return 1
        [ ! -e /etc/hotplug.d/firewall/95-smartgate ] || return 1
        [ ! -e /etc/dnsmasq.d/smartgate.conf ] || return 1
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
        [ -x /etc/hotplug.d/firewall/95-smartgate ] || return 1
        grep -F 'owner=smartgate:firewall-hook:fabric-reconcile' /etc/hotplug.d/firewall/95-smartgate >/dev/null || return 1
        grep -F 'owner=smartgate:dnsmasq:smartgate-upstream' /etc/dnsmasq.d/smartgate.conf >/dev/null || return 1
        ;;
    esac
    return 0
  fi
  # procd `status` exits 0 for "active with no instances" (service enabled at
  # boot, no config); `running` is true only when an instance is really up.
  case "$mode" in
    off|safe)
      ! "$LEGACY_INIT" running >/dev/null 2>&1 || return 1
      [ -z "$(legacy_process_pids)" ] || return 1
      ! ip link show dev tun-sb >/dev/null 2>&1 || return 1
      ! nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || return 1
      ! nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-ipv4-853' >/dev/null || return 1
      [ ! -e /etc/hotplug.d/firewall/95-smartgate ] || return 1
      [ ! -e /etc/dnsmasq.d/smartgate.conf ] || return 1
      ;;
    *)
      "$LEGACY_INIT" running >/dev/null 2>&1 || return 1
      ip link show dev tun-sb >/dev/null 2>&1 || return 1
      ip -6 addr show dev tun-sb | grep -F 'fdfe:dcba:9876::1' >/dev/null || return 1
      netstat -ln 2>/dev/null | grep -F ':1053' >/dev/null || return 1
      grep -F 'smartgate_acquire_lock' /etc/hotplug.d/firewall/95-smartgate >/dev/null || return 1
      grep -F 'owner=smartgate:dnsmasq:wan-cpe-upstream' /etc/dnsmasq.d/smartgate.conf >/dev/null || return 1
      nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || return 1
      nft list ruleset | grep -F 'owner=smartgate:dot-block:lan-ipv4-853' >/dev/null || return 1
      ;;
  esac
}

validate_old_data_plane() {
  old_mode=${1:-$(jsonfilter -i /etc/smartgate/applied-state.json -e '@.gateway_mode' 2>/dev/null || echo off)}
  validate_data_plane "$old_mode"
}

restore_backup() {
  echo "deploy-safe: gate failed; restoring $BACKUP" >&2
  set +e
  rm -rf "$WORK/restore" || return 1
  mkdir -p "$WORK/restore" || return 1
  tar -tzf "$BACKUP" >/dev/null 2>&1 || { echo "deploy-safe: rollback archive validation failed" >&2; return 1; }
  tar -xzf "$BACKUP" -C "$WORK/restore" || return 1
  [ -f "$WORK/restore/usr/bin/smartgate" ] || return 1
  [ -f "$WORK/restore/etc/init.d/smartgate" ] || return 1
  [ -d "$WORK/restore/etc/smartgate" ] || return 1

  /etc/init.d/smartgate stop >/dev/null 2>&1 || return 1
  # The candidate daemon supervises its Workers and Gateway Fabric; retire the
  # data plane only after it is gone, and before its pid files and configs are
  # deleted (the Worker inventory needs them).
  wait_daemon_stopped || echo "deploy-safe: candidate daemon did not stop within 20s; continuing rollback" >&2
  retire_v3_data_plane || echo "deploy-safe: WARNING: V3 data plane was not fully retired; rollback validation decides" >&2
  rm -f "$GATE" || return 1
  rm -rf /tmp/smartgate/workers /tmp/smartgate/worker-runtime || return 1
  rm -rf /usr/bin/smartgate /etc/init.d/smartgate /www/smartgate /etc/smartgate "$LEGACY_CONFIG" "$LEGACY_CONFIG_OWNER" "$LEGACY_SERVICE_OWNER" /etc/hotplug.d/firewall/95-smartgate /etc/dnsmasq.d/smartgate.conf /usr/lib/smartgate/deploy-recover /usr/libexec/smartgate-worker-control || return 1
  copy_if_present "$WORK/restore/usr/bin/smartgate" /usr/bin/smartgate || return 1
  copy_if_present "$WORK/restore/etc/init.d/smartgate" /etc/init.d/smartgate || return 1
  copy_if_present "$WORK/restore/www/smartgate" /www/smartgate || return 1
  copy_if_present "$WORK/restore/etc/smartgate" /etc/smartgate || return 1
  copy_if_present "$WORK/restore$LEGACY_CONFIG" "$LEGACY_CONFIG" || return 1
  copy_if_present "$WORK/restore$LEGACY_CONFIG_OWNER" "$LEGACY_CONFIG_OWNER" || return 1
  copy_if_present "$WORK/restore$LEGACY_SERVICE_OWNER" "$LEGACY_SERVICE_OWNER" || return 1
  copy_if_present "$WORK/restore/etc/hotplug.d/firewall/95-smartgate" /etc/hotplug.d/firewall/95-smartgate || return 1
  copy_if_present "$WORK/restore/etc/dnsmasq.d/smartgate.conf" /etc/dnsmasq.d/smartgate.conf || return 1
  if [ -f /etc/firewall.smartgate-reconcile ] && grep -Eq 'SmartGate V2|SmartGate fw4 lifecycle bridge' /etc/firewall.smartgate-reconcile; then
    rm -f /etc/firewall.smartgate-reconcile || return 1
  fi
  copy_if_present "$WORK/restore/etc/firewall.smartgate-reconcile" /etc/firewall.smartgate-reconcile || return 1
  copy_if_present "$WORK/restore/etc/config/firewall" /etc/config/firewall || return 1
  copy_if_present "$WORK/restore/usr/lib/smartgate/deploy-recover" /usr/lib/smartgate/deploy-recover || return 1
  copy_if_present "$WORK/restore/usr/libexec/smartgate-worker-control" /usr/libexec/smartgate-worker-control || return 1
  chmod 0755 /usr/bin/smartgate /etc/init.d/smartgate 2>/dev/null || return 1
  [ ! -e /usr/libexec/smartgate-worker-control ] || chmod 0755 /usr/libexec/smartgate-worker-control || return 1

  /etc/init.d/firewall reload >/dev/null 2>&1 || return 1
  /etc/init.d/dnsmasq reload >/dev/null 2>&1 || return 1
  restored_schema=$(cat "$WORK/restore/old-schema" 2>/dev/null || echo 1)
  case "$restored_schema" in
    ''|*[!0-9]*) restored_schema=1 ;;
  esac
  old_mode=$(cat "$WORK/restore/old-mode" 2>/dev/null || echo off)
  if is_v3_schema "$restored_schema"; then
    stop_v3_workers
  else
    if [ "$(cat "$WORK/restore/legacy-enabled" 2>/dev/null || echo no)" = yes ]; then
      "$LEGACY_INIT" enable >/dev/null 2>&1 || return 1
    fi
    case "$old_mode" in
      off|safe) "$LEGACY_INIT" stop >/dev/null 2>&1 || return 1 ;;
      *)
        "$LEGACY_INIT" restart >/dev/null 2>&1 || return 1
        sleep 2
        nft list ruleset | grep -F 'owner=smartgate:dns-hijack:lan-ipv4-53' >/dev/null || restore_owner_rules || return 1
        ;;
    esac
  fi
  old_enabled=$(cat "$WORK/restore/smartgate-enabled" 2>/dev/null || echo no)
  if [ "$old_enabled" = yes ]; then /etc/init.d/smartgate enable >/dev/null 2>&1 || return 1; else /etc/init.d/smartgate disable >/dev/null 2>&1 || return 1; fi
  /etc/init.d/smartgate restart >/dev/null 2>&1 || return 1
  wait_api rollback || return 1
  validate_old_data_plane "$old_mode" || return 1
  sync
  rm -f "$JOURNAL" /etc/rc.d/S01smartgate-deploy-recover || return 1
  sync
  set -e
  return 0
}

cleanup() {
  code=$?
  trap - EXIT HUP INT TERM
  if [ "$SUCCESS" -ne 1 ]; then
    capture_failure_diagnostics || true
    if [ "$BACKUP_CREATED" -eq 1 ] && [ -f "$BACKUP" ]; then
      restore_backup || echo "deploy-safe: AUTOMATIC ROLLBACK FAILED; use $BACKUP manually" >&2
    elif [ "$STOPPED" -eq 1 ]; then
      rm -f "$GATE"
      /etc/init.d/smartgate start >/dev/null 2>&1 || true
    else
      rm -f "$GATE"
    fi
  fi
  [ "$SUCCESS" -eq 1 ] || true
  if [ "$LOCK_ACQUIRED" -eq 1 ] && [ "$PRESERVE_LOCK" -ne 1 ]; then
    rm -f "$LOCK_OWNER"
    rmdir "$DEPLOY_LOCK" 2>/dev/null || true
  fi
  if [ "$SUCCESS" -ne 1 ] && [ "$BACKUP_CREATED" -eq 0 ] && recovery_markers_absent; then
    rm -f "$BACKUP.tmp" "$BACKUP" 2>/dev/null || true
    rm -rf "$WORK" 2>/dev/null || true
  fi
  exit "$code"
}

# Test hook: source the script for its functions without running a deployment.
if [ "${SMARTGATE_DEPLOY_SAFE_FUNCTIONS_ONLY:-0}" = 1 ]; then
  return 0 2>/dev/null || exit 0
fi
trap cleanup EXIT HUP INT TERM

mkdir -p "$DEPLOY_ROOT"
recovery_markers_absent || fail_pending_recovery
if mkdir "$DEPLOY_LOCK" 2>/dev/null; then
  LOCK_ACQUIRED=1
else
  owner_pid=$(sed -n '1p' "$LOCK_OWNER" 2>/dev/null || true)
  owner_boot=$(sed -n '2p' "$LOCK_OWNER" 2>/dev/null || true)
  owner_start=$(sed -n '3p' "$LOCK_OWNER" 2>/dev/null || true)
  current_boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
  current_start=$(awk '{print $22}' "/proc/$owner_pid/stat" 2>/dev/null || true)
  current_state=$(awk '{print $3}' "/proc/$owner_pid/stat" 2>/dev/null || true)
  if [ -n "$owner_pid" ] && [ "$owner_boot" = "$current_boot" ] && [ -n "$owner_start" ] && [ "$owner_start" = "$current_start" ] && [ "$current_state" != Z ] && kill -0 "$owner_pid" 2>/dev/null; then
    fail "another deployment is active with pid $owner_pid"
  fi
  recovery_markers_absent || fail_pending_recovery
  rm -rf "$DEPLOY_LOCK"
  mkdir "$DEPLOY_LOCK" || fail "could not acquire deployment lock"
  LOCK_ACQUIRED=1
fi
printf '%s\n%s\n%s\n' "$$" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)" "$(awk '{print $22}' /proc/$$/stat 2>/dev/null || true)" >"$LOCK_OWNER"
recovery_markers_absent || fail_pending_recovery

[ -f "$NEW_BINARY" ] || fail "new binary missing: $NEW_BINARY"
[ -f "$NEW_INIT" ] || fail "new init script missing: $NEW_INIT"
[ -f "$NEW_WWW/index.html" ] || fail "new frontend missing: $NEW_WWW/index.html"
[ -f "$NEW_VERSION" ] || fail "new version metadata missing: $NEW_VERSION"
[ -f "$NEW_RECOVERY" ] || fail "deployment recovery helper missing: $NEW_RECOVERY"
[ -f "$NEW_WORKER_CONTROL" ] || fail "Worker control helper missing: $NEW_WORKER_CONTROL"
[ -f "$NEW_MANIFEST" ] || fail "release hash manifest missing: $NEW_MANIFEST"
[ -n "$AUTH_HEADER" ] || fail "SMARTGATE_AUTH_HEADER is required (example: 'Authorization: Bearer <token>')"
(cd "$ROOT" && sha256sum -c "$(basename "$NEW_MANIFEST")") >/dev/null || fail "release payload hash verification failed"
recovery_markers_absent || fail_pending_recovery
chmod 0755 "$NEW_BINARY"
"$NEW_BINARY" locks >/dev/null
load_legacy_metadata "$NEW_WORKER_CONTROL" || fail "Worker control metadata is invalid"

# Preflight downloads require $WORK to exist before wait_api writes health/version/config snapshots.
mkdir -p "$WORK" || fail "could not create deployment work directory"

wait_api preflight || fail "current service/API is not healthy"
if [ "${SMARTGATE_ALLOW_DIRTY_DEPLOY:-0}" = 1 ]; then
  echo "deploy-safe: emergency hotfix authorized with dirty saved config; controlled reconcile remains mandatory" >&2
else
  grep -F '"dirty":false' "$WORK/config-preflight" >/dev/null || fail "config is dirty; apply or revert it before deployment"
fi
transactions_empty || fail "pending durable transaction exists"
[ ! -e /etc/smartgate/smartgate-rescue.status ] || fail "persistent rescue mode is active"
[ ! -e /tmp/smartgate/smartgate-rescue.status ] || fail "legacy rescue mode is active"
validate_old_data_plane || fail "current applied data plane is not a healthy rollback baseline"
secure_state_permissions || fail "could not tighten SmartGate state permissions"
recovery_markers_absent || fail_pending_recovery

mkdir -p "$DEPLOY_ROOT" "$WORK/stage" /tmp/smartgate
cp "$NEW_BINARY" "$WORK/stage/smartgate"
cp "$NEW_INIT" "$WORK/stage/init"
cp "$NEW_VERSION" "$WORK/stage/version.json"
cp "$NEW_RECOVERY" "$WORK/stage/deploy-recover"
cp "$NEW_WORKER_CONTROL" "$WORK/stage/worker-control"
cp -a "$NEW_WWW" "$WORK/stage/www"
chmod 0755 "$WORK/stage/smartgate" "$WORK/stage/init" "$WORK/stage/deploy-recover" "$WORK/stage/worker-control"
touch "$GATE"

/etc/init.d/smartgate stop
STOPPED=1
wait_daemon_stopped || fail "current daemon did not stop"
stop_v3_workers || fail "current V3 Worker processes could not be stopped"
transactions_empty || fail "transaction appeared while stopping; recovery is required"

mkdir -p "$WORK/backup"
copy_if_present /usr/bin/smartgate "$WORK/backup/usr/bin/smartgate"
copy_if_present /etc/init.d/smartgate "$WORK/backup/etc/init.d/smartgate"
copy_if_present /www/smartgate "$WORK/backup/www/smartgate"
copy_if_present /etc/smartgate "$WORK/backup/etc/smartgate"
copy_if_present "$LEGACY_CONFIG" "$WORK/backup$LEGACY_CONFIG"
copy_if_present "$LEGACY_CONFIG_OWNER" "$WORK/backup$LEGACY_CONFIG_OWNER"
copy_if_present "$LEGACY_SERVICE_OWNER" "$WORK/backup$LEGACY_SERVICE_OWNER"
copy_if_present /etc/hotplug.d/firewall/95-smartgate "$WORK/backup/etc/hotplug.d/firewall/95-smartgate"
copy_if_present /etc/dnsmasq.d/smartgate.conf "$WORK/backup/etc/dnsmasq.d/smartgate.conf"
copy_if_present /etc/firewall.smartgate-reconcile "$WORK/backup/etc/firewall.smartgate-reconcile"
copy_if_present /etc/config/firewall "$WORK/backup/etc/config/firewall"
copy_if_present /usr/lib/smartgate/deploy-recover "$WORK/backup/usr/lib/smartgate/deploy-recover"
copy_if_present /usr/libexec/smartgate-worker-control "$WORK/backup/usr/libexec/smartgate-worker-control"
secure_state_permissions || fail "could not tighten SmartGate state permissions before backup"
if ls /etc/rc.d/S??smartgate >/dev/null 2>&1; then echo yes >"$WORK/backup/smartgate-enabled"; else echo no >"$WORK/backup/smartgate-enabled"; fi
if [ -n "$LEGACY_INIT" ] && "$LEGACY_INIT" enabled >/dev/null 2>&1; then echo yes >"$WORK/backup/legacy-enabled"; else echo no >"$WORK/backup/legacy-enabled"; fi
jsonfilter -i /etc/smartgate/applied-state.json -e '@.gateway_mode' >"$WORK/backup/old-mode" 2>/dev/null || echo off >"$WORK/backup/old-mode"
applied_schema >"$WORK/backup/old-schema" 2>/dev/null || echo 1 >"$WORK/backup/old-schema"
nft list ruleset >"$WORK/backup/nft-ruleset.txt" 2>&1 || true
ip -details link show >"$WORK/backup/ip-links.txt" 2>&1 || true
tar -czf "$BACKUP.tmp" -C "$WORK/backup" .
tar -tzf "$BACKUP.tmp" >/dev/null
mv "$BACKUP.tmp" "$BACKUP"
BACKUP_CREATED=1
sync
cp "$WORK/stage/deploy-recover" "$DEPLOY_ROOT/deploy-recover.new"
chmod 0755 "$DEPLOY_ROOT/deploy-recover.new"
mv "$DEPLOY_ROOT/deploy-recover.new" "$DEPLOY_ROOT/deploy-recover"
printf '#!/bin/sh\n/root/smartgate-deployments/deploy-recover --boot-hook\n' >/etc/rc.d/S01smartgate-deploy-recover
chmod 0755 /etc/rc.d/S01smartgate-deploy-recover
sync
printf '{"phase":"backup_complete","backup":"%s","work":"%s"}\n' "$BACKUP" "$WORK" >"$JOURNAL.tmp"
mv "$JOURNAL.tmp" "$JOURNAL"
sync

old_schema=$(cat "$WORK/backup/old-schema" 2>/dev/null || echo 1)
case "$old_schema" in
  ''|*[!0-9]*) old_schema=1 ;;
esac
quiesce_legacy_data_plane "$old_schema" || fail "legacy V2 data plane could not be quiesced"

cp "$WORK/stage/smartgate" /usr/bin/smartgate.new
chmod 0755 /usr/bin/smartgate.new
mv /usr/bin/smartgate.new /usr/bin/smartgate
cp "$WORK/stage/init" /etc/init.d/smartgate.new
chmod 0755 /etc/init.d/smartgate.new
mv /etc/init.d/smartgate.new /etc/init.d/smartgate
mkdir -p /usr/lib/smartgate
cp "$WORK/stage/deploy-recover" /usr/lib/smartgate/deploy-recover.new
chmod 0755 /usr/lib/smartgate/deploy-recover.new
mv /usr/lib/smartgate/deploy-recover.new /usr/lib/smartgate/deploy-recover
mkdir -p /usr/libexec
cp "$WORK/stage/worker-control" /usr/libexec/smartgate-worker-control.new
chmod 0755 /usr/libexec/smartgate-worker-control.new
mv /usr/libexec/smartgate-worker-control.new /usr/libexec/smartgate-worker-control
printf '{"phase":"payload_installed","backup":"%s","work":"%s"}\n' "$BACKUP" "$WORK" >"$JOURNAL.tmp"
mv "$JOURNAL.tmp" "$JOURNAL"
rm -rf /www/smartgate.new
cp -a "$WORK/stage/www" /www/smartgate.new
rm -rf /www/smartgate
mv /www/smartgate.new /www/smartgate
cp "$WORK/stage/version.json" /etc/smartgate/version.json.new
mv /etc/smartgate/version.json.new /etc/smartgate/version.json

old_enabled=$(cat "$WORK/backup/smartgate-enabled" 2>/dev/null || echo no)
if [ "$old_enabled" = yes ]; then /etc/init.d/smartgate enable; else /etc/init.d/smartgate disable; fi
/etc/init.d/smartgate start
wait_api gated || fail "new API health gate timed out"
pidof smartgate >"$WORK/pid-gated" 2>/dev/null || true
expected_commit=$(jsonfilter -i "$NEW_VERSION" -e '@.commit' 2>/dev/null || true)
[ -z "$expected_commit" ] || grep -F "\"commit\":\"$expected_commit\"" "$WORK/version-gated" >/dev/null || fail "running commit does not match staged release"
transactions_empty || fail "new service left a pending transaction before reconcile"
# A V2 node reference the migration could not resolve is unbound and its pool
# falls to the no_candidate fallback. Every gate below would still pass (no
# Worker left to check), so the box would come up proxying nothing. Roll back.
legacy_migration_resolved "$WORK/runtime-gated" || fail "legacy binding migration left unresolved V2 node references: $(legacy_migration_summary "$WORK/runtime-gated")"

RECONCILE_ID=deploy-reconcile-$$
api_post /api/config/dryrun "{\"op_id\":\"$RECONCILE_ID\"}" "$WORK/reconcile-dryrun" || fail "controlled config dry-run failed: $(api_failure "$WORK/reconcile-dryrun")"
api_post /api/config/apply "{\"dryrun_id\":\"$RECONCILE_ID\",\"description\":\"deployment reconcile\"}" "$WORK/reconcile-apply" || fail "controlled config apply failed: $(api_failure "$WORK/reconcile-apply")"
transactions_empty || fail "controlled reconcile left a pending transaction"
api_get /api/config/state "$WORK/config-reconciled"
grep -F '"dirty":false' "$WORK/config-reconciled" >/dev/null || fail "config remained dirty after reconcile"
validate_data_plane || fail "data plane did not match applied state after reconcile"
/etc/init.d/firewall reload
# fw4 reload rebuilds the table and drops runtime owner rules. Prefer the
# installed hotplug reconcile hook (when present); otherwise reassert the
# required DNS/DoT owner rules before failing the gate.
waited=0
while [ "$waited" -lt 15 ]; do
  if validate_data_plane; then
    break
  fi
  sleep 1
  waited=$((waited + 1))
done
if ! validate_data_plane; then
  # Hotplug may not fire on init.d reload paths that only rewrite nft tables.
  mode=$(jsonfilter -i "$APPLIED_STATE" -e '@.gateway_mode' 2>/dev/null || echo off)
  if is_v3_state; then
    restore_v3_fabric || fail "unable to reassert Gateway Fabric after fw4 reload"
  else
    case "$mode" in
      off|safe) ;;
      *)
        if [ -x /etc/hotplug.d/firewall/95-smartgate ]; then
          sh /etc/hotplug.d/firewall/95-smartgate >/dev/null 2>&1 || true
        fi
        restore_owner_rules || fail "unable to reassert owner rules after fw4 reload"
        sleep 1
        ;;
    esac
  fi
fi
validate_data_plane || fail "data plane failed fw4 reload validation"

rm -f "$GATE"
/etc/init.d/smartgate restart
wait_api resumed || fail "service failed after scheduler resume"
stable=0
while [ "$stable" -lt 3 ]; do
  sleep 5
  transactions_empty || fail "scheduler resume created a pending transaction"
  api_get /api/config/state "$WORK/config-stable-$stable"
  grep -F '"dirty":false' "$WORK/config-stable-$stable" >/dev/null || fail "scheduler resume made config dirty"
  validate_data_plane || fail "data plane drifted after scheduler resume"
  stable=$((stable + 1))
done

sync
rm -f "$JOURNAL"
rm -f /etc/rc.d/S01smartgate-deploy-recover
sync
SUCCESS=1
rm -f "$LOCK_OWNER"
rmdir "$DEPLOY_LOCK" 2>/dev/null || true
trap - EXIT HUP INT TERM
rm -rf "$WORK"
echo "deploy-safe: deployment passed all gates"
echo "deploy-safe: rollback backup retained at $BACKUP"
