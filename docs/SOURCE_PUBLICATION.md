# Source publication

This repository starts from an independent sanitized V3 source snapshot.
Private development history, device state, target details, incident records,
credentials, evidence, backups and generated binaries are excluded.
Examples and test fixtures use synthetic values. Product defaults and public
upstream dependency/rule URLs remain part of the source.

The public snapshot keeps the application and regression tests. Documentation,
publication checks and CI are adapted for a standalone repository. FakeCore
behavior is tested, but its introduction diff cannot be reconstructed from
private development history and is not claimed as a public-history check.
The initial architecture baseline records the current composition-root default-core
regression fixtures; future changes remain subject to the ratchet and tests.

For later updates, export committed source, inspect the complete inventory and
diff, scan for credentials and personal data, then push only reviewed public
commits. Do not push private development branches or tags. Keep target settings,
tokens and backups outside the repository. A source publication is not a device
release and does not certify any installation or long-running validation.
