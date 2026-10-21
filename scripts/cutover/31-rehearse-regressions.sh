#!/usr/bin/env bash
# Controlled fault injection is allowed only on the OpenWrt rehearsal target.
# TARGET=devvm ... <tools|setup-v2|verify-v2|verify-v3|kernel|workers> <token-file>
. "$(cd "$(dirname "$0")" && pwd)/common.sh"
phase=${1:-}
token_file=${2:-}
case "$phase" in tools|setup-v2|verify-v2|verify-v3|reproduce-old-geo|network|kernel|workers) ;; *) die "choose tools, setup-v2, verify-v2, verify-v3, kernel or workers";; esac
load_target
[ "$TARGET" = devvm ] && [ "$TARGET_KIND" = rehearsal ] && [ "$SSH_HOST" = 127.0.0.1 ] && [ "$SSH_PORT" = 2222 ] || die "fault fixtures are restricted to devvm; production is refused"
require_target_reachable
out=$(evidence_dir "regression-$phase")
private_fixture_dir="$HOME/.local/share/smartgate-backups/devvm/20261017-regression-fixture"

provision_tools() {
  mkdir -p "$REPO_ROOT/.tmp" "$private_fixture_dir"
  chmod 0700 "$private_fixture_dir"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$REPO_ROOT/.tmp/m2probe-aarch64" ./internal/fabric/testdata/m2probe
  if [ ! -s "$private_fixture_dir/fixture.key" ]; then
    umask 077
    cat >"$private_fixture_dir/openssl.cnf" <<'CONFIG'
[req]
distinguished_name=dn
x509_extensions=ext
prompt=no
[dn]
CN=SmartGate VM Regression Fixture
[ext]
basicConstraints=critical,CA:true
keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign
extendedKeyUsage=serverAuth
subjectAltName=@alt
[alt]
DNS.1=www.gstatic.com
DNS.2=cloudflare.com
DNS.3=dns.google
IP.1=1.1.1.1
IP.2=1.0.0.1
IP.3=9.9.9.9
CONFIG
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 -config "$private_fixture_dir/openssl.cnf" \
      -keyout "$private_fixture_dir/fixture.key" -out "$private_fixture_dir/fixture.crt" >/dev/null 2>&1
  fi
  remote_sh '[ ! -x /etc/init.d/sg-regression-fixture ] || /etc/init.d/sg-regression-fixture stop
mkdir -p /root/smartgate-regression
chmod 0700 /root/smartgate-regression'
  scp_to_target "$REPO_ROOT/.tmp/m2probe-aarch64" /root/smartgate-regression/probe
  scp_to_target "$private_fixture_dir/fixture.key" /root/smartgate-regression/fixture.key
  scp_to_target "$private_fixture_dir/fixture.crt" /root/smartgate-regression/fixture.crt
  remote_sh 'chmod 0600 /root/smartgate-regression/fixture.key
chmod 0755 /root/smartgate-regression/probe
if [ ! -f /root/smartgate-regression/ca-before.crt ]; then
 cp /etc/ssl/certs/ca-certificates.crt /root/smartgate-regression/ca-before.crt
 cat /root/smartgate-regression/fixture.crt >> /etc/ssl/certs/ca-certificates.crt
fi
printf "%s\n" "{}" > /root/smartgate-regression/mode.json
cat > /etc/init.d/sg-regression-fixture <<"HOOK"
#!/bin/sh /etc/rc.common
START=20
USE_PROCD=1
start_service() {
 procd_open_instance
 procd_set_param command /root/smartgate-regression/probe regression-fixture --tls-cert /root/smartgate-regression/fixture.crt --tls-key /root/smartgate-regression/fixture.key --mode-file /root/smartgate-regression/mode.json
 procd_set_param stdout 1
 procd_set_param stderr 1
 procd_close_instance
}
HOOK
chmod 0755 /etc/init.d/sg-regression-fixture
/etc/init.d/sg-regression-fixture enable
/etc/init.d/sg-regression-fixture start
sleep 1
/etc/init.d/sg-regression-fixture status'
}

run() {
  case "$phase" in
    tools) provision_tools ;;
    setup-v2) provision_tools; python3 -B -u "$CUTOVER_DIR/rehearsal_regression.py" --token-file "$token_file" --phase setup-v2 ;;
    verify-v2|verify-v3|reproduce-old-geo) python3 -B -u "$CUTOVER_DIR/rehearsal_regression.py" --token-file "$token_file" --phase "$phase" ;;
    network) python3 -B -u "$CUTOVER_DIR/rehearsal_network.py" --token-file "$token_file" ;;
    kernel)
      scp_to_target "$REPO_ROOT/scripts/m2-kernel-smoke.sh" /tmp/smartgate-m2-kernel.sh
      remote_sh 'M2_PROBE_BIN=/root/smartgate-regression/probe sh /tmp/smartgate-m2-kernel.sh' ;;
    workers)
      scp_to_target "$REPO_ROOT/scripts/m2-worker-smoke.sh" /tmp/smartgate-m2-worker.sh
      general_runtime_path=$(ssh_target "command -v '${REQUIRED_WORKER_BINS%% *}'")
      optional_worker="${OPTIONAL_WORKER_BINS:-}"
      [ -n "$optional_worker" ] || die "worker smoke needs the optional executor from target metadata"
      ai_runtime_path=$(ssh_target "command -v '${optional_worker%% *}'")
      remote_sh "M2_PROBE_BIN=/root/smartgate-regression/probe M2_GENERAL_RUNTIME_BIN='$general_runtime_path' M2_AI_RUNTIME_BIN='$ai_runtime_path' M2_LOAD_REQUESTS=1000 sh /tmp/smartgate-m2-worker.sh" ;;
  esac
}
set +e
(set -e; run) 2>&1 | redact | tee "$out/results.txt"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" = 0 ]; then result=pass; else result=fail; fi
log_deploy 31-rehearse-regressions.sh "source@$(repo_short_sha)" "$result" "phase=$phase evidence=$out"
info "$phase $result; evidence=$out"
exit "$rc"
