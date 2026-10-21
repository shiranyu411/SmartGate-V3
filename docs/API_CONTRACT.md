# SmartGate V3 API Contract

This document is the final API authority for SmartGate V3. The V2 endpoints
remain available unless a section below explicitly marks an endpoint as
changed. API changes and their implementations must land in the same change.

Base URL on router:

```text
http://192.168.1.1:3000
```

All API responses are JSON except `/healthz` and `/api/runtime/locks`.

Except for `/healthz` and `/api/runtime/locks`, endpoints use an envelope:

```json
{
  "ok": true,
  "data": {}
}
```

Errors use a stable machine-readable code:

```json
{
  "ok": false,
  "error": {
    "code": "validation_failed",
    "message": "请求参数无效",
    "details": {}
  }
}
```

### Language ownership

- Stable machine-readable values such as `code`, `status`, `state`, `kind`,
  `stage`, and documented reason identifiers use ASCII English values. Clients
  may branch on these values and must not treat their spelling as display text.
- Backend-generated, human-readable API fields use Simplified Chinese as their
  primary text when no stable machine code exists. This includes `last_output`,
  `warnings`, Provider refresh `error` / `subscription_error`, and Worker event
  `message` values.
- Structured envelope `error.message`, command `output`, log lines,
  compiler/tool stderr, and `argv` are source diagnostics and can retain their
  original language. The frontend derives the primary Chinese error text from
  `error.code`, then labels or folds the original message as diagnostic content.
  User-authored text such as failover lock `reason` is also returned verbatim.
- Existing persisted records can contain legacy English human-readable text.
  Frontends must tolerate those records during upgrade, but newly generated
  backend records follow the rules above.

`error` is always the structured object above; it is never a string. When an
operation has a safe partial result (for example a bounded dry-run summary or
sanitized command status), that result is returned in `error.details` rather
than as a top-level `data` field on a failed envelope.

List fields used by the frontend are returned as arrays, not `null`. Responses,
errors, dry-run summaries, and logs must never contain subscription URLs,
plaintext node credentials, worker-control credentials, or unmasked
`SecretRef` values.

## Authentication

The production daemon enables auth by default. Store the returned token and send it as:

```text
Authorization: Bearer <token>
```

### `GET /api/auth/check`

Envelope data:

```json
{
  "needs_setup": false,
  "authenticated": true
}
```

### `POST /api/auth/setup`

Used only when `needs_setup` is true.

```json
{
  "password": "change-me"
}
```

Envelope data:

```json
{
  "token": "..."
}
```

### `POST /api/auth/login`

```json
{
  "password": "change-me"
}
```

Envelope data:

```json
{
  "token": "..."
}
```

### `POST /api/auth/logout`

Revokes the current bearer token.

### `PUT /api/auth/password`

```json
{
  "old_password": "old-password",
  "new_password": "new-password"
}
```

On success the current bearer session remains valid; other existing sessions may be revoked.

## Runtime

### `GET /healthz`

Returns plain text:

```text
ok
```

### `GET /api/runtime/status`

```json
{
  "phase": "running",
  "listen_addr": "192.168.1.1:3000",
  "config_epoch": 12,
  "topology_hash": "...",
  "gateway_mode": "on",
  "rescue_active": false,
  "rescue_reason": "",
  "workers": [
    {"id":"w_sb_1","core":"sing-box","state":"running","pools":["general"]},
    {"id":"w_xr_1","core":"xray","state":"running","pools":["ai"]}
  ],
  "bindings": [
    {"id":"pb_...","pool":"general","target":{"kind":"node","smart_node":"sn_...","core":"sing-box","worker":"w_sb_1"},"generation":3,"since":"2026-07-28T08:00:00Z"}
  ],
  "cutovers_in_flight": [],
  "drains": []
}
```

`workers`, `bindings`, `cutovers_in_flight`, and `drains` are always arrays.
Bindings are rebuilt from the durable Applied topology plus durable Selection
state, so runtime cutovers survive restart without rewriting AppliedState.

Two optional fields describe a degraded or freshly upgraded daemon:

- `rescue_reason` is `manual`, `data_plane_unavailable`, or
  `startup_recovery_failed`. The last one means startup recovery (journal
  replay, legacy migration, or the applied data-plane reconcile) failed; the
  daemon entered rescue instead of exiting, the LAN is fail-open, and
  `recovery_error` carries the cause. Repair the topology, then
  `POST /api/runtime/rescue/clear` (which re-runs recovery) or restart.
- `recovery_error` (string) is present while the last startup recovery failed
  and is cleared by the next successful recovery.
- `legacy_migration` is present once pre-SmartNode (V2) pool bindings and
  selector state have been translated on this installation:

```json
{
  "legacy_migration": {
    "migrated_at": "2026-09-03T06:00:00Z",
    "resolved": 3,
    "unresolved": [{"scope": "spec_pool", "pool": "general", "legacy_id": "provider:0123456789abcdef"}],
    "warnings": ["Pool general 的旧节点 provider:0123456789abcdef 无法匹配到任何 SmartNode，已解除绑定；请在节点页重新选择"]
  }
}
```

`unresolved` and `warnings` are always arrays. An unresolved reference was
removed from its pool or selection (the pool then follows its `no_candidate`
policy) and needs a manual rebind on the SmartNodes page. The full report is
durable at `/etc/smartgate/legacy-migration.json`.
Unapplied desired topology changes never appear here. `cutovers_in_flight`
contains `{pool,stage}` and `drains` contains
`{pool,old_generation,remaining,deadline,status,last_error,retry_count}`.
`status` is `draining`, `retrying`, or `failed`. A drain is removed from this
array only after final cleanup succeeds; failed cleanup remains visible and
blocks a later cutover for the same Pool while the daemon retries cleanup in
the background. During an active background cleanup attempt, the in-memory
runtime view transiently reports `retrying`; the last durable state is left
unchanged between attempts (normally `failed`) to avoid flash churn. A drain
persistence error does not stop the retry schedule. `POST /api/runtime/recover`
triggers an immediate retry.

### `GET /api/runtime/locks`

Plain text lock hierarchy for diagnostics.

### `GET /api/cores`

Envelope data is the stable `coredesc.Registry.List()` projection for UI core
choices:

```json
[
  {"id": "core-a", "display_name": "Core A"}
]
```

Each item is derived from the registered Descriptor `ID` and `DisplayName`.
If the runtime has no core registry, the endpoint returns `501`.

### `GET /api/version`

Envelope data:

```json
{
  "name": "smartgate",
  "version": "0.1.0",
  "commit": "abc1234",
  "built_at": "2026-06-26T00:00:00Z",
  "go_version": "go1.26.1",
  "goos": "linux",
  "goarch": "arm64"
}
```

This endpoint describes the SmartGate build only. Registered cores are listed
by `GET /api/cores`; their running Worker versions are reported by
`GET /api/workers`.

### `POST /api/runtime/rescue/enter`

Enter rescue mode. On OpenWrt/ImmortalWrt, this marks rescue active, records
the owner-tagged cleanup plan, stops registered Worker cores, removes all
SmartGate-owned Gateway Fabric/DNS/TUN/HostNetwork artifacts, and records
cleanup execution counts in rescue state. WAN SSH/Dropbear and unowned firewall
resources are never modified.

### `POST /api/runtime/rescue/clear`

Clear rescue mode.

### `POST /api/runtime/recover`

Envelope endpoint. Explicitly runs the commit-lane recovery orchestrator while the management API is already up: finalize any residual durable transaction journals (config and selector), rebind selection to applied topology, and clear the recovery gate.

Use this when write APIs return `423 Locked` with `durable transaction recovery is required`, or when read APIs fail after a crash left a `Committed` journal / selection-ahead-of-applied control-plane mismatch. Prefer this over restarting the process when the daemon is still reachable.

Auth: same as other management endpoints (Bearer token when auth is enabled).

Request body: none.

Success (`200`):

```json
{
  "ok": true,
  "data": {
    "status": "recovered"
  }
}
```

Errors:

- `405 Method Not Allowed`
- `500`: recovery failed (journal remains until a later successful recover)

Related write-path errors that should point operators here:

- `423 Locked` with message containing `durable transaction recovery is required` — call `POST /api/runtime/recover` before retrying the original mutation.

### `GET /api/events`

SSE stream for lightweight runtime status updates. The stream emits `status` events every few seconds.

Auth:

- Browser `EventSource` cannot set custom headers, so pass the token as `?token={token}`.
- Header auth also works for clients that support it.

Example event:

```text
event: status
data: {"time":"2026-06-25T00:00:00Z","runtime":{"phase":"running","listen_addr":"192.168.1.1:3000","config_epoch":1}}
```

## System Operations

System endpoints use the `{ok,data,error}` envelope. Destructive or disruptive operations require `{"confirm": true}` so the UI can enforce a confirmation step.

### `GET /api/system/status`

Envelope data:

```json
{
  "runtime": {
    "phase": "running",
    "listen_addr": "192.168.1.1:3000",
    "config_epoch": 1,
    "rescue_active": false
  },
  "memory": {
    "total_bytes": 268435456,
    "free_bytes": 33554432,
    "available_bytes": 134217728,
    "used_bytes": 134217728
  },
  "disks": [
    {
      "path": "/",
      "total_bytes": 104857600,
      "free_bytes": 52428800,
      "used_bytes": 52428800,
      "used_percent": 50
    }
  ],
  "services": [
    {
      "name": "sing-box",
      "status": "running",
      "result": {
        "ok": true,
        "argv": ["/etc/init.d/sing-box", "status"],
        "output": "running"
      }
    }
  ],
  "processes": [
    {
      "name": "sing-box",
      "running": true,
      "pids": ["1234"],
      "result": {
        "ok": true,
        "argv": ["pidof", "sing-box"],
        "output": "1234"
      }
    }
  ],
  "warnings": [],
  "checked_at": "2026-06-25T00:00:00Z"
}
```

Notes:

- `services` is derived from platform services, Gateway Fabric services,
  registered core `Process.ServiceUnit` values, and applied ExecutionGraph
  Workers.
- `processes` is derived from platform/Fabric processes, registered core
  `Process.ProcessName` values, and applied ExecutionGraph Workers.
- If memory is unavailable, `memory` is omitted and a warning is returned.

### `GET /api/system/network/diagnostics`

