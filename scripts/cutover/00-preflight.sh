#!/usr/bin/env bash
# Read-only preflight of a target before staging a release.
#
#   TARGET=router scripts/cutover/00-preflight.sh [--expect-commit <sha>] [--token-file <path>]
#
# Checks: SSH reachable; installed version.json (optionally == expected
# commit); required packages; free space; no pending deploy/fresh journals;
# no rescue flag; no leftover transactions; /healthz; optional authenticated
# /api/config/state must be clean. Writes a facts file under ops/evidence/.
# Never modifies the target.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

EXPECT_COMMIT=""
TOKEN_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --expect-commit) EXPECT_COMMIT=$2; shift 2 ;;
    --token-file) TOKEN_FILE=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

load_target
need ssh
require_target_reachable
info "preflight against $SSH_USER@$SSH_HOST:$SSH_PORT ($TARGET_KIND)"

out="$(evidence_dir preflight)"
facts="$out/facts.txt"
fail=0
check() { # check <label> <ok:0|1> <detail>
  # tee stays inside the function so fail=1 is not lost to a pipe subshell
  if [ "$2" -eq 0 ]; then
    printf 'PASS  %-34s %s\n' "$1" "$3" | tee -a "$facts"
  else
    printf 'FAIL  %-34s %s\n' "$1" "$3" | tee -a "$facts"
    fail=1
  fi
}

{
  echo "# preflight $(utc_now) target=$TARGET host=$SSH_HOST kind=$TARGET_KIND"
  echo "# workstation repo HEAD: $(repo_short_sha)"
  echo
} | tee "$facts"

version_json="$(remote_version_json)"
installed_commit="$(json_field "$version_json" commit)"
installed_version="$(json_field "$version_json" version)"
printf 'installed version.json: %s\n' "$version_json" | tee -a "$facts"
if [ -n "$installed_commit" ]; then
  if [ -n "$EXPECT_COMMIT" ]; then
    [ "$installed_commit" = "$EXPECT_COMMIT" ] && ok=0 || ok=1
    check "installed commit == expected" $ok "installed=$installed_commit expected=$EXPECT_COMMIT"
  else
    check "installed commit" 0 "$installed_version@$installed_commit (no --expect-commit given)"
  fi
else
  check "installed SmartGate" 0 "none (blank target → fresh install path)"
fi

sysinfo="$(remote_sh '
echo "model=$(cat /tmp/sysinfo/model 2>/dev/null || echo unknown)"
echo "board=$(cat /tmp/sysinfo/board_name 2>/dev/null || echo unknown)"
. /etc/openwrt_release 2>/dev/null || true
echo "distrib=${DISTRIB_ID:-unknown} ${DISTRIB_RELEASE:-} ${DISTRIB_REVISION:-}"
echo "kernel=$(uname -r)"
echo "arch=$(uname -m)"
echo "memtotal_kb=$(awk "/MemTotal/ {print \$2}" /proc/meminfo)"
echo "memavail_kb=$(awk "/MemAvailable/ {print \$2}" /proc/meminfo)"
echo "loadavg=$(cut -d" " -f1-3 /proc/loadavg)"
echo "root_free_kb=$(df -k / | awk "NR==2 {print \$4}")"
echo "tmp_free_kb=$(df -k /tmp | awk "NR==2 {print \$4}")"
')"
printf '%s\n' "$sysinfo" | tee -a "$facts"
root_free=$(printf '%s\n' "$sysinfo" | sed -n 's/^root_free_kb=//p')
tmp_free=$(printf '%s\n' "$sysinfo" | sed -n 's/^tmp_free_kb=//p')
[ "${root_free:-0}" -ge "${MIN_FREE_KB:-20000}" ] && ok=0 || ok=1
check "free space on /" $ok "${root_free}k (min ${MIN_FREE_KB:-20000}k)"
[ "${tmp_free:-0}" -ge "${MIN_FREE_KB:-20000}" ] && ok=0 || ok=1
check "free space on /tmp" $ok "${tmp_free}k"

pkgs="$(ssh_target 'opkg list-installed 2>/dev/null || apk list --installed 2>/dev/null || true')"
for pkg in ${REQUIRED_PACKAGES:-}; do
  line=$(printf '%s\n' "$pkgs" | grep -E "^$pkg( |-[0-9]|$)" | head -n1 || true)
  [ -n "$line" ] && ok=0 || ok=1
  check "package $pkg" $ok "${line:-not installed}"
done
for bin in ${REQUIRED_WORKER_BINS:-} nft ip conntrack; do
  path=$(ssh_target "command -v $bin 2>/dev/null || true")
  ver=""
  if [ -n "$path" ]; then
    ver=$(ssh_target "$bin version 2>/dev/null | head -n1" || true)
    [ -n "$ver" ] || ver=$(ssh_target "$bin --version 2>/dev/null | head -n1" || true)
  fi
  [ -n "$path" ] && ok=0 || ok=1
  check "binary $bin" $ok "${path:-missing} ${ver}"
done
# Optional execution cores: the daemon registers only the cores whose binary
# is installed, so their absence is a fact to record, not a failure.
for pkg in ${OPTIONAL_PACKAGES:-}; do
  line=$(printf '%s\n' "$pkgs" | grep -E "^$pkg( |-[0-9]|$)" | head -n1 || true)
  printf 'optional package %-22s %s\n' "$pkg" "${line:-not installed}" | tee -a "$facts"
