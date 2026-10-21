# SmartGate V3 Release Runbook

This document is the operational authority for what happens **on the
device** during build, install, upgrade, recovery, and acceptance.
The human procedure that drives those scripts against a named target
(QEMU or the production router) is `docs/CUTOVER.md`. Start there.

## Safety rules

- Use `scripts/install.sh` as the release entry point.
- A completely blank target is handled by `scripts/install-fresh.sh`.
- An existing healthy installation is handled only by
  `scripts/deploy-safe.sh`.
- Never hot-overwrite `/usr/bin/smartgate` on a live router.
- Never use fresh bootstrap to cover a partial or unhealthy installation.
- Never expose the management API on WAN or `0.0.0.0`.
- Keep a serial/SSH recovery path and retain the release tarball hash.

## Release identity

| Field | Source |
| --- | --- |
| Version | `VERSION` or an explicit `VERSION=...` make variable |
| Commit | `git rev-parse --short HEAD`, embedded in binary and `version.json` |
| Artifact | `dist/smartgate-openwrt-aarch64.tar.gz` |
| Target | OpenWrt / ImmortalWrt `linux/arm64` |
| Management listen | `192.168.1.1:3000` by default, LAN only |

Build a formal artifact from the intended release commit:

```sh
git status --short
make test
make smoke
cd frontend && npm test && npm run typecheck && npm run lint && npm run build && cd ..
make release
cat dist/release/version.json
shasum -a 256 dist/smartgate-openwrt-aarch64.tar.gz
```

A dirty tree is acceptable for development validation, but a formal release
must use a clean, recorded commit so the embedded identity is reproducible.

The tarball includes the daemon, procd unit, Worker control helper, fresh and
upgrade/recovery scripts, frontend, `version.json`, `SHA256SUMS`, license, and
the current API/architecture/rules/release documentation.

Verify an extracted artifact before any target mutation:

```sh
rm -rf /tmp/smartgate-release
mkdir -p /tmp/smartgate-release
tar -xzf smartgate-openwrt-aarch64.tar.gz -C /tmp/smartgate-release
cd /tmp/smartgate-release
sha256sum -c SHA256SUMS
./smartgate locks
```

## Target prerequisites

Both paths require:

- OpenWrt/ImmortalWrt with procd;
- AArch64 userspace compatible with the packaged static binary;
- sing-box `>= 1.12` available as an executable command; Xray is optional;
- nftables TPROXY/socket support, full `ip`, and `conntrack`;
- enough persistent storage for SmartGate state and rollback artifacts;
- trusted LAN access to `192.168.1.1:3000` or an explicitly configured LAN IP.

Example checks:

```sh
test -f /etc/openwrt_release
test -x /sbin/procd
sing-box version          # required (V2 migration default core, >= 1.12)
xray version              # optional: registered as a selectable core only when installed
nft --version
ip -V
conntrack -V
```

Package names vary by OpenWrt release. Typical requirements are `sing-box kmod-nft-tproxy kmod-nft-socket ip-full conntrack`; install `xray-core` when using Xray. The local VM may also need `tcpdump` and `curl` for diagnostics.

## Path selection

`scripts/install.sh` classifies the target before mutation:

| Target state | Result |
| --- | --- |
| All managed SmartGate paths absent | Execute transactional fresh bootstrap |
| Installed binary + init + data dir present | Execute guarded `deploy-safe.sh` |
| Fresh-install journal present | Run the recorded fresh recovery helper, then classify again |
| Any other managed file, `.new`, boot hook, or `S??`/`K??smartgate` residue | Fail closed; recover or remove residue explicitly |

The entry point never silently treats a partial install as either blank or
healthy.

## Fresh installation

Use this path only for a completely blank SmartGate target. Do not set
`SMARTGATE_AUTH_HEADER`; no admin account exists yet.

```sh
cd /tmp/smartgate-release
sha256sum -c SHA256SUMS
sh scripts/install.sh
```

Fresh bootstrap performs these gates:

1. Re-verifies all release hashes and the daemon lock hierarchy self-check.
2. Verifies OpenWrt/procd, sing-box version, Xray, and required shell tools.
3. Acquires the deployment lock and records `fresh-pending.json` plus a boot
   recovery hook before installing managed payload.
4. Stages and atomically renames the daemon, init script, Worker helper,
   recovery helper, frontend, and version metadata.