Authenticated, read-only network report: `status` (`ok`, `degraded`, `disabled`,
`unavailable`), `checked_at`, and non-null `checks` containing `id`, `label`,
`ok`, and a safe `detail`. Checks cover bound Workers/embedded DNS, dnsmasq,
Fabric DNS interception and policy routes, persistent firewall/DNS loading,
and DNS resolution plus HTTP(S) through the current fallback Pool's selected
executor. Command output and provider configuration are never returned.
Off/safe/rescue report disabled, rather than healthy. Rescue is checked from
the live durable rescue status, including entry after daemon startup.

### `POST /api/system/network/repair`

Requires `{"confirm":true}`. Replays captured Applied configuration and durable
Pool selections through the commit lane, preserving dirty Spec and the applied
epoch. Refuses off/safe/rescue or an absent applied capture. Saves a data-plane
checkpoint and attempts rollback on execution failure. Success returns a fresh
network report; a degraded report still requires attention. Returns `409` on
restoration failure and `503` without an executor. Ordinary V3 startup already
replays the applied execution graph before serving the API. This operation does
not reboot the router or apply pending desired changes.

### `GET /api/system/logs`

Query params:

- `source`: case-insensitive log filter, commonly `system`, `smartgate`, a
  Fabric service, a registered core process, or an applied Worker id.
- `limit`: default `100`, max `500`.

Envelope data:

```json
{
  "source": "sing-box",
  "limit": 100,
  "lines": ["sing-box ready"],
  "result": "ok",
  "time": "2026-06-25T00:00:00Z"
}
```

### `POST /api/system/service/{service}/{action}`

Allowed service names are the same registry-derived names returned by
`GET /api/system/status`: platform/Fabric init services, registered core
`Process.ServiceUnit` values, and applied ExecutionGraph Worker ids.

Allowed actions are service-specific: init services accept `restart` and
`reload`; Worker ids accept `restart`.

Request:

```json
{
  "confirm": true
}
```

Success status is `202 Accepted`.

### `POST /api/system/dns/flush`

Restarts `dnsmasq` to flush DNS cache/state.

```json
{
  "confirm": true
}
```

### `POST /api/system/reboot`

Requests router reboot.

```json
{
  "confirm": true
}
```

## Wi-Fi

Wi-Fi endpoints use the `{ok,data,error}` envelope. Read operations use OpenWrt `ubus`, `uci`, and `iwinfo`; write operations require `{"confirm": true}`.

Interface names follow the live radio (for example `phy0-ap0` / `phy1-ap0`). The legacy default `wlan0` is still accepted as a request hint and is auto-resolved to the first available AP interface when present.

### `GET /api/wifi/status`

Runs `ubus call network.wireless status` and returns a structured
radio/interface view. Raw command output and argv are not returned.

Envelope data:

```json
{
  "radios": [
    {
      "id": "radio0",
      "up": true,
      "pending": false,
      "disabled": false,
      "band": "2g",
      "channel": "1",
      "htmode": "HE20",
      "country": "CN",
      "txpower": 20,
      "interfaces": [
        {
          "section": "default_radio0",
          "ifname": "phy0-ap0",
          "ssid": "Home",
          "mode": "ap",
          "encryption": "psk2",
          "disabled": false,
          "network": "lan",
          "band": "2g",
          "channel": "1",
          "htmode": "HE20"
        }
      ]
    }
  ],
  "interfaces": [
    {
      "section": "default_radio0",
      "ifname": "phy0-ap0",
      "ssid": "Home",
      "mode": "ap",
      "encryption": "psk2",
      "disabled": false,
      "network": "lan",
      "band": "2g",
      "channel": "1",
      "htmode": "HE20"
    }
  ]
}
```

If `ubus` output is not JSON, `radios` / `interfaces` are empty arrays.

### `POST /api/wifi/scan`

Runs `iwinfo {interface} scan`.

```json
{
  "interface": "auto"
}
```

`interface` resolution order:

1. explicit interface that is not `auto` / empty / `wlan0`
2. otherwise discover the first non-disabled AP ifname from `network.wireless status`
3. if discovery fails and the caller sent a concrete interface (including legacy `wlan0`), use that name
4. if discovery fails for `auto` / empty, return `400`

Envelope data:

```json
{
  "interface": "phy0-ap0",
  "networks": [
    {
      "bssid": "02:00:00:00:00:0C",
      "ssid": "Cafe",
      "channel": 6,
      "signal_dbm": -42,
      "quality": "60/70",
      "encryption": "WPA2 PSK (CCMP)"
    }
  ]
}
```

Networks are sorted by descending `signal_dbm`.

### `GET /api/wifi/clients`

Collects associated stations for one interface or all AP interfaces.

Query params:

- `interface`: defaults to `all`. Accepts a concrete ifname (`phy0-ap0`) or `all`.

Data source priority per interface:

1. `ubus call hostapd.{ifname} get_clients` (signal, rates, bytes, auth)
2. fallback `iwinfo {ifname} assoclist`
3. enrich MAC → IP / hostname from `/tmp/dhcp.leases`

Envelope data:

```json
{
  "interface": "all",
  "interfaces": ["phy0-ap0", "phy1-ap0"],
  "clients": [
    {
      "mac": "02:00:00:00:00:0d",
      "interface": "phy0-ap0",
      "ssid": "Home",
      "signal_dbm": -48,
      "hostname": "phone",
      "ip": "192.168.1.50",
      "rx_rate": 144400,
      "tx_rate": 86670,
      "rx_bytes": 1200,
      "tx_bytes": 3400,
      "authorized": true
    }
  ]
}
```

Clients are sorted by descending `signal_dbm`. Missing discovery yields an empty `clients` array rather than a hard failure.

### `GET /api/wifi/config`

Runs `uci show wireless` and returns product-oriented network/device views.
Wireless keys are write-only and are represented by `key_configured`.

Envelope data:

```json
{
  "networks": [
    {
      "section": "default_radio0",
      "device": "radio0",
      "ssid": "Home",
      "mode": "ap",
      "encryption": "psk2",
      "key_configured": true,
      "disabled": false,
      "network": "lan",
      "band": "2g",
      "channel": "1",
      "htmode": "HE20",
      "country": "CN"
    }
  ],
  "devices": [
    {
      "section": "radio0",
      "band": "2g",
      "channel": "1",
      "htmode": "HE20",
      "country": "CN",
      "txpower": "20",
      "disabled": false
    }
  ]
}
```

`networks` are `wifi-iface` sections joined with their parent device
band/channel metadata. The current key is never returned or available for edit
prefill.

### `PUT /api/wifi/config`

Updates one UCI wireless section, commits, then runs `wifi reload`.

```json
{
  "confirm": true,
  "section": "default_radio0",
  "ssid": "Home",
  "encryption": "psk2",
  "key": "password",
  "disabled": false
}
```

Validation:

- `section` must be a safe UCI section id using letters, digits, `_`, or `-`.
- At least one of `ssid`, `encryption`, `key`, or `disabled` is required.

On success the endpoint returns `202` with `{section,status,changed}`. It does
not echo command argv, output, or the submitted key. Wireless briefly
interrupts during reload.

## Overview And Config Workflow

### `GET /api/overview`

Envelope data contains aggregated frontend dashboard state:

```json
{
  "runtime": {},
  "gateway": {
    "mode": "on",
    "dirty": true
  },
  "config": {
    "dirty": true,
    "spec_hash": "...",
    "topology_hash": "",
    "config_epoch": 0,
    "gateway_mode": "on",
    "saved_gateway_mode": "on",
    "applied_gateway_mode": "on",
    "running_gateway_mode": "on"
  },
  "providers": {
    "total": 1,
    "enabled": 1
  },
  "nodes": {
    "total": 10,
    "usable": 8
  },
  "business_pools": {
    "total": 5
  },
  "snapshots": {
    "pending": 0
  }
}
```

### `GET /api/config/state`

Envelope data:

```json
{
  "dirty": true,
  "spec_hash": "...",
  "topology_hash": "",
  "config_epoch": 0,
  "gateway_mode": "on",
  "saved_gateway_mode": "on",
  "applied_gateway_mode": "on",
  "running_gateway_mode": "on"
}
```

Gateway mode fields:

- `gateway_mode` — saved (desired) mode; same value as `saved_gateway_mode` for compatibility.
- `saved_gateway_mode` — mode stored in Spec (`settings.gateway_mode`).
- `applied_gateway_mode` — mode captured in the last successful Applied topology (empty when no apply has succeeded yet).
- `running_gateway_mode` — observed data-plane mode from the latest config apply, rollback, or data-plane supervisor observation pass (may lag or be empty before first observation).

`topology_hash` is a hash of the effective applied/desired routing topology (providers, pools, rules, DNS, remote rule sets, gateway mode, and durable proxy selection). Dirty is true when the saved Spec + selection produce a different topology hash than the Applied capture; dry-run/apply advance Applied and clear dirty when they succeed.

### `POST /api/config/dryrun`

For `on` and `global`, resolves SmartNodes and durable selections into an
Execution Graph, validates that graph and the rule classifier, prepares every
required Worker candidate through its own adapter check, and prepares the
SmartGate Gateway Fabric plan (embedded DNS, per-Pool IPv4/IPv6 nft sets,
nftables TPROXY dispatch, and policy routes). Worker configs contain only
controlled local ingress and node execution; Workers do not own TUN, LAN DNS,
or host policy routes. DNS returns real addresses and writes them to per-Pool
nft sets; Fake-IP is never prepared.

For `safe` and `off`, the target graph has no active Worker bindings and the
prepared network action stops both known Workers, stops embedded DNS, and
removes only SmartGate owner-tagged Fabric/HostNetwork resources. Legacy
owner-tagged TUN artifacts may be removed during cleanup, but V3 never creates
a TUN data plane.

```json
{
  "op_id": "config-apply-001"
}
```

Envelope data:

```json
{
  "id": "config-apply-001",
  "created_at": "2026-06-25T00:00:00Z",
  "spec_hash": "...",
  "topology_hash": "...",
  "config_epoch": 0,
  "gateway_mode": "on",
  "runtime_action": "apply_transparent_proxy",
  "changes_complete": true,
  "changes_omitted": 0,
  "changes": [
    {"category": "worker", "title": "新增 Xray Worker", "before": "stopped", "after": "w_xr_1", "impact": "AI 将由 Xray 执行"},
    {"category": "pool-impact", "title": "业务池 ai 将更新 Binding", "before": "sn_old", "after": "sn_new", "impact": "strict cutover"}
  ],
  "execution_graph_summary": {
    "workers": {"create": ["w_xr_1"], "keep": ["w_sb_1"], "stop": []},
    "executors": {"create": 1, "reuse": 1, "release": 0},
    "pool_bindings": [
      {"pool": "general", "to": "sn_general", "core": "sing-box", "worker": "w_sb_1", "generation": 3},
      {"pool": "ai", "to": "sn_ai", "core": "xray", "worker": "w_xr_1", "generation": 7}
    ]
  }
}
```

