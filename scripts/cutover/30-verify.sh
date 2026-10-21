#!/usr/bin/env bash
# Post-install verification and evidence capture.
#
#   TARGET=router scripts/cutover/30-verify.sh --token-file <path> [--expect-commit <sha>] [--slug <name>]
#
# Gates (any failure → exit 1): /healthz ok; authenticated /api/version commit
# equals the staged/expected commit; /api/runtime/status reachable and not in
# rescue; /api/config/state dirty:false; no leftover transactions or deploy
# journals; embedded DNS listener on :1053 when gateway_mode is on; owner
# tagged nft table present; Route Explain answers for probe targets.
# Everything observed is written (redacted) to ops/evidence/<stamp>-<target>-<slug>/
# so the same output doubles as a baseline, a cutover record, or incident
# evidence. Read-only on the target.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

TOKEN_FILE=""
EXPECT_COMMIT=""
SLUG="verify"
while [ $# -gt 0 ]; do
  case "$1" in
    --token-file) TOKEN_FILE=$2; shift 2 ;;
    --expect-commit) EXPECT_COMMIT=$2; shift 2 ;;
    --slug) SLUG=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

load_target
need ssh; need scp
require_target_reachable
[ -n "$TOKEN_FILE" ] || die "--token-file is required (authenticated gates)"

out="$(evidence_dir "$SLUG")"
report="$out/report.txt"
fail=0
gate() { # gate <label> <ok:0|1> <detail>
  # tee stays inside the function so fail=1 is not lost to a pipe subshell
  if [ "$2" -eq 0 ]; then
    printf 'PASS  %-38s %s\n' "$1" "$3" | tee -a "$report"
  else
    printf 'FAIL  %-38s %s\n' "$1" "$3" | tee -a "$report"
    fail=1
  fi
}

installed_json="$(remote_version_json)"
installed_commit="$(json_field "$installed_json" commit)"
if [ -z "$EXPECT_COMMIT" ]; then
  EXPECT_COMMIT="$(json_field "$(ssh_target "cat $REMOTE_STAGE_DIR/version.json 2>/dev/null || echo '{}'")" commit)"
fi
{
  echo "# verify $(utc_now) target=$TARGET host=$SSH_HOST kind=$TARGET_KIND slug=$SLUG"
  echo "# workstation repo HEAD: $(repo_short_sha)"
  echo "installed version.json: $installed_json"
  echo "expected commit: ${EXPECT_COMMIT:-<none>}"
  echo
} | tee "$report"

remote_token="$(upload_token "$TOKEN_FILE")"
trap 'cleanup_token "$remote_token"' EXIT

# --- unauthenticated ---------------------------------------------------------
healthz="$(ssh_target "wget -qO- -T 5 $API_URL/healthz 2>/dev/null || echo unreachable")"
[ "$healthz" = ok ] && ok=0 || ok=1
gate "/healthz" $ok "$healthz"

# --- authenticated API snapshots ---------------------------------------------
api() { # api <path> [post-json]
  if [ -n "${2:-}" ]; then
    ssh_target "wget -qO- -T 8 --header=\"Authorization: Bearer \$(cat '$remote_token')\" --header='Content-Type: application/json' --post-data='$2' '$API_URL$1' 2>/dev/null || echo '{\"ok\":false,\"error\":{\"code\":\"unreachable\",\"message\":\"wget failed\"}}'"
  else
    ssh_target "wget -qO- -T 8 --header=\"Authorization: Bearer \$(cat '$remote_token')\" '$API_URL$1' 2>/dev/null || echo '{\"ok\":false,\"error\":{\"code\":\"unreachable\",\"message\":\"wget failed\"}}'"
  fi
}
save() { printf '%s\n' "$2" | redact > "$out/$1"; }

