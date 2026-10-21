# SmartGate V3 Rule Authoring Guide

This guide is for agents and operators who create hand-written SmartGate
routing rules. `docs/API_CONTRACT.md` is the normative API authority;
`docs/BUILTIN_RULES.md` describes the shipped defaults.

Desired rules live in Spec. Saving a catalog does not change traffic until a
successful config dry-run and apply. Never hand-edit generated Worker config as
a substitute for SmartGate state.

## Two rule systems

| System | API | Use |
| --- | --- | --- |
| Complete local catalog | `GET/PUT /api/rules` | Small, reviewable domain suffix, keyword, and IP/CIDR rules assigned to business pools, `direct`, or `block` |
| sing-box rule-sets | `GET/PUT /api/rule-sets` | Large GEO/ad lists supplied as local or remote `.srs` data |

Use local user rules by default. Use binary `.srs` files for large catalogs;
do not paste tens of thousands of geosite entries into `/api/rules`.

## Complete catalog API

`GET /api/rules` returns:

```json
{
  "rules": [
    {
      "id": "...",
      "source": "builtin-v2",
      "kind": "domain_suffix",
      "value": "home.arpa",
      "pool_id": "direct",
      "enabled": true
    }
  ],
  "conflicts": []
}
```

`PUT /api/rules` replaces the complete desired catalog atomically. A valid
client must start from the latest `GET`, preserve every built-in/GEO record,
apply its edits, and send all records back in one body:

```json
{
  "rules": [
    "every system rule returned by GET, plus the desired user rules"
  ]
}
```

The string above is explanatory, not a literal payload. A practical merge
workflow is:

```sh
AUTH="Authorization: Bearer $TOKEN"

curl -sS -H "$AUTH" "$API/api/rules" > current-rules.json

# Add/edit/remove only source=user records, or toggle enabled on a system row.
# Produce a real payload shaped as {"rules":[...]}.
jq '{rules:.data.rules}' current-rules.json > rules-put.json

curl -sS -X PUT -H "$AUTH" -H 'Content-Type: application/json' \
  --data-binary @rules-put.json "$API/api/rules"
```

Do not call the removed V2 endpoint `/api/rules/{group}`. Group maps such as
`{"ai":{"domain_suffix":[...]}}` are legacy import shapes, not the V3 HTTP
contract.

## Rule schema

```ts
type RouteRule = {
  id: string;
  source: "builtin-v2" | "geo-default" | "user";
  kind: "domain_suffix" | "domain_keyword" | "ip_cidr" | "geosite" | "geoip";
  value: string;
  pool_id: string;
  enabled: boolean;
  overridden_by?: string;
};
```

User-authored local rules use only `domain_suffix`, `domain_keyword`, or
`ip_cidr` and must set `source:"user"`. `pool_id` is either a configured
business pool ID or the reserved target `direct`/`block`.

System rows (`builtin-v2` and `geo-default`) cannot be deleted, renamed, moved,
or have their matcher changed. Clients may only toggle their `enabled` state.
`overridden_by` is response metadata and should not be authored.

IDs must be unique and stable across edits. Use an operator-scoped prefix, for
example `user-ai-openai` or `user-direct-printer-v4`, rather than regenerating
IDs on every save.

## Normalization and matching

- Domain values are trimmed, lower-cased, and stripped of a leading dot.
- `domain_suffix` matches the apex and subdomains.
- `domain_keyword` is a substring match; use only narrow product terms.
- `ip_cidr` accepts a CIDR or a single IPv4/IPv6 address.
- Single IPs normalize to `/32` or `/128`; IPv4-mapped IPv6 normalizes to the
  equivalent IPv4 host prefix.
- Empty values, duplicate IDs, invalid prefixes, unknown pools, and modified
  system ownership are rejected.

Prefer suffix rules such as `openai.com`. Avoid broad keywords such as
`google`, `cloud`, `api`, `cdn`, `ai`, or an entire TLD.

