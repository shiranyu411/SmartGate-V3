#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

vm_running || { info "VM is not running"; exit 0; }
PID="$(cat "$PID_FILE")"

if [ "${1:-}" = -f ]; then
  kill -9 "$PID" 2>/dev/null || true
else
  kill -TERM "$PID" 2>/dev/null || true
  waited=0
  while vm_running && [ "$waited" -lt 30 ]; do
    sleep 1
    waited=$((waited + 1))
  done
  vm_running && kill -9 "$PID" 2>/dev/null || true
fi

for helper in console-reader console-writer; do
  helper_file="$DEVVM_DIR/$helper.pid"
  [ ! -f "$helper_file" ] || kill "$(cat "$helper_file")" 2>/dev/null || true
  rm -f "$helper_file"
done
rm -f "$PID_FILE" "$MONITOR_SOCKET" "$CONSOLE_PIPE.in" "$CONSOLE_PIPE.out"
info "VM stopped"
