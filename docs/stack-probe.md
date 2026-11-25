# Stack probe: Go + sipgo vs Rust + rvoip-sip

Probe date: 2026-11-25. Goal: decide which stack can implement RFC 3261 §16
proxy behaviour with the least custom code, while still allowing verbatim SIP
traces and precise control over `Via`, `Record-Route`, CANCEL and ACK.

## Recommendation

**Use Go with `github.com/emiago/sipgo` for the v1 build.**

Reasons: it already ships the server/client transaction primitives and a
stateful proxy example, exposes raw `sip.Request`/`sip.Response` objects, and
has explicit helpers for `Via`, `Record-Route` and `Max-Forwards` processing.
Its debug dumper is close to the trace format the project already uses.

Rust is no longer the clear underdog it was when the spec was written:
`rvoip-sip` 0.5.0 (published 2026-11-24) added `rvoip-sip-proxy` with
transaction-stateful proxy primitives. But its own documentation calls the
implementation **partial** and explicitly "not an RFC-conformance claim", and
the application-facing header policy rejects caller-supplied `Via`,
`Record-Route`, `Route`, `Call-ID`, `CSeq` and `Max-Forwards`. That is a good
safety model for a B2BUA and a poor fit for a proxy that must construct those
headers itself. Re-evaluate if that crate matures.

## Comparison

| Need | Go / sipgo v1.7.4 | Rust / rvoip-sip 0.5.0 |
|---|---|---|
| Raw messages | `sip.Request`, `sip.Response`; `sip.SIPDebug = true` dumps verbatim UDP messages | `rvoip_sip_core::Message/Request/Response` exists, but high-level builders classify `Via`, `Record-Route`, `Route`, `Call-ID`, `CSeq`, `Max-Forwards` as `StackManaged` and hard-reject them |
| Server transactions | `srv.OnInvite`, `ServerTransaction`, `tx.Acks()`, `tx.Done()` | `rvoip-sip-proxy::StatefulProxy`, `ProxyEvent`, `ForkMode` |
| Client transactions / serial failover | `client.TransactionRequest`, per-attempt branch, `Responses()` channel | `PreparedTarget`, `ProxyTarget`, fork modes; docs say partial §16.7 response aggregation |
| Proxy helpers | `ClientRequestAddVia`, `ClientRequestAddRecordRoute`, `ClientRequestDecreaseMaxForward`, `ClientRequestBuild` | `RecordRoutePolicy`, `ProxyRoutingPolicy`, `routing` validation primitives |
| Stateful proxy example | `example/proxysip/main.go` in the repo | Examples are mostly endpoint/B2BUA; proxy crate has primitives but no comparable reference proxy |
| CANCEL | `srv.OnCancel` and explicit CANCEL construction in the example; developer owns association rules | `TransactionManager` under the proxy; conformance matrix still lists gaps |
| RFC 6026 2xx handling | `ServerTransaction` waits on `tx.Acks()` for 2xx; retransmissions can be forwarded manually | Documented as a candidate/partial area, not asserted |
| Double `Record-Route` | No helper; append two headers manually (straightforward) | `RecordRoutePolicy` may help, but header policy complicates manual control |
| YAML config + CLI | Mature ecosystem (`gopkg.in/yaml.v3`, `cobra`/`flag`) | `serde_yaml` already in the dependency tree, but heavier overall build |
| Toolchain | A throwaway `golang:1.25-alpine` container compiled the probe cleanly; no local Go install needed | Cargo 1.99.1; `rvoip-sip` 0.4.0 had been used in an earlier prototype |

## Evidence

- sipgo v1.7.4, published 2026-11-16: <https://pkg.go.dev/github.com/emiago/sipgo>
  - README lists RFC 3261/3263/3581/6026 support and a stateful proxy example.
  - Exposes `ClientRequestAddVia`, `ClientRequestAddRecordRoute`,
    `ClientRequestDecreaseMaxForward`, `Server.OnCancel`, `Server.OnAck`,
    `TransactionRequest`, `Responses()` and `sip.SIPDebug`.
  - Reference proxy source: <https://github.com/emiago/sipgo/blob/v1.7.4/example/proxysip/main.go>
- rvoip-sip 0.5.0: <https://docs.rs/rvoip-sip/0.5.0/rvoip_sip/>
  - Documents `Endpoint`/`StreamPeer`/`CallbackPeer`/`UnifiedCoordinator` as the
    main surfaces, with dialog/session state owned by the library.
  - Header policy marks `Via`, `Record-Route`, `Route`, `Call-ID`, `CSeq` and
    `Max-Forwards` `StackManaged` and rejects them at `with_header()` time.
- rvoip-sip-proxy 0.5.0: <https://docs.rs/rvoip-sip-proxy/0.5.0/rvoip_sip_proxy/>
  - "Transaction-stateful SIP proxy primitives", but "partial, not an
    RFC-conformance claim"; points to `docs/RFC3261_CONFORMANCE.md` and
    `docs/CONFORMANCE_STATUS.md` for known gaps.
  - Scope mentions §16.6 request processing and a §16.7 candidate.

## Caveats / follow-up

- The Go side was compile-checked on 2026-11-25 in a throwaway
  `docker.io/library/golang:1.25-alpine` container (Go 1.25.14) with
  `github.com/emiago/sipgo` v1.7.4. A small probe using `sipgo.NewUA`,
  `NewServer`/`OnInvite`/`OnCancel`/`OnAck`, `NewClient`, `TransactionRequest`
  options, `ClientRequestAddVia`, `ClientRequestAddRecordRoute`,
  `ClientRequestDecreaseMaxForward`, `ClientRequestBuild`, `sip.ParseUri`,
  `sip.NewRequest` and `sip.SIPDebug` passed `go mod tidy` and
  `CGO_ENABLED=0 go build`. The probe source and reproduction command are in
  [`probes/sipgo-compile/`](probes/sipgo-compile/). This is a compile/API check,
  not a runtime SIP test; runtime behaviour is still judged against the Kamailio
  traces in Phase 1.
- The Rust 0.5 proxy crate is very new (one day old at probe time). If its
  conformance docs show CANCEL, 2xx retransmission and serial failover are
  complete, it deserves a second look; until then it adds risk to the exact
  edge cases the spec calls out.
- Either way, the Kamailio reference and wire tests are stack-independent and
  remain the acceptance baseline.
