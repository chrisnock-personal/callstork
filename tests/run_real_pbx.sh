#!/usr/bin/env bash
# Run the real-PBX cases (tests/real_pbx_cases.json) through the Kamailio
# reference against a real PBX prepared as in docs/real-pbx.md.
#
# A fresh Kamailio container per case (host networking, so the PBX can reach
# it), with address.list and dispatcher.list written to a temporary directory:
#   pool 1 (inbound-dids): the PBX
#   pool 2 (lab-failover): DEAD_MEMBER first, then the PBX
#
# Required:
#   LEAF_ADDR    the leaf's own address on the PBX's network (it listens on
#                LEAF_ADDR:6060 and :6061; the PBX must reach both)
#   PBX          the PBX's SIP host:port, e.g. 192.0.2.20:5060
# Optional:
#   CALLER_ADDR  address the test caller uses (default LEAF_ADDR)
#   DEAD_MEMBER  an address that never answers (default 192.0.2.1:5060, TEST-NET)
#   RTP_PORT     caller's RTP port (default 40000)
#   CASES        space-separated case names (default: all)
#   CAPTURE=1    also capture each call with tcpdump (needs sudo -n), for
#                tests/tools/ladder.py
#   KAMAILIO_IMAGE
#
#   LEAF_ADDR=192.0.2.5 PBX=192.0.2.20:5060 tests/run_real_pbx.sh out/real-pbx
set -uo pipefail

: "${LEAF_ADDR:?set LEAF_ADDR to the address of the leaf on the network of the PBX}"
: "${PBX:?set PBX to the SIP host:port of the PBX}"
repo=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-"$repo/out/real-pbx"}
caller=${CALLER_ADDR:-$LEAF_ADDR}
dead=${DEAD_MEMBER:-192.0.2.1:5060}
rtp_port=${RTP_PORT:-40000}
image=${KAMAILIO_IMAGE:-ghcr.io/kamailio/kamailio-ci:latest}
container=${REF_CONTAINER:-sip-leaf-real-pbx}
ctl=unix:/tmp/kamailio_sleaf_ctl
pbx_host=${PBX%:*}

mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
cleanup() {
  podman rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

cp "$repo/reference/kamailio/kamailio.cfg" "$work/"
printf '1 %s 32 0 test-caller\n' "$caller" > "$work/address.list"
cat > "$work/dispatcher.list" <<EOF
1 sip:$PBX 8 10 duid=pbx
2 sip:$dead 8 10 duid=dead
2 sip:$PBX 8 5 duid=pbx
EOF

all_cases() {
  python3 -c 'import json, sys; print(" ".join(json.load(open(sys.argv[1]))["cases"]))' \
    "$repo/tests/real_pbx_cases.json"
}
cases=${CASES:-}
[[ -n $cases ]] || cases=$(all_cases)
failures=0
for name in $cases; do
  podman rm -f "$container" >/dev/null 2>&1
  podman run -d --name "$container" --network=host -v "$work:/t:ro,Z" "$image" \
    -f /t/kamailio.cfg -w /t -A PEER_ADDR="$LEAF_ADDR" -A LAN_ADDR="$LEAF_ADDR" \
    -A PEER_PORT=6060 -A LAN_PORT=6061 -e >/dev/null
  for _ in $(seq 50); do
    podman exec "$container" kamcmd -s $ctl core.version >/dev/null 2>&1 && break
    sleep 0.1
  done
  if [[ ${CAPTURE:-0} == 1 ]]; then
    sudo -n tcpdump -i any -nn -l -A -s0 "udp and (port 6060 or port 6061)" \
      > "$out/$name.tcpdump.txt" 2>/dev/null &
    tdpid=$!
    sleep 0.5
  fi
  python3 "$repo/tests/real_pbx.py" --leaf "$LEAF_ADDR:6060" --caller-host "$caller" \
    --pbx-host "$pbx_host" --rtp-port "$rtp_port" --case "$name" --out "$out" \
    || failures=$((failures + 1))
  if [[ ${CAPTURE:-0} == 1 ]]; then
    sleep 0.5
    sudo -n kill "$tdpid" 2>/dev/null
    wait "$tdpid" 2>/dev/null
  fi
  podman logs "$container" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' \
    | grep -E '<script>' > "$out/$name.kamailio.log"
done
echo "$failures case(s) failed"
exit $((failures > 0))
