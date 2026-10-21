#!/bin/sh
set -eu

PROBE=${M2_PROBE_BIN:-}
GENERAL_RUNTIME=${M2_GENERAL_RUNTIME_BIN:-}
AI_RUNTIME=${M2_AI_RUNTIME_BIN:-}
ROUTER_NS=smartgate-m2-router
CLIENT_NS=smartgate-m2-client
GENERAL_NS=smartgate-m2-general
AI_NS=smartgate-m2-ai
TARGET_NS=smartgate-m2-target
TMP_ROOT=${TMPDIR:-/tmp}
WORK=$TMP_ROOT/smartgate-m2-workers.$$
NFT_FILE=$WORK/fabric.nft
GENERAL_PROXY_CONFIG=$WORK/general-proxy.json
AI_PROXY_CONFIG=$WORK/ai-proxy.json
WORKER_CONFIG_DIR=$WORK/workers
WORKER_CONFIG_LOG=$WORK/worker-configs.log
GENERAL_PROXY_LOG=$WORK/general-proxy.log
AI_PROXY_LOG=$WORK/ai-proxy.log
GENERAL_WORKER_LOG=$WORK/general-worker.log
AI_WORKER_LOG=$WORK/ai-worker.log
DNS_LOG=$WORK/dns.log
DNS_ERR=$WORK/dns.err
DNS_UPSTREAM_LOG=$WORK/dns-upstream.log
DNS_UPSTREAM_ERR=$WORK/dns-upstream.err
ECHO_LOG=$WORK/echo.log
ECHO_ERR=$WORK/echo.err
LOAD_ECHO_LOG=$WORK/load-echo.log
LOAD_ECHO_ERR=$WORK/load-echo.err
ROUNDTRIP_LOG=$WORK/roundtrip.log
LOAD_GENERAL4_LOG=$WORK/load-general4.log
LOAD_AI4_LOG=$WORK/load-ai4.log
LOAD_GENERAL6_LOG=$WORK/load-general6.log
LOAD_AI6_LOG=$WORK/load-ai6.log
LOAD_REQUESTS=${M2_LOAD_REQUESTS:-10000}
LOAD_CONCURRENCY=${M2_LOAD_CONCURRENCY:-8}
general_proxy_pid=
ai_proxy_pid=
general_worker_pid=
ai_worker_pid=
dns_pid=
dns_upstream_pid=
echo_pid=
load_general4_pid=
load_ai4_pid=
load_general6_pid=
load_ai6_pid=

dump_failure() {
  for file in \
    "$GENERAL_PROXY_LOG" "$AI_PROXY_LOG" "$GENERAL_WORKER_LOG" "$AI_WORKER_LOG" "$WORKER_CONFIG_LOG" \
    "$DNS_LOG" "$DNS_ERR" "$DNS_UPSTREAM_LOG" "$DNS_UPSTREAM_ERR" \
    "$ECHO_LOG" "$ECHO_ERR" "$LOAD_ECHO_LOG" "$LOAD_ECHO_ERR" "$ROUNDTRIP_LOG" \
    "$LOAD_GENERAL4_LOG" "$LOAD_AI4_LOG" "$LOAD_GENERAL6_LOG" "$LOAD_AI6_LOG"; do
    if [ -s "$file" ]; then
      echo "--- $file ---" >&2
      cat "$file" >&2
    fi
  done
  ip netns exec "$ROUTER_NS" nft -a list table inet smartgate >&2 2>/dev/null || true
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ "$status" -ne 0 ]; then
    dump_failure
  fi
  for pid in "$load_general4_pid" "$load_ai4_pid" "$load_general6_pid" "$load_ai6_pid" \
    "$dns_pid" "$dns_upstream_pid" "$echo_pid" "$general_worker_pid" "$ai_worker_pid" "$general_proxy_pid" "$ai_proxy_pid"; do
    test -z "$pid" || kill "$pid" 2>/dev/null || true
  done
  for namespace in "$CLIENT_NS" "$ROUTER_NS" "$GENERAL_NS" "$AI_NS" "$TARGET_NS"; do
    ip netns del "$namespace" 2>/dev/null || true
  done
  for link in sglan0 sggen0 sgai0 sggent0 sgait0; do
    ip link del "$link" 2>/dev/null || true
  done
  rm -rf "$WORK"
  exit "$status"
}

