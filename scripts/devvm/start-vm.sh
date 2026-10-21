#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

need qemu-system-aarch64 qemu

DETACH=0
case "${1:-}" in
  "") ;;
  -d) DETACH=1 ;;
  *) die "usage: start-vm.sh [-d]" ;;
esac

[ -f "$QCOW_IMAGE" ] || die "image is missing; run fetch-image.sh"
vm_running && die "VM is already running (pid $(cat "$PID_FILE"))"

FIRMWARE="$(find_edk2_firmware)" || die "edk2 AArch64 firmware not found"
LAN_NET="net=192.168.1.0/24,host=192.168.1.2,dhcpstart=192.168.1.100"
LAN_FWD="hostfwd=tcp:127.0.0.1:$HOSTFWD_SSH-192.168.1.1:22"
LAN_FWD="$LAN_FWD,hostfwd=tcp:127.0.0.1:$HOSTFWD_LUCI-192.168.1.1:80"
LAN_FWD="$LAN_FWD,hostfwd=tcp:127.0.0.1:$HOSTFWD_SMARTGATE-192.168.1.1:3000"

if [ -n "${DEVVM_EXTRA_HOSTFWD:-}" ]; then
  for entry in $(printf '%s' "$DEVVM_EXTRA_HOSTFWD" | tr ',' ' '); do
    case "$entry" in
      tcp::*-192.168.1.1:*|udp::*-192.168.1.1:*) LAN_FWD="$LAN_FWD,hostfwd=$entry" ;;
      *) die "invalid DEVVM_EXTRA_HOSTFWD entry: $entry" ;;
    esac
  done
fi

set -- \
  -M virt -cpu host -accel hvf \
  -m "$VM_MEMORY_MB" -smp "$VM_CPUS" \
  -drive "if=pflash,format=raw,readonly=on,file=$FIRMWARE" \
  -drive "file=$QCOW_IMAGE,format=qcow2,if=virtio" \
  -device virtio-net-pci,netdev=lan \
  -netdev "user,id=lan,$LAN_NET,$LAN_FWD" \
  -device virtio-net-pci,netdev=wan -netdev user,id=wan \
  -monitor "unix:$MONITOR_SOCKET,server,nowait"

mkdir -p "$DEVVM_DIR"
if [ "$DETACH" -eq 0 ]; then
  info "foreground console; press Ctrl-A then X to quit"
  exec qemu-system-aarch64 "$@" -nographic -pidfile "$PID_FILE"
fi

if [ "${DEVVM_SERIAL_FILE:-0}" = 1 ]; then
  # Rehearsal snapshots already have SSH; no FIFO readers need to outlive
  # the launching terminal, which can otherwise block the guest at boot.
  qemu-system-aarch64 "$@" -display none -serial "file:$SERIAL_LOG" \
    -pidfile "$PID_FILE" -daemonize
  info "background VM started with file console: $SERIAL_LOG"
  exit 0
fi

rm -f "$CONSOLE_PIPE.in" "$CONSOLE_PIPE.out" "$SERIAL_LOG"
mkfifo "$CONSOLE_PIPE.in" "$CONSOLE_PIPE.out"
qemu-system-aarch64 "$@" -display none -serial "pipe:$CONSOLE_PIPE" \
  -pidfile "$PID_FILE" -daemonize

( cat "$CONSOLE_PIPE.out" >> "$SERIAL_LOG" & echo $! > "$DEVVM_DIR/console-reader.pid" ) 2>/dev/null
( exec 3>"$CONSOLE_PIPE.in"; while vm_running; do sleep 5; done ) >/dev/null 2>&1 &
echo $! > "$DEVVM_DIR/console-writer.pid"
info "background VM started; serial log: $SERIAL_LOG"
