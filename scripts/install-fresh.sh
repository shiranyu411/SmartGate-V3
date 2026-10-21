#!/bin/sh
set -eu

# Transactional first installation for a completely blank OpenWrt target.
# Existing installations must use deploy-safe.sh instead.

umask 077

ROOT=${SMARTGATE_RELEASE_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
TARGET_ROOT=${SMARTGATE_TARGET_ROOT:-}
case "$TARGET_ROOT" in
  ""|/*) ;;
  *) echo "install-fresh: SMARTGATE_TARGET_ROOT must be absolute" >&2; exit 1 ;;
esac
TARGET_ROOT=${TARGET_ROOT%/}

NEW_BINARY=${SMARTGATE_BINARY:-$ROOT/smartgate}
NEW_INIT=${SMARTGATE_INIT_SCRIPT:-$ROOT/configs/init.d/smartgate}
NEW_WWW=${SMARTGATE_WWW_SRC:-$ROOT/www/smartgate}
NEW_VERSION=${SMARTGATE_VERSION_SRC:-$ROOT/version.json}
NEW_RECOVERY=${SMARTGATE_RECOVERY_SRC:-$ROOT/scripts/deploy-recover.sh}
NEW_WORKER_CONTROL=${SMARTGATE_WORKER_CONTROL_SRC:-$ROOT/libexec/smartgate-worker-control}
NEW_MANIFEST=${SMARTGATE_MANIFEST:-$ROOT/SHA256SUMS}

INSTALL_BINARY=${SMARTGATE_INSTALLED_BINARY:-$TARGET_ROOT/usr/bin/smartgate}
INSTALL_INIT=${SMARTGATE_INSTALLED_INIT:-$TARGET_ROOT/etc/init.d/smartgate}
INSTALL_WWW=${SMARTGATE_INSTALLED_WWW_DIR:-$TARGET_ROOT/www/smartgate}
DATA_DIR=${SMARTGATE_INSTALLED_DATA_DIR:-$TARGET_ROOT/etc/smartgate}
RUNTIME_DIR=${SMARTGATE_INSTALLED_RUNTIME_DIR:-$TARGET_ROOT/tmp/smartgate}
INSTALL_VERSION=${SMARTGATE_INSTALLED_VERSION:-$DATA_DIR/version.json}
INSTALL_RECOVERY=${SMARTGATE_INSTALLED_RECOVERY:-$TARGET_ROOT/usr/lib/smartgate/deploy-recover}
INSTALL_WORKER_CONTROL=${SMARTGATE_INSTALLED_WORKER_CONTROL:-$TARGET_ROOT/usr/libexec/smartgate-worker-control}
RC_DIR=${SMARTGATE_RC_DIR:-$TARGET_ROOT/etc/rc.d}

OPENWRT_RELEASE=${SMARTGATE_OPENWRT_RELEASE:-$TARGET_ROOT/etc/openwrt_release}
PROCD_BINARY=${SMARTGATE_PROCD_BINARY:-$TARGET_ROOT/sbin/procd}

API_BASE=${SMARTGATE_API_BASE:-http://192.168.1.1:3000}
API_ATTEMPTS=${SMARTGATE_INSTALL_API_ATTEMPTS:-30}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
DEPLOY_ROOT=${SMARTGATE_DEPLOY_ROOT:-$TARGET_ROOT/root/smartgate-deployments}
WORK=$DEPLOY_ROOT/fresh-work-$STAMP-$$
DEPLOY_LOCK=$DEPLOY_ROOT/lock
LOCK_OWNER=$DEPLOY_LOCK/owner
DEPLOY_JOURNAL=$DEPLOY_ROOT/pending.json
DEPLOY_STARTING_JOURNAL=$DEPLOY_ROOT/pending.starting.json
FRESH_JOURNAL=$DEPLOY_ROOT/fresh-pending.json
RECOVERY_COPY=$DEPLOY_ROOT/install-fresh-recover
BOOT_HOOK=${SMARTGATE_FRESH_BOOT_HOOK:-$RC_DIR/S01smartgate-install-recover}
GATE=${SMARTGATE_DEPLOY_GATE:-$RUNTIME_DIR/deploy-scheduler-disabled}

RECOVERY_ONLY=0
BOOT_RECOVERY=0
for arg in "$@"; do
  case "$arg" in
    --recover) RECOVERY_ONLY=1 ;;
    --boot-hook) RECOVERY_ONLY=1; BOOT_RECOVERY=1 ;;
    *) echo "usage: install-fresh.sh [--recover] [--boot-hook]" >&2; exit 2 ;;
  esac
done

SUCCESS=0
LOCK_ACQUIRED=0
PRESERVE_LOCK=0
MUTATION_STARTED=0

fail() { echo "install-fresh: $*" >&2; exit 1; }

release_lock() {
  [ "$LOCK_ACQUIRED" -eq 1 ] || return 0
  rm -f "$LOCK_OWNER"
  rmdir "$DEPLOY_LOCK" 2>/dev/null || true
  LOCK_ACQUIRED=0
}

acquire_lock() {
  mkdir -p "$DEPLOY_ROOT"
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
    rm -rf "$DEPLOY_LOCK"
    mkdir "$DEPLOY_LOCK" || fail "could not acquire deployment lock"
    LOCK_ACQUIRED=1
  fi
  printf '%s\n%s\n%s\n' "$$" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)" "$(awk '{print $22}' /proc/$$/stat 2>/dev/null || true)" >"$LOCK_OWNER"
}

service_stop_disable() {
  [ -x "$INSTALL_INIT" ] || return 0
  "$INSTALL_INIT" stop >/dev/null 2>&1 || true
  if [ -z "$TARGET_ROOT" ]; then
    attempt=0
    while pidof smartgate >/dev/null 2>&1 && [ "$attempt" -lt 20 ]; do
      sleep 1
      attempt=$((attempt + 1))
    done
    pidof smartgate >/dev/null 2>&1 && return 1
  fi
  "$INSTALL_INIT" disable >/dev/null 2>&1 || true
  return 0
}

remove_fresh_payload() {
  set +e
  cleanup_ok=1
  service_stop_disable || cleanup_ok=0
  journal_work=
  if [ -e "$FRESH_JOURNAL" ]; then
    journal_work=$(sed -n 's/.*"work":"\([^"]*\)".*/\1/p' "$FRESH_JOURNAL" 2>/dev/null || true)
    journal_prefix=$DEPLOY_ROOT/fresh-work-
    case "$journal_work" in
      "$journal_prefix"*)
        journal_tail=${journal_work#"$journal_prefix"}
        case "$journal_tail" in ""|*/*|.|..) cleanup_ok=0 ;; esac
        ;;
      *) cleanup_ok=0 ;;
    esac
  fi
  if [ "$cleanup_ok" -eq 1 ]; then
    rm -f "$GATE"
    rm -f "$RC_DIR"/S??smartgate "$RC_DIR"/K??smartgate
    rm -f "$INSTALL_BINARY" "$INSTALL_BINARY.new" "$INSTALL_INIT" "$INSTALL_INIT.new"
    rm -f "$INSTALL_VERSION" "$INSTALL_VERSION.new"
    rm -f "$INSTALL_RECOVERY" "$INSTALL_RECOVERY.new"
    rm -f "$INSTALL_WORKER_CONTROL" "$INSTALL_WORKER_CONTROL.new"
    rm -rf "$INSTALL_WWW" "$INSTALL_WWW.new" "$DATA_DIR" "$RUNTIME_DIR"
    rmdir "$(dirname "$INSTALL_RECOVERY")" 2>/dev/null || true
    rm -rf "$WORK"
    [ -z "$journal_work" ] || rm -rf "$journal_work"
    rm -f "$FRESH_JOURNAL" "$FRESH_JOURNAL.tmp" "$BOOT_HOOK" "$BOOT_HOOK.new" "$RECOVERY_COPY.new"
    sync
  fi
  set -e
  [ "$cleanup_ok" -eq 1 ]
}

cleanup() {
  code=$?
  trap - EXIT HUP INT TERM
  if [ "$SUCCESS" -ne 1 ]; then
    if [ "$MUTATION_STARTED" -eq 1 ] || [ -e "$FRESH_JOURNAL" ]; then
      echo "install-fresh: installation did not complete; removing fresh payload" >&2
      if remove_fresh_payload; then
        rm -f "$RECOVERY_COPY" "$RECOVERY_COPY.new"
      else
        PRESERVE_LOCK=1
        echo "install-fresh: automatic cleanup failed; run $RECOVERY_COPY --recover" >&2
      fi
    else
      rm -rf "$WORK"
      rm -f "$BOOT_HOOK" "$BOOT_HOOK.new" "$RECOVERY_COPY" "$RECOVERY_COPY.new"
    fi
  fi
  if [ "$PRESERVE_LOCK" -ne 1 ]; then
    release_lock
  fi
  exit "$code"
}
trap cleanup EXIT HUP INT TERM

write_journal() {
  phase=$1
  printf '{"kind":"fresh_install","phase":"%s","work":"%s"}\n' "$phase" "$WORK" >"$FRESH_JOURNAL.tmp"
  mv "$FRESH_JOURNAL.tmp" "$FRESH_JOURNAL"
  sync
}

managed_payload_absent() {
  for path in "$INSTALL_BINARY" "$INSTALL_INIT" "$INSTALL_WWW" "$DATA_DIR" "$INSTALL_RECOVERY" "$INSTALL_WORKER_CONTROL"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || return 1
    [ ! -e "$path.new" ] && [ ! -L "$path.new" ] || return 1
  done
  set -- "$RC_DIR"/S??smartgate "$RC_DIR"/K??smartgate
  [ ! -e "$1" ] && [ ! -L "$1" ] && [ ! -e "$2" ] && [ ! -L "$2" ]
}

verify_platform() {
  if [ -z "$TARGET_ROOT" ] && [ "$(id -u)" != 0 ]; then
    fail "must run as root on the target router"
  fi
  [ -f "$OPENWRT_RELEASE" ] || fail "OpenWrt/ImmortalWrt marker missing: $OPENWRT_RELEASE"
  [ -x "$PROCD_BINARY" ] || fail "OpenWrt procd missing: $PROCD_BINARY"
  for command_name in sha256sum wget sed awk cp mv rm mkdir chmod date sync; do
    command -v "$command_name" >/dev/null 2>&1 || fail "required command missing: $command_name"
  done
  SMARTGATE_WORKER_ROOT="$TARGET_ROOT" "$NEW_WORKER_CONTROL" verify-prerequisites || fail "Worker prerequisites are not satisfied"
}

verify_release() {
  [ -x "$NEW_BINARY" ] || fail "new binary missing or not executable: $NEW_BINARY"
  [ -f "$NEW_INIT" ] || fail "new init script missing: $NEW_INIT"
  [ -f "$NEW_WWW/index.html" ] || fail "new frontend missing: $NEW_WWW/index.html"
  [ -f "$NEW_VERSION" ] || fail "new version metadata missing: $NEW_VERSION"
  [ -f "$NEW_RECOVERY" ] || fail "deployment recovery helper missing: $NEW_RECOVERY"
  [ -f "$NEW_WORKER_CONTROL" ] || fail "Worker control helper missing: $NEW_WORKER_CONTROL"
  [ -f "$NEW_MANIFEST" ] || fail "release hash manifest missing: $NEW_MANIFEST"
  (cd "$ROOT" && sha256sum -c "$(basename "$NEW_MANIFEST")") >/dev/null || fail "release payload hash verification failed"
  "$NEW_BINARY" locks >/dev/null || fail "new binary lock hierarchy self-check failed"
}

api_get() {
  path=$1
  output=$2
  wget -q -T 5 -O "$output" "$API_BASE$path"
}

wait_fresh_api() {
  suffix=$1
  attempt=0
  while [ "$attempt" -lt "$API_ATTEMPTS" ]; do
    attempt=$((attempt + 1))
    if api_get /healthz "$WORK/healthz-$suffix" &&
       grep -Fx 'ok' "$WORK/healthz-$suffix" >/dev/null &&
       api_get /api/auth/check "$WORK/auth-$suffix" &&
       grep -E '"needs_setup"[[:space:]]*:[[:space:]]*true' "$WORK/auth-$suffix" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

recover_fresh_install() {
  if [ ! -e "$FRESH_JOURNAL" ]; then
    rm -f "$BOOT_HOOK"
    [ "$BOOT_RECOVERY" -ne 1 ] || rm -f "$RECOVERY_COPY"
    return 0
  fi
  acquire_lock
  MUTATION_STARTED=1
  remove_fresh_payload || fail "could not stop and remove incomplete fresh installation"
  rm -f "$RECOVERY_COPY" "$RECOVERY_COPY.new"
  release_lock
  SUCCESS=1
  trap - EXIT HUP INT TERM
  echo "install-fresh: incomplete fresh installation removed; retry scripts/install.sh"
}

if [ "$RECOVERY_ONLY" -eq 1 ]; then
  recover_fresh_install
  exit 0
fi

[ ! -e "$FRESH_JOURNAL" ] || fail "pending fresh-install recovery exists; run $RECOVERY_COPY --recover"
[ ! -e "$DEPLOY_JOURNAL" ] && [ ! -e "$DEPLOY_STARTING_JOURNAL" ] || fail "pending upgrade recovery exists; run $DEPLOY_ROOT/deploy-recover first"
verify_release
verify_platform
managed_payload_absent || fail "target is not blank; existing or partial SmartGate payload detected"

acquire_lock
[ ! -e "$FRESH_JOURNAL" ] || fail "pending fresh-install recovery appeared while acquiring lock"
[ ! -e "$DEPLOY_JOURNAL" ] && [ ! -e "$DEPLOY_STARTING_JOURNAL" ] || fail "pending upgrade recovery appeared while acquiring lock"
managed_payload_absent || fail "SmartGate payload appeared while acquiring lock"

mkdir -p "$WORK/stage" "$RC_DIR" "$(dirname "$INSTALL_BINARY")" "$(dirname "$INSTALL_INIT")" "$(dirname "$INSTALL_RECOVERY")" "$(dirname "$INSTALL_WORKER_CONTROL")" "$(dirname "$INSTALL_WWW")" "$(dirname "$DATA_DIR")" "$(dirname "$RUNTIME_DIR")"
cp "$NEW_BINARY" "$WORK/stage/smartgate"
cp "$NEW_INIT" "$WORK/stage/init"
cp "$NEW_VERSION" "$WORK/stage/version.json"
cp "$NEW_RECOVERY" "$WORK/stage/deploy-recover"
cp "$NEW_WORKER_CONTROL" "$WORK/stage/worker-control"
cp -a "$NEW_WWW" "$WORK/stage/www"
chmod 0755 "$WORK/stage/smartgate" "$WORK/stage/init" "$WORK/stage/deploy-recover" "$WORK/stage/worker-control"

cp "$0" "$RECOVERY_COPY.new"
chmod 0755 "$RECOVERY_COPY.new"
mv "$RECOVERY_COPY.new" "$RECOVERY_COPY"
printf '#!/bin/sh\nexec "%s" --recover --boot-hook\n' "$RECOVERY_COPY" >"$BOOT_HOOK.new"
chmod 0755 "$BOOT_HOOK.new"
mv "$BOOT_HOOK.new" "$BOOT_HOOK"
write_journal prepared
MUTATION_STARTED=1

mkdir -p "$DATA_DIR" "$RUNTIME_DIR"
chmod 0700 "$DATA_DIR"
chmod 0700 "$RUNTIME_DIR"
touch "$GATE"

cp "$WORK/stage/smartgate" "$INSTALL_BINARY.new"
chmod 0755 "$INSTALL_BINARY.new"
mv "$INSTALL_BINARY.new" "$INSTALL_BINARY"
cp "$WORK/stage/init" "$INSTALL_INIT.new"
chmod 0755 "$INSTALL_INIT.new"
mv "$INSTALL_INIT.new" "$INSTALL_INIT"
cp "$WORK/stage/deploy-recover" "$INSTALL_RECOVERY.new"
chmod 0755 "$INSTALL_RECOVERY.new"
mv "$INSTALL_RECOVERY.new" "$INSTALL_RECOVERY"
cp "$WORK/stage/worker-control" "$INSTALL_WORKER_CONTROL.new"
chmod 0755 "$INSTALL_WORKER_CONTROL.new"
mv "$INSTALL_WORKER_CONTROL.new" "$INSTALL_WORKER_CONTROL"
rm -rf "$INSTALL_WWW.new"
cp -a "$WORK/stage/www" "$INSTALL_WWW.new"
mv "$INSTALL_WWW.new" "$INSTALL_WWW"
cp "$WORK/stage/version.json" "$INSTALL_VERSION.new"
mv "$INSTALL_VERSION.new" "$INSTALL_VERSION"
write_journal payload_installed

"$INSTALL_INIT" enable
write_journal service_enabled
"$INSTALL_INIT" start
write_journal service_started
wait_fresh_api gated || fail "new API did not become healthy with auth setup pending"

rm -f "$GATE"
"$INSTALL_INIT" restart
wait_fresh_api resumed || fail "new API failed after scheduler resume"

sync
rm -f "$FRESH_JOURNAL"
sync
SUCCESS=1
rm -f "$BOOT_HOOK" "$BOOT_HOOK.new" "$RECOVERY_COPY" "$RECOVERY_COPY.new"
rm -rf "$WORK"
release_lock
trap - EXIT HUP INT TERM
echo "install-fresh: installation passed health and auth-setup gates"