version="$(api /api/version)"; save api-version.json "$version"
api_commit="$(json_field "$version" commit)"
if [ -n "$EXPECT_COMMIT" ]; then
  [ "$api_commit" = "$EXPECT_COMMIT" ] && ok=0 || ok=1
  gate "/api/version commit == expected" $ok "api=$api_commit expected=$EXPECT_COMMIT"
else
  [ -n "$api_commit" ] && ok=0 || ok=1
  gate "/api/version" $ok "commit=$api_commit"
fi
[ "$api_commit" = "$installed_commit" ] && ok=0 || ok=1
gate "running commit == version.json" $ok "api=$api_commit file=$installed_commit"

runtime="$(api /api/runtime/status)"; save api-runtime-status.json "$runtime"
case "$runtime" in *'"ok":true'*) ok=0 ;; *) ok=1 ;; esac
gate "/api/runtime/status" $ok ""
case "$runtime" in *'"rescue":true'*|*'"rescue_active":true'*) ok=1 ;; *) ok=0 ;; esac
gate "not in rescue mode" $ok ""
# A V2 node reference V3 could not resolve is unbound and its pool sits on the
# no_candidate fallback: the box works but proxies nothing for that pool.
case "$runtime" in
  *'"legacy_migration"'*)
    case "$runtime" in *'"unresolved":[]'*|*'"unresolved":null'*) ok=0 ;; *) ok=1 ;; esac
    gate "legacy V2 node bindings fully resolved" $ok "$(printf '%s' "$runtime" | sed -nE 's/.*"legacy_migration":(\{[^}]*\}).*/\1/p' | head -c 300)"
    ;;
esac

config="$(api /api/config/state)"; save api-config-state.json "$config"
case "$config" in *'"dirty":false'*) ok=0 ;; *) ok=1 ;; esac
gate "/api/config/state dirty:false" $ok ""
gateway_mode="$(json_field "$config" gateway_mode)"
[ -n "$gateway_mode" ] || gateway_mode="$(json_field "$runtime" gateway_mode)"
echo "gateway_mode=${gateway_mode:-unknown}" | tee -a "$report"

for path in /api/overview /api/rules /api/business-pools /api/workers /api/cores /api/providers /api/nodes /api/connections /api/system/status /api/dns; do
  body="$(api "$path")"
  save "api$(printf '%s' "$path" | tr '/' '-').json" "$body"
  case "$body" in *'"ok":true'*) ok=0 ;; *) ok=1 ;; esac
  gate "GET $path" $ok ""
done
rules="$(cat "$out/api-api-rules.json")"
case "$rules" in *builtin*) ok=0 ;; *) ok=1 ;; esac
gate "/api/rules contains built-in catalog" $ok ""

: > "$out/route-explain.txt"
for target in api.openai.com www.baidu.com www.google.com router.lan 192.168.1.10; do
  explain="$(api /api/routes/explain "{\"target\":\"$target\"}")"
  printf '%s -> %s\n' "$target" "$explain" | redact >> "$out/route-explain.txt"
  case "$explain" in *'"ok":true'*) ok=0 ;; *) ok=1 ;; esac
  gate "route explain $target" $ok ""
done

cleanup_token "$remote_token"; trap - EXIT

# --- host state ---------------------------------------------------------------
remote_sh "
echo '## transactions'; ls -la $REMOTE_DATA_DIR/transactions 2>/dev/null || true
echo '## deploy dir'; ls -la $REMOTE_DEPLOY_DIR 2>/dev/null || true
echo '## rescue flag'; ls -la $REMOTE_DATA_DIR/smartgate-rescue.status $REMOTE_RUNTIME_DIR/smartgate-rescue.status 2>/dev/null || echo none
echo '## listeners'; netstat -ln 2>/dev/null | grep -E ':(53|1053|3000|7890|9090) ' || true
echo '## processes'; ps w 2>/dev/null | grep -E '$(process_alt_re)' | grep -v grep || true
echo '## memory'; free 2>/dev/null || cat /proc/meminfo | head -5
echo '## load'; cat /proc/loadavg
echo '## disk'; df -k / /tmp
echo '## rss/fd per process'
for p in \$(pidof smartgate $(worker_pidof_args) 2>/dev/null); do
  name=\$(cat /proc/\$p/comm 2>/dev/null); rss=\$(awk '/VmRSS/ {print \$2}' /proc/\$p/status 2>/dev/null); fds=\$(ls /proc/\$p/fd 2>/dev/null | wc -l | tr -d ' ')
  echo \"pid=\$p name=\$name rss_kb=\$rss fds=\$fds\"
