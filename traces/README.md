# Reference traces

These are the captured outputs of all 31 scenarios in `tests/scenarios/` (24
from spec §9 and seven added since: `test_line_fixed_target`, three
setup-deadline scenarios, `all_members_fail_503`, `late_2xx_after_failover` and
`late_2xx_while_ringing`) run against the Kamailio reference config, last
re-captured on 2026-11-25 after the late-2xx and `Retry-After` fixes (#1, #2).
They were run on WSL2 with `REF_NET=pod`.

| Directory | Mode | How it was started |
|---|---|---|
| `kamailio/` | one `Record-Route` | `REF_NET=pod REF_MODE=single tests/run_ref_suite.sh traces/kamailio` |
| `kamailio-double/` | two `Record-Route` headers, RFC 5658 order | `REF_NET=pod REF_MODE=double tests/run_ref_suite.sh traces/kamailio-double` |

Each scenario has:

- `<scenario>.sip.txt` — timestamped SEND/RECV lines followed by the verbatim
  SIP messages.
- `<scenario>.summary.json` — the harness's observable outcome: final status,
  which PBX was contacted, dialog and CANCEL flags, `Record-Route` count and
  in-dialog requests.

The suite starts a fresh Kamailio container per scenario because the fake PBXs
exist only during that scenario and OPTIONS probing would otherwise mark them
down. Use `tests/run_ref_suite.sh` to regenerate; do not compare raw timestamps
or ephemeral ports between runs — compare the SIP messages and the summaries.

These traces are the acceptance baseline for the Phase 1 binary. A different
outcome means the binary, the harness or the config contract needs fixing.
