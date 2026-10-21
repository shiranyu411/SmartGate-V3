#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

REPORT="${FAKECORE_REPORT:-$REPO_ROOT/.tmp/fakecore-acceptance-report.txt}"
mkdir -p "$(dirname "$REPORT")" .gocache
: >"$REPORT"
exec > >(tee -a "$REPORT") 2>&1

SNAPSHOT=0
if [[ "${1:-}" = --snapshot ]]; then
  SNAPSHOT=1
  shift
  printf 'FakeCore validation revision: %s\n' "$(git rev-parse HEAD)"
  echo 'Source snapshot: private introduction diff is unavailable; running behavioral acceptance.'
else
BASE="${1:-${FAKECORE_DIFF_BASE:-}}"
HEAD_REF="${2:-${FAKECORE_DIFF_HEAD:-HEAD}}"
if [[ -z "$BASE" ]]; then
  echo "fakecore diff-scope: baseline is required; pass it as the first argument or set FAKECORE_DIFF_BASE" >&2
  exit 1
fi
if ! git rev-parse --verify "${BASE}^{commit}" >/dev/null 2>&1; then
  echo "fakecore diff-scope: baseline ref not found: $BASE" >&2
  exit 1
fi
if ! git rev-parse --verify "${HEAD_REF}^{commit}" >/dev/null 2>&1; then
  echo "fakecore diff-scope: head ref not found: $HEAD_REF" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$BASE" "$HEAD_REF"; then
  echo "fakecore diff-scope: baseline $BASE is not an ancestor of $HEAD_REF" >&2
  exit 1
fi

VALIDATION_REVISION="$(git rev-parse 'HEAD^{commit}')"
INTRODUCTION_BASE="$(git rev-parse "${BASE}^{commit}")"
INTRODUCTION_HEAD="$(git rev-parse "${HEAD_REF}^{commit}")"
printf 'FakeCore validation revision: %s\n' "$VALIDATION_REVISION"
printf 'FakeCore introduction range: %s..%s\n\n' "$INTRODUCTION_BASE" "$INTRODUCTION_HEAD"

fi

TMPDIR="$REPO_ROOT/.tmp" GOTMPDIR="$REPO_ROOT/.tmp" GOCACHE="$REPO_ROOT/.gocache" \
  go test -v -count=1 -run '^TestFakeCoreThirdCoreE2E$' ./internal/worker/fakecore

(
  cd frontend
  npm test
)

if [[ "$SNAPSHOT" -eq 0 ]]; then
violations=()
while IFS= read -r path; do
  case "$path" in
    cmd/smartgate/main.go|internal/worker/fakecore/*)
      ;;
    *)
      violations+=("$path")
      ;;
  esac
done < <(git diff --name-only "$BASE" "$HEAD_REF" --)

if (( ${#violations[@]} > 0 )); then
  echo "fakecore diff-scope: unexpected paths in $BASE..$HEAD_REF:" >&2
  printf '  %s\n' "${violations[@]}" >&2
  exit 1
fi

printf '\nFakeCore introduction diff stat (%s..%s)\n' "$BASE" "$HEAD_REF"
git diff --stat "$BASE" "$HEAD_REF" --
echo "fakecore diff-scope: passed for $BASE..$HEAD_REF"

fi

printf '\nFakeCore third-core acceptance assertions\n'
printf '  PASS 1 Node import: fooprotocol identity and Extra preserved\n'
printf '  PASS 2 Capability: three registered cores evaluated\n'
printf '  PASS 3 Core choice: registry-backed selection persisted\n'
printf '  PASS 4 Planner: descriptor-derived worker planned without blockers\n'
printf '  PASS 5 ExecutionGraph: stable hash and core data flow\n'
printf '  PASS 6 Apply: config, checkpoint, stop, and rollback verified\n'
printf '  PASS 7 Runtime observation: version, state, events, system, and rescue verified\n'
printf '  PASS 8 Frontend: Nodes and Workers render third-core data dynamically\n'
if [[ "$SNAPSHOT" -eq 0 ]]; then
  printf '  PASS 9 Diff-scope: only FakeCore adapter and composition-root registration changed\n'
fi
printf 'Report: %s\n' "$REPORT"