## Priority and conflict behavior

Enabled user rules have higher routing priority than built-in and GEO defaults.
When a user matcher supersedes a system default, the system row remains visible
with `overridden_by` so Route Explain can show provenance.

Two enabled user rules with the same normalized matcher assigned to different
pools create an unresolved conflict. `PUT /api/rules` accepts this draft so it
can be reviewed, but traffic cannot be changed while it exists:

- config dry-run returns a blocking rule-conflict change;
- config apply independently returns `422 rule_conflict_unresolved`;
- no implicit winner is selected.

Resolve conflicts in the UI or call:

```json
POST /api/rules/resolve-conflict
{
  "rule_key": "domain_suffix\u0000youtube.com",
  "keep_pool": "streaming",
  "op_id": "rule-resolve-001"
}
```

## Authoring policy

| Target | Guidance |
| --- | --- |
| AI pool | Product apex/API/CDN domains that need stable foreign egress; keep Gemini hosts narrow rather than routing all Google |
| Streaming pool | Region-sensitive media services such as Netflix or Disney+ |
| Banking pool | Official banking, identity, tax, brokerage, and payment domains; omit uncertain domains |
| Crypto pool | Official exchange, wallet, custody, and on-ramp domains; omit uncertain domains |
| General pool | Explicit sites that must follow the general selector rather than a specialty pool |
| `direct` | LAN names and explicit operator-approved direct sites; do not recreate all CN GEO data by hand |
| `block` | Small, unambiguous ad/tracker hosts; never shared application CDNs |

Do not put `0.0.0.0/0`, `::/0`, or private ranges into `block`. Avoid forcing
all Google traffic direct; when CN GEO direct is enabled, SmartGate inserts
proxy-safety exceptions before `geosite-cn`.

## Remote `.srs` rule-sets

Use `/api/rule-sets` only when a list is too large for a hand-reviewed local
catalog. Preferred production workflow:

1. Author sing-box source rule-set JSON off-router.
2. Compile it with `sing-box rule-set compile`.
3. Install the binary `.srs` under `/etc/sing-box/rule-set/`.
4. Register an absolute local path, binary format, and explicit target.
5. Dry-run and inspect memory-guard warnings before apply.

Defaults are `max_enabled_sets=12`, `max_set_bytes=4 MiB`, and
`max_total_bytes=8 MiB`, with lower warning thresholds. Remote ad blocking is
disabled by default. Do not enable a large remote ads set without explicit
operator acceptance of memory and breakage risk.

## Verification workflow

Before saving:

- verify each user ID is unique and each pool exists;
- remove duplicate matchers and resolve cross-pool ownership;
- keep domains lower-case with no scheme, path, or port;
- keep hand lists small (prefer under 1,000 total entries);
- preserve all system rows from the latest `GET /api/rules`.

Then:

```sh
# Save complete catalog first.
curl -sS -X PUT -H "$AUTH" -H 'Content-Type: application/json' \
  --data-binary @rules-put.json "$API/api/rules"

# Preview representative targets before changing traffic.
curl -sS -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"target":"api.openai.com"}' "$API/api/routes/explain"

curl -sS -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"op_id":"rules-dryrun-001"}' "$API/api/config/dryrun"

# Use the returned dry-run ID.
curl -sS -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"dryrun_id":"rules-dryrun-001"}' "$API/api/config/apply"
```

Suggested Route Explain cases are an AI domain, a streaming domain, a LAN
suffix, a user IP/CIDR, and an unmatched public domain that should fall back to
the general pool.

## Hard prohibitions

- Do not expose the management API to WAN or `0.0.0.0`.
- Do not include credentials, subscription URLs, or tokens in a rule artifact.
- Do not hot-overwrite the daemon or Worker binaries to apply rules.
- Do not hand-edit generated nftables, sing-box, or Xray state.
- Do not close all connections as a substitute for a correct apply/cutover.
