# Data-plane validation

Run the Linux/OpenWrt fixtures in `scripts/m2-kernel-smoke.sh` and
`scripts/m2-worker-smoke.sh` only in an isolated test environment. They exercise
nftables TPROXY, IPv4/IPv6, TCP/UDP, embedded DNS, Worker control and recovery.
The local QEMU workflow is described in `scripts/devvm/README.md`.

Record CPU, RSS, file descriptors, storage headroom, throughput, DNS behavior,
selection persistence and rollback results on the intended deployment hardware.
VM or host-unit-test results do not substitute for target hardware acceptance.
This source snapshot contains no private device measurements or claim that a
particular deployment passed these checks.
