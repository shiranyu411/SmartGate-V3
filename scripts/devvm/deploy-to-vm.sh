#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

vm_running || die "VM is not running"
ssh_vm true 2>/dev/null || die "SSH is unavailable; run provision.sh"

TARBALL="$REPO_ROOT/dist/smartgate-openwrt-aarch64.tar.gz"
[ -f "$TARBALL" ] || die "release artifact is missing; run make release"
AUTH_TOKEN_FILE="${DEVVM_AUTH_TOKEN_FILE:-}"
REMOTE_RELEASE=/tmp/smartgate-release
REMOTE_TOKEN="/tmp/smartgate-devvm-auth-$$"
AUTH_UPLOADED=0

cleanup_auth() {
  [ "$AUTH_UPLOADED" -eq 1 ] || return 0
  ssh_vm "rm -f '$REMOTE_TOKEN'" >/dev/null 2>&1 || true
}
trap cleanup_auth EXIT

if [ -n "$AUTH_TOKEN_FILE" ]; then
  [ -s "$AUTH_TOKEN_FILE" ] || die "DEVVM_AUTH_TOKEN_FILE is missing or empty: $AUTH_TOKEN_FILE"
  scp_vm "$AUTH_TOKEN_FILE" "root@127.0.0.1:$REMOTE_TOKEN"
  ssh_vm "chmod 0600 '$REMOTE_TOKEN'"
  AUTH_UPLOADED=1
fi

scp_vm "$TARBALL" root@127.0.0.1:/tmp/smartgate.tar.gz
ssh_vm "rm -rf '$REMOTE_RELEASE' && mkdir -p '$REMOTE_RELEASE' && tar -xzf /tmp/smartgate.tar.gz -C '$REMOTE_RELEASE' && cd '$REMOTE_RELEASE' && sha256sum -c SHA256SUMS"
if [ "$AUTH_UPLOADED" -eq 1 ]; then
  ssh_vm "cd '$REMOTE_RELEASE' && SMARTGATE_AUTH_HEADER=\"Authorization: Bearer \$(cat '$REMOTE_TOKEN')\" sh scripts/install.sh"
else
  ssh_vm "cd '$REMOTE_RELEASE' && sh scripts/install.sh"
fi
ssh_vm "test \"\$(wget -qO- http://192.168.1.1:3000/healthz)\" = ok && wget -qO- http://192.168.1.1:3000/api/auth/check"
cleanup_auth
AUTH_UPLOADED=0
trap - EXIT
info "SmartGate is available at http://127.0.0.1:$HOSTFWD_SMARTGATE"
