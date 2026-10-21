#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

need qemu-img qemu
[ -f "$QCOW_IMAGE" ] || die "image is missing; run fetch-image.sh"

ACTION="${1:-list}"
NAME="${2:-}"
if [ "$ACTION" != list ] && vm_running; then
  die "stop the VM before changing snapshots"
fi

case "$ACTION" in
  create) [ -n "$NAME" ] || die "usage: snapshot.sh create <name>"; qemu-img snapshot -c "$NAME" "$QCOW_IMAGE" ;;
  restore) [ -n "$NAME" ] || die "usage: snapshot.sh restore <name>"; qemu-img snapshot -a "$NAME" "$QCOW_IMAGE" ;;
  delete) [ -n "$NAME" ] || die "usage: snapshot.sh delete <name>"; qemu-img snapshot -d "$NAME" "$QCOW_IMAGE" ;;
  list) qemu-img snapshot -l "$QCOW_IMAGE" ;;
  *) die "usage: snapshot.sh create|restore|delete|list [name]" ;;
esac
