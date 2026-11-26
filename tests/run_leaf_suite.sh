#!/usr/bin/env bash
# Run the wire scenarios against the callstork binary, a fresh process per
# scenario (as run_ref_suite.sh does for Kamailio), with real restart and
# reload hooks.
#
#   LEAF_MODE=single|double  one Record-Route, or the RFC 5658 pair (default single)
#   LEAF_NET=netns|host      netns (default where `unshare -rn` works): each
#                            scenario in its own network namespace with a normal
#                            loopback (needed under WSL2, which drops loopback UDP
#                            over 1472 bytes); host: the host's loopback (CI)
#   SCENARIOS="a b"          run only these (names from tests/scenarios/)
#   CALLSTORK=path           an already-built binary (default: build one)
#   LEAF_TRACE=1             run the leaf with --trace (every SIP message in its log)
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-"$repo/out/leaf"}
mode=${LEAF_MODE:-single}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bin=${CALLSTORK:-}
if [[ -z $bin ]]; then
  bin=$work/callstork
  if command -v go >/dev/null; then
    (cd "$repo" && CGO_ENABLED=0 go build -o "$bin" ./cmd/callstork) || exit 2
  else
    podman run --rm -v "$repo:/src:Z" -v "$work:/out:Z" -v callstork-gomod:/go/pkg/mod -w /src \
      -e CGO_ENABLED=0 "${GO_IMAGE:-docker.io/library/golang:1.26-alpine}" \
      go build -o /out/callstork ./cmd/callstork || exit 2
  fi
fi

cfg=$work/leaf.yaml
rr=1
case $mode in
  single) cp "$repo/tests/leaf-lab.yaml" "$cfg" ;;
  double)
    sed 's/lan:  { address: 127.0.0.1, port: 6060 }/lan:  { address: 127.0.0.1, port: 6061 }/' \
      "$repo/tests/leaf-lab.yaml" > "$cfg"
    rr=2 ;;
  *) echo "LEAF_MODE must be single or double" >&2; exit 2 ;;
esac
"$bin" check --strict "$cfg" >/dev/null || { "$bin" check "$cfg"; exit 2; }

net=${LEAF_NET:-}
if [[ -z $net ]]; then
  if unshare -rn true 2>/dev/null; then net=netns; else net=host; fi
fi

if [[ -n ${SCENARIOS:-} ]]; then
  scenarios=$(for s in $SCENARIOS; do echo "$repo/tests/scenarios/$s.json"; done)
else
  scenarios=$(ls "$repo"/tests/scenarios/*.json)
fi

failures=0 skipped=0
for scenario in $scenarios; do
  name=$(basename "$scenario" .json)
  args=(run "$work/state-$name" "$bin" "$cfg" "$scenario" "$out" "$rr")
  if [[ $net == netns ]]; then
    unshare -rn "$repo/tests/leaf_scenario.sh" "${args[@]}"
  else
    "$repo/tests/leaf_scenario.sh" "${args[@]}"
  fi
  rc=$?
  cp "$work/state-$name/leaf.log" "$out/$name.leaf.log" 2>/dev/null
  case $rc in
    0) echo "PASS $name" ;;
    77) echo "SKIP $name"; skipped=$((skipped + 1)) ;;
    *) echo "FAIL $name ($out/$name.summary.json)"; failures=$((failures + 1)) ;;
  esac
done
echo "$failures scenario(s) failed, $skipped skipped (LEAF_MODE=$mode, LEAF_NET=$net)"
exit $((failures > 0))