wait_log() {
  file=$1
  pattern=$2
  attempts=0
  while ! grep -q "$pattern" "$file" 2>/dev/null; do
    attempts=$((attempts + 1))
    test "$attempts" -lt 200 || return 1
    sleep 0.05 2>/dev/null || sleep 1
  done
}

enable_interface() {
  namespace=$1
  interface=$2
  ip -n "$namespace" link set "$interface" up
  ip netns exec "$namespace" sysctl -qw "net.ipv6.conf.$interface.accept_dad=0"
}

trap cleanup EXIT INT TERM

test "$(uname -s)" = Linux || { echo "m2 worker smoke requires Linux" >&2; exit 1; }
test "$(id -u)" -eq 0 || { echo "m2 worker smoke requires root" >&2; exit 1; }
case "$LOAD_REQUESTS:$LOAD_CONCURRENCY" in
  *[!0-9:]*|0:*|*:0) echo "M2 load count and concurrency must be positive integers" >&2; exit 1 ;;
esac
for binary in "$PROBE" "$GENERAL_RUNTIME" "$AI_RUNTIME"; do
  test -n "$binary" && test -x "$binary" || { echo "M2 probe and worker runtimes must be executable" >&2; exit 1; }
done
for command in ip nft grep awk ps; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done

mkdir -p "$WORKER_CONFIG_DIR"
for namespace in "$CLIENT_NS" "$ROUTER_NS" "$GENERAL_NS" "$AI_NS" "$TARGET_NS"; do
  ip netns del "$namespace" 2>/dev/null || true
  ip netns add "$namespace"
  ip -n "$namespace" link set lo up
done

ip link add sglan0 type veth peer name sgclient0
ip link set sglan0 netns "$ROUTER_NS"
ip link set sgclient0 netns "$CLIENT_NS"
ip link add sggen0 type veth peer name sggenp0
ip link set sggen0 netns "$ROUTER_NS"
ip link set sggenp0 netns "$GENERAL_NS"
ip link add sgai0 type veth peer name sgaip0
ip link set sgai0 netns "$ROUTER_NS"
ip link set sgaip0 netns "$AI_NS"
ip link add sggent0 type veth peer name sgtgen0
ip link set sggent0 netns "$GENERAL_NS"
ip link set sgtgen0 netns "$TARGET_NS"
ip link add sgait0 type veth peer name sgtai0
ip link set sgait0 netns "$AI_NS"
ip link set sgtai0 netns "$TARGET_NS"

enable_interface "$ROUTER_NS" sglan0
enable_interface "$ROUTER_NS" sggen0
enable_interface "$ROUTER_NS" sgai0
ip -n "$ROUTER_NS" addr add 10.200.0.1/24 dev sglan0
ip -n "$ROUTER_NS" -6 addr add fd42:200::1/64 dev sglan0
ip -n "$ROUTER_NS" addr add 10.210.1.1/30 dev sggen0
ip -n "$ROUTER_NS" addr add 10.210.2.1/30 dev sgai0
ip -n "$ROUTER_NS" route add default via 10.210.1.2
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.ip_forward=1
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv6.conf.all.forwarding=1
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.conf.all.rp_filter=0
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.conf.sglan0.rp_filter=0

enable_interface "$CLIENT_NS" sgclient0
ip -n "$CLIENT_NS" addr add 10.200.0.2/24 dev sgclient0
ip -n "$CLIENT_NS" -6 addr add fd42:200::2/64 dev sgclient0
ip -n "$CLIENT_NS" route add default via 10.200.0.1
ip -n "$CLIENT_NS" -6 route add default via fd42:200::1

enable_interface "$GENERAL_NS" sggenp0
enable_interface "$GENERAL_NS" sggent0
ip -n "$GENERAL_NS" addr add 10.210.1.2/30 dev sggenp0
ip -n "$GENERAL_NS" addr add 192.0.2.1/30 dev sggent0
ip -n "$GENERAL_NS" -6 addr add fd42:220:1::1/64 dev sggent0
ip -n "$GENERAL_NS" route add 8.8.4.4/32 via 192.0.2.2
ip -n "$GENERAL_NS" route add 198.51.100.53/32 via 192.0.2.2
ip -n "$GENERAL_NS" route add default via 192.0.2.2
ip -n "$GENERAL_NS" -6 route add 2001:4860:4860::8844/128 via fd42:220:1::2