`changes` and `changes_complete` are required by the bundled frontend. Apply
must be disabled when the readable summary is absent or incomplete. The server
binds the dry-run to the complete durable per-Pool Selection tuple
(`smart_node`, `target`, `generation`, `bound_epoch`, and
`bound_topology_hash`), not only to the displayed node name. Any tuple change
invalidates the preview and requires a new dry-run. A persisted preview created
by an older binary without that Selection snapshot is rejected with `428`.
Raw Worker candidates, node credentials, subscription URLs, generated DNS
state, and executable network commands are persisted server-side for the
transaction and are intentionally omitted from the API response. Server-side
candidate metadata is keyed through neutral `core_candidate` records. Runtime
candidate files are ephemeral and have no legacy compatibility contract.

If graph validation or Worker candidate verification fails, dry-run returns a
non-2xx envelope whose `error.details.dryrun` contains the same redacted summary
and blocking changes. Any `blocking:true` change prevents Apply. Apply independently
revalidates the classifier and prepared graph, so a direct API caller cannot
bypass unresolved rule conflicts. Dry-run prepare failures use neutral error
codes: `execution_graph_invalid` for graph blockers and `worker_check_failed`
for Worker candidate checks. During the compatibility window,
`error.details.legacy_error_codes` may include the previous generic
`validation_failed` alias.

Failure messages identify the actual blocking issue. Clients display
`error.details.dryrun.changes` entries with `blocking:true`, including the
affected node and Pool, instead of discarding the failed preview. Removing
the previously applied selection from a Pool resolves a replacement from its
remaining members (or its `no_candidate` policy) in the next preview; it does
not mutate the live selection before Apply.

### `POST /api/config/apply`

Applies the latest matching dry-run through the global commit lane. The
transaction checks stale Spec/topology/config-epoch/selection inputs, runs the
aggregate Worker candidate resource guard, writes the journal and durable
preimage, checkpoints Worker/Fabric state, activates the prepared Workers,
starts embedded DNS and applies the owner-tagged Gateway Fabric, then persists
Applied state and advances `config_epoch`. A failure before the durable commit
restores the previous Worker/Fabric checkpoint; fail-closed cleanup is used if
the previous graph cannot be restored.

```json
{
  "dryrun_id": "config-apply-001",
  "description": "optional UI text"
}
```

Success data includes:

```json
{
  "status": "applied",
  "config_epoch": 1,
  "topology_hash": "...",
  "workers": [
    {"id": "w_sb_1", "status": "running"},
    {"id": "w_xr_1", "status": "running"}
  ],
  "core_results": [
    {"core": "sing-box", "status": "running", "workers": ["w_sb_1"]}
  ],
  "fabric_applied": true,
  "runtime_action": "apply_transparent_proxy",
  "openwrt": {},
  "openwrt_applied": true,
  "resource_guard": {
    "enabled": true,
    "overlay_path": "/etc/smartgate",
    "free_bytes": 10485760,
    "candidate_bytes": 54321,
    "soft_limit_bytes": 524288,
    "hard_limit_bytes": 2097152,
    "warnings": []
  },
  "warnings": []
}
```

`workers` reports Worker activations performed by this transaction.
`core_results` is the neutral per-core summary derived from Worker results.
`fabric_applied` is the authoritative V3 data-plane result. `openwrt` and
`openwrt_applied` expose low-level network evidence; clients must not infer a
specific Worker core or TUN architecture from them. The response does not
include an implementation-named `singbox` field.

Resource guard behavior:

- Each Worker candidate and the aggregate candidate set are bounded; values
  over the reported hard limit are rejected.
- Overlay free space below 2 MB is rejected with `507 Insufficient Storage`.
- Overlay free space below 5 MB or aggregate Worker candidates over 512 KB
  return warnings on success.

Live apply rollback behavior:

- The journal and durable preimage are written before any Worker/Fabric live
  mutation.
- Worker checkpoints include the active Worker set and its status ledger;
  rollback restores the previous set even when a later Worker or Fabric stage
  fails.
- Fabric rollback restores embedded DNS inputs and owner-tagged network state;
  if restoration fails, SmartGate enters fail-closed cleanup rather than
  allowing an intermediate Pool to fall through to Direct.
- Failed transactions do not advance `config_epoch`, Binding, or Generation.
- Rollback detail reports the independently verified recovery state of the old
  Worker set, Gateway Fabric, Pool Bindings, and durable Selection. It never
  includes generated configs, process output, node credentials, or subscription
  material.
- Existing frontends may ignore rollback detail. The bundled frontend renders
  all four recovery dimensions.

Failure envelope with rollback data:

```json
{
  "ok": false,
  "error": {
    "code": "upstream_failed",
    "message": "nft apply failed: ...",
    "details": {
      "rollback": {
        "attempted": true,
        "ok": true,
        "stage": "transaction_apply",
        "reason": "nft apply failed: ...",
        "workers_restored": true,
        "fabric_restored": true,
        "bindings_restored": true,
        "selection_restored": true,
        "errors": []
      }
    }
  }
}
```

Rollback `stage` values are neutral transaction-stage identifiers. Deprecated
core-specific stage/error aliases are accepted only during the compatibility
window and are not emitted by new config apply responses.

### `POST /api/config/discard`

Discards the latest dry-run record. It does not revert already-persisted provider or pool edits.

### `GET /api/gateway/mode`

Envelope data:

```json
{
  "mode": "on"
}
```

### `POST /api/gateway/mode`

Updates desired gateway mode in spec and makes config dirty until `/api/config/dryrun` + `/api/config/apply`.

```json
{
  "mode": "global"
}
```

Supported modes:

- `on`
- `safe`
- `global`
- `off`

Mode semantics:

- `on`: normal whole-home transparent proxy mode. Gateway Fabric owns LAN DNS, nftables TPROXY dispatch, policy routes, and per-Pool IPv4/IPv6 classification sets. Each Pool is sent to its applied Binding; Workers own no TUN, DNS listener, or host route.
- `safe`: safe direct/rescue mode. Applying this mode stops all SmartGate Workers and removes only SmartGate owner-tagged Fabric, DNS, TUN, and HostNetwork resources so LAN devices can use native direct connectivity. Provider refresh remains allowed, but runtime tasks that require an active data plane are locked.
- `global`: whole-home global proxy mode. It uses the same Gateway Fabric takeover as `on`, but public/default traffic and DNS use the applied proxy fallback path. LAN/private destinations remain direct, geo-CN direct fallbacks are skipped, and an unavailable proxy fallback is blocked rather than silently sent Direct.
- `off`: fully disables SmartGate gateway automation. Applying this mode stops Workers and runs the same owner-tagged cleanup plan. Provider refresh, runtime failover, delay test, and automatic config apply are locked while the mode is `off`.

Changing the mode only updates desired spec. The frontend should show config dirty state and call `/api/config/dryrun` then `/api/config/apply` to make it active.

## DNS

DNS management endpoints use the `{ok,data,error}` envelope. Saving DNS settings updates spec and makes config dirty until `/api/config/dryrun` + `/api/config/apply`.

### `GET /api/dns`

Envelope data:

```json
{
  "strategy": "prefer_ipv4",
  "classification_mode": "real_ip_nft_set",
  "fakeip_enabled": false,
  "fakeip_range": "",
  "final": "direct",
  "direct_servers": ["223.5.5.5", "119.29.29.29"],
  "proxy_servers": [],
  "fallback_servers": []
}
```

`classification_mode` is read-only and fixed to `real_ip_nft_set`. SmartGate
returns real A/AAAA answers and writes those addresses, with DNS TTLs, to the
selected Pool's owner-tagged IPv4/IPv6 nft sets. `fakeip_enabled` and
`fakeip_range` remain response/request compatibility fields only and are
always `false` and `""`.

### `PUT /api/dns`

Replaces the desired DNS settings. Omitted fields fall back to defaults.

```json
{
  "strategy": "ipv4_only",
  "final": "proxy",
  "direct_servers": ["1.1.1.1"],
  "proxy_servers": ["https://dns.google/dns-query"],
  "fallback_servers": ["tls://8.8.8.8"]
}
```

Validation:

- `strategy`: `prefer_ipv4`, `prefer_ipv6`, `ipv4_only`, or `ipv6_only`
- `final`: `direct`, `proxy`, or `fallback`
- `direct_servers` must contain at least one server
- `proxy_servers` is required when `final` is `proxy`
- `fallback_servers` is required when `final` is `fallback`
- `fakeip_enabled:true` is rejected with `422 fakeip_not_supported`
- `classification_mode`, when sent, must be `real_ip_nft_set`

The SmartGate embedded Go DNS service listens dual-stack on TCP and UDP
`:1053`; owner-tagged nft ingress guards permit only loopback and the configured
LAN device to reach that socket. It owns Direct/Proxy/fallback upstream
selection. Proxy upstream requests are dialed through the selected Pool's
Worker SOCKS ingress. Workers receive real destinations and do not perform an
independent business DNS decision.

Gateway Fabric rejects `HTTPS`/`SVCB` (TYPE65/64) before any upstream request,
redirects LAN IPv4/IPv6 TCP and UDP 53 to `:1053`, blocks LAN TCP and UDP 853,
and idempotently replaces only SmartGate owner-tagged DNS/DoT resources. If a
real answer cannot be committed to its Pool set, DNS returns `SERVFAIL` so the
flow cannot leak into a Direct fallback.

## Providers

### `GET /api/providers`

```json
{
  "providers": []
}
```

Provider shape:

```json
{
  "id": "airport1",
  "name": "Airport 1",
  "type": "subscription",
  "enabled": true,
  "fetch_mode": "direct",
  "roles": ["general", "ai"],
  "bootstrap": "",
  "url_configured": true,
  "url_masked": "https://example.com",
  "file_path_configured": false,
  "user_agent": "clash-verge/v2.5.2",
  "priority": 10,
  "include_keywords": ["hk", "premium"],
  "exclude_keywords": ["expire", "low"],
  "node_count": 3,
  "last_refresh_at": "2026-07-04T12:00:00Z",
  "subscription": {
    "upload_bytes": 1024,
    "download_bytes": 2048,
    "used_bytes": 3072,
    "total_bytes": 8192,
    "remaining_bytes": 5120,
    "expire_unix": 1799999999,
    "expire_at": "2027-01-15T08:53:19Z",
    "updated_at": "2026-07-04T12:00:00Z"
  }
}
```

