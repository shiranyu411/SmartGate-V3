#!/usr/bin/env bash
# Roll the target back to the pre-upgrade release.
#
#   TARGET=router scripts/cutover/40-rollback.sh --auto
#       Runs the target's deploy-recover helper. This is the supported path
#       whenever a pending deployment journal exists (an upgrade that failed
#       after the backup point, or a boot that interrupted one). It restores
#       the retained before-*.tar.gz, validates the data plane for the old
#       schema, and clears the journal.
#
#   TARGET=router scripts/cutover/40-rollback.sh --list
#       Lists journals and retained backups on the target.
#
#   TARGET=router scripts/cutover/40-rollback.sh --guide [before-<stamp>.tar.gz]
#       There is no automated path yet for rolling back an upgrade that
#       already passed all gates (no journal). This prints the manual
#       procedure with the target's real paths filled in and exits 2. See
#       docs/RELEASE_READINESS.md item B20 for the automation work.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

MODE=""
BACKUP=""
PRIVATE_BACKUP=""
while [ $# -gt 0 ]; do
  case "$1" in
    --auto) MODE=auto; shift ;;
    --list) MODE=list; shift ;;
    --guide) MODE=guide; shift; [ $# -gt 0 ] && [ "${1#--}" = "$1" ] && { BACKUP=$1; shift; } ;;
    --from-backup) MODE=private; PRIVATE_BACKUP=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$MODE" ] || die "one of --auto | --list | --guide | --from-backup <directory> is required"

load_target
need ssh
require_target_reachable

list_state() {
  remote_sh "
echo '## journals'
ls -la $REMOTE_DEPLOY_DIR/pending.json $REMOTE_DEPLOY_DIR/pending.starting.json $REMOTE_DEPLOY_DIR/fresh-pending.json 2>/dev/null || echo none
echo '## retained backups'
ls -la $REMOTE_DEPLOY_DIR/before-*.tar.gz 2>/dev/null || echo none
echo '## recovery helper'
ls -la $REMOTE_DEPLOY_DIR/deploy-recover /usr/lib/smartgate/deploy-recover 2>/dev/null || echo none
echo '## installed'
cat $REMOTE_DATA_DIR/version.json 2>/dev/null || echo '{}'
"
}

case "$MODE" in
  list)
    list_state
    ;;
  private)
    need python3
    need shasum
    before="$(json_field "$(remote_version_json)" commit)"
    baseline="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$PRIVATE_BACKUP/BACKUP_VERIFIED.json")"
    python3 "$CUTOVER_DIR/backup.py" check "$PRIVATE_BACKUP" \
      --forbid "$(dirname "$REPO_ROOT")" --forbid "$OPS_DIR" \
      --target "$TARGET" --host "$SSH_HOST" --commit "$baseline"
    artifact="$REPO_ROOT/dist/smartgate-openwrt-aarch64.tar.gz"
    gate="$REPO_ROOT/dist/release/GATE_PASSED"
    [ -f "$gate" ] || die "a gated V3 recovery helper is required"
    gate_sha="$(sed -n 's/^sha256=//p' "$gate")"
    [ "$(shasum -a 256 "$artifact" | cut -d' ' -f1)" = "$gate_sha" ] || die "recovery artifact does not match GATE_PASSED"
    required_kb="$(python3 -c '
