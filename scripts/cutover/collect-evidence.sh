#!/usr/bin/env bash
# Read-only evidence collection for incident triage.
#
#   TARGET=router scripts/cutover/collect-evidence.sh <slug> [--token-file <path>]
#
# Collects state files, deploy journals, logs, nft/ip state, processes and
# packages from the target into ops/evidence/<stamp>-<target>-<slug>/, with
# subscription URLs, credentials and tokens redacted before anything touches
# the workstation disk. auth.json is never collected. Works for both V2 and V3
# installations. Without --token-file the authenticated API snapshots are
# skipped; everything else still works.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

SLUG=""
TOKEN_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token-file) TOKEN_FILE=$2; shift 2 ;;
    --*) die "unknown argument: $1" ;;
    *) SLUG=$1; shift ;;
  esac
done
[ -n "$SLUG" ] || die "usage: TARGET=<name> $0 <slug> [--token-file <path>]"

load_target
need ssh; need scp
require_target_reachable

out="$(evidence_dir "$SLUG")"
info "collecting into $out"

version_json="$(remote_version_json)"
{
  echo "collected_at=$(utc_now)"
  echo "target=$TARGET"
  echo "host=$SSH_HOST"
  echo "kind=$TARGET_KIND"
  echo "workstation_repo_head=$(repo_short_sha)"
  echo "version_json=$version_json"
  echo "collector=scripts/cutover/collect-evidence.sh"
  echo "redaction=url,password,psk,key,token,uuid,secret,auth,bearer,userinfo"
  echo "not_collected=auth.json"
} > "$out/manifest.txt"
printf '%s\n' "$version_json" > "$out/version.json"

# State files (redacted). Missing files are recorded as such.
for f in spec.json applied-state.json selection-state.json failover_state.json failover_events.json cutover-drains.json smartgate-rescue.status; do
  if ssh_target "[ -f $REMOTE_DATA_DIR/$f ]"; then
    ssh_target "cat $REMOTE_DATA_DIR/$f" | redact > "$out/$f"
  else
    echo "absent" > "$out/$f.absent"
  fi
done
ssh_target "cat $REMOTE_RUNTIME_DIR/runtime.json 2>/dev/null || echo absent" | redact > "$out/runtime.json"
mkdir -p "$out/transactions" "$out/deploy"
ssh_target "ls $REMOTE_DATA_DIR/transactions 2>/dev/null" | while read -r tx; do
  [ -n "$tx" ] || continue
  ssh_target "cat $REMOTE_DATA_DIR/transactions/$tx" | redact > "$out/transactions/$tx"
done
ssh_target "ls -la $REMOTE_DEPLOY_DIR 2>/dev/null || echo none" > "$out/deploy/listing.txt"
ssh_target "for f in $REMOTE_DEPLOY_DIR/*.json $REMOTE_DEPLOY_DIR/*.log $REMOTE_DEPLOY_DIR/lock/owner; do [ -f \"\$f\" ] || continue; echo \"### \$f\"; cat \"\$f\"; echo; done 2>/dev/null" | redact > "$out/deploy/journals.txt"
# Per-deployment work directories: gate snapshots, controlled reconcile
# responses, rescue logs and the failure diagnostics deploy-safe captures
# before it rolls back. Binaries and staged payloads are skipped.
ssh_target "for f in $REMOTE_DEPLOY_DIR/work-*/*-preflight $REMOTE_DEPLOY_DIR/work-*/*-gated $REMOTE_DEPLOY_DIR/work-*/*-rollback $REMOTE_DEPLOY_DIR/work-*/pid-* $REMOTE_DEPLOY_DIR/work-*/reconcile-* $REMOTE_DEPLOY_DIR/work-*/config-* $REMOTE_DEPLOY_DIR/work-*/*.log $REMOTE_DEPLOY_DIR/work-*/failure-diagnostics.txt; do [ -f \"\$f\" ] || continue; echo \"### \$f\"; cat \"\$f\"; echo; done 2>/dev/null" | redact > "$out/deploy/work.txt"
ssh_target "ls $REMOTE_DATA_DIR/lastgood 2>/dev/null || true" > "$out/lastgood-files.txt"
ssh_target "ls -la $REMOTE_DATA_DIR/snapshots $REMOTE_DATA_DIR/backups 2>/dev/null || true" > "$out/snapshots-backups-listing.txt"

