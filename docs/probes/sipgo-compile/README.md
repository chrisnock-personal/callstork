# sipgo compile probe

This is a compile-only API check for the Phase 0 stack decision. It is not the
product binary and does not open sockets.

```sh
podman run --rm --userns=keep-id \
  -e HOME=/tmp -e GOCACHE=/tmp/go-build \
  -v "$PWD:/work:z" -w /work \
  docker.io/library/golang:1.25-alpine \
  sh -c 'go version && go mod tidy && CGO_ENABLED=0 go build -o /tmp/sipgo-probe ./...'
```

Verified 2026-11-25 with Go 1.25.14 and `github.com/emiago/sipgo` v1.7.4. The
probe imports both `sipgo` and `sipgo/sip` and references the server, client and
proxy helpers Phase 1 needs: `NewUA`, `NewServer`/`OnInvite`/`OnCancel`/`OnAck`,
`NewClient`, `TransactionRequest` options, `ClientRequestAddVia`,
`ClientRequestAddRecordRoute`, `ClientRequestDecreaseMaxForward`,
`ClientRequestBuild`, `sip.ParseUri`, `sip.NewRequest` and `sip.SIPDebug`.

Runtime behaviour is not asserted here; the acceptance baseline remains the
Kamailio traces in `traces/kamailio/` and `traces/kamailio-double/`.