import os,sys,tarfile
with tarfile.open(sys.argv[1]) as backup, tarfile.open(sys.argv[2]) as release:
    total = sum(item.size for item in backup if item.isfile())
    total += release.getmember("smartgate").size + os.path.getsize(sys.argv[1])
    print((total + 5*1024*1024 + 1023)//1024)
' "$PRIVATE_BACKUP/v2-rollback.tar.gz" "$artifact")"
    available_kb="$(ssh_target "df -k / | awk 'NR==2 {print \$4}'")"
    [ "$available_kb" -ge "$required_kb" ] || die "recovery needs ${required_kb}k free, only ${available_kb}k available"
    remote_sh "
[ ! -e '$REMOTE_DEPLOY_DIR/pending.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/pending.starting.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/fresh-pending.json' ]
[ ! -d '$REMOTE_DEPLOY_DIR/lock' ]
" || die "pending recovery already exists; use --auto first"
    confirm_production rollback
    umask 077
    local_work="$(mktemp -d)"
    trap 'rm -rf "$local_work"' EXIT
    tar -xzf "$artifact" -C "$local_work" smartgate scripts/deploy-recover.sh libexec/smartgate-worker-control
    python3 "$CUTOVER_DIR/backup.py" restore-hashes "$PRIVATE_BACKUP" >"$local_work/payload.sha256"
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    work="$REMOTE_DEPLOY_DIR/work-private-restore-$stamp"
    remote_backup="$REMOTE_DEPLOY_DIR/private-v2-$stamp.tar.gz"
    backup_sha="$(shasum -a 256 "$PRIVATE_BACKUP/v2-rollback.tar.gz" | cut -d' ' -f1)"
    log_deploy 40-rollback.sh "restored@$baseline" started "private full baseline restore from=$before backup=$PRIVATE_BACKUP"
    remote_sh "
umask 077
mkdir -p '$work/stage'
chmod 0700 '$work'
: >'$remote_backup'
chmod 0600 '$remote_backup'
"
    scp_to_target "$PRIVATE_BACKUP/v2-rollback.tar.gz" "$remote_backup" >/dev/null
    scp_to_target "$local_work/smartgate" "$work/stage/smartgate" >/dev/null
    scp_to_target "$local_work/libexec/smartgate-worker-control" "$work/stage/worker-control" >/dev/null
    scp_to_target "$local_work/scripts/deploy-recover.sh" "$work/deploy-recover" >/dev/null
    scp_to_target "$local_work/payload.sha256" "$work/payload.sha256" >/dev/null
    set +e
    remote_sh "
[ \"\$(sha256sum '$remote_backup' | cut -d' ' -f1)\" = '$backup_sha' ]
tar -tzf '$remote_backup' >/dev/null
chmod 0755 '$work/stage/smartgate' '$work/stage/worker-control' '$work/deploy-recover'
[ ! -e '$REMOTE_DEPLOY_DIR/pending.json' ]
[ ! -e '$REMOTE_DEPLOY_DIR/pending.starting.json' ]
mkdir '$REMOTE_DEPLOY_DIR/lock'
printf '%s\n%s\n%s\n' \"\$\$\" \"\$(cat /proc/sys/kernel/random/boot_id)\" \"\$(awk '{print \$22}' /proc/\$\$/stat)\" >'$REMOTE_DEPLOY_DIR/lock/owner'
cp '$work/deploy-recover' '$REMOTE_DEPLOY_DIR/deploy-recover'
printf '#!/bin/sh\n%s --boot-hook\n' '$REMOTE_DEPLOY_DIR/deploy-recover' >/etc/rc.d/S01smartgate-deploy-recover
chmod 0755 /etc/rc.d/S01smartgate-deploy-recover
sync
printf '{\"phase\":\"backup_complete\",\"backup\":\"%s\",\"work\":\"%s\"}\n' '$remote_backup' '$work' >'$REMOTE_DEPLOY_DIR/pending.json.tmp'
mv '$REMOTE_DEPLOY_DIR/pending.json.tmp' '$REMOTE_DEPLOY_DIR/pending.json'
sync
SMARTGATE_API_BASE='$API_URL' '$REMOTE_DEPLOY_DIR/deploy-recover'
" 2>&1 | redact
    rc=${PIPESTATUS[0]}
    set -e
    after="$(json_field "$(remote_version_json)" commit)"
    if [ "$rc" -ne 0 ] || [ "$after" != "$baseline" ]; then
      log_deploy 40-rollback.sh "restored@$baseline" fail "private recovery exit=$rc installed=$after; retained journal and backup"
      die "private recovery did not pass; keep the journal and retry --auto"
    fi
    log_deploy 40-rollback.sh "restored@$baseline" ok "private full baseline restore from=$before; data-plane gates passed"
    info "SmartGate $baseline restored from verified private backup"
    ;;
  auto)
    state="$(list_state)"
    printf '%s\n' "$state"
    case "$state" in
      *pending.json*|*pending.starting.json*) ;;
      *) die "no pending deployment journal on target; automatic recovery has nothing to do. For a post-success rollback run: $0 --guide" ;;
    esac
    helper="$(ssh_target "for h in $REMOTE_DEPLOY_DIR/deploy-recover /usr/lib/smartgate/deploy-recover; do [ -x \"\$h\" ] && { echo \"\$h\"; break; }; done")"
    [ -n "$helper" ] || die "deploy-recover helper missing on target"
    confirm_production rollback
    before="$(json_field "$(remote_version_json)" commit)"
    set +e
    ssh_target "SMARTGATE_API_BASE='$API_URL' '$helper'" 2>&1 | redact
    rc=${PIPESTATUS[0]}
    set -e
    after="$(json_field "$(remote_version_json)" commit)"
    if [ "$rc" -eq 0 ]; then
      log_deploy 40-rollback.sh "restored@${after:-?}" ok "auto from=${before:-?}"
      info "recovery finished; installed commit now ${after:-?}. Run 30-verify.sh with --expect-commit ${after:-<sha>}"
    else
      log_deploy 40-rollback.sh "from=${before:-?}" fail "auto exit=$rc"
      die "deploy-recover exited $rc; keep journal and backups, capture evidence (ops/collect-evidence.sh), and recover via console/SSH per docs/RELEASE_RUNBOOK.md"
    fi
    ;;
  guide)
    state="$(list_state)"
    printf '%s\n' "$state"
    if [ -z "$BACKUP" ]; then
      BACKUP="$(printf '%s\n' "$state" | sed -n 's/.*\(before-[0-9TZ]*\.tar\.gz\).*/\1/p' | tail -n1)"
    fi
    cat <<EOF