5. Enables/starts SmartGate while scheduler work remains gated.
6. Requires `/healthz` to return `ok` and `/api/auth/check` to report
   `needs_setup:true`.
7. Removes the scheduler gate, restarts, repeats both API gates, syncs, and only
   then removes the fresh journal and recovery hook.

Expected terminal result:

```text
install-fresh: installation passed health and auth-setup gates
```

Initialize auth immediately through the UI or API:

```sh
TOKEN="$(wget -qO- \
  --header='Content-Type: application/json' \
  --post-data='{"password":"REPLACE_WITH_STRONG_PASSWORD"}' \
  http://192.168.1.1:3000/api/auth/setup \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"

test -n "$TOKEN"
wget -qO- --header="Authorization: Bearer $TOKEN" \
  http://192.168.1.1:3000/api/version
```

Fresh install seeds the V3 default Spec, including built-in local rules and the
three disabled-by-default GEO/ad catalog entries described in
`docs/BUILTIN_RULES.md`. Live proxy traffic still requires valid providers,
pool candidates, and a successful config dry-run/apply.

### Interrupted fresh install

If the process or device stops after the journal is written, boot runs the
recorded helper. A manual recovery is:

```sh
/root/smartgate-deployments/install-fresh-recover --recover
```

The helper accepts only its own `fresh-work-*` path under the deployment root,
stops/disables the service, removes managed payload and atomic residue, and
then removes the journal. If that validation fails, do not manually re-run the
installer until the journal and filesystem state have been inspected.

## Upgrade existing installation

This path requires an already healthy SmartGate baseline and a valid admin
Cookie or Bearer token. `install.sh` routes it to `deploy-safe.sh`.

### Preflight

```sh
AUTH='Authorization: Bearer YOUR_TOKEN'
API='http://192.168.1.1:3000'

/etc/init.d/smartgate status
test "$(wget -qO- "$API/healthz")" = ok
wget -qO- --header="$AUTH" "$API/api/version"
wget -qO- --header="$AUTH" "$API/api/runtime/status"
wget -qO- --header="$AUTH" "$API/api/config/state"
# Require dirty:false.

set -- /etc/smartgate/transactions/*.json
test ! -e "$1"
test ! -e /root/smartgate-deployments/pending.json
test ! -e /root/smartgate-deployments/pending.starting.json
test ! -e /root/smartgate-deployments/fresh-pending.json
test ! -e /etc/smartgate/smartgate-rescue.status
```

Refuse the upgrade if the API is unhealthy, config is dirty, a transaction or
deploy journal exists, rescue is active, or the current data plane is not a
valid rollback baseline.

### Install

```sh
cd /tmp/smartgate-release
sha256sum -c SHA256SUMS
export SMARTGATE_AUTH_HEADER="$AUTH"
# Optional only when the LAN address differs:
# export SMARTGATE_API_BASE='http://192.168.50.1:3000'
sh scripts/install.sh
```

`deploy-safe.sh` verifies hashes and identity, preflights the authenticated API,
quiesces SmartGate, captures a rollback tarball, records a boot-recoverable
journal, atomically swaps payload, checks the staged commit, performs a
controlled config dry-run/apply, verifies the Fabric/Workers, reloads firewall,
resumes scheduler work, and waits for a stable clean state.

The rollback data-plane gate follows the `AppliedState.schema_version`, not the
saved Spec schema. Legacy schema `1` retains the V2 single sing-box/TUN path;
schema `2` and newer validate embedded DNS on `:1053`, the owner-tagged Gateway
Fabric, the firewall reconcile hook, dnsmasq override, and every Worker named by
the applied Binding graph. A V3 recovery never restarts the legacy monolithic
sing-box service.

When the baseline is schema `1`, the script first makes the durable journal and
rollback archive, then stops the old sing-box service and runs the staged binary's
owner-safe rescue cleanup in an isolated temporary state directory. It verifies
that the old `:1053` listener, TUN, DNS/DoT rules, hook, and dnsmasq override are
gone before starting the new daemon. Any failure in that window automatically
uses the durable rollback archive; do not remove the journal while diagnosing it.

Expected terminal result:

```text
deploy-safe: deployment passed all gates
deploy-safe: rollback backup retained at /root/smartgate-deployments/before-....tar.gz
```

