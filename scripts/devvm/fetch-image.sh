#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

need curl
need qemu-img qemu
need shasum
need gunzip

mkdir -p "$DEVVM_DIR"

if [ -f "$QCOW_IMAGE" ]; then
  info "$QCOW_IMAGE already exists; remove it to rebuild"
  exit 0
fi

ARCHIVE="$DEVVM_DIR/$OPENWRT_IMAGE"
PART="$ARCHIVE.part"
URL="https://downloads.openwrt.org/releases/$OPENWRT_VERSION/targets/$OPENWRT_TARGET/$OPENWRT_IMAGE"

if [ ! -f "$ARCHIVE" ]; then
  info "downloading $URL"
  curl -fL -C - --retry 10 --retry-delay 5 --retry-all-errors \
    --connect-timeout 20 -o "$PART" "$URL"
  mv "$PART" "$ARCHIVE"
fi

info "verifying SHA-256"
ACTUAL="$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')"
if [ "$ACTUAL" != "$OPENWRT_SHA256" ]; then
  rm -f "$ARCHIVE" "$PART"
  die "SHA-256 mismatch: expected $OPENWRT_SHA256, got $ACTUAL"
fi

gunzip -kf "$ARCHIVE"
qemu-img resize -f raw "$RAW_IMAGE" "$VM_DISK_SIZE"
qemu-img convert -f raw -O qcow2 "$RAW_IMAGE" "$QCOW_IMAGE"
rm -f "$RAW_IMAGE"
info "ready: $QCOW_IMAGE"