MANUAL ROLLBACK GUIDE (target=$TARGET host=$SSH_HOST) — not automated; see RELEASE_READINESS B20
Backup to restore: ${REMOTE_DEPLOY_DIR}/${BACKUP:-<pick a before-*.tar.gz from the list above>}

Preconditions
  1. Keep a serial/SSH recovery route open. Do not run this from the LAN you are about to change.
  2. Capture evidence first:  ops/collect-evidence.sh $TARGET pre-rollback

Procedure (run on the target as root, one step at a time)
  3. /etc/init.d/smartgate stop
  4. Remove V3 ownership of the data plane with the currently installed (V3) binary:
       SMARTGATE_RESCUE_EXECUTE=1 /usr/bin/smartgate rescue -data-dir $REMOTE_DATA_DIR -runtime-dir $REMOTE_RUNTIME_DIR enter
     then confirm nothing V3 remains:
       nft list table inet smartgate            # must fail (table gone)
       netstat -ln | grep ':1053 '              # must be empty
       ls /etc/hotplug.d/firewall/95-smartgate /etc/dnsmasq.d/smartgate.conf $REMOTE_DATA_DIR/fabric-reconcile.sh   # must not exist
  5. Stop any V3 Workers still up:
       for w in \$(jsonfilter -i $REMOTE_DATA_DIR/applied-state.json -e '@.execution_graph.workers[*].id'); do /usr/libexec/smartgate-worker-control stop "\$w"; done
  6. Restore the retained backup payload:
       R=$REMOTE_DEPLOY_DIR/manual-restore-\$(date +%s); mkdir -p "\$R"
       tar -xzf ${REMOTE_DEPLOY_DIR}/${BACKUP:-before-<stamp>.tar.gz} -C "\$R"
       cp -a "\$R/usr/bin/smartgate" /usr/bin/smartgate
       cp -a "\$R/etc/init.d/smartgate" /etc/init.d/smartgate
       rm -rf /www/smartgate && cp -a "\$R/www/smartgate" /www/smartgate
       rm -rf $REMOTE_DATA_DIR && cp -a "\$R/etc/smartgate" $REMOTE_DATA_DIR
       [ -n "${REMOTE_LEGACY_PLANE_DIR:-}" ] && [ -d "\$R${REMOTE_LEGACY_PLANE_DIR}" ] && cp -a "\$R${REMOTE_LEGACY_PLANE_DIR}/." ${REMOTE_LEGACY_PLANE_DIR}/
       [ -f "\$R/etc/hotplug.d/firewall/95-smartgate" ] && cp -a "\$R/etc/hotplug.d/firewall/95-smartgate" /etc/hotplug.d/firewall/95-smartgate
       [ -f "\$R/etc/dnsmasq.d/smartgate.conf" ] && cp -a "\$R/etc/dnsmasq.d/smartgate.conf" /etc/dnsmasq.d/smartgate.conf
  7. Clear the V3 rescue flag written in step 4 and start the restored (V2) service:
       rm -f $REMOTE_DATA_DIR/smartgate-rescue.status $REMOTE_RUNTIME_DIR/smartgate-rescue.status
       /etc/init.d/smartgate start
       /etc/init.d/firewall reload; /etc/init.d/dnsmasq restart
  8. Verify V2 is back:
       cat $REMOTE_DATA_DIR/version.json         # commit must be the V2 production commit from STATUS.md
       wget -qO- $API_URL/healthz                 # ok
     then from the workstation:
       TARGET=$TARGET scripts/cutover/30-verify.sh --token-file <v2 token> --expect-commit <v2 sha> --slug post-rollback
  9. Record: append to ops/deployments.log and update STATUS.md (实机 = V2 again), open ops/incidents/<id>.md.

If any step fails, stop and collect evidence; do not improvise further changes on the router.
EOF
    log_deploy 40-rollback.sh "guide" info "manual guide printed backup=${BACKUP:-?}"
    exit 2
    ;;
esac