enable_interface "$AI_NS" sgaip0
enable_interface "$AI_NS" sgait0
ip -n "$AI_NS" addr add 10.210.2.2/30 dev sgaip0
ip -n "$AI_NS" addr add 192.0.2.5/30 dev sgait0
ip -n "$AI_NS" -6 addr add fd42:220:2::1/64 dev sgait0
ip -n "$AI_NS" route add 9.9.9.9/32 via 192.0.2.6
ip -n "$AI_NS" route add 198.51.100.53/32 via 192.0.2.6
ip -n "$AI_NS" route add default via 192.0.2.6
ip -n "$AI_NS" -6 route add 2001:db8:9::9/128 via fd42:220:2::2

enable_interface "$TARGET_NS" sgtgen0
enable_interface "$TARGET_NS" sgtai0
ip -n "$TARGET_NS" addr add 192.0.2.2/30 dev sgtgen0
ip -n "$TARGET_NS" -6 addr add fd42:220:1::2/64 dev sgtgen0
ip -n "$TARGET_NS" addr add 192.0.2.6/30 dev sgtai0
ip -n "$TARGET_NS" -6 addr add fd42:220:2::2/64 dev sgtai0
ip -n "$TARGET_NS" addr add 8.8.4.4/32 dev lo
ip -n "$TARGET_NS" addr add 9.9.9.9/32 dev lo
ip -n "$TARGET_NS" addr add 198.51.100.53/32 dev lo
ip -n "$TARGET_NS" -6 addr add 2001:4860:4860::8844/128 dev lo
ip -n "$TARGET_NS" -6 addr add 2001:db8:9::9/128 dev lo

cat >"$GENERAL_PROXY_CONFIG" <<'JSON'
{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [{"type": "socks", "tag": "socks-in", "listen": "10.210.1.2", "listen_port": 1080}],
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {"final": "direct"}
}
JSON
cat >"$AI_PROXY_CONFIG" <<'JSON'
{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [{"type": "socks", "tag": "socks-in", "listen": "10.210.2.2", "listen_port": 1080}],
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {"final": "direct"}
}
JSON

"$GENERAL_RUNTIME" check -c "$GENERAL_PROXY_CONFIG"
"$GENERAL_RUNTIME" check -c "$AI_PROXY_CONFIG"
"$PROBE" worker-export -output-dir "$WORKER_CONFIG_DIR" -general-proxy 10.210.1.2:1080 -ai-proxy 10.210.2.2:1080 >"$WORKER_CONFIG_LOG"
general_worker_id=$(awk 'NR == 1 && $1 == "WORKER_CONFIG" { print $2 }' "$WORKER_CONFIG_LOG")
general_worker_config=$(awk 'NR == 1 && $1 == "WORKER_CONFIG" { print $3 }' "$WORKER_CONFIG_LOG")
ai_worker_id=$(awk 'NR == 2 && $1 == "WORKER_CONFIG" { print $2 }' "$WORKER_CONFIG_LOG")
ai_worker_config=$(awk 'NR == 2 && $1 == "WORKER_CONFIG" { print $3 }' "$WORKER_CONFIG_LOG")
test -n "$general_worker_id"
test -n "$general_worker_config"
test -n "$ai_worker_id"
test -n "$ai_worker_config"
test "$general_worker_id" != "$ai_worker_id"
"$GENERAL_RUNTIME" check -c "$general_worker_config"
"$AI_RUNTIME" run -test -config "$ai_worker_config"

ip netns exec "$TARGET_NS" "$PROBE" echo-serve -count 8 -timeout 40s >"$ECHO_LOG" 2>"$ECHO_ERR" &
echo_pid=$!
ip netns exec "$TARGET_NS" "$PROBE" dns-upstream-serve -count 4 -timeout 30s >"$DNS_UPSTREAM_LOG" 2>"$DNS_UPSTREAM_ERR" &
dns_upstream_pid=$!
wait_log "$ECHO_LOG" '^READY$'
wait_log "$DNS_UPSTREAM_LOG" '^READY '

