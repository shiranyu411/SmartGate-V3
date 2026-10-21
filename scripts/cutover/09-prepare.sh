#!/usr/bin/env bash
# Reclaim only immutable deployment archives already verified off-device.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"
load_target
need python3
[ -n "${BACKUP_DIR:-}" ] || die "BACKUP_DIR is required"
require_target_reachable
installed="$(json_field "$(remote_version_json)" commit)"
[ -n "${FROM_COMMIT:-}" ] && [ "$installed" = "$FROM_COMMIT" ] || die "FROM_COMMIT does not match the device"
python3 "$CUTOVER_DIR/backup.py" check "$BACKUP_DIR" \
  --forbid "$(dirname "$REPO_ROOT")" --forbid "$OPS_DIR" \
  --target "$TARGET" --host "$SSH_HOST" --commit "$installed"
umask 077
plan="$(ssh_target "sha256sum '$REMOTE_DEPLOY_DIR'/before-*.tar.gz" \
  | python3 "$CUTOVER_DIR/backup.py" reclaim-plan "$BACKUP_DIR" --deploy-root "$REMOTE_DEPLOY_DIR")"
info "will retain the three newest device archives; older copies are already in the verified private backup"
confirm_production prepare
log_deploy 09-prepare.sh "v2@$installed" started "verified off-device archive reclamation"
remote_sh "
[ ! -e '$REMOTE_DEPLOY_DIR/pending.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/pending.starting.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/fresh-pending.json' ]
mkdir '$REMOTE_DEPLOY_DIR/lock'
trap 'rm -f \"$REMOTE_DEPLOY_DIR/lock/owner\"; rmdir \"$REMOTE_DEPLOY_DIR/lock\"' EXIT HUP INT TERM
printf '%s\n%s\n%s\n' \"\$\$\" \"\$(cat /proc/sys/kernel/random/boot_id)\" \"\$(awk '{print \$22}' /proc/\$\$/stat)\" >'$REMOTE_DEPLOY_DIR/lock/owner'
[ \"\$(jsonfilter -i '$REMOTE_DATA_DIR/version.json' -e '@.commit')\" = '$installed' ]
while read -r expected name; do
  [ -n \"\$expected\" ] || continue
  actual=\$(sha256sum '$REMOTE_DEPLOY_DIR/'\"\$name\" | cut -d' ' -f1)
  [ \"\$actual\" = \"\$expected\" ] || exit 1
  rm '$REMOTE_DEPLOY_DIR/'\"\$name\"
  printf 'offloaded %s\n' \"\$name\"
done <<'VERIFIED_ARCHIVES'
$plan
VERIFIED_ARCHIVES
sync
df -k / /tmp
" || {
  log_deploy 09-prepare.sh "v2@$installed" fail "archive reclamation stopped; private backups preserved"
  die "preparation stopped"
}
log_deploy 09-prepare.sh "v2@$installed" ok "old archives offloaded; three newest retained; unrelated services untouched"
