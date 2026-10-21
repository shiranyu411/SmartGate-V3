# Shared helpers for scripts/cutover/*.sh. Source this file; do not execute it.
#
# Every cutover script is target-agnostic: TARGET=devvm|router selects a file
# under $SMARTGATE_OPS_DIR/targets/<name>.env that supplies SSH and remote
# path facts (never secrets). The same scripts therefore rehearse against the
# QEMU VM and run the production cutover.
set -euo pipefail

CUTOVER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$CUTOVER_DIR/../.." && pwd)"
OPS_DIR="${SMARTGATE_OPS_DIR:-$(cd "$REPO_ROOT/.." && pwd)/ops}"
TARGET="${TARGET:-}"

die()  { printf 'cutover[%s]: %s\n' "${TARGET:-?}" "$*" >&2; exit 1; }
info() { printf 'cutover[%s]: %s\n' "${TARGET:-?}" "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing $1"; }

load_target() {
  [ -n "$TARGET" ] || die "TARGET is required (TARGET=devvm or TARGET=router)"
  [ -d "$OPS_DIR" ] || die "ops directory not found: $OPS_DIR (set SMARTGATE_OPS_DIR)"
  local env_file="$OPS_DIR/targets/$TARGET.env"
  [ -f "$env_file" ] || die "target file not found: $env_file"
  # shellcheck disable=SC1090
  . "$env_file"
  # WAN access changes only the SSH transport, never the LAN-only API bind.
  SSH_HOST="${CUTOVER_SSH_HOST:-$SSH_HOST}"
  SSH_PORT="${CUTOVER_SSH_PORT:-$SSH_PORT}"
  SSH_KEY="${CUTOVER_SSH_KEY:-${SSH_KEY:-}}"
  : "${SSH_HOST:?}" "${SSH_PORT:?}" "${SSH_USER:?}" "${API_URL:?}" \
    "${REMOTE_DATA_DIR:?}" "${REMOTE_RUNTIME_DIR:?}" "${REMOTE_DEPLOY_DIR:?}" \
    "${REMOTE_STAGE_DIR:?}" "${TARGET_KIND:?}"
  if [ -n "${SSH_KEY:-}" ] && [ "${SSH_KEY#/}" = "$SSH_KEY" ]; then
    SSH_KEY="$REPO_ROOT/$SSH_KEY"
  fi
  if [ -n "${SSH_KEY:-}" ] && [ ! -f "$SSH_KEY" ]; then
    die "SSH_KEY $SSH_KEY does not exist (devvm: run scripts/devvm/provision.sh)"
  fi
}

# Worker runtime names live in the target env (not scanned by archguard).
# These helpers expand them so cutover scripts stay core-token-free. Evidence
# and verification look for every core this build can drive, required or
# optional; only preflight distinguishes the two.
worker_alt_re() {
  local out="" b
  for b in ${REQUIRED_WORKER_BINS:-} ${OPTIONAL_WORKER_BINS:-}; do
    if [ -z "$out" ]; then out="$b"; else out="$out|$b"; fi
  done
  printf '%s' "$out"
}

process_alt_re() {
  local extra
  extra=$(worker_alt_re)
  if [ -n "$extra" ]; then
    printf 'smartgate|%s' "$extra"
  else
    printf 'smartgate'
  fi
}

package_alt_re() {
  local out="kmod-nft|nftables|ip-full|conntrack|dnsmasq|firewall|smartgate" p
  for p in ${REQUIRED_PACKAGES:-} ${OPTIONAL_PACKAGES:-}; do
    out="$out|$p"
  done
  printf '%s' "$out"
}

worker_pidof_args() {
  printf '%s' "${REQUIRED_WORKER_BINS:-} ${OPTIONAL_WORKER_BINS:-}"
}

# Build SSH/SCP argv into the global _cutover_ssh_args. Avoids mapfile so the
# scripts run on macOS /bin/bash 3.2 as well as bash 4+.
build_ssh_args() {
  _cutover_ssh_args=(-p "$SSH_PORT" -o ConnectTimeout=8 -o LogLevel=ERROR)
  if [ -n "${SSH_KEY:-}" ]; then
    _cutover_ssh_args+=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o PasswordAuthentication=no)
  fi
  if [ "$TARGET_KIND" = rehearsal ]; then
    _cutover_ssh_args+=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null)
  fi
}

ssh_target() {
  build_ssh_args
  ssh "${_cutover_ssh_args[@]}" "$SSH_USER@$SSH_HOST" "$@"
}

scp_target() {
  build_ssh_args
  # -O keeps the legacy scp protocol that OpenWrt's dropbear understands.
  # Translate ssh -p into scp -P without bash-4 mapfile.
  _cutover_scp_args=()
  _i=0
  while [ "$_i" -lt "${#_cutover_ssh_args[@]}" ]; do
    if [ "${_cutover_ssh_args[$_i]}" = -p ]; then
      _cutover_scp_args+=(-P)
    else
      _cutover_scp_args+=("${_cutover_ssh_args[$_i]}")
    fi
    _i=$((_i + 1))
  done
  scp -O "${_cutover_scp_args[@]}" "$@"
}