ip netns exec "$GENERAL_NS" "$GENERAL_RUNTIME" run -c "$GENERAL_PROXY_CONFIG" >"$GENERAL_PROXY_LOG" 2>&1 &
general_proxy_pid=$!
ip netns exec "$AI_NS" "$GENERAL_RUNTIME" run -c "$AI_PROXY_CONFIG" >"$AI_PROXY_LOG" 2>&1 &
ai_proxy_pid=$!
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 10.210.1.2:1080 -timeout 10s
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 10.210.2.2:1080 -timeout 10s

ip netns exec "$ROUTER_NS" "$GENERAL_RUNTIME" run -c "$general_worker_config" >"$GENERAL_WORKER_LOG" 2>&1 &
general_worker_pid=$!
ip netns exec "$ROUTER_NS" "$AI_RUNTIME" run -config "$ai_worker_config" >"$AI_WORKER_LOG" 2>&1 &
ai_worker_pid=$!
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 127.0.0.1:9090 -timeout 10s
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 127.0.0.1:10085 -timeout 10s
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 127.0.0.1:16002 -timeout 10s
ip netns exec "$ROUTER_NS" "$PROBE" wait-tcp -address 127.0.0.1:17002 -timeout 10s

"$PROBE" export -output "$NFT_FILE" >/dev/null
ip netns exec "$ROUTER_NS" nft --check -f "$NFT_FILE"
ip netns exec "$ROUTER_NS" nft -f "$NFT_FILE"
ip netns exec "$ROUTER_NS" ip -4 route add local 0.0.0.0/0 dev lo table 1001 proto 186
ip netns exec "$ROUTER_NS" ip -6 route add local ::/0 dev lo table 1001 proto 186
ip netns exec "$ROUTER_NS" ip -4 rule add pref 11001 fwmark 0x50000100/0xffffff00 table 1001
ip netns exec "$ROUTER_NS" ip -6 rule add pref 11001 fwmark 0x50000100/0xffffff00 table 1001
ip netns exec "$ROUTER_NS" ip -4 route add local 0.0.0.0/0 dev lo table 1002 proto 186
ip netns exec "$ROUTER_NS" ip -6 route add local ::/0 dev lo table 1002 proto 186
ip netns exec "$ROUTER_NS" ip -4 rule add pref 11002 fwmark 0x50000200/0xffffff00 table 1002
ip netns exec "$ROUTER_NS" ip -6 rule add pref 11002 fwmark 0x50000200/0xffffff00 table 1002