Notes:

- `roles`, `include_keywords`, and `exclude_keywords` are always arrays.
- `fetch_mode` and `bootstrap` are always present as strings; empty values are `""`.
- Provider source values are write-only. `url_configured` /
  `file_path_configured` report whether a value exists. `url_masked` contains
  only the URL scheme and host and is omitted when no URL is configured.
- `user_agent` is the optional `User-Agent` header sent when fetching a
  `subscription` provider. It is a plain read/write string (not a secret) and
  is omitted when empty. Empty means the built-in default: `sing-box/1.12.0`
  for `sing-box://import-remote-profile` deep links, otherwise `SmartGate/2.0`.
  Some subscription services return a different format or reject the request
  based on the client UA; set this to a known client string such as
  `clash-verge/v2.5.2` when needed. Accepted values are 1–256 printable ASCII
  characters (`0x20`–`0x7E`); anything else fails provider dryrun/apply with
  `400` like other provider validation errors. The field
  is ignored and cleared for `manual`, `local-file`, and `temporary` providers.
- `node_count`, `last_refresh_at`, and `subscription` come from provider last-good state and are optional.
- `subscription` is present only after a successful provider refresh with a valid `Subscription-Userinfo` response header.
- Frontend should not fetch provider subscription URLs directly. The backend parses subscription usage during `/api/providers/refresh-all` or `/api/providers/{provider_id}/refresh`.
- Traffic display: use `remaining_bytes` for remaining traffic, `used_bytes` for used traffic, `total_bytes` for total traffic, and `expire_at` or `expire_unix` for expiry.
- Usage ratio should be `used_bytes / total_bytes` only when `total_bytes > 0`. `remaining_bytes` is clamped to `0` if used traffic exceeds total traffic.

Supported provider `type`:

- `subscription`
- `manual`
- `local-file`
- `temporary`

Supported `fetch_mode`:

- `direct`
- `via_bootstrap`

### `POST /api/providers/dryrun`

Does not persist.

```json
{
  "op_id": "provider-edit-001",
  "changes": [
    {
      "action": "upsert",
      "provider": {
        "id": "airport1",
        "name": "Airport 1",
        "type": "subscription",
        "enabled": true,
        "url": "https://example.com/sub",
        "fetch_mode": "direct",
        "priority": 10
      }
    }
  ]
}
```

Change actions:

- `upsert`
- `delete`

For an existing provider, omitting `url` or `file_path` preserves the stored
source value. Sending a non-empty value replaces it. The API never returns the
stored source value for edit prefill.

`user_agent` does not use the preserve-on-omit rule: the upsert payload is the
full desired value, and an omitted or empty `user_agent` resets the provider to
the default UA. Clients prefill it from `GET /api/providers`.

Delete example:

```json
{
  "op_id": "provider-delete-001",
  "changes": [
    { "action": "delete", "id": "airport1" }
  ]
}
```

### `POST /api/providers/apply`

Same payload as dryrun. Persists desired provider spec through the global commit lane. This does **not** mean the live Worker/Fabric runtime has applied the new topology; `/api/config/state` remains dirty until `/api/config/dryrun` and `/api/config/apply` succeed.

Provider apply also reconciles persisted node state:

- Disabling a provider keeps its last-good file so it can be re-enabled without forcing an immediate refresh.
- Deleting a provider prunes that provider's last-good file.
- Any last-good files whose provider IDs are no longer present in spec are pruned during provider apply.
- Business-pool node IDs, runtime proxy selections, node health, and failover state are cleaned when they point at deleted/disabled/non-usable nodes.

Possible status:

- `202 Accepted`: applied
- `423 Locked`: rescue mode or commit gate blocked
- `400 Bad Request`: validation error

Dry-run and apply return an API-safe transaction summary containing `plan`,
`provider_count`, `config_epoch`, `topology_hash`, and `dirty`. They never
return the complete Spec or Runtime state.

### `POST /api/providers/refresh-all`

Refresh all enabled refreshable providers.

Refreshable provider types:

- `subscription`: fetches `url`.
- `local-file`: reads and parses `file_path`.

### `POST /api/providers/{provider_id}/refresh`

Refresh one provider.

Refresh result shape:

```json
{
  "plan": {
    "steps": [
      {
        "provider_id": "airport1",
        "fetch_mode": "direct"
      }
    ]
  },
  "results": [
    {
      "provider_id": "airport1",
      "status": "ok",
      "changed": true,
      "metadata_only": false,
      "count": 3,
      "subscription": {
        "upload_bytes": 1024,
        "download_bytes": 2048,
        "used_bytes": 3072,
        "total_bytes": 8192,
        "remaining_bytes": 5120,
        "expire_unix": 1799999999,
        "expire_at": "2027-01-15T08:53:19Z",
        "updated_at": "2026-07-04T12:00:00Z"
      }
    }
  ],
  "changed": true
}
```

If the provider refresh succeeds but `Subscription-Userinfo` is malformed, refresh still succeeds and the result item includes `subscription_error`; `subscription` is omitted in that case.

`metadata_only:true` means the refreshed nodes changed only alias/provenance
metadata (for example, Provider node names) and did not add/remove a
SmartNode, increment a SmartNode Revision, or change an execution identity.
The frontend must present this as a metadata update rather than node churn.

Refresh `status` values:

- `ok`
- `unchanged`
- `stale`
- `failed`

### `GET /api/providers/{provider_id}/nodes`

Envelope endpoint. Returns an API-safe last-good summary. Raw outbound
credentials are never returned.

```json
{
  "ok": true,
  "data": {
    "provider_id": "manual",
    "saved_at": "2026-06-25T00:00:00Z",
    "count": 1,
    "subscription": {
      "upload_bytes": 1024,
      "download_bytes": 2048,
      "used_bytes": 3072,
      "total_bytes": 8192,
      "remaining_bytes": 5120,
      "expire_unix": 1799999999,
      "expire_at": "2027-01-15T08:53:19Z",
      "updated_at": "2026-07-04T12:00:00Z"
    },
    "configured": true
  }
}
```

### `PUT /api/providers/{provider_id}/nodes`

Envelope endpoint. Writes manual node payload into last-good. Allowed only for `manual` and `temporary` providers. This is protected by the commit lane and does not dirty provider spec.

The success response uses the same safe summary as GET and does not echo the
submitted outbounds.

```json
{
  "provider_id": "manual-1",
  "saved_at": "2026-06-25T00:00:00Z",
  "count": 1,
  "configured": true
}
```

### `GET /api/providers/logs`

Envelope endpoint. Returns recent provider refresh logs from runtime state.

```json
{
  "ok": true,
  "data": {
    "logs": [
      {
        "time": "2026-06-25T00:00:00Z",
        "scope": "all",
        "provider_id": "airport1",
        "status": "ok",
        "changed": true,
        "count": 3
      }
    ]
  }
}
```

## SmartNodes

### `GET /api/nodes`

Returns the merged SmartNode catalog built from desired provider last-good
snapshots and saved SmartNode execution choices. IDs are independent of
Provider ID and alias. Supported filters are `provider`, `protocol`, `core`,
`capability`, and `health`.

Nodes without an explicit execution-core choice default to `sing-box`. This
choice is persisted on import, subscription refresh and save; startup migrates
older saved nodes with an empty choice before scheduled tasks run. Existing
explicit choices, including Xray, are retained. Assigning the default does not
bypass core capability validation or add nodes to a business Pool.

The catalog is ordered by configured Provider order, then subscription node
order within each Provider, not by SmartNode hash. Shared nodes appear once,
under their first active Provider. Retained nodes without an active source
appear last.

```json
{
  "nodes": [
    {
      "id": "sn_0123456789abcdef",
      "revision": 1,
      "display_name": "node-a",
      "identity": {
        "protocol": "vless",
        "server": "example.com",
        "port": 443,
        "transport": {
          "type": "xhttp",
          "path": "/api"
        },
        "security": {
          "type": "reality",
          "sni": "www.example.com",
          "fingerprint": "chrome",
          "flow": "xtls-rprx-vision",
          "public_key_masked": "****cafe",
          "short_id": "0123abcd"
        },
        "secret_ref_masked": "sha256:****beef"
      },
      "aliases": [
        {
          "provider_id": "airport1",
          "raw_tag": "node-a",
          "first_seen": "2026-07-28T00:00:00Z",
          "last_seen": "2026-07-28T00:00:00Z"
        }
      ],
      "capability": {
        "sing-box": {
          "status": "degraded",
          "reasons": ["transport.xhttp_semantics_incomplete"],
          "reason_texts": ["sing-box 无法完整表达该 XHTTP 传输语义"]
        },
        "xray": {
          "status": "supported",
          "reasons": []
        }
      },
      "core_choice": "xray",
      "referenced_by_pools": ["ai"],
      "health": {
        "status": "ok",
        "delay_ms": 88,
        "updated_at": "2026-07-28T00:00:00Z",
        "context": {
          "revision": 1,
          "core": "xray",
          "worker": "w_xr_1",
          "method": "worker_route_http_204"
        }
      }
    }
  ]
}
```

`identity.secret_ref_masked` and `security.public_key_masked` are display-only
masks. The API never returns the stored digest or the source credential.
`aliases`, `referenced_by_pools`, and each capability's `reasons` are always
arrays, including `[]` when empty; they are never `null`. Clients should
also tolerate `null` from older V3 builds during an upgrade.

### `GET /api/nodes/{id}`

Returns one SmartNode in the same shape as `GET /api/nodes`, including full
alias history and health context. Unknown IDs return `404 node_not_found`.

### `PUT /api/nodes/{id}/core-choice`

Persists the desired execution core and makes config dirty. It does not alter
the applied Execution Graph until config dry-run/apply succeeds.

```json
{
  "core": "xray",
  "confirm_degraded": false,
  "op_id": "node-core-001"
}
```

Success data:

```json
{
  "smart_node": {},
  "affected_pools": ["ai", "crypto"],
  "dirty": true
}
```

`affected_pools` is an array, including `[]` when the node is not referenced.

Validation:

- `unsupported_by_core` returns `422 capability_unsupported` with `reasons`.
- `degraded` without `confirm_degraded:true` returns
  `409 confirm_degraded_required`.
- `core` must identify a registered core returned by `GET /api/cores`.
- The backend independently evaluates capability and never trusts client
  capability data.

### `POST /api/nodes/{id}/delay`

