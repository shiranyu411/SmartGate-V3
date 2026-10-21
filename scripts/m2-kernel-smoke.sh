#!/bin/sh
set -eu

PROBE=${M2_PROBE_BIN:-}
ROUTER_NS=smartgate-m2-router
CLIENT_NS=smartgate-m2-client
TMP_ROOT=${TMPDIR:-/tmp}
NFT_FILE=$TMP_ROOT/smartgate-m2-fabric.$$.nft
DNS_LOG=$TMP_ROOT/smartgate-m2-dns.$$.log
DNS_ERR=$TMP_ROOT/smartgate-m2-dns.$$.err
EVENT_LOG=$TMP_ROOT/smartgate-m2-events.$$.log
EVENT_ERR=$TMP_ROOT/smartgate-m2-events.$$.err
dns_pid=
listener_pid=

worker_for_payload() {
  awk -F '"' -v payload="$1" '$10 == "payload" && $12 == payload { print $4 }' "$EVENT_LOG"
}

assert_worker() {
  payload=$1
  expected=$2
  test "$(worker_for_payload "$payload")" = "$expected"
}

assert_event() {
  payload=$1
  expected_worker=$2
  expected_network=$3
  expected_original=$4
  assert_worker "$payload" "$expected_worker"
  test "$(awk -F '"' -v payload="$payload" '$10 == "payload" && $12 == payload { print $8 }' "$EVENT_LOG")" = "$expected_network"
  test "$(awk -F '"' -v payload="$payload" '$10 == "payload" && $12 == payload { print $16 }' "$EVENT_LOG")" = "$expected_original"
}

cleanup() {
  test -z "$dns_pid" || kill "$dns_pid" 2>/dev/null || true
  test -z "$listener_pid" || kill "$listener_pid" 2>/dev/null || true
  ip netns del "$CLIENT_NS" 2>/dev/null || true
  ip netns del "$ROUTER_NS" 2>/dev/null || true
  ip link del sglan0 2>/dev/null || true
  rm -f "$NFT_FILE" "$DNS_LOG" "$DNS_ERR" "$EVENT_LOG" "$EVENT_ERR"
}
trap cleanup EXIT INT TERM

test "$(uname -s)" = Linux || { echo "m2 kernel smoke requires Linux" >&2; exit 1; }
test "$(id -u)" -eq 0 || { echo "m2 kernel smoke requires root" >&2; exit 1; }
test -n "$PROBE" && test -x "$PROBE" || { echo "M2_PROBE_BIN must name an executable probe" >&2; exit 1; }
for command in ip nft grep awk; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done

cleanup
trap cleanup EXIT INT TERM

ip netns add "$ROUTER_NS"
ip netns add "$CLIENT_NS"
ip link add sglan0 type veth peer name sgclient0
ip link set sglan0 netns "$ROUTER_NS"
ip link set sgclient0 netns "$CLIENT_NS"

ip netns exec "$ROUTER_NS" ip link set lo up
ip netns exec "$ROUTER_NS" ip link set sglan0 up
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv6.conf.sglan0.accept_dad=0
ip netns exec "$ROUTER_NS" ip addr add 10.200.0.1/24 dev sglan0
ip netns exec "$ROUTER_NS" ip -6 addr add fd42:200::1/64 dev sglan0
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.ip_forward=1
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv6.conf.all.forwarding=1
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.conf.all.rp_filter=0
ip netns exec "$ROUTER_NS" sysctl -qw net.ipv4.conf.sglan0.rp_filter=0

ip netns exec "$CLIENT_NS" ip link set lo up
ip netns exec "$CLIENT_NS" ip link set sgclient0 up
ip netns exec "$CLIENT_NS" sysctl -qw net.ipv6.conf.sgclient0.accept_dad=0
ip netns exec "$CLIENT_NS" ip addr add 10.200.0.2/24 dev sgclient0
ip netns exec "$CLIENT_NS" ip -6 addr add fd42:200::2/64 dev sgclient0
ip netns exec "$CLIENT_NS" ip route add default via 10.200.0.1
ip netns exec "$CLIENT_NS" ip -6 route add default via fd42:200::1

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

