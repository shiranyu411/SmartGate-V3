#!/bin/sh
# Fail if the tree contains anything that looks like a real credential,
# subscription endpoint, or personal absolute path. Used by release-gate and
# oss-export. Patterns are intentionally broad; add an allowlist entry in
# scripts/secret-scan.allow (one regex per line) for known-safe matches.
set -eu

root=${1:-.}
here=$(cd "$(dirname "$0")" && pwd)
allow="$here/secret-scan.allow"

if ! command -v rg >/dev/null 2>&1; then
	echo "secret-scan: ripgrep (rg) is required" >&2
	exit 2
fi

patterns=$(mktemp)
trap 'rm -f "$patterns"' EXIT
cat > "$patterns" <<'EOF'
Bearer [A-Za-z0-9._-]{16,}
[?&]token=[A-Za-z0-9._-]{8,}
"(password|passwd|secret|api_key|apikey|private_key)"\s*:\s*"[^"]{4,}"
(password|passwd|secret)\s*=\s*["'][^"']{4,}["']
-----BEGIN (RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----
/Users/[A-Za-z0-9._-]+
/home/[a-z][A-Za-z0-9._-]+/(Desktop|Documents|Downloads)
(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})
https?://[a-z0-9.-]+\.(top|xyz|cc|icu|club|site|online|shop|vip|cyou)(/|$)
sing[-]box://import-remote-profile\?url=https?%3A%2F%2F(?!example\.com)
EOF

status=0
while IFS= read -r pattern; do
	[ -n "$pattern" ] || continue
	hits=$(rg -n --no-heading --pcre2 \
		--glob '!.git' --glob '!.gocache' --glob '!.tmp' --glob '!.smoke' \
		--glob '!.devvm' --glob '!dist' --glob '!frontend/node_modules' \
		--glob '!frontend/dist' --glob '!frontend/package-lock.json' \
		--glob '!**/scripts/secret-scan.sh' --glob '!**/scripts/secret-scan.allow' \
		-e "$pattern" "$root" 2>/dev/null || true)
	[ -n "$hits" ] || continue
	if [ -f "$allow" ]; then
		while IFS= read -r allowed; do
			[ -n "$allowed" ] || continue
			case "$allowed" in \#*) continue ;; esac
			hits=$(printf '%s\n' "$hits" | rg -v -e "$allowed" || true)
		done < "$allow"
	fi
	[ -n "$hits" ] || continue
	printf 'secret-scan: pattern /%s/ matched:\n%s\n\n' "$pattern" "$hits" >&2
	status=1
done < "$patterns"

if [ "$status" -ne 0 ]; then
	echo "secret-scan: FAIL" >&2
	exit 1
fi
echo "secret-scan: clean ($root)"
