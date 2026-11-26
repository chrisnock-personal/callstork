#!/usr/bin/env bash
# One wire scenario against a fresh callstork process (used by
# run_leaf_suite.sh), and the restart/reload hooks the runner calls.
#   leaf_scenario.sh run <state-dir> <binary> <config> <scenario> <out> <record-routes>
#   leaf_scenario.sh restart|reload <state-dir>
set -uo pipefail
cmd=$1 state=$2
here=$(cd "$(dirname "$0")" && pwd)

start() {
  : > "$state/started"
  trace=(); [[ ${LEAF_TRACE:-0} == 1 ]] && trace=(--trace)
  "$(cat "$state/binary")" run "${trace[@]}" "$(cat "$state/config")" >> "$state/leaf.log" 2>&1 &
  echo $! > "$state/pid"
  for _ in $(seq 50); do
    [[ $(grep -c 'msg=listening' "$state/leaf.log") -gt $(cat "$state/starts" 2>/dev/null || echo 0) ]] && break
    sleep 0.1
  done
  echo $(( $(cat "$state/starts" 2>/dev/null || echo 0) + 1 )) > "$state/starts"
}

stop() {
  kill "$(cat "$state/pid")" 2>/dev/null
  wait "$(cat "$state/pid")" 2>/dev/null
  for _ in $(seq 30); do kill -0 "$(cat "$state/pid")" 2>/dev/null || break; sleep 0.1; done
}

case $cmd in
  run)
    binary=$3 config=$4 scenario=$5 out=$6 rr=$7
    mkdir -p "$state"
    echo "$binary" > "$state/binary"
    echo "$config" > "$state/config"
    : > "$state/leaf.log"
    ip link set lo up 2>/dev/null || true   # inside a fresh network namespace
    start
    python3 "$here/run_scenario.py" --leaf 127.0.0.1:6060 --scenario "$scenario" --out "$out" \
      --expect-record-routes "$rr" --quiet \
      --restart-cmd "$here/leaf_scenario.sh restart $state" \
      --reload-cmd "$here/leaf_scenario.sh reload $state"
    rc=$?
    stop
    exit $rc
    ;;
  restart) stop; start ;;
  reload) kill -HUP "$(cat "$state/pid")"; sleep 0.3 ;;
  *) echo "unknown command $cmd" >&2; exit 2 ;;
esac