done
for bin in ${OPTIONAL_WORKER_BINS:-}; do
  path=$(ssh_target "command -v $bin 2>/dev/null || true")
  printf 'optional core %-25s %s\n' "$bin" "${path:-not installed (SmartNodes cannot select this core)}" | tee -a "$facts"
done

state="$(remote_sh "
echo pending_json=\$([ -e $REMOTE_DEPLOY_DIR/pending.json ] && echo yes || echo no)
echo pending_starting=\$([ -e $REMOTE_DEPLOY_DIR/pending.starting.json ] && echo yes || echo no)
echo fresh_pending=\$([ -e $REMOTE_DEPLOY_DIR/fresh-pending.json ] && echo yes || echo no)
echo rescue_flag=\$([ -e $REMOTE_DATA_DIR/smartgate-rescue.status ] || [ -e $REMOTE_RUNTIME_DIR/smartgate-rescue.status ] && echo yes || echo no)
set -- $REMOTE_DATA_DIR/transactions/*.json
echo transactions=\$([ -e \"\$1\" ] && echo present || echo none)
echo backups=\$(ls $REMOTE_DEPLOY_DIR/before-*.tar.gz 2>/dev/null | wc -l | tr -d ' ')
echo healthz=\$(wget -qO- -T 5 $API_URL/healthz 2>/dev/null || echo unreachable)
echo auth_check=\$(wget -qO- -T 5 $API_URL/api/auth/check 2>/dev/null || echo unreachable)
")"
printf '%s\n' "$state" | tee -a "$facts"
val() { printf '%s\n' "$state" | sed -n "s/^$1=//p"; }
[ "$(val pending_json)" = no ] && [ "$(val pending_starting)" = no ] && ok=0 || ok=1
check "no pending deploy journal" $ok "pending.json=$(val pending_json) starting=$(val pending_starting)"
[ "$(val fresh_pending)" = no ] && ok=0 || ok=1
check "no fresh-install journal" $ok ""
[ "$(val rescue_flag)" = no ] && ok=0 || ok=1
check "rescue mode inactive" $ok ""
[ "$(val transactions)" = none ] && ok=0 || ok=1
check "no leftover transactions" $ok ""
if [ -n "$installed_commit" ]; then
  [ "$(val healthz)" = ok ] && ok=0 || ok=1
  check "/healthz" $ok "$(val healthz)"
fi

if [ -n "$TOKEN_FILE" ] && [ -n "$installed_commit" ]; then
  remote_token="$(upload_token "$TOKEN_FILE")"
  trap 'cleanup_token "$remote_token"' EXIT
  authed="$(remote_sh "
H=\"Authorization: Bearer \$(cat '$remote_token')\"
echo version=\$(wget -qO- -T 5 --header=\"\$H\" $API_URL/api/version 2>/dev/null || echo unreachable)
echo config_state=\$(wget -qO- -T 5 --header=\"\$H\" $API_URL/api/config/state 2>/dev/null || echo unreachable)
echo runtime_status=\$(wget -qO- -T 5 --header=\"\$H\" $API_URL/api/runtime/status 2>/dev/null || echo unreachable)
" | redact)"
  cleanup_token "$remote_token"; trap - EXIT
  printf '%s\n' "$authed" | tee -a "$facts"
  case "$authed" in
    *'"dirty":false'*) ok=0 ;;
    *) ok=1 ;;
  esac
  check "config state clean (dirty:false)" $ok ""
  case "$authed" in
    *'"authenticated":true'*|*'"ok":true'*) ok=0 ;;
    *) ok=1 ;;
  esac
  check "token accepted by API" $ok ""
  # fw4 never runs hotplug.d/firewall, so owner-tagged DNS rules disappear on
  # every fw4 reload. deploy-safe refuses an on/global baseline without them.
  applied_mode=$(printf '%s\n' "$authed" | sed -n 's/^config_state=//p' | sed -nE 's/.*"applied_gateway_mode":"([a-z]*)".*/\1/p')
  case "$applied_mode" in
    on|global)
      owner_rules=$(ssh_target "nft list ruleset 2>/dev/null | grep -c 'owner=smartgate:dns-hijack'" 2>/dev/null || true)
      [ "${owner_rules:-0}" -ge 2 ] && ok=0 || ok=1
      check "owner-tagged DNS hijack rules present" $ok "mode=$applied_mode count=${owner_rules:-0}; if 0, run 'sh /etc/hotplug.d/firewall/95-smartgate' on the target and rerun preflight"
      ;;
  esac
  for bin in ${REQUIRED_WORKER_BINS:-}; do
    printf 'legacy service %s: %s\n' "$bin" "$(ssh_target "uci -q show $bin 2>/dev/null | grep -E 'enabled|user' | tr '\n' ' '")" | tee -a "$facts"
  done
else
  info "no --token-file: authenticated checks (dirty state, version) skipped"
fi

echo | tee -a "$facts"
if [ "$fail" -eq 0 ]; then
  echo "PREFLIGHT: PASS" | tee -a "$facts"
  log_deploy 00-preflight.sh "installed=${installed_version:-none}@${installed_commit:-none}" pass "$out"
else
  echo "PREFLIGHT: FAIL (see $facts)" | tee -a "$facts"
  log_deploy 00-preflight.sh "installed=${installed_version:-none}@${installed_commit:-none}" fail "$out"
  exit 1
fi
