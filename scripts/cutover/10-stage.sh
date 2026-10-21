#!/usr/bin/env bash
# Stage a gated release artifact on the target (no installation yet).
#
#   TARGET=router scripts/cutover/10-stage.sh [--tarball <path>] [--allow-ungated]
#
# Default tarball: dist/smartgate-openwrt-aarch64.tar.gz. For production the
# artifact must carry dist/release/GATE_PASSED written by `make release-gate`
# for the same sha256 and the current HEAD. --allow-ungated is accepted only
# for rehearsal targets. The remote copy is verified with sha256sum -c.
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

TARBALL="$REPO_ROOT/dist/smartgate-openwrt-aarch64.tar.gz"
GATE_FILE="$REPO_ROOT/dist/release/GATE_PASSED"
ALLOW_UNGATED=0
EMERGENCY_HOTFIX=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tarball) TARBALL=$2; shift 2 ;;
    --allow-ungated) ALLOW_UNGATED=1; shift ;;
    --emergency-hotfix) EMERGENCY_HOTFIX=1; shift ;;
    *) die "unknown argument: $1" ;;
  esac
done

load_target
need ssh; need scp; need shasum
[ -f "$TARBALL" ] || die "release artifact missing: $TARBALL (run make release-gate)"
local_sha="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
gate_status=passed

if [ "$EMERGENCY_HOTFIX" -eq 1 ]; then
  [ -n "${HOTFIX_COMMIT:-}" ] && [ -n "${HOTFIX_SHA256:-}" ] || die "emergency hotfix requires an explicitly approved HOTFIX_COMMIT and HOTFIX_SHA256"
  [ "$local_sha" = "$HOTFIX_SHA256" ] || die "emergency hotfix artifact does not match its approved digest"
  artifact_version="$(tar -xOf "$TARBALL" version.json)"
  artifact_commit="$(json_field "$artifact_version" commit)"
  [ "$artifact_commit" = "$HOTFIX_COMMIT" ] && [ "$artifact_commit" = "$(repo_short_sha)" ] || die "emergency hotfix commit does not match the artifact and repository HEAD"
  [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] || die "emergency hotfix requires a clean source tree"
  gate_status=emergency-not-run
  info "EMERGENCY HOTFIX: commit=$artifact_commit sha256=$local_sha; release gate NOT run"
elif [ -f "$GATE_FILE" ]; then
  gate_sha="$(sed -n 's/^sha256=//p' "$GATE_FILE")"
  gate_commit="$(sed -n 's/^commit=//p' "$GATE_FILE")"
  gate_version="$(sed -n 's/^version=//p' "$GATE_FILE")"
  if [ "$gate_sha" = "$local_sha" ]; then
    info "artifact is gated: version=$gate_version commit=$gate_commit"
    if [ "$TARGET_KIND" = production ] && [ "$gate_commit" != "$(repo_short_sha)" ]; then
      die "GATE_PASSED commit $gate_commit != repository HEAD $(repo_short_sha); re-run make release-gate on the commit you intend to ship"
    fi
  else
    [ "$ALLOW_UNGATED" -eq 1 ] && [ "$TARGET_KIND" = rehearsal ] || die "artifact sha256 does not match GATE_PASSED; re-run make release-gate"
    info "WARNING: artifact does not match GATE_PASSED; continuing because --allow-ungated on a rehearsal target"
  fi
else
  [ "$ALLOW_UNGATED" -eq 1 ] && [ "$TARGET_KIND" = rehearsal ] || die "no GATE_PASSED for this artifact; run make release-gate (or --allow-ungated on a rehearsal target)"
  info "WARNING: staging an ungated artifact on rehearsal target"
fi

require_target_reachable
require_private_backup
info "uploading $(basename "$TARBALL") ($(du -h "$TARBALL" | cut -f1)) to $SSH_HOST:/tmp/smartgate.tar.gz"
scp_to_target "$TARBALL" /tmp/smartgate.tar.gz >/dev/null
remote_sh "
remote_sha=\$(sha256sum /tmp/smartgate.tar.gz | cut -d' ' -f1)
[ \"\$remote_sha\" = '$local_sha' ] || { echo 'stage: uploaded artifact hash mismatch' >&2; exit 1; }
rm -rf '$REMOTE_STAGE_DIR'
mkdir -p '$REMOTE_STAGE_DIR'
tar -xzf /tmp/smartgate.tar.gz -C '$REMOTE_STAGE_DIR'
cd '$REMOTE_STAGE_DIR'
sha256sum -c SHA256SUMS >/dev/null
echo 'stage: manifest verified'
cat version.json
"
staged_version="$(ssh_target "cat $REMOTE_STAGE_DIR/version.json")"
staged_commit="$(json_field "$staged_version" commit)"
staged_ver="$(json_field "$staged_version" version)"
log_deploy 10-stage.sh "v3@$staged_commit" ok "version=$staged_ver sha256=$local_sha gate=$gate_status stage=$REMOTE_STAGE_DIR"
info "staged $staged_ver@$staged_commit at $REMOTE_STAGE_DIR"