scp_to_target() {
  local src=$1 dst=$2
  scp_target "$src" "$SSH_USER@$SSH_HOST:$dst"
}

# remote_sh runs a POSIX sh snippet on the target. Keep snippets busybox-safe.
remote_sh() {
  ssh_target "sh -s" <<EOF
set -eu
$1
EOF
}

# The cutover makes dozens of SSH calls; an interactive password prompt on each
# would turn the one-shot run into a typing exercise (or hang it when stdin is
# not a terminal). Require key-based, non-interactive SSH up front.
require_target_reachable() {
  build_ssh_args
  local probe
  if probe="$(ssh "${_cutover_ssh_args[@]}" -o BatchMode=yes "$SSH_USER@$SSH_HOST" true 2>&1)"; then
    return 0
  fi
  case "$probe" in
    *"Permission denied"*)
      die "SSH to $SSH_USER@$SSH_HOST:$SSH_PORT wants a password: your public key is not on the target. Once, by hand: ssh -p $SSH_PORT $SSH_USER@$SSH_HOST 'mkdir -p /etc/dropbear && cat >> /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys' < ~/.ssh/id_rsa.pub  (or set SSH_KEY in $OPS_DIR/targets/$TARGET.env), then verify: ssh -o BatchMode=yes -p $SSH_PORT $SSH_USER@$SSH_HOST true" ;;
    *"Host key verification failed"*|*"REMOTE HOST IDENTIFICATION"*)
      die "SSH host key for $SSH_HOST is unknown or changed; connect once by hand (ssh -p $SSH_PORT $SSH_USER@$SSH_HOST true) and accept it, then rerun" ;;
    *)
      die "cannot reach $SSH_USER@$SSH_HOST:$SSH_PORT over SSH: $(printf '%s' "$probe" | tail -n1)" ;;
  esac
}

# confirm_production makes a human type the exact phrase before any step
# that writes to a production target. Rehearsal targets skip the prompt.
confirm_production() {
  local action=$1
  [ "$TARGET_KIND" = production ] || return 0
  if [ "${CUTOVER_ASSUME_YES:-0}" = 1 ]; then
    die "CUTOVER_ASSUME_YES is not honoured for production targets; a human must confirm"
  fi
  [ -t 0 ] || die "production $action requires an interactive terminal for confirmation"
  printf '\nThis will %s on PRODUCTION target %s (%s).\nType exactly "%s %s" to continue: ' \
    "$action" "$TARGET" "$SSH_HOST" "$(printf '%s' "$action" | tr '[:lower:]' '[:upper:]')" "$TARGET"
  local answer
  read -r answer
  [ "$answer" = "$(printf '%s' "$action" | tr '[:lower:]' '[:upper:]') $TARGET" ] || die "confirmation did not match; aborting"
}

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

repo_short_sha() { git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown; }

# log_deploy appends one line to ops/deployments.log.
#   log_deploy <script> <version> <result> [note]
log_deploy() {
  local script=$1 version=$2 result=$3 note=${4:-}
  mkdir -p "$OPS_DIR"
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$(utc_now)" "$TARGET" "$script" "$version" "$result" "$note" >> "$OPS_DIR/deployments.log"
}

evidence_dir() {
  local slug=$1
  local dir="$OPS_DIR/evidence/$(date -u +%Y%m%d-%H%M%S)-$TARGET-$slug"
  mkdir -p "$dir"
  printf '%s\n' "$dir"
}

# redact strips values that must not be written to disk on the workstation.
redact() {
  python3 "$CUTOVER_DIR/redact.py"
}

remote_version_json() {
  ssh_target "cat $REMOTE_DATA_DIR/version.json 2>/dev/null || echo '{}'"
}

require_private_backup() {
  [ "$TARGET_KIND" = production ] || return 0
  need python3
  [ -n "${BACKUP_DIR:-}" ] || die "BACKUP_DIR is required before production staging or upgrade"
  local installed spec_sha
  installed="$(json_field "$(remote_version_json)" commit)"
  spec_sha="$(ssh_target "sha256sum '$REMOTE_DATA_DIR/spec.json'" | cut -d' ' -f1)"
  python3 "$CUTOVER_DIR/backup.py" check "$BACKUP_DIR" \
    --forbid "$(dirname "$REPO_ROOT")" --forbid "$OPS_DIR" \
    --target "$TARGET" --host "$SSH_HOST" --commit "$installed" --spec-sha "$spec_sha"
}

json_field() {
  # json_field <json> <field>  — tolerant single-level extractor for version.json
  printf '%s' "$1" | sed -nE "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"([^\"]*)\".*/\1/p" | head -n1
}

# upload_token copies a local token file to a 0600 remote temp path and prints
# the remote path. Callers must remove it (cleanup_token) even on failure.
upload_token() {
  local local_file=$1
  [ -s "$local_file" ] || die "token file is missing or empty: $local_file"
  local remote="/tmp/smartgate-cutover-token-$$"
  scp_to_target "$local_file" "$remote" >/dev/null
  ssh_target "chmod 0600 '$remote'"
  printf '%s\n' "$remote"
}

cleanup_token() {
  local remote=$1
  [ -n "$remote" ] || return 0
  ssh_target "rm -f '$remote'" >/dev/null 2>&1 || true
}
