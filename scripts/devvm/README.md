# SmartGate V3 Development VM

This local-only QEMU VM runs the pinned OpenWrt `armsr/armv8` image with two
CPUs, 512 MiB RAM, and no configured swap. It is the supported substitute for
fresh-install, nftables/TPROXY, rescue, and deployment testing when no real
router is authorized. VM success is evidence for software compatibility, not a
substitute for final performance measurements on the deployment-class router.

The OpenWrt image and SSH key live under ignored `.devvm/`; never commit or copy
that directory. The scripts only target `127.0.0.1` QEMU forwards.

## Workflow

```sh
scripts/devvm/fetch-image.sh
scripts/devvm/start-vm.sh -d
scripts/devvm/provision.sh
scripts/devvm/stop-vm.sh
scripts/devvm/snapshot.sh create fresh-openwrt

make release
scripts/devvm/start-vm.sh -d
scripts/devvm/deploy-to-vm.sh
```

For an existing installation with auth already initialized, pass the token by
file so it is never embedded in the host or remote command line:

```sh
DEVVM_AUTH_TOKEN_FILE=/secure/path/to/token scripts/devvm/deploy-to-vm.sh
```

Host forwards are SSH `2222`, LuCI `8080`, and SmartGate `13000`. Before any
destructive recovery or power-loss test, stop the VM and create a qcow2
snapshot. The pinned image is OpenWrt `25.12.5`; `image.env` records the SHA-256
shown by the official OpenWrt download directory.

## Cutover rehearsal

Create your own baseline snapshots with `scripts/devvm/snapshot.sh` and configure
the private `devvm` target as described in `docs/CUTOVER.md`. Use synthetic nodes
and credentials. Grow the VM disk/filesystem if the image lacks room for Worker
runtimes, SmartGate and rollback data. No preconfigured snapshot or VM password
is distributed with the source.
