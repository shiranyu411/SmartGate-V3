# V2 migration compatibility

V3 contains migration and regression coverage for legacy applied-state schema
1, provider user agents, durable selections, routing precedence, network
recovery, bounded full-node measurements, failover confirmation and connection
cleanup. See `docs/CHANGELOG.md`, `internal/state/legacy_migration.go`,
`internal/commitlane/legacy_binding_migration.go` and their tests.

For a V2 upgrade, rehearse with your own private baseline, confirm all saved
bindings resolve, and retain a complete rollback backup. Private V2 incident
records and commit history are not published here. This document does not
certify an operator's target or replace migration acceptance.
