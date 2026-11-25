#!/usr/bin/env bash
# Run every scenario against a freshly started Kamailio reference.
#
# A fresh container per scenario is deliberate: the fake PBXs exist only while
# a scenario is running, so OPTIONS keepalive marks them down between runs and
# a reused container eventually replies 478/503 instead of dispatching.
#
# REF_NET=host (default) runs Kamailio with --network=host and the harness on
# the host. REF_NET=pod runs Kamailio in a throwaway pod and the harness (host
# python3) inside the pod's network namespace via `podman unshare nsenter`, so
# both share an ordinary loopback. Use it under WSL2 mirrored networking, where
# 127.0.0.1 is routed via a 1500-MTU interface and UDP datagrams over 1472
# bytes are silently dropped (oversized_invite then sees no reply at all).
#
# restart_leaf and reload_config steps run real hooks: `podman restart` of the
# Kamailio container, and a dispatcher + address-list reload over kamcmd.
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-"$repo/out/kamailio"}
mode=${REF_MODE:-single}
container=${REF_CONTAINER:-callstork-ref-suite}
image=${KAMAILIO_IMAGE:-ghcr.io/kamailio/kamailio-ci:latest}
net=${REF_NET:-host}
pod=${container}-pod
ctl=unix:/tmp/kamailio_sleaf_ctl

extra=()
rr_expected=1
if [[ $mode == double ]]; then
  extra=(-A DOUBLE_RR)
  rr_expected=2
elif [[ $mode != single ]]; then
  echo "REF_MODE must be single or double" >&2
  exit 2
fi
if [[ $net != host && $net != pod ]]; then
  echo "REF_NET must be host or pod" >&2
  exit 2
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
cleanup() {
  podman rm -f "$container" >/dev/null 2>&1 || true
  if [[ $net == pod ]]; then
    podman pod rm -f "$pod" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Wait until Kamailio's control socket answers, i.e. it has finished starting.
wait_ready="for i in \$(seq 50); do podman exec $container kamcmd -s $ctl core.version >/dev/null 2>&1 && exit 0; sleep 0.1; done; exit 1"
restart_cmd="podman restart -t 0 $container >/dev/null && sh -c '$wait_ready'"
reload_cmd="podman exec $container kamcmd -s $ctl dispatcher.reload >/dev/null && podman exec $container kamcmd -s $ctl permissions.addressReload >/dev/null"

failures=0
skipped=0
for scenario in "$repo"/tests/scenarios/*.json; do
  name=$(basename "$scenario" .json)
  cleanup
  if [[ $net == pod ]]; then
    podman pod create --name "$pod" >/dev/null
    netargs=(--pod "$pod")
  else
    netargs=(--network=host)
  fi
  podman run -d --name "$container" "${netargs[@]}" \
    -v "$repo:/repo:ro,Z" "$image" \
    -f /repo/reference/kamailio/kamailio.cfg -w /repo/reference/kamailio \
    -A PEER_PORT=6060 -A LAN_PORT=6061 "${extra[@]}" -e >/dev/null
  sh -c "$wait_ready" || echo "WARN $name: Kamailio control socket not ready" >&2

  runner=(python3 "$repo/tests/run_scenario.py"
    --leaf 127.0.0.1:6060 --scenario "$scenario" --out "$out"
    --expect-record-routes "$rr_expected"
    --restart-cmd "$restart_cmd" --reload-cmd "$reload_cmd" --quiet)
  if [[ $net == pod ]]; then
    infra=$(podman pod inspect "$pod" -f '{{.InfraContainerID}}')
    pid=$(podman inspect -f '{{.State.Pid}}' "$infra")
    podman unshare nsenter -t "$pid" -n "${runner[@]}"
  else
    "${runner[@]}"
  fi
  case $? in
    0) echo "PASS $name" ;;
    77) echo "SKIP $name ($out/$name.summary.json)"; skipped=$((skipped + 1)) ;;
    *) echo "FAIL $name ($out/$name.summary.json)"; failures=$((failures + 1)) ;;
  esac
done

echo "$failures scenario(s) failed, $skipped skipped"
exit $((failures > 0))
