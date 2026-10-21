#!/usr/bin/env bash
# Run the staged release's install.sh on the target.
#
#   TARGET=router scripts/cutover/20-upgrade.sh --token-file <path>
#   TARGET=devvm  scripts/cutover/20-upgrade.sh [--token-file <path>]   # blank VM needs no token
#
# install.sh picks the guarded upgrade (deploy-safe.sh) when SmartGate is
# already installed and the transactional fresh bootstrap on a blank target.
# The token is copied to a 0600 temp file on the target and removed afterwards;
# it is never placed on a command line. Production requires typing
# "UPGRADE router" at the prompt.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

TOKEN_FILE=""
EMERGENCY_HOTFIX=0
while [ $# -gt 0 ]; do
  case "$1" in
    --token-file) TOKEN_FILE=$2; shift 2 ;;
    --emergency-hotfix) EMERGENCY_HOTFIX=1; shift ;;
    *) die "unknown argument: $1" ;;
  esac
done

load_target
need ssh; need scp
require_target_reachable

staged_json="$(ssh_target "cat $REMOTE_STAGE_DIR/version.json 2>/dev/null || true")"
[ -n "$staged_json" ] || die "nothing staged at $REMOTE_STAGE_DIR; run 10-stage.sh first"
staged_commit="$(json_field "$staged_json" commit)"
staged_version="$(json_field "$staged_json" version)"
installed_json="$(remote_version_json)"
installed_commit="$(json_field "$installed_json" commit)"

if [ -n "$installed_commit" ] && [ -z "$TOKEN_FILE" ]; then
  die "target already runs SmartGate ($installed_commit); the guarded upgrade needs --token-file with a valid admin Bearer token"
fi

info "installed: ${installed_commit:-none}   staged: $staged_version@$staged_commit"
require_private_backup
confirm_production upgrade

remote_token=""
if [ -n "$TOKEN_FILE" ]; then
  remote_token="$(upload_token "$TOKEN_FILE")"
fi
cleanup() { cleanup_token "$remote_token"; }
trap cleanup EXIT

started="$(utc_now)"
deploy_env=""
if [ "$EMERGENCY_HOTFIX" -eq 1 ]; then
  deploy_env="SMARTGATE_ALLOW_DIRTY_DEPLOY=1 "
fi
set +e
if [ -n "$remote_token" ]; then
  ssh_target "cd '$REMOTE_STAGE_DIR' && ${deploy_env}SMARTGATE_API_BASE='$API_URL' SMARTGATE_AUTH_HEADER=\"Authorization: Bearer \$(cat '$remote_token')\" sh scripts/install.sh" 2>&1 | redact
else
  ssh_target "cd '$REMOTE_STAGE_DIR' && ${deploy_env}SMARTGATE_API_BASE='$API_URL' sh scripts/install.sh" 2>&1 | redact
fi
rc=${PIPESTATUS[0]}
set -e
cleanup; trap - EXIT

if [ "$rc" -ne 0 ]; then
  log_deploy 20-upgrade.sh "v3@$staged_commit" fail "from=${installed_commit:-none} started=$started exit=$rc"
  cat >&2 <<EOF
cutover[$TARGET]: install.sh exited $rc.
  - Before backup/swap: old service should still be running; fix the preflight cause and retry.
  - After backup: deploy-safe attempts automatic rollback from $REMOTE_DEPLOY_DIR/before-*.tar.gz.
  - If $REMOTE_DEPLOY_DIR/pending.json remains: run  TARGET=$TARGET scripts/cutover/40-rollback.sh --auto
  - Do not delete journals or backups while diagnosing.
EOF
  exit "$rc"
fi

now_json="$(remote_version_json)"
now_commit="$(json_field "$now_json" commit)"
[ "$now_commit" = "$staged_commit" ] || die "install.sh succeeded but installed commit is $now_commit, expected $staged_commit"
log_deploy 20-upgrade.sh "v3@$staged_commit" ok "from=${installed_commit:-none} started=$started"
info "target now runs $staged_version@$staged_commit; next: 30-verify.sh"
