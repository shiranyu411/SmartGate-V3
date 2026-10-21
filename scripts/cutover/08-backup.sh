#!/usr/bin/env bash
# Stream a complete private file backup off-device before staging V3.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

OUTPUT="${BACKUP_DIR:-}"
EXPECT_COMMIT="${FROM_COMMIT:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --output) OUTPUT=$2; shift 2 ;;
    --expect-commit) EXPECT_COMMIT=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
load_target
need python3
need ssh
[ -n "$OUTPUT" ] || die "BACKUP_DIR must name a new private directory outside the workspace"
[ -n "$EXPECT_COMMIT" ] || die "FROM_COMMIT is required for a baseline backup"
require_target_reachable
installed="$(json_field "$(remote_version_json)" commit)"
[ "$installed" = "$EXPECT_COMMIT" ] || die "installed commit changed: expected $EXPECT_COMMIT, observed $installed"
remote_sh "
[ ! -e '$REMOTE_DEPLOY_DIR/pending.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/pending.starting.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/fresh-pending.json' ]
[ ! -d '$REMOTE_DEPLOY_DIR/lock' ]
set -- '$REMOTE_DATA_DIR'/transactions/*.json
[ ! -e \"\$1\" ]
" || die "baseline has an active deployment or pending recovery"

backup_tool="$CUTOVER_DIR/backup.py"
private_args=(--forbid "$(dirname "$REPO_ROOT")" --forbid "$OPS_DIR")
identity_args=(--target "$TARGET" --host "$SSH_HOST" --commit "$EXPECT_COMMIT")
if [ -f "$OUTPUT/BACKUP_VERIFIED.json" ]; then
  spec_sha="$(ssh_target "sha256sum '$REMOTE_DATA_DIR/spec.json'" | cut -d' ' -f1)"
  python3 "$backup_tool" check "$OUTPUT" "${private_args[@]}" "${identity_args[@]}" --spec-sha "$spec_sha"
  exit 0
fi
python3 "$backup_tool" init "$OUTPUT" "${private_args[@]}"
umask 077
info "streaming persistent filesystem to private backup (no device files are changed)"
ssh_target 'set -eu
set --
for path in /* /.[!.]* /..?*; do
  [ -e "$path" ] || [ -L "$path" ] || continue
  case "$path" in /dev|/proc|/sys|/tmp|/overlay|/rom|/mnt) continue ;; esac
  set -- "$@" "${path#/}"
done
tar -czf - -C / "$@"
' >"$OUTPUT/persistent-root.tar.gz" 2>"$OUTPUT/persistent-root.stderr"

legacy_metadata="$("$REPO_ROOT/scripts/smartgate-worker-control" legacy-metadata)"
legacy_init="$(printf '%s\n' "$legacy_metadata" | sed -n '1p')"
legacy_owner="$(printf '%s\n' "$legacy_metadata" | sed -n '5p')"
{
  printf '%s\n' etc/smartgate usr/bin/smartgate etc/init.d/smartgate www/smartgate \
    usr/lib/smartgate/deploy-recover usr/libexec/smartgate-worker-control \
    etc/hotplug.d/firewall/95-smartgate etc/dnsmasq.d/smartgate.conf \
    etc/firewall.smartgate-reconcile etc/config/firewall
  printf '%s\n' "$legacy_metadata" | sed -n '3,5s#^/##p'
  printf '%s\n' "${REMOTE_LEGACY_PLANE_DIR#/}" "${legacy_init#/}" "etc/config/$(basename "$legacy_init")"
} >"$OUTPUT/recovery-paths.txt"
paths="etc usr/bin/smartgate www/smartgate"
for path in usr/lib/smartgate usr/libexec/smartgate-worker-control "${legacy_owner#/}"; do
  if ssh_target "[ -e '/$path' ]"; then paths="$paths $path"; fi
done
for bin in ${REQUIRED_WORKER_BINS:-} ${OPTIONAL_WORKER_BINS:-}; do
  path="$(ssh_target "command -v '$bin' 2>/dev/null || true")"
  [ -z "$path" ] || paths="$paths ${path#/}"
done
remote_sh "
if /etc/init.d/smartgate enabled >/dev/null 2>&1; then echo smartgate=yes; else echo smartgate=no; fi
if '$legacy_init' enabled >/dev/null 2>&1; then echo legacy=yes; else echo legacy=no; fi
" >"$OUTPUT/services.txt"
hash_sources() {
  ssh_target "set -eu
cd /
find $paths -type f -exec sha256sum {} +
" | LC_ALL=C sort
}
info "capturing baseline files and checking every persistent source file before and after"
for attempt in 1 2 3; do
  hash_sources >"$OUTPUT/v2-before.sha256"
  ssh_target "tar -czf - -C / $paths '${REMOTE_RUNTIME_DIR#/}'" \
    >"$OUTPUT/v2-files.tar.gz" 2>"$OUTPUT/v2-files.stderr"
  hash_sources >"$OUTPUT/v2-after.sha256"
  if cmp -s "$OUTPUT/v2-before.sha256" "$OUTPUT/v2-after.sha256"; then break; fi
  info "source changed during snapshot attempt $attempt; retrying the baseline file capture"
  mv "$OUTPUT/v2-files.tar.gz" "$OUTPUT/v2-files-attempt-$attempt.tar.gz"
done
python3 "$backup_tool" finalize "$OUTPUT" "${private_args[@]}" "${identity_args[@]}" \
  --legacy-directory "$REMOTE_LEGACY_PLANE_DIR"
log_deploy 08-backup.sh "installed@$installed" pass "verified private off-device backup=$OUTPUT; no secrets in ops"
