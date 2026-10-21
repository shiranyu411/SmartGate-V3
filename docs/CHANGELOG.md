# Changelog

## Unreleased

- Reuse unchanged local GEO snapshots, healthy unchanged Workers, captured rollback checkpoints and per-pass nftables reads during configuration apply.

- Prefer reported hostnames and clearly labelled recent DNS associations in
  connection targets; retain IP endpoints and shared-address ambiguity.
  Replace generation jargon with current/old line status and show scoped
  forwarding paths when a core does not report a connection chain.
- Replace the shared native select popup with themed, keyboard-accessible
  controls across the panel, retaining form change, validation and reset behavior.

## 3.0.1 — 2026-10-17

- Persist sing-box as the default core for nodes without an explicit choice,
  including new imports, refreshes and a startup migration of saved nodes.
  Preserve existing choices and independent Pool selection state.
- Keep isolated full-node probing active when another node cannot use it;
  unavailable core/route setups report unknown instead of upstream failure.

- Generate sing-box Worker routes with terminal reject actions and explicit
  direct node-hostname DNS, preserving Fabric ownership and selector behavior.
- Verify generated configurations and local runtime behavior against sing-box
  1.12.25 and 1.14.3; document the supported upgrade procedure and its limits.
- Preserve identities and credential references needed by the applied execution
  graph when subscription refresh removes or changes nodes, including identities
  no longer present in the desired SmartNode catalog.
- Carry the merged last-good snapshots and prepared topology from dry-run into
  apply, backup and recovery so a successful apply also survives restart.

## 3.0.0 — 2026-10-17

- Preserve V2 routing in native Fabric: Google/GFW domain protection takes
  precedence over GEO Direct while business and user rules retain priority;
  global mode excludes saved custom/GEO Direct rules. Existing V2 pools migrate
  to strict cutovers so old connections close immediately. Latency measurements
  match V2 availability semantics and retain TLS certificate verification.
- Add production-refusing OpenWrt fault fixtures for full-node deadlines,
  alternate probes, all-failed candidate sets and existing TLS connection cleanup.
  Rehearsal finalization now explicitly leaves production status unchanged.


- Port V2 network diagnostics and repair. Recovery replays captured Applied
  state and durable selections, keeps dirty Spec, and attempts rollback on
  failure. Install a persistent fw4 bridge and the owned DNS override in
  dnsmasq's actual loaded instance directories; verify them during supervision
  and remove the owned runtime overrides in off/rescue mode.


- Port V2 full-node measurements and failover confirmation: the UI and scheduled
  task share 24 probes with 50 ms launch pacing and a 28-second round deadline.
  Private batch cores cover nodes outside Pools without changing live routes;
  unfinished probes remain unknown. Watchdog evidence retains its own revision
  and core context, and an all-failed round keeps the current selection.


First V3 release. Replaces the V2 single-core sing-box/TUN data plane with a
core-independent Gateway Fabric. Upgrades from a healthy V2 installation
(`AppliedState.schema_version` 1) are supported through `scripts/install.sh`;
see `docs/RELEASE_RUNBOOK.md` and `docs/CUTOVER.md`.

### Added
- Node-page "test all" with bounded concurrency, progress, and cancellation; Pool editing groups nodes by Provider and supports filtering selected members and removing filtered members.
- Complete rule catalog: built-in/GEO provenance, arbitrary Pool targets, user overrides, conflict resolution, Route Explain provenance, and apply-time conflict gates.
- Gateway Fabric routing with per-core sing-box/Xray Workers, durable Binding/Generation selection, richer Connections control, and crash-safe apply/rollback state.
- Core descriptor registry: Worker capabilities, planner inputs, runtime surfaces, rescue cleanup, and frontend core choices are derived from registered descriptors instead of sing-box-specific code paths. A FakeCore third-core acceptance test and an architecture guard (`make archguard`) enforce the boundary in CI.
- Data-plane supervisor replaces the sing-box restart watcher; supervision is serialized per Worker and verified in parallel tests.
- Transactional blank-target bootstrap with release hash/Worker prerequisite checks, boot recovery journal, health/auth-setup gates, and local OpenWrt QEMU tooling.
- Provider `user_agent`: per-subscription `User-Agent` override with UI presets (Clash Verge, sing-box, mihomo, Shadowrocket) and printable-ASCII validation. Generalizes the legacy host-specific UA fix without hard-coding subscription hosts; existing providers keep the default UA until one is set.
- `POST /api/runtime/recover` explicitly runs `RecoveryOrchestrator.Recover` to clear leftover transaction journals; `423` / `ErrRecoveryRequired` messages point at it.
- Release gate (`make release-gate`) and target-agnostic cutover scripts (`scripts/cutover/`) that drive the same upgrade/verify/rollback procedure against the QEMU VM and the production router.

