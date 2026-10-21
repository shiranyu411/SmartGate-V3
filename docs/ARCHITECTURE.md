# SmartGate V3 architecture (authority map)

Short map of **who is allowed to tell the truth**. Multi-agent and multi-UI bugs almost always come from picking the wrong authority.

## Planes

| Plane | Responsibility |
|-------|----------------|
| Control plane | SmartGate daemon: API, UI static, state files, dry-run/apply, scheduler |
| Data plane | Per-core Workers (`sing-box` and `Xray`) plus SmartGate embedded Go DNS |
| Gateway Fabric | DNS classification, per-Pool nft sets, nft TPROXY dispatch, Binding/Generation marks |
| Host network | OpenWrt policy-routing and nftables resources carrying SmartGate owner tags |

## State authorities

| Concern | Authority | Notes |
|---------|-----------|--------|
| Desired config (saved) | `SpecState` | User edits land here; may be dirty |
| Last successfully applied topology | `AppliedState` with `SpecCaptured=true` | Includes the committed `ExecutionGraph` |
| Live Pool Binding choices | durable `SelectionState` | Includes SmartNode, Generation, bound epoch/hash; never inferred from Worker defaults |
| UI “running gateway mode” | `DataPlaneObservation.RunningGatewayMode` | Observe runtime, don’t invent |
| Runtime process status | `RuntimeState` / `/api/runtime/status` | Phase, rescue flag, etc. |

## Mutation path

Connection display may use a bounded, memory-only DNS association index keyed
by source client and destination IP. Associations follow answer/CNAME TTL and
are hints, never input to classification, connection identity or routing.
Observed Worker hostnames and chains retain their original meaning; an inferred
DNS association or a path derived from a scoped Pool Binding is labelled in the UI.

Execution-core default: the application composition root designates sing-box
as the default for a SmartNode with no saved choice. The catalog and Spec
normalization share this policy. Import/refresh/save persist it; bootstrap
migrates older saved choices before tasks start. Explicit choices are never
replaced. Probe availability and protocol capability are validated separately;
an unavailable probe must not be recorded as an upstream connectivity failure.

```text
UI/API edit → Spec (dirty)
    → POST /api/config/dryrun
    → compile and validate ExecutionGraph + all Worker candidates + Fabric plan
    → POST /api/config/apply
    → checkpoint old Workers/Fabric/Bindings/Selection
    → start target Workers → switch Fabric → verify active Bindings
    → persist durable Selection → advance config_epoch + AppliedState last
```

On failure, rollback runs in reverse safety order: old Fabric first, then the old
Worker set, then durable Binding/Selection state. There is **no** supported
“edit live Worker JSON by hand and call it SmartGate state” path.

## Provider refresh

```text
fetch subscription → parse nodes → update Provider last-good + SmartNode catalog
    → mark dirty (Applied Binding remains unchanged)
    → dry-run + apply to push data plane
```

Refresh success without apply means last-good/disk updated; traffic path may still be previous apply.

## Snapshots and backups

- New snapshots capture complete durable `SelectionState`; legacy snapshots
  without Generation metadata are rebound monotonically from the current graph.
- Backup schema 3 includes `selection.json` with `spec.json`, `applied.json`,
  `runtime.json`, `failover.json`, and last-good files.
- Transaction journals capture exact source and target Selection snapshots so a
  crash replay cannot silently reset Generation or Binding metadata.

## Deploy

- **Release entry point:** `scripts/install.sh` classifies the target and fails
  closed on partial or ambiguous state.
- **Blank target:** `scripts/install-fresh.sh` verifies the release and Worker
  prerequisites, writes a fresh-install journal/boot recovery hook, installs
  atomically, and requires health plus auth-setup gates before committing.
- **Existing healthy target:** `scripts/deploy-safe.sh` requires authenticated
  preflight, clean Spec, empty journals, a valid data-plane baseline, and keeps
  a rollback tarball through all post-swap gates. Recovery branches on the
  durable `AppliedState.schema_version`: schema 1 restores the V2 monolithic
  sing-box/TUN path, while schema 2+ restores embedded DNS, Gateway Fabric,
  dnsmasq/hotplug ownership, and the Workers referenced by Applied Bindings.
  A schema-1-to-schema-2 upgrade journals the archive before stopping the old
  sing-box service and runs owner-safe rescue cleanup before the new daemon can
  claim `:1053`; cleanup failure rolls back automatically.
- Fresh bootstrap never overwrites an existing/partial installation; upgrades
  never bypass `deploy-safe.sh`.
- **Rescue:** durable flag under data dir; suppresses normal start until cleared.
- **Listen address:** default LAN IP only (`ValidateLANOnly` rejects `0.0.0.0`).

## Frontend

- SPA under `frontend/`, served from daemon `-static-dir` (packaged as `/www/smartgate`).
- Auth token in `localStorage`; SSE may pass `?token=` because `EventSource` cannot set headers.

## Rule authoring (agents)

Hand-written business splits use the complete local rule catalog
(`GET/PUT /api/rules`), not removed per-group endpoints or giant remote geosite
dumps. User rules override system defaults; unresolved cross-pool user conflicts
are saved for review but block dry-run and apply. See
`docs/BUILTIN_RULES.md` and `docs/RULESET_AUTHORING.md`.

## Non-goals (current)

- Public Internet exposure of the management port
- Multi-tenant / multi-admin RBAC
- Automatic migration of arbitrary partial V2 filesystem state into V3; clean
  V3 installation and guarded upgrade from a healthy installed baseline are
  separate supported paths
