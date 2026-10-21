#!/usr/bin/env bash
# Record a completed cutover: tag the repository, copy the artifact into
# ops/releases, and print the STATUS.md / PRODUCTION.md edits that a human
# must make. Runs only after 30-verify.sh passed.
#
#   TARGET=router scripts/cutover/90-finalize.sh [--tarball <path>]
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

TARBALL="$REPO_ROOT/dist/smartgate-openwrt-aarch64.tar.gz"
while [ $# -gt 0 ]; do
  case "$1" in
    --tarball) TARBALL=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

load_target
need ssh; need shasum; need git
require_target_reachable

installed="$(remote_version_json)"
commit="$(json_field "$installed" commit)"
version="$(json_field "$installed" version)"
built="$(json_field "$installed" built_at)"
[ -n "$commit" ] || die "target has no version.json; nothing to finalize"

last_verify="$(grep -E "	$TARGET	30-verify.sh	v3@$commit	pass" "$OPS_DIR/deployments.log" | tail -n1 || true)"
[ -n "$last_verify" ] || die "no passing 30-verify.sh entry for v3@$commit on $TARGET in ops/deployments.log; verify before finalizing"

today="$(date -u +%Y-%m-%d)"
if [ -f "$TARBALL" ]; then
  sha="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
  dest="$OPS_DIR/releases/v3-$commit-$(date -u +%Y%m%d).tar.gz"
  if [ ! -f "$dest" ]; then
    cp "$TARBALL" "$dest"
    (cd "$OPS_DIR/releases" && printf '%s  %s\n' "$sha" "$(basename "$dest")" >> SHA256SUMS)
    info "artifact archived: $dest"
  fi
else
  sha="(tarball not present locally)"
  dest="(not archived)"
fi

if [ "$TARGET_KIND" = production ]; then
  tag="prod/$today"
  tagged_commit="$(git -C "$REPO_ROOT" rev-parse --short "$tag^{commit}" 2>/dev/null || true)"
  if [ -n "$tagged_commit" ] && [ "$tagged_commit" != "$commit" ]; then
    tag="$tag-$commit"
  fi
  if git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
    [ "$(git -C "$REPO_ROOT" rev-parse --short "$tag^{commit}")" = "$commit" ] || die "tag $tag points at a different commit"
    info "tag $tag already exists"
  else
    head_short="$(repo_short_sha)"
    [ "$head_short" = "$commit" ] || die "repository HEAD $head_short != installed $commit; check out the shipped commit before tagging"
    git -C "$REPO_ROOT" tag -a "$tag" -m "Production deployment $today: $version@$commit built $built sha256 $sha"
    info "tagged $tag"
  fi
  log_deploy 90-finalize.sh "v3@$commit" ok "tag=$tag artifact=$dest"
else
  log_deploy 90-finalize.sh "v3@$commit" ok "rehearsal; no tag"
fi

if [ "$TARGET_KIND" = rehearsal ]; then
  info "rehearsal finalized for $TARGET ($version@$commit); production STATUS.md, router.md and V2 retirement state are unchanged"
  exit 0
fi

cat <<EOF

FINALIZE: done for $TARGET ($version@$commit)

Human edits still required (agents: do these now, then commit nothing in v3 for them):
  1. $OPS_DIR/../STATUS.md
       - 当前阶段 → P4 观察 72h（production）
       - 实机 表格 → 版本线 V3 / 仓库 v3/ / commit $commit / built_at $built / 产物 $(basename "$dest") / sha256 $sha / 部署日期 $today
       - 变更记录 加一行
  2. $OPS_DIR/router.md  — 用 ops/evidence 里最新 preflight facts 更新型号/版本/包
  3. v2/PRODUCTION.md    — 状态改为 "回滚参考（V3 已上机 ${today}）"
  4. Post-upgrade step from CHANGELOG "Upgrade notes": set user_agent on providers that need it, refresh, confirm nodes load.
  5. Schedule 30-verify.sh --slug soak-24h and --slug soak-72h.
EOF