ip netns exec "$ROUTER_NS" "$PROBE" dns-serve -count 4 -timeout 15s >"$DNS_LOG" 2>"$DNS_ERR" &
dns_pid=$!
attempt=0
while ! grep -q '^READY ' "$DNS_LOG" 2>/dev/null; do
  attempt=$((attempt + 1))
  test "$attempt" -lt 100 || { cat "$DNS_ERR" >&2; exit 1; }
  sleep 0.05 2>/dev/null || sleep 1
done
ip netns exec "$ROUTER_NS" "$PROBE" dns-query -network udp -name probe.ai.example -type A
ip netns exec "$ROUTER_NS" "$PROBE" dns-query -network tcp -name probe.ai.example -type A
ip netns exec "$ROUTER_NS" "$PROBE" dns-query -network udp -name probe.ai.example -type AAAA
ip netns exec "$ROUTER_NS" "$PROBE" dns-query -network tcp -name probe.ai.example -type AAAA
wait "$dns_pid"
dns_pid=
grep -F '"queries":4' "$DNS_LOG" >/dev/null
grep -F '"failures":0' "$DNS_LOG" >/dev/null
grep -F '"classified":4' "$DNS_LOG" >/dev/null
grep -F '"fallback":0' "$DNS_LOG" >/dev/null
grep -F '"set_writes":4' "$DNS_LOG" >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_ai_v4 | grep -F '9.9.9.9' >/dev/null
ip netns exec "$ROUTER_NS" nft list set inet smartgate sg_ai_v6 | grep -F '2001:4860:4860::8888' >/dev/null

ip netns exec "$ROUTER_NS" "$PROBE" listen -count 7 -timeout 15s >"$EVENT_LOG" 2>"$EVENT_ERR" &
listener_pid=$!
attempt=0
while ! grep -q '^READY$' "$EVENT_LOG" 2>/dev/null; do
  attempt=$((attempt + 1))
  test "$attempt" -lt 100 || { cat "$EVENT_ERR" >&2; exit 1; }
  sleep 0.05 2>/dev/null || sleep 1
done

ip netns exec "$CLIENT_NS" "$PROBE" send -network udp4 -destination 203.0.113.9:443 -payload ai-v4-quic
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp4 -destination 203.0.113.9:8443 -payload ai-v4-port-reuse
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp4 -destination 8.8.8.8:443 -payload general-v4-fallback
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp6 -destination '[2606:4700:4700::1111]:443' -payload ai-v6-quic
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp6 -destination '[2606:4700:4700::1111]:8443' -payload ai-v6-port-reuse
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp4 -destination 9.9.9.9:443 -payload ai-v4-dns-set
ip netns exec "$CLIENT_NS" "$PROBE" send -network udp6 -destination '[2001:4860:4860::8888]:443' -payload ai-v6-dns-set

if ! wait "$listener_pid"; then
  listener_pid=
  cat "$EVENT_LOG" >&2
  cat "$EVENT_ERR" >&2
  ip netns exec "$ROUTER_NS" nft -a list table inet smartgate >&2 || true
  exit 1
fi
listener_pid=
test "$(grep -c '^{' "$EVENT_LOG")" -eq 7
general_worker=$(worker_for_payload general-v4-fallback)
ai_worker=$(worker_for_payload ai-v4-quic)
test -n "$general_worker"
test -n "$ai_worker"
test "$general_worker" != "$ai_worker"
assert_event ai-v4-quic "$ai_worker" udp4 203.0.113.9:443
assert_event ai-v4-port-reuse "$ai_worker" udp4 203.0.113.9:8443
assert_event general-v4-fallback "$general_worker" udp4 8.8.8.8:443
assert_event ai-v6-quic "$ai_worker" udp6 '[2606:4700:4700::1111]:443'
assert_event ai-v6-port-reuse "$ai_worker" udp6 '[2606:4700:4700::1111]:8443'
assert_event ai-v4-dns-set "$ai_worker" udp4 9.9.9.9:443
assert_event ai-v6-dns-set "$ai_worker" udp6 '[2001:4860:4860::8888]:443'

cat "$DNS_LOG"
cat "$EVENT_LOG"
echo "M2 kernel smoke passed"
