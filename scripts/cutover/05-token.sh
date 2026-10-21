#!/usr/bin/env bash
# Obtain an admin Bearer token from the target's SmartGate API and store it in
# a 0600 file for the other cutover scripts.
#
#   TARGET=router scripts/cutover/05-token.sh --token-file <path> [--if-missing]
#
# The password is read from the terminal (or, for rehearsal automation, from
# SMARTGATE_PASSWORD_FILE) and travels to the API on curl's stdin, so it never
# appears on a command line, in a shell history entry, or in this repository.
# The token is written with umask 077 and is only ever passed to curl through
# a config file. --if-missing keeps an existing non-empty token file untouched,
# which lets `make cutover` run unattended once the token has been minted.
# TOKEN_API_URL overrides the API base when the target's API_URL is not
# reachable from the workstation (devvm forwards it to 127.0.0.1:13000).
. "$(cd "$(dirname "$0")" && pwd)/common.sh"

TOKEN_FILE=""
IF_MISSING=0
while [ $# -gt 0 ]; do
  case "$1" in
    --token-file) TOKEN_FILE=$2; shift 2 ;;
    --if-missing) IF_MISSING=1; shift ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$TOKEN_FILE" ] || die "usage: TARGET=<name> $0 --token-file <path> [--if-missing]"

load_target
need curl; need python3

if [ "$IF_MISSING" -eq 1 ] && [ -s "$TOKEN_FILE" ]; then
  info "token file $TOKEN_FILE already exists; keeping it"
  exit 0
fi

api="${TOKEN_API_URL:-$API_URL}"
curl -fsS -m 5 "$api/healthz" >/dev/null 2>&1 || die "SmartGate API $api is not reachable from this workstation (set TOKEN_API_URL if it is forwarded elsewhere)"

if [ -n "${SMARTGATE_PASSWORD_FILE:-}" ]; then
  [ -s "$SMARTGATE_PASSWORD_FILE" ] || die "SMARTGATE_PASSWORD_FILE is missing or empty"
  IFS= read -r password < "$SMARTGATE_PASSWORD_FILE" || true
else
  [ -t 0 ] || die "a terminal is required to enter the admin password (or set SMARTGATE_PASSWORD_FILE)"
  printf 'SmartGate admin password for %s (%s): ' "$TARGET" "$api" >&2
  read -rs password
  printf '\n' >&2
fi
[ -n "$password" ] || die "empty password"

response="$(printf '%s' "$password" \
  | python3 -c 'import json, sys; print(json.dumps({"password": sys.stdin.read()}))' \
  | curl -sS -m 15 -H 'Content-Type: application/json' --data-binary @- "$api/api/auth/login")" \
  || die "login request to $api failed"
unset password

token="$(printf '%s' "$response" | python3 -c '
import json, sys
try:
    body = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if body.get("ok") and isinstance(body.get("data"), dict):
    print(body["data"].get("token", ""))
')"
if [ -z "$token" ]; then
  die "login was rejected: $(printf '%s' "$response" | redact | head -c 300)"
fi

umask 077
mkdir -p "$(dirname "$TOKEN_FILE")"
printf '%s\n' "$token" > "$TOKEN_FILE.tmp"
mv "$TOKEN_FILE.tmp" "$TOKEN_FILE"
chmod 0600 "$TOKEN_FILE"

# Confirm the stored token authenticates. The header goes through a curl
# config file so the token never becomes a process argument.
check="$(curl -sS -m 5 -K <(printf 'header = "Authorization: Bearer %s"\n' "$token") "$api/api/auth/check" 2>/dev/null || true)"
unset token
case "$check" in
  *'"authenticated":true'*) ;;
  *) rm -f "$TOKEN_FILE"; die "stored token was not accepted by $api/api/auth/check: $(printf '%s' "$check" | redact | head -c 200)" ;;
esac
info "admin token for $TARGET stored in $TOKEN_FILE (mode 0600); it stays out of ops/, evidence and logs"
