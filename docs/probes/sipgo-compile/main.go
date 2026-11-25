package main

import (
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Compile-only probe for the sipgo APIs that CallStork Phase 1 needs:
// a stateful server, a proxy client, Via/Record-Route/Max-Forwards handling,
// raw Request objects and the debug dumper.
func main() {
	var recipient sip.Uri
	if err := sip.ParseUri("sip:07700900999@127.0.0.1:5071", &recipient); err != nil {
		panic(err)
	}
	req := sip.NewRequest(sip.INVITE, recipient)

	ua, err := sipgo.NewUA()
	if err != nil {
		panic(err)
	}
	defer ua.Close()

	client, err := sipgo.NewClient(ua, sipgo.WithClientAddr("127.0.0.1:6062"))
	if err != nil {
		panic(err)
	}
	req.SetDestination("127.0.0.1:5071")

	for _, opt := range []sipgo.ClientRequestOption{
		sipgo.ClientRequestAddVia,
		sipgo.ClientRequestAddRecordRoute,
		sipgo.ClientRequestDecreaseMaxForward,
		sipgo.ClientRequestBuild,
	} {
		if err := opt(client, req); err != nil {
			panic(err)
		}
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		panic(err)
	}
	srv.OnInvite(func(_ *sip.Request, _ sip.ServerTransaction) {})
	srv.OnCancel(func(_ *sip.Request, _ sip.ServerTransaction) {})
	srv.OnAck(func(_ *sip.Request, _ sip.ServerTransaction) {})

	sip.SIPDebug = true
	fmt.Println(req.String())
}