ip netns exec "$ROUTER_NS" "$PROBE" dns-worker-serve -count 4 -timeout 30s >"$DNS_LOG" 2>"$DNS_ERR" &
dns_pid=$!
wait_log "$DNS_LOG" '^READY '
ip netns exec "$CLIENT_NS" "$PROBE" dns-query -server 10.200.0.1:53 -network udp -name probe.general.example -type A
ip netns exec "$CLIENT_NS" "$PROBE" dns-query -server 10.200.0.1:53 -network tcp -name probe.general.example -type AAAA
ip netns exec "$CLIENT_NS" "$PROBE" dns-query -server 10.200.0.1:53 -network udp -name probe.ai.example -type A
ip netns exec "$CLIENT_NS" "$PROBE" dns-query -server 10.200.0.1:53 -network tcp -name probe.ai.example -type AAAA
wait "$dns_pid"
dns_pid=
wait "$dns_upstream_pid"
dns_upstream_pid=
grep -F '"queries":4' "$DNS_LOG" >/dev/null
grep -F '"failures":0' "$DNS_LOG" >/dev/null
grep -F '"classified":2' "$DNS_LOG" >/dev/null
grep -F '"fallback":2' "$DNS_LOG" >/dev/null
grep -F '"set_writes":4' "$DNS_LOG" >/dev/null
grep -F '"general":2' "$DNS_LOG" >/dev/null
grep -F '"ai":2' "$DNS_LOG" >/dev/null
grep -F '"name":"probe.general.example.","type":"A","remote":"192.0.2.1:' "$DNS_UPSTREAM_LOG" >/dev/null
grep -F '"name":"probe.general.example.","type":"AAAA","remote":"192.0.2.1:' "$DNS_UPSTREAM_LOG" >/dev/null
grep -F '"name":"probe.ai.example.","type":"A","remote":"192.0.2.5:' "$DNS_UPSTREAM_LOG" >/dev/null
grep -F '"name":"probe.ai.example.","type":"AAAA","remote":"192.0.2.5:' "$DNS_UPSTREAM_LOG" >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_general_v4 | grep -F '8.8.4.4' >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_general_v6 | grep -F '2001:4860:4860::8844' >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_ai_v4 | grep -F '9.9.9.9' >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_ai_v6 | grep -F '2001:db8:9::9' >/dev/null

ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network tcp4 -destination 8.8.4.4:18080 -payload general-tcp4 >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network tcp4 -destination 9.9.9.9:18080 -payload ai-tcp4 >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network udp4 -destination 8.8.4.4:443 -payload general-udp4-quic >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network udp4 -destination 9.9.9.9:443 -payload ai-udp4-quic >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network tcp6 -destination '[2001:4860:4860::8844]:18080' -payload general-tcp6 >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network tcp6 -destination '[2001:db8:9::9]:18080' -payload ai-tcp6 >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network udp6 -destination '[2001:4860:4860::8844]:443' -payload general-udp6-quic >>"$ROUNDTRIP_LOG"
ip netns exec "$CLIENT_NS" "$PROBE" roundtrip -network udp6 -destination '[2001:db8:9::9]:443' -payload ai-udp6-quic >>"$ROUNDTRIP_LOG"
wait "$echo_pid"
echo_pid=
test "$(grep -c '^ROUNDTRIP {' "$ROUNDTRIP_LOG")" -eq 8
grep -F '"payload":"general-tcp4","local":"8.8.4.4:18080","remote":"192.0.2.1:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"ai-tcp4","local":"9.9.9.9:18080","remote":"192.0.2.5:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"general-udp4-quic","local":"8.8.4.4:443","remote":"192.0.2.1:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"ai-udp4-quic","local":"9.9.9.9:443","remote":"192.0.2.5:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"general-tcp6","local":"[2001:4860:4860::8844]:18080","remote":"[fd42:220:1::1]:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"ai-tcp6","local":"[2001:db8:9::9]:18080","remote":"[fd42:220:2::1]:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"general-udp6-quic","local":"[2001:4860:4860::8844]:443","remote":"[fd42:220:1::1]:' "$ROUNDTRIP_LOG" >/dev/null
grep -F '"payload":"ai-udp6-quic","local":"[2001:db8:9::9]:443","remote":"[fd42:220:2::1]:' "$ROUNDTRIP_LOG" >/dev/null