Runs an HTTP(S) health probe using the SmartNode's selected execution core.
When the desired node matches an applied executor, the existing Worker route
is reused. Otherwise a short-lived, loopback-only probe process uses the
desired provider snapshot and exits after the request. Pool membership and
config Apply are not prerequisites for a manual probe. Probing never adds
nodes to a Pool or changes live Pool selections, Spec, or Applied state.
There is no TCP-only reachability fallback.
For legacy captures whose desired core choice is still unset, a probe may
reuse the applied core only when the identity and revision are unchanged;
this does not persist a desired choice.

The default target is `https://www.gstatic.com/generate_204`. Missing sources,
unset/unsupported cores, and probe setup failures return a non-2xx error and
do not overwrite node health as a connectivity failure. Only a completed
HTTP(S) probe records healthy/failed runtime health. Isolated probe processes
are globally serialized to bound router memory use, have a bounded lifetime,
and remove their private temporary configs on exit. The node page's "test all"
action uses the bounded full-node executor below and can cancel its request.
`timeout_ms` defaults to 5000 and must be in `0..30000` (0 selects the
default); it bounds the HTTP(S) measurement, with a separate bounded setup
budget. The URL must use HTTP(S) and cannot contain user-info credentials.
An isolated result uses `health.context.method=isolated_worker_http_204`
without a persistent Worker ID. Health from a different node revision or
core is not presented as the current node's health or used for applied
failover decisions.

A Worker that no Pool Binding references (its Pool is on the `direct`/`block`
fallback) is installed but stopped; the probe starts it on demand through the
commit lane before dialling, so the failover watchdog can see the candidate
healthy again and recover the Pool (`healthy_candidate_recovered`). The
started Worker stays up until the next config apply reconciles the Worker set.
In `off`/`safe` mode no applied Worker is started. Explicit manual probes can
still use an isolated process without enabling the gateway; watchdog probes
remain bound to the applied topology.

Xray probes use a temporary loopback-only SOCKS inbound and an appended routing
rule pinned to the requested executor. Probing an unselected candidate does
not override a live Pool balancer or change its Binding. The inbound and rule
are removed after success, failure, or cancellation; cleanup has a separate
bounded deadline. A failed manual route probe is retried with an isolated
process, so control-channel failures are not treated as proof that the
upstream is unreachable. Watchdog route failures remain visible to failover.

```json
{
  "url": "https://www.gstatic.com/generate_204",
  "timeout_ms": 5000
}
```

Envelope data:

```json
{
  "node_id": "sn_0123456789abcdef",
  "health": {
    "status": "ok",
    "delay_ms": 88,
    "updated_at": "2026-07-28T00:00:00Z",
    "context": {
      "revision": 1,
      "core": "xray",
      "worker": "w_xr_1",
      "method": "worker_route_http_204"
    }
  }
}
```

`status` is `ok` or `failed`. Failed tests keep the error string in `health.error`.

### `POST /api/nodes/{id}/deep-test`

Reserved for the deferred deep measurement system. Returns
`501 deep_test_not_implemented` and does not start background work.

## Workers

Worker endpoints are read-only. Worker lifecycle is owned by ConfigTxn,
startup recovery, supervision, and Rescue; there are no manual start/stop
endpoints.

### `GET /api/workers`

Envelope data:

```json
{
  "workers": [
    {
      "id": "w_xr_1",
      "core": "xray",
      "version": "26.3.27",
      "state": "running",
      "executors": [
        {"id":"ne_...","smart_node":"sn_...","revision":1,"core":"xray","worker":"w_xr_1"}
      ],
      "pools": ["ai"],
      "stats": {"rss_kb": 40960,"fd": 32,"connections": 2},
      "last_event": {"at":"2026-07-28T08:00:00Z","worker":"w_xr_1","kind":"ready","state":"running","message":"Worker 已就绪"}
    }
  ]
}
```

Every registered Worker is returned, including Workers that are stopped.
`executors` and `pools` are always arrays. Candidate paths, control secrets,
and raw process output are never returned. On live startup, each built-in
Adapter probes its own binary and the process fails to start if a version
cannot be obtained. The detected value is registered in the core Descriptor,
copied into the ExecutionGraph Worker target, and returned as `version` for
running and stopped Workers. Development-only adapters may declare a static
Descriptor version; `version` is omitted when a non-live adapter declares none.

### `GET /api/workers/{id}/events`

Envelope data is `{ "worker_id":"w_xr_1", "events":[...] }`. Events are a
bounded in-memory lifecycle ring and use the same event shape as
`last_event`. `kind` and `state` remain stable machine values, while `message`
is a Chinese lifecycle summary. Failed or unknown events can additionally
include a sanitized `diagnostic` containing the original adapter/process error;
clients must present it as secondary diagnostic content. An unknown worker
returns `404 worker_not_found`.

### `POST /api/nodes/delay-all`

Runs the same `delay_test` executor as the scheduled full-node task against
non-excluded nodes in the applied topology. Uses up to 24 concurrent probes,
50 ms global launch pacing, five-second probe deadlines, and a 28-second round
deadline. Results are merged once without changing configuration, Pool choices,
or watchdog failure evidence. Private loopback probe processes group all nodes
by their applied execution core, including nodes outside Pools; one batch shares
the isolated-probe memory slot and closes every process/listener/config on exit.
Returns `409` for an overlapping full-node round,
`423` in off/safe mode, and `501` without an executor. `data.delay_test` contains
`total`, `tested`, `ok`, `failed`, and `unknown`; unfinished probes are unknown.
Latency availability matches V2 core URL tests: a verified HTTP exchange is
measurable regardless of the origin status; transport/TLS failures fail the
measurement. Request cancellation leaves uncompleted nodes untouched. Probe setup failures
do not count as connectivity failures. Each result retains
its applied revision/core context, including unknown results.

A node whose chosen core cannot provide an isolated probe uses an installed
route only for that node; it never disables batch probing for other nodes.
Missing core/route support is `unknown`, not a failed upstream measurement.

Watchdog failures are independently confirmed against
`https://cloudflare.com/cdn-cgi/trace`. Runtime health retains
`watchdog_status`, `watchdog_updated_at`, `watchdog_context`, and
`consecutive_failures` separately
from manual/bulk measurements. Automatic switching requires a fresh healthy
replacement verified by the watchdog; when all probes fail it keeps the current
node. Health evidence from another revision or core is never reused.

## Connections

Connections endpoints use the `{ok,data,error}` envelope and merge observations
from the active Worker controls with the durable Applied Binding metadata. They
are intended for polling from the frontend; `/api/events` currently streams
runtime status, not per-connection deltas. They require a captured Applied
Topology; SmartGate does not fall back to a global core-specific control payload.

### `GET /api/connections`

Returns current active connections and traffic totals derived from neutral
Worker connection records and the Fabric connection ledger. Optional query
parameters `pool` and `worker` filter the normalized list. Each connection always includes
`id`, `metadata`, `upload`, `download`, `chains`, `pool`, `generation`,
`stale_generation`, `smart_node`, `worker`, and `core`; missing observation
fields are normalized to `""`, `{}`, `0`, `false`, or `[]`.
In particular, kernel-only flows have `chains: []`, never `null`.

Envelope data:

```json
{
  "connections": [
    {
      "id": "conn-1",
      "metadata": {
        "host": "example.com",
        "network": "tcp",
        "type": "http"
      },
      "upload": 128,
      "download": 1024,
      "chains": ["route_ai", "ne_xr_abcd"],
      "pool": "ai",
      "generation": 7,
      "stale_generation": false,
      "smart_node": "sn_0123456789abcdef",
      "worker": "w_xr_1",
      "core": "xray"
    }
  ],
  "download_total": 1024,
  "upload_total": 512
}
```

`stale_generation:true` means the observed connection belongs to an older
Generation than the current durable Binding for that Pool.

`metadata.host` remains the hostname reported by the Worker. When available,
`metadata.dnsHosts` contains up to four recent DNS names associated with the
same source client and destination address. These are DNS associations, not
proof of the hostname used by this connection: multiple names can share an IP.
Associations expire with DNS TTL, remain memory-only and bounded, and never
trigger reverse DNS queries. Queries bypassing SmartGate DNS may have no hint.
`chains` remains empty when the core does not report a chain; the UI can show
the scoped Pool/Core/SmartNode binding as a separately labelled forwarding path.

Raw Worker control
payloads are not returned because they may contain implementation details that
are not part of the public contract. When a Worker explicitly reports that
connection observation is unsupported, the Fabric connection ledger supplies
that scope's records. Ledger failures and other Worker errors still fail the
request; unsupported observation without a Fabric ledger also remains an error.
If no Applied Topology is captured, the endpoint returns an error.

### `POST /api/connections/close`

Closes one connection, all connections belonging to one Pool Binding, or all
observed connections. Binding and global scopes are disruptive and require
confirmation. Global close is retained for diagnostics only.

```json
{
  "scope": "binding",
  "pool": "ai",
  "confirm": true
}
```

Payload rules:

- `scope:"single"` requires `id`; `confirm` is optional.
- `scope:"binding"` requires `pool` and `confirm:true`.
- `scope:"all"` requires `confirm:true`.

Success status is `202 Accepted` with
`{scope,closed,closed_exact,pool?,id?}`. `closed_exact` is `true` when SmartGate
can verify the number against its live connection ledger. Global close requires
the Applied Binding scope; without an Applied Topology it returns
`501 Not Implemented` instead of using a core-specific global close fallback.
Unknown connections or Pools return `404`.

## Proxy Groups

### `GET /api/proxy-groups`

Envelope endpoint. Returns the live Binding and available Applied candidates for
each business pool. Candidate membership comes only from the Applied topology;
unapplied provider refreshes or desired edits never become selectable here.
Reserved `Direct` and `Block` targets have no `core`.

```json
{
  "ok": true,
  "data": {
    "groups": [
      {
        "pool": "ai",
        "binding": {
          "id": "pb_8c6d...",
          "pool": "ai",
          "target": {
            "kind": "node",
            "smart_node": "sn_8c6d...",
            "executor": "ne_xr_...",
            "core": "xray",
            "worker": "w_xr_1"
          },
          "generation": 7,
          "since": "2026-07-29T08:00:00Z"
        },
        "candidates": [
          {"smart_node":"sn_8c6d...","core":"xray","health":"ok","delay_ms":82},
          {"smart_node":"Block","health":"unknown"}
        ],
        "lock": null,
        "cooldown_until": null,
        "failover_enabled": true
      }
    ]
  }
}
```

### `POST /api/proxy-groups/{id}/select`

Envelope endpoint. Performs a Pool-scoped CutoverTxn against the live Binding.
The full `source` object is a CAS precondition; both SmartNode and Generation
must match the durable live Binding. `expected_config_epoch` protects the
Applied topology and `op_id` is required for transaction journaling.

