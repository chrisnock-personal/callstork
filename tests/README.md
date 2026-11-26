# Wire-test harness

`run_scenario.py` stands up a UAC and two controllable fake PBXs, runs one
scenario from `scenarios/*.json`, and writes a SIP trace plus a JSON outcome
summary. It is dependency-light: only Python 3's standard library is used.

## Run

Against a dispatcher already listening on `127.0.0.1:5060`:

```sh
python3 tests/run_scenario.py \
  --leaf 127.0.0.1:5060 \
  --scenario tests/scenarios/happy_path.json \
  --out out/kamailio
```

Use `--out out/leaf` when running the same scenarios against the future binary,
then compare the two `<scenario>.sip.txt` files.

For a quick harness smoke test with no dispatcher, point `--leaf` directly at
the fake PBX A (`127.0.0.1:5071`). The fake PBX answers the INVITE itself, so
`happy_path.json` still passes; this exercises the UAC, PBX and trace writer
without Kamailio.

To run every scenario against the Kamailio reference (a fresh container per
scenario, because OPTIONS probing marks the short-lived fake PBXs down between
runs):

```sh
REF_MODE=single tests/run_ref_suite.sh out/kamailio
REF_MODE=double tests/run_ref_suite.sh out/kamailio-double
```

Under WSL2 mirrored networking, add `REF_NET=pod`: there 127.0.0.1 is routed
through a 1500-MTU interface and UDP datagrams over 1472 bytes are dropped, so
`oversized_invite` gets no reply. Pod mode runs Kamailio in a throwaway pod and
the harness (host `python3`) inside the pod's network namespace through
`podman unshare nsenter`, so both share an ordinary loopback.

The suite passes the runner real hooks: `--restart-cmd` (`podman restart` of
the Kamailio container, then wait for its control socket) and `--reload-cmd`
(`kamcmd dispatcher.reload` and `permissions.addressReload`). A scenario with a
`restart_leaf` or `reload_config` step and no matching hook is reported as
**SKIP** (exit code 77), never PASS. When the leaf binary exists, give the
runner its own restart/reload commands.

`REF_MODE=double` starts the reference with `-A DOUBLE_RR` and checks two
`Record-Route` headers instead of one. Port 6060/6061 are used instead of
5060/5061; the script removes its container on exit. Captured traces are in
`traces/kamailio/` and `traces/kamailio-double/`.

Unit tests:

```sh
python3 -m unittest discover -s tests -v
```

## Trace format

Every SEND/RECV begins with a line like:

```
=== +00000 ms (unix 1764000000123) SEND udp 127.0.0.1:50001 -> 127.0.0.1:5060 len=321 redacted=no endpoint=uac
```

The verbatim SIP message follows; `endpoint=` identifies which fixture sent or received it. Traces are not
redacted by default: use a lab number range. If a scenario is shared outside
the lab, strip `From`, `To`, `P-Asserted-Identity` and SDP addresses first.

## Scenario JSON

A scenario has:

- `name`, `description`, optional `duration` (seconds).
- `uac`: `number`, optional `to_number`, `from_user`, `port`, `max_forwards`,
  `body` (`"oversized"` for the MTU test), `auto_ack`.
- `pbxs`: `a` and `b`, each with `behavior`, `port`, `enabled`, optional
  `retransmit_200_after`.
