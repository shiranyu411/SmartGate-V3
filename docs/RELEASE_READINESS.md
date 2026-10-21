# Release readiness

Source publication does not constitute a device release. Run the software gate
and record target-specific rehearsal results before a production cutover.

## Software blocking

| # | Item | Status | Evidence |
|---|---|---|---|
| B1 | API authentication, redaction and state permissions | [x] | Regression coverage in `internal/runtime/` and `internal/state/`; rerun by the gate |
| B2 | Configuration, migration and recovery behavior | [x] | Regression coverage in `internal/commitlane/`; rerun by the gate |
| B3 | Architecture boundary and FakeCore behavior | [x] | `make archguard` and `make fakecore-e2e`; rerun by the gate |
| B4 | Frontend type checks, lint, tests and build | [x] | `make frontend-check`; rerun by the gate |
| B5 | Source credential scan and release identity | [x] | Enforced by `scripts/release-gate.sh` |

The rows describe automated checks present in the source; they are not a claim
that the public snapshot has received a device release gate. A gate must run
on the exact clean `main` commit and write `dist/release/GATE_PASSED` for the
artifact being staged.

## Rehearsal

| # | Item | Status | Evidence |
|---|---|---|---|
| B13 | Fresh installation and authenticated setup | [ ] | Run in an isolated OpenWrt VM |
| B14 | Existing-install upgrade and state preservation | [ ] | Run using a private, representative baseline |
| B15 | Injected failure, rollback and restart recovery | [ ] | Run in the same isolated environment |

Record and review these results for your target before a production release.
Do not publish raw configurations or credentials as evidence.

## Soak

Validate sustained CPU, memory, storage, traffic, DNS and failover behavior on
the intended hardware. This snapshot includes no completed long-duration
acceptance or certification of a particular router.
