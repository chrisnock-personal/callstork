# Decisions (2026-11-25)

These answer the open questions in the (unpublished) design spec, §11. They are the input to Phase 0; revisit them only with a concrete reason.

| # | Question | Decision | Consequence |
|---|---|---|---|
| 1 | What kind of trunk is first? | **Any SIP peer**: carrier, PBX or direct endpoint. Assume IP-authenticated initially. | Config uses `peers` with per-peer `allow` and `match_field`. No REGISTER in the first build; keep the field optional for a real registration trunk. |
| 1b | NAT / double `Record-Route` | Not yet known; design supports both. Single `Record-Route` when the two listen addresses are the same, otherwise two (RFC 5658 order: LAN/outgoing on top, peer-facing below). | Reference config has both modes; both were wire-tested in Phase 0 (`traces/kamailio*`). The production choice still depends on the real peer's reachability. |
| 2 | Build or configure? | **Build, with Kamailio as the reference and fallback.** Phase 0 reference config comes first. | Wire tests run against the reference first; any divergence is a bug in one of them. |
| 3 | `Record-Route` on? | **Yes** (carried over from the spec's recommendation). | BYE visibility and `least-calls` work; in-dialog requests are loose-routed. |
| 4 | Simultaneous forking? | **Out of scope.** Serial failover only. | No forking state; one call → one active member. |
| 5 | Language/stack | Probe both; recommendation is **Go + `emiago/sipgo`**, compile-checked in a throwaway Go 1.25 container. | See [`stack-probe.md`](stack-probe.md) and [`probes/sipgo-compile/`](probes/sipgo-compile/). Rust remains an option if its proxy conformance gaps close. |
| 6 | Caller-ID policy | No decision yet. Prefer PBX-side normalisation or PAI; never rewrite `From` in v1. | Config has no `rewrite.from_user`. |
| 7 | Initial pool members | Not fixed. Example uses two lab PBXs on `127.0.0.1:5071/5072`; real members are set per deployment. | Dispatcher list is data, not compiled in. |
| 8 | Failover timers | Keep the spec defaults for now: 2 s attempt timeout, 8 s setup deadline; confirm against the real peer before production. | Reference config sets both per attempt with `t_set_fr()`: 2 s to any response (100 stops it), then whatever is left of the 8 s deadline (counted from the caller's INVITE) until the first 18x; after that up to 3 min of ringing. |
| 9 | UI timing | Config file + CLI checker/tester first; web UI in phase 4. | No API/UI in Phase 0. |

## Working name

The spec called "SIP Leaf" provisional and asked for a rename before code. The
name is **CallStork** (decided 2026-11-25, #4): it delivers each call to the
right home, and it joins the family of call tools named after birds. The binary
and its commands are `callstork` (`callstork check`, `callstork test`) and the
repository is `callstork`. The name stays out of config keys, SIP headers and
metric labels, so a later rename would not change the wire behaviour or the
config contract. The docs and tests keep "the leaf" for the dispatcher role.

## What changed from the spec's examples

- `carriers:` → `peers:` in `config/example.yaml`, for decision 1.
- `match_field: request_uri_user | to_user` is retained; `to_user` is useful for
  registration trunks and for PBXs that send the DID in `To`.
- `register:` is retained as an optional peer block but marked "not in v1".
- Source allowlists are per peer group, not a single global carrier CIDR list.

## Go version (2026-11-25)

Phase 1 code targets **Go 1.26**, not the 1.25 used for the stack probe:
`sipgo` v1.7.4 pulls in `github.com/gobwas/ws`, whose fix for GO-2026-6940
(unbounded allocation from a peer-declared frame length) is in v1.5.0, which
requires Go 1.26. `govulncheck` runs in CI and reports no known
vulnerabilities.
