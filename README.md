# CallStork

**Status:** Phase 0 — reference implementation and test harness, no product code yet.

The source-of-truth design spec is not published; references such as "spec §9"
point into it. The behaviour it requires is captured here by the config
contract, the reference config and the wire scenarios.
Decisions made so far: [`docs/decisions.md`](docs/decisions.md).
Stack probe: [`docs/stack-probe.md`](docs/stack-probe.md).

CallStork is a carrier-facing (more generally, *peer-facing*) inbound SIP dispatcher:
it matches the dialled number, applies ordered rules and rewrites, and distributes
each call to one member of a pool of PBXs. It is a transaction-stateful proxy, not
a B2BUA: `From`/`To` and SDP are left alone, and `Record-Route` keeps it in the
signalling path without requiring dialog-state lookup.

The project was called SIP Leaf while it was being designed. Its docs, tests
and reference config still call the dispatcher role "the leaf": the peer-facing
leaf in front of the PBXs.

## Scope correction from the design review

The first source is **not assumed to be a carrier**. It can be an ITSP, another
PBX, or a SIP endpoint sending a direct INVITE. Consequently the config calls
these `peers` rather than `carriers`; each peer has an `allow` list and a
`match_field` (`request_uri_user` or `to_user`). Registration-based trunks remain
optional per peer and are deferred until a real trunk needs them.

## Repository layout

```
config/
  example.yaml          intended YAML contract for the future product
  address.list          peer allowlist (Kamailio permissions format)
  dispatcher.list       pool members (Kamailio dispatcher format)
reference/kamailio/
  kamailio.cfg          reference implementation of the same behaviour
  README.md             how to run and validate it
tests/
  sipfixture.py         minimal SIP UAC/UAS and verbatim trace writer
  run_scenario.py       scenario runner (UAC + fake PBXs)
  run_ref_suite.sh      run all scenarios against a fresh Kamailio reference
  scenarios/*.json      named wire scenarios from spec §9
  test_fixture.py       unit tests for parsing/message building
traces/
  kamailio/             captured traces, single Record-Route
  kamailio-double/      captured traces, RFC 5658 double Record-Route
docs/
  decisions.md          decisions and open questions
  stack-probe.md        Go/sipgo vs Rust/rvoip-sip evaluation
  probes/sipgo-compile/ compile-only sipgo API check (not product code)
```

## What exists today

- A Kamailio reference config covering matching, dispatch, serial failover,
  `Record-Route`, loose routing, local responses and health probing.
- A config contract that the future `callstork check` / `callstork test` commands
  will consume.
- A stdlib-only Python harness that stands up two controllable fake PBX UASs
  and a UAC, captures verbatim, timestamped SIP traces, and asserts the observable outcome of a scenario, including the
  Request-URI every fake PBX receives.
- Captured reference traces for all 28 scenarios (24 from spec §9, plus
  `test_line_fixed_target` and three setup-deadline scenarios) in both `Record-Route` modes
  (`traces/`), plus a compile-only `sipgo` API probe documenting the stack
  decision (`docs/probes/sipgo-compile/`).

## The `callstork` program (Phase 1, in progress)

`callstork check` validates a config file without sending any SIP: schema and
types, peers and CIDRs, regexes and their capture groups, rules that an earlier
rule shadows, actions, pools and timers, header edits and template variables.
Every problem is reported as `file:line:column: severity: message`.

```sh
go build -o callstork ./cmd/callstork          # Go 1.26
./callstork check config/example.yaml          # 0 error(s), 0 warning(s)
./callstork check --json --strict my.yaml      # JSON; warnings fail too
```

Exit status: 0 with no errors (warnings allowed), 1 with errors (or warnings
under `--strict`), 2 when the file can't be read. Without a local Go:
`podman run --rm -v "$PWD:/src:Z" -w /src golang:1.26-alpine go test ./...`.

## Quick start

Run the parser/message unit tests:

```sh
python3 -m unittest discover -s tests -v
```

Run the full 28-scenario suite against the Kamailio reference (a throwaway
container per scenario, ports 6060/6061):

```sh
REF_MODE=single tests/run_ref_suite.sh out/kamailio
REF_MODE=double tests/run_ref_suite.sh out/kamailio-double
```

Both modes pass all scenarios; the committed traces are in `traces/kamailio/`
and `traces/kamailio-double/`. To run one scenario against an already running
leaf:

```sh
python3 tests/run_scenario.py \
  --leaf 127.0.0.1:6060 \
  --scenario tests/scenarios/happy_path.json \
  --out out/kamailio
```

The trace is written to `out/kamailio/happy_path.sip.txt`. Each message is
preceded by a header line:

```
=== +123 ms (unix 1764000000123) SEND udp 127.0.0.1:xxxx -> 127.0.0.1:5060 len=321 redacted=yes
INVITE ...
```

See [`tests/README.md`](tests/README.md) for the fake PBX behaviours and how to
compare traces against Kamailio and, later, the built leaf.

## Phase 0 exit criteria

- [x] Decisions recorded, including the source-agnostic peer change.
- [x] Kamailio reference config and data files written.
- [x] Stack probe completed (recommendation: Go + `sipgo`; see docs).
- [x] First wire scenarios runnable against the reference.
- [x] Reference config syntax checked with Kamailio 6.1 in a throwaway container
  (both single and `-A DOUBLE_RR` modes).
- [x] Kamailio reference started and baseline traces captured for all 24 §9
  scenarios, in single and double `Record-Route` modes.
- [x] Review fixes (2026-11-25): timeout failovers kept the caller's original
  Request-URI (`$var` lost in `failure_route`; now `$avp`), the `test-line`
  rule was shadowed by the broad DID rule, and the `lab-failover` rewrite had
  no capture group. The harness now checks every Request-URI a PBX receives.
- [x] Timer fixes (2026-11-25): ringing calls were cut off with 408 after 8 s,
  and the 8 s setup deadline was per attempt (a member sending 100 then going
  quiet was failed over at 8 s instead of a 503). Timers now follow spec §6.3.
  The harness now restarts/reloads the reference for real (SKIP without a
  hook), checks in-dialog outcomes, and ACKs non-2xx finals (it never did).
- [x] Divergences between config contract and reference config reconciled (the
  lab-only `lab-failover` rule and deterministic set 2 are documented in both;
  remaining product-side gaps are listed in `reference/kamailio/README.md`).

Phase 0 is complete. Phase 1 starts with the Go/`sipgo` product binary and its
config checker/tester commands.
