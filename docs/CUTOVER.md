# Guarded deployment

Use `scripts/cutover/` for both an isolated rehearsal (`TARGET=devvm`) and a
production device (`TARGET=router`). On-device behavior is documented in
[the release runbook](RELEASE_RUNBOOK.md).

## Private target configuration

Create an operations directory outside the source checkout, set
`SMARTGATE_OPS_DIR` to its absolute path, and place target files under its
`targets/` directory. Start with `configs/targets/*.env.example`, replace the
reserved example address with your own trusted LAN address, and configure
key-based SSH. These templates contain no real target identity or credentials.
Create `evidence/` and `releases/` subdirectories as needed. Keep tokens and
backup archives private and outside both the checkout and operations directory.

The scripts use SSH to reach the device and access its LAN-only API from there.
`CUTOVER_SSH_HOST` and `CUTOVER_SSH_PORT` can override the SSH transport without
exposing the management API. Verify the SSH host key before a production run.

## Release and upgrade

1. Use a clean `main` commit; install frontend dependencies and run `make release-gate`.
2. Complete the rehearsal checklist in `RELEASE_READINESS.md` with the same artifact.
3. Confirm a healthy, clean running configuration and no unresolved recovery journal.
4. Record the installed commit and create a verified off-device private backup.
5. Run the guarded cutover from an interactive terminal and review its confirmation.

```sh
make cutover TARGET=router \
  TOKEN_FILE=/secure/path/admin-token \
  FROM_COMMIT=<installed-commit> \
  BACKUP_DIR=/secure/path/new-backup
```

The token file has mode 0600; when missing, the cutover helper prompts for the
admin password. Backups contain secrets and must never enter Git or evidence.
Staging verifies `GATE_PASSED`, commit identity and the artifact checksum.
The installer retains rollback data and verifies health after the swap.
Deployment operations append to the private operations log.

For a blank device, follow the transactional fresh-install path in the release
runbook. For a healthy V2 baseline, first rehearse migration and verify provider
user agents and resolved node bindings before routing real traffic.

## Recovery

Use `scripts/cutover/40-rollback.sh --list` to inspect retained recovery state,
`--auto` for a pending deployment recovery, or `--from-backup <private-directory>`
for a validated baseline archive. Retain journals and backups if recovery fails;
do not cover an unhealthy or partial install with a fresh bootstrap.

An explicitly authorized emergency hotfix can use `EMERGENCY_HOTFIX=1` with
the exact `HOTFIX_COMMIT` and `HOTFIX_SHA256`. It retains backup, validation,
confirmation and rollback checks, records `gate=emergency-not-run`, and must
never be described as a passing release gate.
