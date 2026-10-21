# sing-box compatibility

The Worker compiler uses a terminal reject route action and explicit direct
DNS for node hostnames. Gateway Fabric owns DNS interception, routing and
business classification. The Worker bootstrap resolver defaults to the public
resolver `223.5.5.5:53` and prefers IPv4.

The compatibility tests target sing-box 1.12.25 and 1.14.3. Run them against
each candidate binary before upgrading:

```sh
SMARTGATE_SINGBOX_BIN=/path/to/sing-box go test ./internal/worker/singbox -count=1
```

These checks exercise generated protocol configurations, hostname bootstrap,
SOCKS traffic, Clash control, rejection and restart selection. Tests requiring
a real binary are skipped when no binary is configured. Linux TPROXY checks
are available in `scripts/m2-worker-smoke.sh`.

Use a package compatible with the target OpenWrt ABI and architecture. Preserve
the previous package and an off-device private backup. Quiesce services using
the core during replacement, validate every consumer, then verify actual DNS,
traffic and control behavior. A minimum version check or successful config
validation alone does not certify all future versions or device performance.

See the upstream [migration guide](https://sing-box.sagernet.org/migration/)
and [dial fields](https://sing-box.sagernet.org/configuration/shared/dial/).