### Security
- All `/api/*` routes accept only `Authorization: Bearer`; `?token=` is honored solely on `/api/events` for `EventSource`.
- API responses and error envelopes pass through a redaction layer; provider URLs, node credentials, Wi-Fi keys, and raw configs are never echoed. Provider and Wi-Fi payloads expose `*_configured` / `*_masked` fields instead of source values.
- State directory is enforced to `0700` and durable state files to `0600` on every write, including files inherited from older installs.
- Login back-off (5 consecutive failures → 30 s `429`) and a mutex around the auth store to prevent concurrent session loss.

### Fixed
- Replay durable selector choices after a service restart without applying dirty
  desired configuration; keep Worker status reads free of state mutation under
  concurrent observation, and report the live rescue state in diagnostics.
- Validate Pool transitions and compensation against the exact pinned GEO
  snapshots, including recovery when the original source rule file is removed.
- Connection listings use the Fabric ledger for Workers without per-connection observation support instead of failing the whole dual-core endpoint; real observation failures remain visible.
- Pool transition validation copies dispatch entries before comparison, so a rejected preview cannot corrupt the live Fabric graph and trigger false route-loss rescue.
- Node catalogs retain configured Provider and subscription order instead of sorting SmartNode hashes.
- Manual node probes no longer require Pool membership: a temporary, serialized core process can test the desired identity without installing a Worker or modifying Pool membership. HTTPS is the default test target, probe setup failures preserve prior health, and stale core/revision results are excluded from current health.
- Removing the selected Pool member, including one selected after the last config apply, resolves a replacement in the next dry-run without changing the live selection before Apply. Failed previews now show their blocking node/Pool details and actual core check errors.
- Xray node health checks use a temporary, executor-pinned loopback SOCKS route instead of reporting unsupported; probes preserve live Pool selections and clean up their inbound and routing rule on cancellation.
- DNS address classification uses targeted netlink set updates on Linux instead of spawning `nft` twice per answer while holding a global lock. Dynamic address sets use per-element timeouts without interval encoding; DNS work has a deadline and request-local DoH transports release idle sockets.
- Empty node aliases, pool references, capability reasons, and connection chains serialize as arrays. The frontend also tolerates older V3 null arrays, including kernel-only flows arriving during polling, and contains rendering errors within the current page.
- Private pre-cutover backups now preserve both V2 and V3 applied schemas, so a V3 hotfix can use the same guarded upgrade and recovery procedure.
- Bare IP rules normalize consistently to IPv4 `/32` or IPv6 `/128` across backend and frontend, including IPv4-mapped IPv6 input.
- Fresh-install recovery removes atomic `.new`, stale staging, and `S??`/`K??smartgate` residue while rejecting unjournaled partial installations.
- Selector switch: multi-pass sticky rebind (immediate + residual sweeps for old-node/TCP/QUIC keep-alives) instead of a single best-effort close.
- Routing/DNS: when `geosite-cn` Direct is enabled, Google/GFW-sensitive domains use Proxy DNS with real-address Pool sets before geo-CN, so polluted `114.x` Direct paths stop producing browser “Your connection is not private” errors after rebinds.
- First config apply on a blank device no longer leaves an unrecoverable `recovery_required` journal after a data-plane failure; without a `SpecCaptured` baseline the transaction cleans up best-effort and always removes the journal so the daemon can start again.
- Failover CAS failure no longer pollutes the pool `Current` early.
- Direct DNS resolver falls back across the nameserver list.
- `EffectiveRemoteRuleSettings` preserves an explicit `Enabled:false` sentinel so a user’s disabled state survives a read-back.
- Adblock activation is tri-state (`remote_rules.enabled && ad_block.enabled && set.enabled`); `ad_block.sets` no longer overrides per-set switches.
- Graceful drain: cleanup failures are tracked with `retrying`/`failed` status, retry count, and last error instead of being silently treated as complete.
- Selector transaction last-good normalization and compatibility-reference binding fixes surfaced by the core-independence audit.
- Node/proxy-group queries refresh after a successful apply; rescue-mode UI wording finalized.
- Frontend: mobile modal forms collapse to a single column; Modal/drawer lock body scroll; long errors and paths wrap. Remaining raw backend enum values (drain, health, apply, route decision/reason, service action, rule format, rescue reason, dry-run category, gateway mode) are localized, and core names render through the registry.
- `deploy-safe` `AUTH_HEADER` example uses Bearer; `deploy-recover` skips version check when commit is absent; `oss-export` `sed -i` is cross-platform.
- Upgrade from a V2 `on`-mode install no longer crash-loops: pre-SmartNode pool bindings and selector state (`provider:fingerprint` ids) are migrated to SmartNodes on first start, with unresolvable references unbound and reported (`legacy_migration` in `/api/runtime/status`, durable `legacy-migration.json`); a failed startup recovery now degrades to rescue `startup_recovery_failed` with the API up instead of exiting; `applied-state.json` schema 1 is promoted to 2.
- Gateway Fabric host state (owner nft table, policy routes, hook, dnsmasq override) is verified by the data-plane supervisor on every poll and reasserted when a `firewall stop`/`nft flush` removed it; persistent drift enters rescue. Policy-route verification accepts iproute2 output without a full mask.
- `deploy-safe` legacy quiesce now retires the V2-owned core config and its owner marker (the V3 rescue plan has no action for them) and disables the stock core service so it cannot resurrect the V2 config at boot; `deploy-safe` / `deploy-recover` rollback re-enable it from the backup's `legacy-enabled`. Without this every upgrade from a V2 that had applied in `on` mode failed after the backup point and auto-rolled back (found in the on-mode QEMU rehearsal).
- `deploy-safe` / `deploy-recover` probe the legacy sing-box service with procd `running` instead of `status`; a service that is enabled at boot with no instance ("active with no instances") no longer makes a V2 `off`/`safe` baseline look unhealthy and block the upgrade (found in the QEMU rehearsal after a reboot).
- Worker control (`smartgate-worker-control`) no longer aborts on OpenWrt's busybox `sleep`, which rejects fractional seconds: under `set -eu` the first `sleep 0.1` in a wait loop exited the script, so every stop or restart of a running Worker failed — the controlled apply of an `on`-mode upgrade was rejected with 502 and rollback could not stop the Worker. Waits now fall back to whole seconds and are bounded by wall-clock deadlines; the Gateway Fabric reconcile and fw4 hook lock loops got the same fix.
- The data-plane supervisor's post-restart selection reconcile no longer probes the upstream node while holding the control-plane locks (it read the selector back instead); with an unreachable node the management API used to stall for the probe timeout on every poll.
- A Pool that fell to its `direct`/`block` fallback now recovers on its own: node health probes start the Pool's stopped Worker on demand (through the commit lane) instead of failing forever against its closed control port, so the failover watchdog can see a candidate healthy again and switch back (`healthy_candidate_recovered`).
- `deploy-safe` rollback and `deploy-recover` retire the failed candidate's data plane from the live inventory (staged rescue plan, then Workers by pid file / active config / process cmdline, owner-checked `table inet smartgate`, proto-186 policy routes) before deleting anything; a leaked Worker used to keep the stock core from starting and left V2 half-restored. `deploy-safe` records each API call's HTTP status and snapshots daemon/listener/procd/log state into the work directory before rolling back, and the daemon logs rejected dry-run/apply requests, so a failed gate is diagnosable afterwards.
- Clash connection cleanup skips entries without an id; sing-box dry-run cleans candidate directories after responding; the restart watcher no longer spams rescue/recovery gate logs.
- Deployment recovery branches on `AppliedState.schema_version`: V2→V3 upgrades isolate the old sing-box/TUN/DNS data plane after the durable journal; V3 rollback verifies embedded DNS, Gateway Fabric, hooks, dnsmasq, and Applied-Binding Workers without restarting the old monolithic sing-box.
- Conflicting-rule dry-run summaries are de-duplicated while keeping the independent `422 rule_conflict_unresolved` re-validation on apply; deployments that fail before the durable journal clean up staging and the scheduler gate.
- Local OpenWrt VM deploy helper passes existing auth through a `0600` token file, never on the command line or in the repository.
- Execution cores are registered only when their binary is installed; the daemon no longer refuses to start on a host without `xray-core` (supports installations with a single runtime), rescue still knows how to stop every Worker this build can start, and `smartgate-worker-control verify-prerequisites` accepts any single installed core.
- `deploy-safe` fails the upgrade gate (and rolls back to V2) when the legacy binding migration leaves unresolved V2 node references, instead of letting the affected pool come up on its `direct`/`block` fallback; `30-verify.sh` reports the same condition. Preflight's SSH check names the actual cause (missing key vs unknown host key) instead of a generic unreachable error.
- `make cutover` mints the admin Bearer token itself when `TOKEN_FILE` does not exist yet (`cutover-token`: password typed on the terminal, sent on curl's stdin) and refuses to start unless key-based SSH to the target works non-interactively, so a production cutover is one command plus one typed confirmation.

### Publishing
- Source export uses committed Git trees instead of copying the working directory,
  rejects dirty tracked files, and excludes ignored/untracked private material.
  Export checks cover local-file exclusion, monorepo boundaries and credential
  pattern rejection. The runtime binary is unchanged by this publishing fix.

### Docs
- V3 API contract, architecture, built-in rules, complete-catalog authoring, dual-path install/upgrade, release readiness, V2 parity ledger, and cutover runbook.

### Upgrade notes
- Providers migrated from V2 have no `user_agent`; subscription services that require a client UA (the V2 special case) must have it set in the Provider editor after the upgrade, then refreshed.
- V3 requires `kmod-nft-tproxy`, `kmod-nft-socket`, `ip-full` and `conntrack` (TPROXY dispatch, policy routing, connection tracking) in addition to `sing-box` ≥ 1.12. `xray-core` is optional: execution cores are discovered on the host, a SmartNode can only select a core whose binary is installed, and a missing core is logged at start instead of stopping the daemon. `scripts/cutover/00-preflight.sh` checks the required packages and reports the optional ones.
