# Kamailio reference

This config is the executable version of `config/example.yaml`. It is the
behavioural baseline for the wire tests: if the future binary disagrees with
these traces, the binary (or the test, or the config contract) is wrong.

## Run

Run it natively or in a container with the repo mounted, from the repository
root:

```sh
kamailio -f reference/kamailio/kamailio.cfg -DD -E -e
```

Syntax was checked with Kamailio 6.1 (`kamailio -c`, also with
`-A DOUBLE_RR`) using the `ghcr.io/kamailio/kamailio-ci` image. All 28
scenarios (24 from §9, four added in review) pass in both modes; the traces are in `traces/kamailio/` and
`traces/kamailio-double/`. No service is left running.

It listens by default on:

- `udp:127.0.0.1:5060` — peer-facing (UAC) socket
- `udp:127.0.0.1:5061` — LAN/PBX-facing socket

The container examples override both ports (6060/6061), so they don't clash
with a SIP service already on 5060:

```sh
podman run --rm --network=host \
  -v "$PWD:/repo:ro,Z" ghcr.io/kamailio/kamailio-ci:latest \
  -f /repo/reference/kamailio/kamailio.cfg -w /repo/reference/kamailio \
  -A PEER_PORT=6060 -A LAN_PORT=6061 -e
```

The image entrypoint is already `kamailio -DD -E`; do not wrap the command in
`sh -c` or repeat those flags.

To exercise the RFC 5658 double `Record-Route` mode, add `-A DOUBLE_RR`. In that
mode two headers are inserted: top = LAN/outgoing, second = peer-facing. The UAC
stores the route set in reverse order, so its BYE carries `Route: peer` then
`Route: LAN`. A single network uses `-A DOUBLE_RR` omitted and one header.

The config reads data files relative to its own directory:

- `../../config/address.list` — peer allowlist. Group 1 means the DID is in
  `Request-URI` user; group 2 means it is in `To` user.
- `../../config/dispatcher.list` — pool members: set 1 uses dispatcher
  algorithm 10 (least-calls) with `duid`; set 2 is a deterministic lab pool
  using algorithm 8 (priority order) so traces always try PBX A before PBX B.
  Both sets use OPTIONS probing.

Change `PEER_ADDR`/`PEER_PORT` and `LAN_ADDR`/`LAN_PORT` together if you move the
reference to another host; the double `Record-Route` addresses are built from
those port values at runtime. `force_rport=yes` answers from the source
address/port, which keeps the fake UAC behind NAT-style source ports happy.

## Behaviour

- Max-Forwards is capped at 70 (`483`); UDP messages over 1300 bytes get `513`.
- Peer source address must be in `address.list` (`403` otherwise).
- Leading `+` is stripped; leading zero is not (matches `defaults` in YAML).
- Rules are evaluated in order:
  1. `447700900999` → the lab-only `lab-failover` rule: rewrite to
     `07700900999`, pool `lab-failover` (dispatcher set 2, deterministic A→B
     order), add `X-Inbound-Rule: lab-failover`.
  2. `447700900123` → fixed target `sip:127.0.0.1:5072` (test-line rule).
  3. `^44\d{10}$` → rewrite to `0` + the ten digits, pool `dids` (dispatcher
     set 1), add `X-Inbound-Rule: inbound-dids`. The two exact-number rules
     must precede it because it matches their numbers too.
  4. anything else → local `404`.
- The rewritten user, rule name and header flag are kept in `$avp`s, not
  `$var`s, because `failure_route` needs them and may run in a different
  process (the timer process, for a timeout). `$var` is per-process; using it
  sent timeout failovers to the next member with the caller's original
  Request-URI.
- `From` and `To` are never rewritten. The `Request-URI` user is rewritten; the
  selected member provides host and port.
- Timers follow spec §6.3. Each attempt gets 2 s for any response at all; a
  100 Trying stops that timer. The 8 s setup deadline runs from the caller's
  INVITE across all attempts until the first 18x: when it passes, the current
  attempt is CANCELled and the caller gets a local 503, with no further
  failover. After an 18x the branch may ring for up to 3 min (Timer C). This is
  done per attempt with `t_set_fr()` from the time left on the deadline
  (`route[ATTEMPT_TIMERS]`, `route[DEADLINE]`, `onreply_route[LOAD]`).
- Serial failover happens on 5xx, 408, timeout and transport failure **only if
  the caller has not seen 180/183**; a 5xx after ringing is relayed. All other
  final responses (including 486/488) go straight back to the caller. Exhausted
  branches produce local `503`.
- INVITEs get `100 Trying` automatically. CANCEL and ACK for local non-2xx
  responses are absorbed by tm; 2xx ACKs without a Route set are forwarded.
- In-dialog BYE/re-INVITE/OPTIONS are loose-routed. `ds_load_update()` on BYE
  and on 2xx keeps least-calls counters from drifting.

## Validate with the harness

The harness fakes the UAC and both PBXs, so no real PBX is needed. Run the whole
suite against a freshly started reference for each scenario:

```sh
REF_MODE=single tests/run_ref_suite.sh out/kamailio
REF_MODE=double tests/run_ref_suite.sh out/kamailio-double
```

The script starts a throwaway Kamailio container per scenario, uses ports
6060/6061, passes the expected `Record-Route` count (1 or 2), and removes the
container on exit. A fresh process per scenario matters: the fake PBXs exist
only while a scenario runs, and OPTIONS probing marks them down between runs,
after which a reused Kamailio replies 478/503 instead of dispatching.

For a single scenario against an already running leaf:

```sh
python3 tests/run_scenario.py \
  --leaf 127.0.0.1:6060 \
  --scenario tests/scenarios/happy_path.json \
  --out out/kamailio
```

Each run writes `out/kamailio/<scenario>.sip.txt`: timestamped SEND/RECV lines
followed by verbatim SIP messages, plus a JSON summary of the observed outcome. Compare
this trace with a `tcpdump` capture if wire fidelity matters:

```sh
sudo tcpdump -ni lo 'udp port 6060 or udp port 6061 or udp port 5071 or udp port 5072' -w out/kamailio/capture.pcap
```

## Known deviations to reconcile in Phase 1

- The `503` sent for an exhausted pool or a passed deadline has no
  `Retry-After` header, which spec §6.12 asks for.
- The per-peer `match_field` is encoded in the address-list group id, not as a
  named field.
- Health `accept: [200]` is implicit: dispatcher treats any final OPTIONS reply
  as up unless configured otherwise.
- `rewrite.remove_headers` and PAI policy are not implemented; v1 does not
  rewrite caller ID anyway.
