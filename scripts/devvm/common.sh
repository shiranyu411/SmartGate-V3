# Shared helpers for scripts/devvm/*.sh. Source this file; do not execute it.
set -eu

DEVVM_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
REPO_ROOT="$(cd "$DEVVM_SCRIPT_DIR/../.." && pwd)"
DEVVM_DIR="${DEVVM_DIR:-$REPO_ROOT/.devvm}"

. "$DEVVM_SCRIPT_DIR/image.env"

RAW_IMAGE="$DEVVM_DIR/${OPENWRT_IMAGE%.gz}"
QCOW_IMAGE="$DEVVM_DIR/devvm.qcow2"
PID_FILE="$DEVVM_DIR/qemu.pid"
MONITOR_SOCKET="$DEVVM_DIR/qemu-monitor.sock"
SERIAL_LOG="$DEVVM_DIR/serial.log"
CONSOLE_PIPE="$DEVVM_DIR/console"
SSH_KEY="$DEVVM_DIR/id_devvm"
SSH_PORT="$HOSTFWD_SSH"

die() { printf 'devvm: %s\n' "$*" >&2; exit 1; }
info() { printf 'devvm: %s\n' "$*"; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing $1; install ${2:-$1} first"; }

find_edk2_firmware() {
  local prefix candidate
  prefix="$(brew --prefix qemu 2>/dev/null || brew --prefix 2>/dev/null || echo /opt/homebrew)"
  for candidate in \
    "$prefix/share/qemu/edk2-aarch64-code.fd" \
    "$prefix/share/qemu/edk2-arm-code.fd" \
    "/opt/homebrew/share/qemu/edk2-aarch64-code.fd" \
    "/usr/local/share/qemu/edk2-aarch64-code.fd"; do
    [ -f "$candidate" ] && { printf '%s' "$candidate"; return 0; }
  done
  return 1
}

vm_running() {
  [ -f "$PID_FILE" ] || return 1
  kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

ssh_vm() {
  [ -f "$SSH_KEY" ] || die "SSH key is missing; run provision.sh"
  ssh -p "$SSH_PORT" -i "$SSH_KEY" \
    -o BatchMode=yes \
    -o PasswordAuthentication=no \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o IdentitiesOnly=yes \
    -o LogLevel=ERROR \
    -o ConnectTimeout=5 \
    "root@127.0.0.1" "$@"
}

console_send() {
  printf '%s\n' "$*" > "$CONSOLE_PIPE.in"
}

console_wait() {
  local pattern="$1" timeout="${2:-120}" waited=0
  while [ "$waited" -lt "$timeout" ]; do
    grep -q "$pattern" "$SERIAL_LOG" 2>/dev/null && return 0
    sleep 2
    waited=$((waited + 2))
  done
  return 1
}

scp_vm() {
  [ -f "$SSH_KEY" ] || die "SSH key is missing; run provision.sh"
  scp -O -P "$SSH_PORT" -i "$SSH_KEY" \
    -o BatchMode=yes \
    -o PasswordAuthentication=no \
    -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    "$@"
}