done
" 2>&1 | redact > "$out/host-state.txt" || true

remote_sh "nft list ruleset 2>/dev/null | grep -n 'owner=smartgate' || true; echo '## tables'; nft list tables 2>/dev/null || true" > "$out/nft-owner-rules.txt" 2>&1 || true
remote_sh "echo '## ip rule'; ip rule 2>/dev/null; echo '## ip -6 rule'; ip -6 rule 2>/dev/null; echo '## routes (non-main)'; for t in \$(ip rule 2>/dev/null | sed -n 's/.*lookup \([0-9a-z_]*\).*/\1/p' | sort -u); do echo \"# table \$t\"; ip route show table \$t 2>/dev/null; done" > "$out/policy-routing.txt" 2>&1 || true
remote_sh "logread 2>/dev/null | grep -E '$(process_alt_re)|deploy' | tail -n 400 || true" | redact > "$out/logread.txt" 2>&1 || true
remote_sh "opkg list-installed 2>/dev/null | grep -E '^($(package_alt_re))' || true" > "$out/packages.txt" 2>&1 || true

transactions_present=$(grep -c '\.json' "$out/host-state.txt" | head -n1 || echo 0)
tx_block="$(sed -n '/## transactions/,/## deploy dir/p' "$out/host-state.txt" | grep -c '\.json' || true)"
[ "${tx_block:-0}" -eq 0 ] && ok=0 || ok=1
gate "no leftover transactions" $ok ""
pending="$(sed -n '/## deploy dir/,/## rescue flag/p' "$out/host-state.txt" | grep -cE 'pending(\.starting)?\.json|fresh-pending\.json' || true)"
[ "${pending:-0}" -eq 0 ] && ok=0 || ok=1
gate "no pending deploy journal" $ok ""
if [ "$gateway_mode" = on ]; then
  grep -q ':1053 ' "$out/host-state.txt" && ok=0 || ok=1
  gate "embedded DNS listening on :1053" $ok ""
  grep -q 'table inet smartgate' "$out/nft-owner-rules.txt" && ok=0 || ok=1
  gate "nft table inet smartgate present" $ok ""
  grep -q 'owner=smartgate:dns-hijack' "$out/nft-owner-rules.txt" && ok=0 || ok=1
  gate "owner-tagged DNS hijack rule" $ok ""
else
  echo "INFO  gateway_mode=$gateway_mode: data-plane gates skipped" | tee -a "$report"
fi
grep -q 'name=smartgate' "$out/host-state.txt" && ok=0 || ok=1
gate "smartgate process running" $ok "$(grep 'name=smartgate' "$out/host-state.txt" | head -n1)"

echo | tee -a "$report"
echo "resources: $(grep -E '^pid=' "$out/host-state.txt" | tr '\n' ' ')" | tee -a "$report"
echo "evidence: $out" | tee -a "$report"
if [ "$fail" -eq 0 ]; then
  echo "VERIFY: PASS" | tee -a "$report"
  log_deploy 30-verify.sh "v3@${api_commit:-?}" pass "$SLUG $out"
else
  echo "VERIFY: FAIL" | tee -a "$report"
  log_deploy 30-verify.sh "v3@${api_commit:-?}" fail "$SLUG $out"
  exit 1
fi