### Upgrade failure

| Condition | Required action |
| --- | --- |
| Failure before backup/swap | Old service should remain; correct the failed preflight |
| Failure while quiescing a schema-1 baseline | The durable archive restores the V2 service and owner-tagged data plane; keep the journal if automatic rollback fails |
| Failure after backup | Automatic rollback runs from `before-*.tar.gz` |
| Pending upgrade journal | Run `/root/smartgate-deployments/deploy-recover` before retrying |
| Automatic rollback failure | Keep the journal/backup and restore through console/SSH; do not delete evidence first |
| Config becomes dirty | Resolve/apply/discard the desired change before another upgrade |

## Post-install acceptance

Use the authenticated token after fresh setup or the retained upgrade token:

```sh
AUTH='Authorization: Bearer YOUR_TOKEN'
API='http://192.168.1.1:3000'

test "$(wget -qO- "$API/healthz")" = ok
wget -qO- "$API/api/auth/check"
wget -qO- --header="$AUTH" "$API/api/version"
wget -qO- --header="$AUTH" "$API/api/runtime/status"
wget -qO- --header="$AUTH" "$API/api/config/state"
wget -qO- --header="$AUTH" "$API/api/rules"
wget -qO- --header="$AUTH" "$API/api/connections"
```

Required observations:

- running version/commit match `version.json`;
- config has no pending transactions and is clean after any intended apply;
- `/api/rules` contains `builtin-v2` and `geo-default` records;
- Route Explain identifies built-in/user/GEO provenance correctly;
- unresolved rule conflicts block dry-run and direct apply with
  `rule_conflict_unresolved`;
- live Connections are either returned or explicitly report no supported
  connection source, without fabricating rows;
- restart restores durable Selection/Binding state and leaves no deploy journal.

Representative Route Explain calls:

```sh
wget -qO- --header="$AUTH" --header='Content-Type: application/json' \
  --post-data='{"target":"api.openai.com"}' \
  "$API/api/routes/explain"

wget -qO- --header="$AUTH" --header='Content-Type: application/json' \
  --post-data='{"target":"router.lan"}' \
  "$API/api/routes/explain"
```

After providers/pools are configured, run dry-run/apply and verify the applied
Worker bindings, Worker status, TPROXY rules, embedded DNS, and reboot recovery.
For an upgrade, also inspect the recovery log and confirm that a schema-2+
rollback did not invoke `/etc/init.d/sing-box restart`. The exact API contract is in
`docs/API_CONTRACT.md`.

## Rescue

```sh
/etc/init.d/smartgate rescue
/etc/init.d/smartgate rescue_status
/etc/init.d/smartgate rescue_clear
/etc/init.d/smartgate restart
```

Rescue is durable under the data directory. On OpenWrt it stops Workers and
removes only SmartGate owner-tagged Fabric/DNS/TUN/host-network resources so
LAN clients can return to native direct connectivity.

## Local OpenWrt VM

The supported non-production compatibility workflow is:

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

When the VM already has auth initialized, provide its retained token through a
mode-`0600` host file instead of placing the token in a shell command:

```sh
DEVVM_AUTH_TOKEN_FILE=/secure/path/to/token scripts/devvm/deploy-to-vm.sh
```

The scripts target only `127.0.0.1` QEMU forwards and keep image/key material
under ignored `.devvm/`. Do not commit, print, copy, or attach that directory.

VM acceptance is valid for software compatibility, fresh install, API, rules,
Route Explain, Connections, apply/recovery, and nftables/TPROXY behavior. It is
not a substitute for deployment-router performance acceptance.

## Final release gate

Before declaring V3 complete:

1. `make test`, `make smoke`, frontend tests/typecheck/lint/build all pass.
2. The tarball extracts and `sha256sum -c SHA256SUMS` passes.
3. A clean local OpenWrt VM completes real `scripts/install.sh` fresh bootstrap.
4. Health, auth setup, rules, Route Explain, Connections, apply, restart, and
   recovery evidence is recorded as reproducible commands plus output.
5. `git diff --check` and a secret/path audit pass.
6. CPU/RSS/FD acceptance in `docs/M2_VALIDATION.md` is repeated by the user on
   the deployment-class router. VM numbers may be recorded as diagnostics only
   and must never be reported as target-hardware completion.
