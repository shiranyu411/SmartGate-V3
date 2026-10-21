#!/bin/sh
# Mechanical release gate. Writes dist/release/GATE_PASSED only when every
# check below succeeds. Invoked as `make release-gate`.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

fail() { echo "release-gate: FAIL: $*" >&2; exit 1; }
info() { echo "release-gate: $*"; }

VERSION=$(tr -d ' \n' < VERSION)
COMMIT=$(git rev-parse --short HEAD)
BRANCH=$(git rev-parse --abbrev-ref HEAD)
OPS_DIR=${SMARTGATE_OPS_DIR:-$(CDPATH= cd -- "$repo_root/.." && pwd)/ops}

info "version=$VERSION commit=$COMMIT branch=$BRANCH"

# A failed gate must never leave a stale GO behind.
rm -f dist/release/GATE_PASSED

# 1. Clean tree on main.
[ "$BRANCH" = main ] || fail "branch is $BRANCH, want main"
[ -z "$(git status --porcelain)" ] || fail "working tree is not clean"
git merge-base --is-ancestor HEAD HEAD >/dev/null

# 2. Version / changelog / readiness / parity.
[ "$VERSION" != "0.1.2" ] || fail "VERSION is still 0.1.2"
grep -q "^## ${VERSION}" docs/CHANGELOG.md || fail "docs/CHANGELOG.md has no '## ${VERSION}' section"

software_blocking=$(awk '/^## Software blocking/{p=1;next} /^## /{p=0} p' docs/RELEASE_READINESS.md)
if printf '%s\n' "$software_blocking" | grep -E '\| B[0-9]+ \|' | grep -E '\[ \]|\[~\]' >/dev/null; then
	printf '%s\n' "$software_blocking" | grep -E '\| B[0-9]+ \|' | grep -E '\[ \]|\[~\]' >&2 || true
	fail "docs/RELEASE_READINESS.md Software blocking still has open [ ] or [~] rows"
fi
if grep -E '\|[[:space:]]*pending[[:space:]]*\|' docs/V2_PARITY.md >/dev/null; then
	fail "docs/V2_PARITY.md still has a pending row"
fi

# 3. Static checks.
info "go vet"
mkdir -p .tmp .gocache
TMPDIR=$repo_root/.tmp GOTMPDIR=$repo_root/.tmp GOCACHE=$repo_root/.gocache go vet ./...

info "make test"
make test

info "make archguard"
make archguard

info "make fakecore-e2e"
make fakecore-e2e

info "frontend-check"
make frontend-check

info "secret-scan"
./scripts/secret-scan.sh .

# 4. Build the artifact and pin identity.
info "make release"
make release

artifact=dist/smartgate-openwrt-aarch64.tar.gz
[ -f "$artifact" ] || fail "release artifact missing"
built_commit=$(sed -n 's/.*"commit": "\([^"]*\)".*/\1/p' dist/release/version.json | head -n1)
[ "$built_commit" = "$COMMIT" ] || fail "version.json.commit=$built_commit != HEAD $COMMIT"

sha=$(shasum -a 256 "$artifact" | awk '{print $1}')
built_at=$(sed -n 's/.*"built_at": "\([^"]*\)".*/\1/p' dist/release/version.json | head -n1)

# 5. Record the gate and archive the tarball next to the V2 production artifact.
{
	echo "version=$VERSION"
	echo "commit=$COMMIT"
	echo "built_at=$built_at"
	echo "sha256=$sha"
	echo "artifact=$artifact"
	echo "passed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > dist/release/GATE_PASSED

if [ -d "$OPS_DIR/releases" ]; then
	dest="$OPS_DIR/releases/v3-$COMMIT-$(date -u +%Y%m%d).tar.gz"
	cp "$artifact" "$dest"
	( cd "$OPS_DIR/releases" && printf '%s  %s\n' "$sha" "$(basename "$dest")" >> SHA256SUMS )
	info "archived $dest"
fi

echo
echo "release-gate: GO"
echo "  $VERSION@$COMMIT  sha256=$sha"
echo "  GATE_PASSED=dist/release/GATE_PASSED"
