#!/usr/bin/env bash
# Review/apply an existing saved configuration before a guarded upgrade.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"
load_target
need python3
[ -n "${TOKEN_FILE:-}" ] || die "TOKEN_FILE is required"
action="${1:-dryrun}"
op="${2:-cutover-baseline-$(date -u +%Y%m%dT%H%M%SZ)}"
case "$op" in ''|*[!a-zA-Z0-9_-]*) die "invalid operation id" ;; esac
case "$action" in
  dryrun) body="{\"op_id\":\"$op\"}" ;;
  apply)
    [ -n "${BACKUP_DIR:-}" ] || die "BACKUP_DIR is required before applying saved changes"
    installed="$(json_field "$(remote_version_json)" commit)"
    spec_sha="$(ssh_target "sha256sum '$REMOTE_DATA_DIR/spec.json'" | cut -d' ' -f1)"
    python3 "$CUTOVER_DIR/backup.py" check "$BACKUP_DIR" \
      --forbid "$(dirname "$REPO_ROOT")" --forbid "$OPS_DIR" \
      --target "$TARGET" --host "$SSH_HOST" --commit "$installed" --spec-sha "$spec_sha"
    confirm_production apply
    body="{\"dryrun_id\":\"$op\",\"description\":\"pre-cutover saved configuration\"}"
    ;;
  *) die "usage: 06-reconcile.sh dryrun|apply [operation-id]" ;;
esac
require_target_reachable
remote_token="$(upload_token "$TOKEN_FILE")"
trap 'cleanup_token "$remote_token"' EXIT
out="$(evidence_dir "baseline-$action")"
set +e
ssh_target "wget -T 45 -O- --header=\"Authorization: Bearer \$(cat '$remote_token')\" --header='Content-Type: application/json' --post-data='$body' '$API_URL/api/config/$action'" \
  2>"$out/request.log" | redact | tee "$out/response.json"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
  log_deploy 06-reconcile.sh "$action" fail "op=$op evidence=$out"
  die "$action failed; see $out/request.log"
fi
log_deploy 06-reconcile.sh "$action" ok "op=$op evidence=$out"
info "$action completed; operation=$op"