- `steps`: timed actions.
- `expect`: assertions on observed final status, PBXs used, dialog, CANCEL,
  `record_routes`, in-dialog requests, and `ruri_user` (the user part every
  initial INVITE at a PBX must carry after rewriting). Independently of
  `expect`, every INVITE a fake PBX receives must have a Request-URI addressed
  to that PBX's own host:port; a failover that forwards the original
  Request-URI fails the scenario. Also: `final_after: [min, max]` (seconds
  from start to the INVITE's final response), `final_count` (how often that
  final arrived; above 1 means the leaf retransmitted it, i.e. our ACK was not
  absorbed), `dialog_responses` (final status of each in-dialog request the
  UAC sent, by method, `re-INVITE` for an in-dialog INVITE) and `pbx_received`
  (in-dialog requests a PBX must have received; `ACK` is the ACK for a non-2xx
  final, `ACK-2xx` the end-to-end ACK for a 2xx), and `final_headers`
  (`{"Retry-After": "30"}`: headers the INVITE's final response must carry).
  Also always checked: the caller must never receive a **stray 2xx**, one
  after a non-2xx final or from a second PBX (another To tag). `--expect-record-routes N`
  overrides only scenarios that already assert `record_routes`.

The UAC ACKs every non-2xx final (and each retransmission of it) with the
response's To tag, as RFC 3261 §17.1.1 requires; `auto_ack: false` turns this
off.

Available actions: `invite`, `retransmit_invite`, `cancel`, `ack_final`, `bye`,
`reinvite`, `in_dialog_options`, `pbx_bye`, and the hook actions
`restart_leaf` and `reload_config` (run synchronously: the event loop waits for
the command to finish). `note` and `wait` are no-ops.

PBX behaviours:

| Behaviour | Effect |
|---|---|
| `answer_200` | immediate 200 OK with Contact and SDP |
| `answer_500` / `answer_503` | immediate 5xx |
| `reply_486` / `reply_404` | immediate non-failover response |
| `silent` | no SIP response at all (tests the 2 s attempt timer) |
| `delayed_200` | 200 at 0.8 s with no provisional |
| `ring_until_cancel` | 180 at 0.15 s, then 487 when CANCEL arrives |
| `ring_then_200` / `ring_then_500` | 180 at 0.15 s, final at 0.8 s |
| `late_200` | 180 at 0.15 s, 200 at 2.8 s, even after CANCEL |
| `trying_then_silent` | 100 Trying at once, then nothing (stops the attempt timer; only the setup deadline ends it) |
| `ring_long_then_200` | 180 at 0.15 s, 200 at 12 s (rings past the 8 s setup deadline) |
| `silent_then_late_200` | nothing until 200 at 2.5 s, after the 2 s attempt timer has moved on |

## Scenario coverage

The files in `scenarios/` are named after spec §9:

| Scenario | What it checks |
|---|---|
| `happy_path` | match, dispatch, 200, dialog, one `Record-Route` |
| `no_match` | local 404, no PBX contact |
| `silent_failover` | 2 s attempt timeout → next member |
| `failover_5xx` | immediate 5xx → next member |
| `reply_486_no_failover` | busy is terminal |
| `ring_then_5xx_no_failover` | final after 180 is terminal |
| `all_down_503` | no active members → local 503 |
| `cancel_before_provisional`, `cancel_during_ringing`, `cancel_between_attempts`, `cancel_after_200` | CANCEL races |
| `late_2xx_abandoned_branch` | late 2xx after CANCEL: not relayed; the leaf ACKs and BYEs it |
| `late_2xx_after_failover`, `late_2xx_while_ringing` | (spec §6.4) a late 2xx from an abandoned attempt, after the next member answered or while it rings: ACK and BYE, caller keeps one dialog |
| `all_members_fail_503` | every member 5xx: each tried, then 503 with `Retry-After` |
| `non_2xx_ack_absorbed` | ACK for local final absorbed |
| `invite_retransmission_failover` | retransmission during failover does not fork |
| `twoxx_retransmission` | 2xx retransmission reaches the UAC |
| `in_dialog_bye_from_uac`, `in_dialog_bye_from_pbx`, `reinvite`, `in_dialog_options` | loose routing |
| `leaf_restart_with_call_up`, `config_reload_with_call` | restart / reload hook with a call up, then BYE must reach the PBX |
| `dialog_timeout_then_bye` | BYE after 9 s still routes (run the leaf with a dialog timeout below 9 s) |
| `max_forwards_loop`, `oversized_invite` | local rejection guards |
| `test_line_fixed_target` | (not §9) exact-number rule wins over the broad DID rule; fixed target |
| `trying_then_silent_deadline_503` | (spec §6.3) 100 stops the attempt timer: no failover, 503 at the 8 s deadline |
| `deadline_across_attempts` | (spec §6.3) the 8 s deadline counts from the caller's INVITE, not per attempt |
| `long_ringing_then_200` | (spec §6.3) the deadline ends at the first 18x; a call may ring past 8 s |

## Limitations

- UDP/IPv4 only; no TCP, TLS, RTP or media checks.
- The fixtures are not a conformance SIP stack. They implement only the header
  behaviour these scenarios need.
- Restarts and reloads need hook commands; without them those scenarios SKIP.
- The harness asserts observable UAC/PBX outcomes. ACK absorption for a
  stateful final shows up as `final_count: 1`; for a stateless local reply
  (404, 483, 513) and for a late 2xx it must still be confirmed by reading the
  trace (and, for wire fidelity, against `tcpdump`).
