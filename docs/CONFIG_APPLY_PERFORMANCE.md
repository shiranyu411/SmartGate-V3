# Configuration apply performance

The current implementation reduces repeated preparation while preserving
validation, durable transactions and recovery:

- Reuse verified local GEO snapshots when the source contents are unchanged.
- Keep healthy FileRuntime Workers running when their configurations are unchanged.
- Prefer captured runtime checkpoints for rollback preparation.
- Reuse an nftables table read within a single validation pass.

Changed inputs, incomplete checkpoints, runtime drift and failed checks fall
back to the full preparation/rebuild path. These optimizations do not change
the authority of Spec, AppliedState or durable selections. Device timings and
private operational measurements are not included; measure on your own target.
