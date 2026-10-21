#!/bin/sh
set -eu

# Release entry point. Existing installations always use the guarded upgrade
# path; a completely blank OpenWrt target uses the separate fresh-bootstrap
# transaction. Ambiguous/partial installations fail closed.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TARGET_ROOT=${SMARTGATE_TARGET_ROOT:-}
case "$TARGET_ROOT" in
  ""|/*) ;;
  *) echo "install: SMARTGATE_TARGET_ROOT must be absolute" >&2; exit 1 ;;
esac
TARGET_ROOT=${TARGET_ROOT%/}

DEPLOY_ROOT=${SMARTGATE_DEPLOY_ROOT:-$TARGET_ROOT/root/smartgate-deployments}
FRESH_JOURNAL=$DEPLOY_ROOT/fresh-pending.json
FRESH_RECOVERY=$DEPLOY_ROOT/install-fresh-recover
FRESH_BOOT_HOOK=${SMARTGATE_FRESH_BOOT_HOOK:-$TARGET_ROOT/etc/rc.d/S01smartgate-install-recover}

INSTALLED_BINARY=${SMARTGATE_INSTALLED_BINARY:-$TARGET_ROOT/usr/bin/smartgate}
INSTALLED_INIT=${SMARTGATE_INSTALLED_INIT:-$TARGET_ROOT/etc/init.d/smartgate}
INSTALLED_DATA=${SMARTGATE_INSTALLED_DATA_DIR:-$TARGET_ROOT/etc/smartgate}
INSTALLED_WWW=${SMARTGATE_INSTALLED_WWW_DIR:-$TARGET_ROOT/www/smartgate}
INSTALLED_RECOVERY=${SMARTGATE_INSTALLED_RECOVERY:-$TARGET_ROOT/usr/lib/smartgate/deploy-recover}
INSTALLED_WORKER_CONTROL=${SMARTGATE_INSTALLED_WORKER_CONTROL:-$TARGET_ROOT/usr/libexec/smartgate-worker-control}

FRESH_SCRIPT=${SMARTGATE_FRESH_INSTALL_SCRIPT:-$SCRIPT_DIR/install-fresh.sh}
DEPLOY_SCRIPT=${SMARTGATE_DEPLOY_SAFE_SCRIPT:-$SCRIPT_DIR/deploy-safe.sh}

fail() { echo "install: $*" >&2; exit 1; }

if [ "$#" -gt 0 ] && [ -n "$1" ]; then
  SMARTGATE_BINARY=$1
  export SMARTGATE_BINARY
fi

if [ -e "$FRESH_JOURNAL" ]; then
  [ -x "$FRESH_RECOVERY" ] || fail "fresh-install recovery is pending but helper is missing: $FRESH_RECOVERY"
  "$FRESH_RECOVERY" --recover || fail "fresh-install recovery failed; inspect $FRESH_JOURNAL"
fi

if [ -x "$INSTALLED_BINARY" ] && [ -x "$INSTALLED_INIT" ] && [ -d "$INSTALLED_DATA" ]; then
  exec "$DEPLOY_SCRIPT"
fi

set -- \
  "$INSTALLED_BINARY" \
  "$INSTALLED_INIT" \
  "$INSTALLED_DATA" \
  "$INSTALLED_WWW" \
  "$INSTALLED_RECOVERY" \
  "$INSTALLED_WORKER_CONTROL" \
  "$FRESH_RECOVERY" \
  "$FRESH_BOOT_HOOK"
for path do
  { [ ! -e "$path" ] && [ ! -L "$path" ] && [ ! -e "$path.new" ] && [ ! -L "$path.new" ]; } || fail "partial SmartGate installation detected at $path; recover or remove the residue before retrying"
done

for pattern in "$TARGET_ROOT"/etc/rc.d/S??smartgate "$TARGET_ROOT"/etc/rc.d/K??smartgate; do
  [ ! -e "$pattern" ] && [ ! -L "$pattern" ] || fail "partial SmartGate service enablement detected at $pattern; recover or remove the residue before retrying"
done

[ -x "$FRESH_SCRIPT" ] || fail "fresh installer missing or not executable: $FRESH_SCRIPT"
exec "$FRESH_SCRIPT"