load_total=$((LOAD_REQUESTS * 4))
ip netns exec "$TARGET_NS" "$PROBE" echo-serve -count "$load_total" -quiet -timeout 120s >"$LOAD_ECHO_LOG" 2>"$LOAD_ECHO_ERR" &
echo_pid=$!
wait_log "$LOAD_ECHO_LOG" '^READY$'
clock_ticks=$("$PROBE" clock-ticks)
vcpus=$("$PROBE" cpu-count)
general_worker_ticks_before=$(awk '{ print $14 + $15 }' "/proc/$general_worker_pid/stat")
ai_worker_ticks_before=$(awk '{ print $14 + $15 }' "/proc/$ai_worker_pid/stat")
load_started_ns=$("$PROBE" unix-nano)
ip netns exec "$CLIENT_NS" "$PROBE" load -network udp4 -destination 8.8.4.4:443 -payload-prefix general4 -count "$LOAD_REQUESTS" -concurrency "$LOAD_CONCURRENCY" >"$LOAD_GENERAL4_LOG" 2>&1 &
load_general4_pid=$!
ip netns exec "$CLIENT_NS" "$PROBE" load -network udp4 -destination 9.9.9.9:443 -payload-prefix ai4 -count "$LOAD_REQUESTS" -concurrency "$LOAD_CONCURRENCY" >"$LOAD_AI4_LOG" 2>&1 &
load_ai4_pid=$!
ip netns exec "$CLIENT_NS" "$PROBE" load -network udp6 -destination '[2001:4860:4860::8844]:443' -payload-prefix general6 -count "$LOAD_REQUESTS" -concurrency "$LOAD_CONCURRENCY" >"$LOAD_GENERAL6_LOG" 2>&1 &
load_general6_pid=$!
ip netns exec "$CLIENT_NS" "$PROBE" load -network udp6 -destination '[2001:db8:9::9]:443' -payload-prefix ai6 -count "$LOAD_REQUESTS" -concurrency "$LOAD_CONCURRENCY" >"$LOAD_AI6_LOG" 2>&1 &
load_ai6_pid=$!
wait "$load_general4_pid"; load_general4_pid=
wait "$load_ai4_pid"; load_ai4_pid=
wait "$load_general6_pid"; load_general6_pid=
wait "$load_ai6_pid"; load_ai6_pid=
wait "$echo_pid"; echo_pid=
load_finished_ns=$("$PROBE" unix-nano)
general_worker_ticks_after=$(awk '{ print $14 + $15 }' "/proc/$general_worker_pid/stat")
ai_worker_ticks_after=$(awk '{ print $14 + $15 }' "/proc/$ai_worker_pid/stat")
load_elapsed_ns=$((load_finished_ns - load_started_ns))
general_worker_load_cpu=$(awk -v delta="$((general_worker_ticks_after - general_worker_ticks_before))" -v hz="$clock_ticks" -v elapsed="$load_elapsed_ns" 'BEGIN { printf "%.2f", delta * 100000000000 / (hz * elapsed) }')
ai_worker_load_cpu=$(awk -v delta="$((ai_worker_ticks_after - ai_worker_ticks_before))" -v hz="$clock_ticks" -v elapsed="$load_elapsed_ns" 'BEGIN { printf "%.2f", delta * 100000000000 / (hz * elapsed) }')
combined_load_cpu=$(awk -v left="$general_worker_load_cpu" -v right="$ai_worker_load_cpu" 'BEGIN { printf "%.2f", left + right }')
load_headroom=$(awk -v used="$combined_load_cpu" -v cpus="$vcpus" 'BEGIN { printf "%.2f", 100 - used / cpus }')
grep -F "SERVED $load_total" "$LOAD_ECHO_LOG" >/dev/null
for file in "$LOAD_GENERAL4_LOG" "$LOAD_AI4_LOG" "$LOAD_GENERAL6_LOG" "$LOAD_AI6_LOG"; do
  grep -F '"requests":'"$LOAD_REQUESTS" "$file" >/dev/null
done

general_worker_rss=$(awk '/^VmRSS:/ { print $2 }' "/proc/$general_worker_pid/status")
ai_worker_rss=$(awk '/^VmRSS:/ { print $2 }' "/proc/$ai_worker_pid/status")
general_worker_fd=$(find "/proc/$general_worker_pid/fd" -mindepth 1 -maxdepth 1 | wc -l | awk '{ print $1 }')
ai_worker_fd=$(find "/proc/$ai_worker_pid/fd" -mindepth 1 -maxdepth 1 | wc -l | awk '{ print $1 }')
general_worker_cpu=$general_worker_load_cpu
ai_worker_cpu=$ai_worker_load_cpu

"$GENERAL_RUNTIME" version | sed -n '1p'
"$AI_RUNTIME" version | sed -n '1p'
cat "$DNS_LOG"
cat "$DNS_UPSTREAM_LOG"
cat "$ROUNDTRIP_LOG"
cat "$ECHO_LOG"
cat "$LOAD_GENERAL4_LOG"
cat "$LOAD_AI4_LOG"
cat "$LOAD_GENERAL6_LOG"
cat "$LOAD_AI6_LOG"
cat "$LOAD_ECHO_LOG"
echo "WORKER_STATS $general_worker_id rss_kb=$general_worker_rss fd=$general_worker_fd cpu_percent=$general_worker_cpu"
echo "WORKER_STATS $ai_worker_id rss_kb=$ai_worker_rss fd=$ai_worker_fd cpu_percent=$ai_worker_cpu"
echo "LOAD_STATS requests=$load_total elapsed_ns=$load_elapsed_ns vcpus=$vcpus general_worker_cpu_percent=$general_worker_load_cpu ai_worker_cpu_percent=$ai_worker_load_cpu combined_cpu_percent=$combined_load_cpu headroom_percent=$load_headroom"
echo "M2 real worker smoke passed"