```json
{
  "target_smart_node": "sn_f2a1...",
  "source": {"smart_node":"sn_8c6d...","generation":7},
  "expected_config_epoch": 12,
  "op_id": "manual-cutover-ai-20260729-1"
}
```

For a `strict` pool, success is returned only after old-Generation connections
are closed and the new Binding path passes exit verification:

```json
{
  "ok": true,
  "data": {
    "result": "cutover_complete",
    "binding": {"id":"pb_...","pool":"ai","target":{"kind":"node","smart_node":"sn_f2a1...","executor":"ne_xr_...","core":"xray","worker":"w_xr_1"},"generation":8,"since":"2026-07-29T08:01:00Z"},
    "closed_connections": 4,
    "verified": true
  }
}
```

If an explicitly permitted strict policy leaves residual connections, the
result is `cutover_partial` and includes `residual_connections`. The current V3
policy model does not permit residual strict success, so a non-zero residual is
normally a `cutover_failed` response.

For a `graceful` pool, the new Binding is committed immediately and the old
Generation drains asynchronously until empty or until the policy deadline:

```json
{
  "ok": true,
  "data": {
    "result": "committed",
    "binding": {"id":"pb_...","pool":"general","target":{"kind":"node","smart_node":"sn_f2a1...","executor":"ne_sb_...","core":"sing-box","worker":"w_sb_1"},"generation":4,"since":"2026-07-29T08:01:00Z"},
    "verified": true,
    "draining": {"old_generation":3,"remaining":9,"deadline":"2026-07-29T08:01:30Z"}
  }
}
```

Pool selection and compensation reuse the currently applied GEO snapshots,
including materialized GeoIP sets; they do not refresh rule-set sources.
Validation compares the resolved Fabric state and rejects changes outside the
target Pool. The transient snapshot-pinning flag is not a configuration change.

Cutover errors use stable codes:

- `409 selection_conflict`: `details` contains `pool`, `expected`, `actual`,
  and optionally `reason`.
- `409 requires_config_apply`: the target needs a Worker/Executor layout not
  present in the Applied graph; `details` contains `pool`, `target`, `reason`.
- `409 cutover_conflict`: the Pool already has a cutover or old-Generation
  drain in progress.
- `502 cutover_failed`: execution or verification failed; `details` contains
  `pool`, `stage`, `rolled_back`, and `blocked`. A local persistence failure may
  use HTTP `500` with the same code and details.

Cutover never performs a global connection close and does not dirty config.

### `POST /api/proxy-groups/failover/run`

Envelope endpoint. Evaluates proxy groups against persisted node health and
applies every accepted decision through the same Pool-scoped CutoverTxn used by
manual selection. No-candidate behavior comes from each Pool policy; it is not
derived from the Pool name. Pool candidates come from the Applied graph.

Success data:

```json
{
  "decisions": [
    {
      "pool_id": "ai",
      "role": "ai",
      "from": "airport1:old",
      "to": "Block",
      "kind": "fail_closed",
      "reason": "no_healthy_sensitive_pool_candidate",
      "time": "2026-06-25T00:00:00Z"
    }
  ],
  "applied": [
    {
      "pool_id": "ai",
      "role": "ai",
      "from": "airport1:old",
      "to": "Block",
      "kind": "fail_closed",
      "reason": "no_healthy_sensitive_pool_candidate",
      "time": "2026-06-25T00:00:00Z"
    }
  ],
  "events": [
    {
      "time": "2026-06-25T00:00:00Z",
      "pool_id": "ai",
      "role": "ai",
      "from": "airport1:old",
      "to": "Block",
      "kind": "fail_closed",
      "reason": "no_healthy_sensitive_pool_candidate",
      "generation_from": 7,
      "generation_to": 8,
      "executor": "ne_xr_...",
      "cutover_result": "cutover_complete",
      "source": "manual",
      "status": "applied"
    }
  ]
}
```

### `GET /api/proxy-groups/failover/events`

Envelope endpoint. Returns the retained failover event ring buffer from persistent storage. Events are capped to the newest 200 records.

```json
{
  "ok": true,
  "data": {
    "events": [
      {
        "time": "2026-06-25T00:00:00Z",
        "pool_id": "ai",
        "role": "ai",
        "from": "airport1:old",
        "to": "Block",
        "kind": "fail_closed",
        "reason": "no_healthy_sensitive_pool_candidate",
        "generation_from": 7,
        "generation_to": 8,
        "executor": "ne_xr_...",
        "cutover_result": "cutover_complete",
        "source": "watchdog",
        "status": "applied"
      }
    ]
  }
}
```

### `GET /api/proxy-groups/failover/state`

Envelope endpoint. Returns persistent failover control state from `failover_state.json`.

```json
{
  "ok": true,
  "data": {
    "enabled": true,
    "cooldown_seconds": 60,
    "locks": {
      "ai": {
        "reason": "maintenance",
        "locked_at": "2026-06-25T00:00:00Z"
      }
    },
    "pools": {
      "ai": {
        "pool_id": "ai",
        "role": "ai",
        "current": "Block",
        "previous": "airport1:old",
        "last_kind": "fail_closed",
        "last_reason": "no_healthy_sensitive_pool_candidate",
        "last_changed_at": "2026-06-25T00:00:00Z",
        "cooldown_until": "2026-06-25T00:01:00Z",
        "terminal": true
      }
    }
  }
}
```

### `POST /api/proxy-groups/failover/enable`

```json
{
  "enabled": true
}
```

### `POST /api/proxy-groups/failover/lock`

Locks one pool so automatic failover will skip it.

```json
{
  "pool_id": "ai",
  "reason": "maintenance"
}
```

### `POST /api/proxy-groups/failover/unlock`

```json
{
  "pool_id": "ai"
}
```

### `POST /api/proxy-groups/failover/revert`

Reverts a pool to its previous recorded target through the same Pool-scoped
CutoverTxn, using the current durable Binding as the full CAS source, then
records a `manual_revert` event with Generation/executor/cutover-result fields.

```json
{
  "pool_id": "ai"
}
```

## Business Pools

### `GET /api/business-pools`

```json
{
  "business_pools": [
    {
      "id": "general",
      "name": "General",
      "node_ids": ["sn_0123456789abcdef"],
      "policy": {
        "cutover": "graceful",
        "graceful_drain_seconds": 30,
        "no_candidate": "direct",
        "auto_failover": true
      },
      "builtin_origin": "general",
      "members_core_summary": {"sing-box": 1, "xray": 0, "unset": 0}
    }
  ]
}
```

Pool shape:

```json
{
  "id": "ai",
  "name": "AI",
  "node_ids": ["sn_0123456789abcdef"],
  "policy": {
    "cutover": "strict",
    "graceful_drain_seconds": 30,
    "no_candidate": "block",
    "auto_failover": true
  },
  "builtin_origin": "ai"
}
```

`node_ids` is always an array.

Policy values:

- `cutover`: `strict` or `graceful`. V2 pools with no policy migrate to
  `strict` to preserve immediate connection interruption. Explicit V3 policies
  and the graceful defaults for newly created V3 general/streaming pools remain.
  Strict new-path verification confirms a failed primary target through the
  independent Cloudflare target before rejecting the candidate.
- `graceful_drain_seconds`: `1..3600`; ignored by strict cutover.
- `no_candidate`: `block`, `direct`, or `hold`.
- `auto_failover`: boolean.

`builtin_origin` is provenance only and must never drive runtime policy.
Template defaults are written into the initial pool objects. Saved policy is
always authoritative, including after a pool is renamed.

### `POST /api/business-pools/dryrun`

```json
{
  "op_id": "pool-edit-001",
  "changes": [
    {
      "action": "upsert",
      "pool": {
        "id": "ai",
        "name": "AI",
        "node_ids": ["sn_0123456789abcdef"],
        "policy": {
          "cutover": "strict",
          "graceful_drain_seconds": 30,
          "no_candidate": "block",
          "auto_failover": true
        },
        "builtin_origin": "ai"
      }
    }
  ]
}
```

### `POST /api/business-pools/apply`

Same payload as dryrun. Persists desired business-pool spec through the global commit lane. This does **not** mean the live Worker/Fabric runtime has applied the new topology; `/api/config/state` remains dirty until `/api/config/dryrun` and `/api/config/apply` succeed.

Dry-run blocks members whose SmartNode has no `core_choice` or whose selected
core is unsupported. A `direct` no-candidate policy is accepted and produces a
`direct_fallback` risk change in the global config dry-run.

## Runtime Dry Runs

Data-plane configuration dry-runs use `POST /api/config/dryrun`. Runtime
debug dry-runs under `/api/runtime` are limited to platform/Fabric plans.

### `POST /api/runtime/openwrt/dryrun`

```json
{
  "op_id": "openwrt-dryrun-001"
}
```

Returns owner-tagged OpenWrt command plan.

### `POST /api/runtime/openwrt/rescue-cleanup/dryrun`

```json
{
  "op_id": "openwrt-cleanup-001"
}
```

Returns cleanup plan for SmartGate-owned resources. The live rescue enter path executes this plan on OpenWrt/ImmortalWrt; non-OpenWrt host smoke runs record the plan without executing system commands unless `SMARTGATE_RESCUE_EXECUTE=1` is set.

## Routes

When an enabled `geosite-cn` rule routes to Direct, Fabric inserts the V2
Google/GFW-sensitive domain exceptions ahead of GEO Direct. Explicit user
rules and business-domain routes retain precedence; exceptions target the
configured general-role Pool, including a renamed Pool. DNS uses that same
classification and records the returned real addresses in the selected Pool's
sets. In global mode, GEO Direct and saved custom Direct rules are omitted from
the effective graph while built-in Direct exceptions remain, matching V2.


> Hand-authoring guide for AI agents (schemas, boundaries, anti-patterns): [`docs/RULESET_AUTHORING.md`](./RULESET_AUTHORING.md).


### `GET /api/rules`

Envelope data is `{rules:[...],conflicts:[...]}`. `rules` is the complete desired
rule catalog; `conflicts` is the current unresolved user-rule ownership conflict
list. Rules are target-neutral records; `pool_id` may name any configured Pool or
the reserved targets `direct` and `block`. Built-in V2 seeds and GEO defaults are
explicit records, not silently reconstructed fixed groups.