# Logs and host state
ssh_target "logread 2>/dev/null | grep -E '$(process_alt_re)|dnsmasq|firewall|deploy' | tail -n 1500 || true" | redact > "$out/logread.txt"
ssh_target "dmesg 2>/dev/null | tail -n 200 || true" > "$out/dmesg.txt"
remote_sh "
echo '## uptime'; uptime
echo '## processes'; ps w 2>/dev/null | grep -E '$(process_alt_re)|dnsmasq' | grep -v grep || true
echo '## rss/fd'; for p in \$(pidof smartgate $(worker_pidof_args) 2>/dev/null); do echo \"pid=\$p name=\$(cat /proc/\$p/comm) rss_kb=\$(awk '/VmRSS/ {print \$2}' /proc/\$p/status) fds=\$(ls /proc/\$p/fd | wc -l | tr -d ' ')\"; done
echo '## memory'; free 2>/dev/null || head -5 /proc/meminfo
echo '## load'; cat /proc/loadavg
echo '## disk'; df -k
echo '## listeners'; netstat -ln 2>/dev/null || true
echo '## conntrack count'; cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || true
echo '## services'; for s in smartgate $(worker_pidof_args) dnsmasq firewall; do printf '%s: ' \$s; /etc/init.d/\$s status 2>/dev/null || echo n/a; done
" 2>&1 | redact > "$out/resources.txt" || true
ssh_target "nft list ruleset 2>/dev/null || true" | grep -nE 'owner=smartgate|table |chain ' > "$out/nft-ruleset.txt" || true
ssh_target "nft list ruleset 2>/dev/null | grep -c . || true" >> "$out/nft-ruleset.txt"
remote_sh "echo '## ip rule'; ip rule; echo '## ip -6 rule'; ip -6 rule; echo '## ip route'; ip route; echo '## ip -6 route'; ip -6 route; echo '## ip addr'; ip -br addr" > "$out/ip.txt" 2>&1 || true
ssh_target "opkg list-installed 2>/dev/null | grep -E '^($(package_alt_re))' || true" > "$out/packages.txt"
ssh_target "cat /tmp/sysinfo/model /tmp/sysinfo/board_name 2>/dev/null; cat /etc/openwrt_release 2>/dev/null; uname -a" > "$out/system.txt"
remote_sh "for d in www.baidu.com api.openai.com www.google.com; do echo \"## \$d via 127.0.0.1\"; nslookup \$d 127.0.0.1 2>&1 | tail -n +3; echo \"## \$d via 127.0.0.1:1053\"; nslookup -port=1053 \$d 127.0.0.1 2>&1 | tail -n +3; done" > "$out/dns.txt" 2>&1 || true

# Authenticated snapshots
if [ -n "$TOKEN_FILE" ]; then
  remote_token="$(upload_token "$TOKEN_FILE")"
  trap 'cleanup_token "$remote_token"' EXIT
  api() { ssh_target "wget -qO- -T 8 --header=\"Authorization: Bearer \$(cat '$remote_token')\" '$API_URL$1' 2>/dev/null || echo '{\"ok\":false,\"error\":{\"code\":\"unreachable\"}}'"; }
  for path in /api/version /api/runtime/status /api/config/state /api/overview /api/providers /api/nodes /api/proxy-groups /api/business-pools /api/workers /api/connections /api/rules /api/rules/conflicts /api/system/status /api/scheduler /api/snapshots /api/dns; do
    api "$path" | redact > "$out/api$(printf '%s' "$path" | tr '/' '-').json"
  done
  : > "$out/route-explain.txt"
  for target in api.openai.com www.baidu.com www.google.com github.com router.lan; do
    printf '%s -> ' "$target" >> "$out/route-explain.txt"
    ssh_target "wget -qO- -T 8 --header=\"Authorization: Bearer \$(cat '$remote_token')\" --header='Content-Type: application/json' --post-data='{\"target\":\"$target\"}' '$API_URL/api/routes/explain' 2>/dev/null || echo unreachable" | redact >> "$out/route-explain.txt"
  done
  cleanup_token "$remote_token"; trap - EXIT
else
  echo "skipped: no --token-file" > "$out/api.skipped"
fi

# Final sweep: the workstation copy must not contain anything credential-like.
if grep -rIlE 'Bearer [A-Za-z0-9._-]{16,}|[?&]token=[A-Za-z0-9._-]{8,}' "$out" >/dev/null 2>&1; then
  die "redaction sweep found a credential-like string under $out; delete the directory and report"
fi
echo "files=$(find "$out" -type f | wc -l | tr -d ' ')" >> "$out/manifest.txt"
log_deploy collect-evidence.sh "installed@$(json_field "$version_json" commit)" ok "$SLUG $out"
info "done: $out"
