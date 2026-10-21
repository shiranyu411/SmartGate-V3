#!/usr/bin/env bash
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

need ssh openssh
need ssh-keygen openssh
vm_running || die "VM is not running; run start-vm.sh -d"
[ -p "$CONSOLE_PIPE.in" ] || die "background console is unavailable; use start-vm.sh -d"

WORKER_CONTROL=$REPO_ROOT/scripts/smartgate-worker-control
PACKAGES="${DEVVM_PACKAGES:-kmod-nft-tproxy kmod-nft-socket ip-full conntrack tcpdump curl $("$WORKER_CONTROL" package-names)}"

install_packages() {
  if ssh_vm "command -v apk >/dev/null 2>&1"; then
    ssh_vm "apk update >/dev/null && apk add $PACKAGES"
  elif ssh_vm "command -v opkg >/dev/null 2>&1"; then
    ssh_vm "opkg update >/dev/null && opkg install $PACKAGES"
  else
    die "neither apk nor opkg is available"
  fi
}

if [ ! -f "$SSH_KEY" ]; then
  ssh-keygen -t ed25519 -N "" -C "smartgate-devvm" -f "$SSH_KEY" >/dev/null
fi
PUBKEY="$(cat "$SSH_KEY.pub")"
KEY_BODY="$(awk '{print $2}' "$SSH_KEY.pub")"

if ssh_vm true 2>/dev/null; then
  info "existing SSH key is active"
else
  info "waiting for OpenWrt console"
  console_wait "Please press Enter to activate this console" 180 \
    || info "console-ready marker not seen; attempting provisioning"
  sleep 2
  console_send ""
  sleep 1
  console_send "mkdir -p /etc/dropbear && printf '%s\\n' '$PUBKEY' > /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys && /etc/init.d/dropbear enable && /etc/init.d/dropbear restart && echo DEVVM_KEY_INSTALLED"
  console_wait "DEVVM_KEY_INSTALLED" 90 || die "SSH key injection failed; inspect $SERIAL_LOG"
fi

attempt=0
while [ "$attempt" -lt 40 ]; do
  ssh_vm true 2>/dev/null && break
  sleep 2
  attempt=$((attempt + 1))
done
ssh_vm true 2>/dev/null || die "SSH is unavailable; inspect $SERIAL_LOG"
ssh_vm "grep -q '$KEY_BODY' /etc/dropbear/authorized_keys" || die "authorized key verification failed"

info "installing: $PACKAGES"
install_packages || die "package installation failed"
scp_vm "$WORKER_CONTROL" root@127.0.0.1:/tmp/smartgate-worker-control
ssh_vm "chmod 0755 /tmp/smartgate-worker-control && /tmp/smartgate-worker-control verify-prerequisites && rm -f /tmp/smartgate-worker-control"
ssh_vm "nft --version; ip -V; conntrack -V"
ssh_vm "modprobe nft_tproxy && modprobe nft_socket && lsmod | grep -E 'nft_tproxy|nft_socket'" \
  || die "TPROXY kernel modules are unavailable"
info "provisioning complete"