```json
{
  "rules": [
    {
      "id": "builtin-v2-direct-domain_suffix-home-arpa",
      "source": "builtin-v2",
      "kind": "domain_suffix",
      "value": "home.arpa",
      "pool_id": "direct",
      "enabled": true
    },
    {
      "id": "user-openai",
      "source": "user",
      "kind": "domain_suffix",
      "value": "openai.com",
      "pool_id": "ai",
      "enabled": true
    }
  ],
  "conflicts": []
}
```

Fields:

- `source`: `builtin-v2`, `geo-default`, or `user`.
- `kind`: `domain_suffix`, `domain_keyword`, `ip_cidr`, `geosite`, or `geoip`.
- `overridden_by`: present on an enabled built-in/GEO rule when an enabled user
  rule with the same normalized `kind + value` overrides it.

### `PUT /api/rules`

Atomically persists the complete desired rule catalog. Built-in/GEO rule
content and provenance are immutable; clients may change only their `enabled`
state. User rules may be added, edited, moved, disabled, or deleted. Draft
conflicts are accepted so the UI can save work before resolving them. Clients
must begin with the latest `GET /api/rules` result and send every system record
back alongside user edits; omitted built-in/GEO records are rejected.

```json
{
  "rules": [
    {
      "id": "builtin-v2-...",
      "source": "builtin-v2",
      "kind": "domain_suffix",
      "value": "home.arpa",
      "pool_id": "direct",
      "enabled": true
    },
    {
      "id": "user-openai",
      "source": "user",
      "kind": "domain_suffix",
      "value": "openai.com",
      "pool_id": "ai",
      "enabled": true
    }
  ]
}
```

Envelope data is `{rules:[...],conflicts:[...]}`. Conflict entries use
`{rule_key,kind,value,pools,rule_ids}`. Saving makes config dirty.

### `GET /api/rules/conflicts`

Returns the current desired conflict array. Two or more enabled user rules with
the same normalized `kind + value` and different targets are a conflict. A user
rule overrides matching built-in/GEO defaults and therefore does not conflict
with those defaults.

### `POST /api/rules/resolve-conflict`

```json
{
  "rule_key": "domain_suffix\\u0000youtube.com",
  "keep_pool": "streaming",
  "op_id": "rule-resolve-001"
}
```

Keeps the matching enabled user rule in `keep_pool` and removes matching user
rules owned by other targets. `rule_key` must name a currently unresolved
conflict and `keep_pool` must be one of that conflict's current owners; arbitrary
targets cannot be used to delete all conflicting rules. Repeating a successful
request is idempotent and returns the same `{rules,conflicts}` shape as
`GET/PUT /api/rules`.

Config dry-run emits one blocking `rule-conflict` change per unresolved
conflict. Config apply independently revalidates the desired rules and returns
`422 rule_conflict_unresolved`; API callers cannot bypass the dry-run gate.

Effective rules are compiled into the Gateway Fabric. User rules have higher
priority than built-in/GEO defaults even when the competing matchers use
different domain matcher kinds (for example, a user suffix overrides a GEO
exact-domain entry). Within the default tier, ordering remains Block → Direct →
business Pools → fallback Pool.

## Remote Rule Sets

Remote rule sets let the frontend manage core rule-set entries for geo fallback and ad-block lists without forcing SmartGate to load full lists into backend memory. SmartGate supports both remote URL rule sets and pre-downloaded local `.srs` files under registered core config directories.

Business split rules for AI, streaming, banking, and crypto are managed by SmartGate's local rule groups and business pools, not by default remote geosite rule sets. Deprecated business geosite IDs are filtered if sent back by older frontends.

### `GET /api/rule-sets`

Envelope data is the effective remote-rule configuration, including built-in defaults and warnings.

Defaults:

- `enabled: true`
- `download_detour: Proxy`
- `geosite-cn`: present but disabled, target `direct`
- `geoip-cn`: present but disabled, target `direct`
- `geosite-category-ads`: present but disabled; effective only when `enabled` (master) && `ad_block.enabled` && the set's own `enabled`
- `ad_block.enabled: false`
- memory guard rejects obviously too-large enabled sets before saving

```json
{
  "enabled": true,
  "default_format": "binary",
  "download_detour": "Proxy",
  "update_interval": "24h",
  "memory_guard": {
    "max_enabled_sets": 12,
    "max_total_bytes": 8388608,
    "max_set_bytes": 4194304,
    "warn_total_bytes": 5242880,
    "warn_enabled_sets": 8
  },
  "sets": [
    {
      "id": "geosite-category-ads",
      "name": "Ads Domain Blocklist",
      "enabled": false,
      "kind": "adblock",
      "target": "block",
      "format": "binary",
      "type": "remote",
      "url": "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-category-ads.srs",
      "download_detour": "Proxy",
      "update_interval": "24h",
      "estimated_bytes": 2097152,
      "tags": ["ads", "block", "geo"]
    },
    {
      "id": "geosite-cn",
      "name": "GeoSite CN",
      "enabled": false,
      "kind": "geo",
      "target": "direct",
      "format": "binary",
      "type": "remote",
      "url": "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs",
      "download_detour": "Proxy",
      "update_interval": "24h",
      "estimated_bytes": 819200,
      "tags": ["geo", "cn", "direct"]
    },
    {
      "id": "geoip-cn",
      "name": "GeoIP CN",
      "enabled": false,
      "kind": "geo",
      "target": "direct",
      "format": "binary",
      "type": "remote",
      "url": "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs",
      "download_detour": "Proxy",
      "update_interval": "24h",
      "estimated_bytes": 921600,
      "tags": ["geo", "cn", "direct"]
    }
  ],
  "ad_block": {
    "enabled": false,
    "mode": "remote",
    "sets": ["geosite-category-ads"]
  },
  "warnings": []
}
```

### `PUT /api/rule-sets`

Persists normalized remote rule-set settings into spec. Config becomes dirty until `/api/config/dryrun` + `/api/config/apply`.

```json
{
  "enabled": true,
  "default_format": "binary",
  "download_detour": "Proxy",
  "update_interval": "24h",
  "memory_guard": {
    "max_enabled_sets": 12,
    "max_total_bytes": 8388608,
    "max_set_bytes": 4194304,
    "warn_total_bytes": 5242880,
    "warn_enabled_sets": 8
  },
  "sets": [
    {
      "id": "geosite-cn",
      "name": "GeoSite CN",
      "enabled": true,
      "kind": "geo",
      "target": "direct",
      "format": "binary",
      "type": "local",
      "path": "/etc/core-config/rule-set/geosite-cn.srs",
      "estimated_bytes": 819200
    }
  ],
  "ad_block": {
    "enabled": true,
    "mode": "remote",
    "sets": ["geosite-category-ads"]
  }
}
```

Allowed values:

- `target`: `direct`, `proxy`, `block`
- `kind`: `geo`, `adblock`, `custom`, `provider`
- `format`: `binary`, `source`
- `type`: `remote`, `local`

Validation:

- duplicate or unsafe IDs return `400`.
- remote rule sets require `url` and URLs must be `http(s)://`.
- local rule sets require absolute `path` under a registered core Descriptor
  `ConfigDirs[]` entry. If the runtime has no core registry or no registered
  config directories, local rule-set writes return `400`. Local rule sets omit
  `url`, `download_detour`, and `update_interval` after normalization.
- enabled sets whose `estimated_bytes` exceed `memory_guard.max_set_bytes`, `max_total_bytes`, or `max_enabled_sets` return `400`.
- `warnings` are advisory only and are returned when enabled sets exceed warn thresholds.

Generated runtime behavior:

- `on`: enabled `geosite-cn` and `geoip-cn` produce `Direct` route rules, preserving the classic geo fallback behavior. On production routers, prefer local `.srs` paths to avoid runtime startup depending on remote downloads.
- `global`: default geo direct remote rules are omitted so public/default traffic stays global proxy. LAN/private protection remains direct.
- ad-block: an adblock-kind set is active only when remote rules master `enabled`, `ad_block.enabled`, and the set's own `enabled` are all true; it then routes to `Block`. `ad_block.sets` is retained for compatibility and does **not** gate enablement, including in the native rule catalog and pinned Fabric matcher. The large remote ad list is disabled by default (`item.enabled=false`, `ad_block.enabled=false`) to avoid surprising small-router memory pressure.

### `POST /api/routes/explain`

When an Applied topology exists, this endpoint explains the **live** route by
rebuilding the Execution Graph from the captured Applied Spec plus durable
Selection state, then evaluating the exact pinned GEO matcher snapshots used by
Gateway Fabric. Saved-but-unapplied rule edits are not exposed as running
behavior. There is no `.cn` suffix heuristic: GEO results are returned only
when a pinned matcher actually matches. Before the first successful Apply, the
endpoint provides a desired-rule preview for non-GEO rules and omits Binding and
DNS-path runtime details.

```json
{
  "target": "https://chatgpt.com/codex"
}
```

```json
{
  "target": "chatgpt.com",
  "decision": "AI",
  "pool_id": "ai",
  "reason": "custom_domain_rule",
  "matched": "chatgpt.com",
  "matched_rule": {
    "id": "builtin-v2-...",
    "source": "builtin-v2",
    "kind": "domain_suffix",
    "value": "chatgpt.com",
    "pool_id": "ai"
  },
  "binding": {
    "id": "pb_...",
    "pool": "ai",
    "generation": 3,
    "smart_node": "sn_...",
    "executor": "ne_...",
    "core": "xray",
    "worker": "w_xray"
  },
  "dns_path": [
    "smartgate-dns",
    "classify",
    "rule:builtin-v2-...",
    "pool:ai",
    "dns-inbound:in_...",
    "target:w_xray",
    "worker:w_xray",
    "smart-node:sn_..."
  ],
  "chain": ["normalize_target", "domain_rules", "custom_rules"]
}
```

For GEO matches, `matched_rule.kind` is `geosite` or `geoip`, `matched` is the
rule-set ID, and `reason` is `geo_domain_rule` or `geo_ip_rule`. `decision`
continues to describe the resulting target/Pool (`Direct`, `Block`, a business
role, or `Pool`), while `matched_rule.source` identifies GEO provenance.

Known decisions:

- `LAN_Bypass`
- `AI`
- `Banking`
- `Crypto`
- `Streaming`
- `Direct`
- `GeneralProxy`
- `Block`
- `Geo`

## Snapshots

Snapshot endpoints use the `{ok,data,error}` envelope. New snapshots capture
spec, runtime metadata, the complete durable `SelectionState` (including each
Pool Generation and binding metadata), and last-good nodes for rollback. Legacy
snapshots without structured Selection remain readable; rollback rebinds their
node-name selections monotonically from the current applied graph. Snapshots are
not tar backups; use Backups for portable archive files.

### `GET /api/snapshots`

Envelope data:

```json
{
  "snapshots": [
    {
      "id": "snap-1",
      "kind": "manual",
      "description": "before edit",
      "topology_hash": "...",
      "saved_at": "2026-06-25T00:00:00Z",
      "last_good_captured": true,
      "provider_count": 1,
      "business_pool_count": 2,
      "last_good_count": 1
    }
  ]
}
```

### `POST /api/snapshots`

Creates a rollback snapshot of the current state.

```json
{
  "id": "snap-1",
  "kind": "manual",
  "description": "before provider edit"
}
```

### `POST /api/snapshots/rollback/dryrun`

Builds and stores the latest rollback dry-run binding. The apply call must use the same `dryrun_id` and `snapshot_id`.

```json
{
  "dryrun_id": "rollback-1",
  "snapshot_id": "snap-1"
}
```

Envelope data includes the same top-level preview fields used by config dry-run, plus the detailed rollback plan:

```json
{
  "id": "rollback-1",
  "snapshot_id": "snap-1",
  "created_at": "2026-06-25T00:00:00Z",
  "spec_hash": "...",
  "topology_hash": "...",
  "config_epoch": 12,
  "gateway_mode": "safe",
  "rollback_plan": {}
}
```

### `POST /api/snapshots/rollback/apply`

Applies the latest matching rollback dry-run through the global commit lane. It restores spec and captured last-good nodes, increments `config_epoch`, and sets `last_rollback_at`.

```json
{
  "dryrun_id": "rollback-1",
  "snapshot_id": "snap-1"
}
```

Envelope data:

```json
{
  "status": "ok",
  "config_epoch": 13,
  "topology_hash": "...",
  "warnings": [],
  "rollback": {}
}
```

Possible status:

- `200 OK`: rollback applied
- `409 Conflict`: dry-run binding does not match
- `423 Locked`: rescue mode or commit gate blocked
- `428 Precondition Required`: run rollback dry-run first

## Backups

### `POST /api/backups/create`

Creates backup schema `3`. The archive contains `metadata.json`, `spec.json`,
`runtime.json`, `applied.json`, `selection.json`, `failover.json`, and bounded
`lastgood/*.json` entries. `selection.json` is required whenever the archived
AppliedState contains a production topology. Schema `1` and `2` archives remain
readable for restore compatibility but cannot preserve structured Pool
Generation metadata.

```json
{
  "op_id": "backup-create-001",
  "kind": "manual"
}
```

Returns envelope data backup record:

```json
{
  "id": "manual-1780000000000000000",
  "path": "/etc/smartgate/backups/manual-1780000000000000000.tar.gz",
  "size": 12345,
  "sha256": "...",
  "metadata": {},
  "created_at": "2026-06-25T00:00:00Z"
}
```

### `POST /api/backups/restore`

```json
{
  "op_id": "backup-restore-001",
  "path": "/etc/smartgate/backups/manual-1780000000000000000.tar.gz"
}
```

Restore is validated before write and then committed through global commit lane.

## Scheduler

Scheduler management endpoints use the `{ok,data,error}` envelope. The daemon automatically runs enabled tasks with a schedule in the background and stores runtime metadata in `last_run`, `last_status`, `last_output`, and `next_run`.
Scheduler task `spec` and legacy `last_output` values are sanitized again when
read. Sensitive or non-scalar `spec` values are returned as `[REDACTED]`, and
persisted output cannot expose URL paths, credentials, or tokens.

Task fields:

```json
{
  "id": "provider-auto-refresh",
  "name": "Provider auto refresh",
  "type": "provider_refresh",
  "enabled": true,
  "schedule": "@every 4h",
  "spec": {},
  "running": false,
  "last_run": "2026-06-25T00:00:00Z",
  "last_status": "ok",
  "last_output": "订阅刷新完成：共 1 个，成功 1 个，失败 0 个",
  "next_run": "2026-06-25T04:00:00Z"
}
```

Supported task types:

- `provider_refresh`: executable now.
- `delay_test`: executable when the daemon has Worker route control configured; writes `runtime.node_health`.
- `failover_watchdog`: executable when the daemon has Worker route control configured; evaluates persisted node health, applies needed selector changes, and writes failover events. Background runs skip pools with no fresh node health samples to avoid boot-time or stale-data false failover, and are locked when gateway mode is `safe` or `off`.
- `backup`: executable now when the backend is started with its backup transaction executor; creates a `scheduled-*.tar.gz` archive through the same transaction path as `POST /api/backups/create`.
- `apply_config`: executable now when the backend has runtime apply configured. It creates an internal dry-run, applies that candidate, runs the same resource guard as `POST /api/config/apply`, and does not overwrite the UI's latest dry-run file. Automatic runs are locked when gateway mode is `safe` or `off`.
- `restart_service`: executable now when the backend has a system runner; requires `spec.service` and runs that service restart through the same service command whitelist as `POST /api/system/service/{name}/restart`.
- `restart_singbox`: legacy read alias for `{"type":"restart_service","spec":{"service":"sing-box"}}`; persisted user specs using this value are normalized on read and the alias remains accepted for compatibility.
- `reboot_router`: executable now when the backend has a system runner. Manual run requires `{"confirm":true}` and executes the fixed `reboot` command. Background execution only runs when the task is explicitly enabled and due.
- `custom_shell`: persisted and visible now; run returns `501` until a safe predefined-script executor is explicitly designed.

Background execution uses the same executors as manual run. If an enabled scheduled task has no executor yet, it is recorded as `unsupported` and `next_run` advances according to its schedule. `last_output` is a backend-generated Simplified Chinese summary: backup success includes the backup ID and size; config apply success includes the new `config_epoch` and OpenWrt step count; service restart and router reboot name the completed action; and failover watchdog output includes decision, applied, failed, and event counts. Resource guard failures return `507`, Worker/Fabric execution failures return `502`, and service command failures return `502`.

Default tasks:

- `provider-auto-refresh`: enabled, `@every 4h`.
- `daily-backup`: enabled, `0 5 * * *`.
- `nightly-service-restart`: disabled, `0 4 * * *`, `spec.service="sing-box"`.
- `weekly-router-reboot`: disabled, `0 4 * * 0`.
- `node-delay-test`: enabled, `@every 1h`.
- `failover-watchdog`: enabled, `@every 1m`.

Run result payloads may include task-specific fields. Both manual-run endpoints
return an API-safe result DTO: provider refresh subscription metadata omits its
internal `raw` header value, and nested error messages are sanitized before
serialization.

```json
{
  "task": "apply_config",
  "started_at": "2026-06-25T00:00:00Z",
  "ended_at": "2026-06-25T00:00:03Z",
  "config": {
    "dryrun_id": "scheduler-apply-1780000000000000000",
    "status": "applied",
    "config_epoch": 2,
    "topology_hash": "...",
    "openwrt_applied": true,
    "openwrt_steps": 5,
    "warnings": []
  }
}
```

For `restart_service`, the task-specific field is `service`:

```json
{
  "task": "restart_service",
  "started_at": "2026-06-25T00:00:00Z",
  "ended_at": "2026-06-25T00:00:01Z",
  "service": {
    "service": "sing-box",
    "action": "restart",
    "status": "ok",
    "argv": ["/etc/init.d/sing-box", "restart"]
  }
}
```

For `reboot_router`, the task-specific field is `router`:

```json
{
  "task": "reboot_router",
  "started_at": "2026-06-25T00:00:00Z",
  "ended_at": "2026-06-25T00:00:01Z",
  "router": {
    "action": "reboot",
    "status": "ok",
    "argv": ["reboot"]
  }
}
```

For `failover_watchdog`, the task-specific field is `failover`:

```json
{
  "task": "failover_watchdog",
  "started_at": "2026-06-25T00:00:00Z",
  "ended_at": "2026-06-25T00:00:01Z",
  "failover": {
    "decisions": 3,
    "applied": 1,
    "events": 1
  }
}
```

### `GET /api/scheduler`

Returns:

```json
{
  "ok": true,
  "data": {
    "tasks": []
  }
}
```

### `POST /api/scheduler`

Creates a task. `id` is optional; `refresh_provider` is accepted as an alias and normalized to `provider_refresh`.

```json
{
  "id": "custom-refresh",
  "name": "Custom refresh",
  "type": "provider_refresh",
  "enabled": true,
  "schedule": "@every 2h",
  "spec": {}
}
```

### `PUT /api/scheduler/{id}`

Patches a task definition. The path `id` is authoritative; omitted fields keep their current values, so `{ "enabled": false }` is valid.

### `DELETE /api/scheduler/{id}`

Deletes a task and its runtime metadata.

### `POST /api/scheduler/{id}/run`

Runs a task by ID and records `last_run`, `last_status`, `last_output`, and `next_run`.

For `reboot_router`, the body must include:

```json
{
  "confirm": true
}
```

Possible status:

- `202 Accepted`: task ran.
- `400 Bad Request`: invalid body or `reboot_router` missing `confirm:true`.
- `409 Conflict`: same executable task already running.
- `423 Locked`: gateway off or rescue/commit blocked.
- `501 Not Implemented`: task type exists but executor is not wired yet.

### `POST /api/scheduler/run-now`

Legacy compatibility endpoint. Prefer `POST /api/scheduler/{id}/run` in new frontend code.

```json
{
  "task": "provider_refresh",
  "confirm": false
}
```

Possible status:

- `202 Accepted`: task ran
- `409 Conflict`: same task already running
- `423 Locked`: gateway off or rescue/commit blocked
- `501 Not Implemented`: task type exists but executor is not wired yet

## Frontend Notes

- `op_id` should be unique and stable per user action, for example `ui-${Date.now()}-${randomSuffix}`.
- Treat `409` and `423` as busy/locked UI states, not generic failure.
- Use `dryrun` before `apply` for provider and business pool editors.
- Auth is implemented and enabled by default in the daemon.
- CORS is available but disabled by default. Start with `-cors-origin http://localhost:5173` for a separately served dev frontend.
- Static frontend asset serving is available with `-static-dir /www/smartgate`; non-API paths fall back to `index.html`.
- `/api/events` provides SSE runtime status updates. WebSocket and per-connection streaming are not implemented yet.
- The frontend can still be served separately during development and call the router API via proxy.
- `/api/config/apply` activates validated sing-box/Xray Workers and atomically switches the owner-tagged Gateway Fabric (embedded DNS, nft sets, TPROXY dispatch, and policy routes). Keep the UI explicit: dry-run first, then apply.
- Scheduler `last_*` fields are runtime metadata and do not dirty the config hash. Editing task definitions does dirty config state.
