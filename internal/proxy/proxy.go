package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	// RFC 3261 §18.1.1: a UDP request this large should have gone over TCP.
	maxUDPMessage = 1300
	// Timer C (> 3 min): how long a ringing attempt may go unanswered.
	ringingTimeout = 3 * time.Minute
)

// Proxy is one running leaf.
type Proxy struct {
	rt    atomic.Pointer[Runtime]
	state *State
	log   *slog.Logger

	ua          *sipgo.UserAgent
	srv         *sipgo.Server
	peer, lan   side
	ctx         context.Context
	probeMu     sync.Mutex
	probeCancel context.CancelFunc
}

// side is one of the leaf's two faces: a client bound to its socket.
type side struct {
	client *sipgo.Client
	laddr  sip.Addr
	sock   Socket
}

// New builds a proxy for a runtime. Nothing listens until Run.
func New(rt *Runtime, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{state: NewState(), log: log}
	p.rt.Store(rt)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("CallStork"),
		sipgo.WithUserAgentTransactionLayerOptions(
			sip.WithTransactionLayerUnhandledResponseHandler(p.onUnhandledResponse),
		),
	)
	if err != nil {
		return nil, err
	}
	p.ua = ua
	if p.srv, err = sipgo.NewServer(ua); err != nil {
		return nil, err
	}
	if p.peer, err = newSide(ua, rt.Peer); err != nil {
		return nil, err
	}
	p.lan = p.peer
	if rt.DoubleRR {
		if p.lan, err = newSide(ua, rt.LAN); err != nil {
			return nil, err
		}
	}

	p.srv.OnInvite(p.onInvite)
	p.srv.OnAck(p.onAck)
	p.srv.OnCancel(p.onStrayCancel)
	for _, m := range []sip.RequestMethod{sip.BYE, sip.OPTIONS, sip.UPDATE, sip.INFO, sip.PRACK,
		sip.REFER, sip.NOTIFY, sip.MESSAGE, sip.SUBSCRIBE, sip.PUBLISH, sip.REGISTER} {
		p.srv.OnRequest(m, p.onOther)
	}
	p.srv.OnNoRoute(p.onOther)
	return p, nil
}

func newSide(ua *sipgo.UserAgent, s Socket) (side, error) {
	host, _, _ := net.SplitHostPort(s.Listen)
	c, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname(s.Advertise),
		sipgo.WithClientPort(s.Port),
		sipgo.WithClientConnectionAddr(s.Listen),
	)
	if err != nil {
		return side{}, err
	}
	return side{client: c, laddr: sip.Addr{IP: net.ParseIP(host), Port: s.Port}, sock: s}, nil
}

// Run listens until ctx is cancelled.
func (p *Proxy) Run(ctx context.Context) error {
	p.ctx = ctx
	rt := p.rt.Load()
	errc := make(chan error, 2)
	go func() { errc <- p.srv.ListenAndServe(ctx, "udp", rt.Peer.Listen) }()
	if rt.DoubleRR && rt.LAN.Listen != rt.Peer.Listen {
		go func() { errc <- p.srv.ListenAndServe(ctx, "udp", rt.LAN.Listen) }()
	}
	p.startProbes(rt)
	p.log.Info("listening", "peer", rt.Peer.Listen, "lan", rt.LAN.Listen, "double_record_route", rt.DoubleRR)
	select {
	case <-ctx.Done():
		p.ua.Close()
		return nil
	case err := <-errc:
		p.ua.Close()
		return err
	}
}

// Reload swaps in a new runtime. Calls in progress finish with the old one;
// the sockets can't change without a restart.
func (p *Proxy) Reload(rt *Runtime) error {
	old := p.rt.Load()
	if rt.Peer != old.Peer || rt.LAN != old.LAN {
		return errors.New("listen addresses changed: restart to apply them")
	}
	p.rt.Store(rt)
	p.startProbes(rt)
	return nil
}

func (p *Proxy) startProbes(rt *Runtime) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if p.probeCancel != nil {
		p.probeCancel()
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.probeCancel = cancel
	(&prober{client: p.lan.client, laddr: p.lan.laddr, state: p.state, log: p.log}).run(ctx, rt)
}

// ---- helpers ----

func respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string, hdrs ...sip.Header) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	for _, h := range hdrs {
		res.AppendHeader(h)
	}
	_ = tx.Respond(res)
}

func retryAfter(d time.Duration) sip.Header {
	return sip.NewHeader("Retry-After", strconv.Itoa(int(d.Seconds())))
}

func hostPort(u sip.Uri) string {
	port := u.Port
	if port == 0 {
		port = 5060
	}
	return net.JoinHostPort(u.Host, strconv.Itoa(port))
}

// ours reports whether a URI names one of the leaf's sockets.
func (p *Proxy) ours(u sip.Uri) bool {
	hp := hostPort(u)
	for _, s := range []Socket{p.peer.sock, p.lan.sock} {
		if hp == s.hostPort() || hp == s.Listen {
			return true
		}
	}
	return false
}

// popRoutes removes the leaf's own Route headers from the top (one, or the
// RFC 5658 pair). It reports whether there were any.
func (p *Proxy) popRoutes(req *sip.Request) bool {
	popped := false
	for r := req.Route(); r != nil && p.ours(r.Address); r = req.Route() {
		req.RemoveHeader("Route")
		popped = true
	}
	return popped
}

// nextHop is the first remaining Route, else the Request-URI.
func nextHop(req *sip.Request) string {
	if r := req.Route(); r != nil {
		return hostPort(r.Address)
	}
	return hostPort(req.Recipient)
}

// sideFor picks the socket to send from: the peer-facing one towards a peer's
// address, the LAN one towards everything else.
func (p *Proxy) sideFor(dest string) side {
	host, _, _ := net.SplitHostPort(dest)
	if ip, err := netip.ParseAddr(host); err == nil && p.rt.Load().PeerFor(ip) != nil {
		return p.peer
	}
	return p.lan
}

func (s side) laddrOpt() sipgo.ClientRequestOption {
	laddr := s.laddr
	return func(_ *sipgo.Client, r *sip.Request) error {
		r.Laddr = laddr
		return nil
	}
}

// stripVia removes the leaf's own Via from a response to relay it. The clone
// keeps the address the response arrived on as its destination, so that is
// cleared: the next Via (with received/rport) decides where it goes.
func stripVia(res *sip.Response) *sip.Response {
	out := res.Clone()
	out.RemoveHeader("Via")
	out.SetDestination("")
	return out
}

// guards applies the checks every request gets. It reports whether to go on.
func (p *Proxy) guards(req *sip.Request, tx sip.ServerTransaction) bool {
	if strings.EqualFold(req.Transport(), "udp") && len(req.String()) > maxUDPMessage {
		if !req.IsAck() {
			respond(tx, req, 513, "Message Too Large")
		}
		return false
	}
	max := p.rt.Load().MaxForwards
	if mf := req.MaxForwards(); mf != nil {
		if mf.Val() == 0 {
			if !req.IsAck() {
				respond(tx, req, 483, "Too Many Hops")
			}
			return false
		}
		if int(mf.Val()) > max {
			v := sip.MaxForwardsHeader(max)
			req.ReplaceHeader(&v)
		}
	} else {
		v := sip.MaxForwardsHeader(max)
		req.AppendHeader(&v)
	}
	return true
}

func decMaxForwards(req *sip.Request) {
	if mf := req.MaxForwards(); mf != nil && mf.Val() > 0 {
		mf.Dec()
	}
}

func inDialog(req *sip.Request) bool {
	return req.To() != nil && req.To().Params.Has("tag")
}

// ---- requests ----

func (p *Proxy) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if !p.guards(req, tx) {
		return
	}
	if inDialog(req) {
		p.popRoutes(req)
		p.relay(req, tx)
		return
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Trying", nil))

	rt := p.rt.Load()
	src, err := netip.ParseAddrPort(req.Source())
	peer := (*Peer)(nil)
	if err == nil {
		peer = rt.PeerFor(src.Addr())
	}
	if peer == nil {
		p.log.Info("refused", "from", req.Source(), "call_id", callID(req))
		respond(tx, req, 403, "Forbidden")
		return
	}
	number := req.Recipient.User
	if peer.MatchToUser && req.To() != nil {
		number = req.To().Address.User
	}
	number = rt.Normalise(number)
	rule, caps := rt.Match(string(sip.INVITE), number, req)
	if rule == nil {
		p.log.Info("no rule", "number", number, "peer", peer.Name, "call_id", callID(req))
		respond(tx, req, 404, "Not Found")
		return
	}
	p.popRoutes(req) // a preloaded Route to the leaf itself
	c := &call{p: p, rt: rt, req: req, tx: tx, rule: rule, caps: caps, start: time.Now(),
		cancelled: make(chan struct{})}
	var once sync.Once
	tx.OnCancel(func(*sip.Request) { once.Do(func() { close(c.cancelled) }) })
	c.run(number)
}

func callID(m sip.Message) string {
	if h := m.CallID(); h != nil {
		return h.Value()
	}
	return ""
}

// onAck forwards the ACK for a 2xx along the route set, statelessly. The ACK
// for a non-2xx response never gets here: its server transaction absorbs it.
func (p *Proxy) onAck(req *sip.Request, _ sip.ServerTransaction) {
	if !p.guards(req, nil) {
		return
	}
	if !p.popRoutes(req) && p.ours(req.Recipient) {
		return // for a response the leaf sent itself: nothing to forward
	}
	out := req.Clone()
	decMaxForwards(out)
	dest := nextHop(out)
	s := p.sideFor(dest)
	out.SetDestination(dest)
	if err := s.client.WriteRequest(out, sipgo.ClientRequestAddVia, s.laddrOpt()); err != nil {
		p.log.Warn("ACK not forwarded", "dest", dest, "error", err)
	}
}

func (p *Proxy) onStrayCancel(req *sip.Request, tx sip.ServerTransaction) {
	respond(tx, req, 481, "Call/Transaction Does Not Exist")
}

// onOther handles every other method: in-dialog requests are loose-routed,
// out-of-dialog OPTIONS to the leaf itself are answered, the rest refused.
func (p *Proxy) onOther(req *sip.Request, tx sip.ServerTransaction) {
	if !p.guards(req, tx) {
		return
	}
	popped := p.popRoutes(req)
	if !popped && (!inDialog(req) || p.ours(req.Recipient)) {
		switch {
		case req.Method == sip.OPTIONS:
			respond(tx, req, 200, "OK", sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS"))
		case inDialog(req):
			respond(tx, req, 481, "Call/Transaction Does Not Exist")
		default:
			respond(tx, req, 405, "Method Not Allowed", sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS"))
		}
		return
	}
	if req.Method == sip.BYE {
		p.state.CallEnded(callID(req))
	}
	p.relay(req, tx)
}

// relay forwards an in-dialog request statefully and relays every response.
func (p *Proxy) relay(req *sip.Request, tx sip.ServerTransaction) {
	out := req.Clone()
	decMaxForwards(out)
	dest := nextHop(out)
	s := p.sideFor(dest)
	out.SetDestination(dest)
	ctx, err := s.client.TransactionRequest(context.Background(), out, sipgo.ClientRequestAddVia, s.laddrOpt())
	if err != nil {
		p.log.Warn("in-dialog request not forwarded", "method", req.Method, "dest", dest, "error", err)
		respond(tx, req, 503, "Service Unavailable")
		return
	}
	defer ctx.Terminate()
	if req.IsInvite() {
		ctx.OnRetransmission(func(r *sip.Response) {
			if r.IsSuccess() {
				_ = tx.Respond(stripVia(r))
			}
		})
	}
	for {
		select {
		case res := <-ctx.Responses():
			if res.StatusCode == 100 {
				continue
			}
			_ = tx.Respond(stripVia(res))
			if res.StatusCode >= 200 {
				return
			}
		case <-ctx.Done():
			respond(tx, req, 408, "Request Timeout")
			return
		}
	}
}

// onUnhandledResponse relays a response no transaction matches, statelessly
// along the Via: a 2xx retransmission after the client transaction ended
// (RFC 6026 makes the proxy pass these on).
func (p *Proxy) onUnhandledResponse(res *sip.Response) {
	v := res.Via()
	if v == nil {
		return
	}
	port := v.Port
	if port == 0 {
		port = 5060
	}
	if !p.ours(sip.Uri{Host: v.Host, Port: port}) {
		return
	}
	out := stripVia(res)
	if out.Via() == nil {
		return
	}
	if err := p.srv.WriteResponse(out); err != nil {
		p.log.Debug("unmatched response not relayed", "error", err)
	}
}

// ---- one call ----

type call struct {
	p         *Proxy
	rt        *Runtime
	req       *sip.Request
	tx        sip.ServerTransaction
	rule      *Rule
	caps      []string
	start     time.Time
	cancelled chan struct{}

	ringing  bool
	tried    []string
	canceled bool
}

type outcome int

const (
	done outcome = iota
	next
)

func (c *call) run(number string) {
	rule := c.rule
	var result string
	defer func() {
		c.p.log.Info("call", "call_id", callID(c.req), "number", number, "rule", rule.Name,
			"tried", strings.Join(c.tried, ","), "result", result, "ms", time.Since(c.start).Milliseconds())
	}()

	if rule.Pool == nil {
		var u sip.Uri
		if err := sip.ParseUri(Expand(rule.Target, c.caps, nil), &u); err != nil {
			result = "bad target"
			respond(c.tx, c.req, 500, "Server Internal Error")
			return
		}
		m := &Member{URI: u, Key: hostPort(u)}
		c.tried = append(c.tried, m.Key)
		out := c.build(m, u)
		if r := c.attempt(out, m, defaultAttempt, defaultDeadline, map[string]bool{}); r == next {
			result = "503"
			respond(c.tx, c.req, 503, "Service Unavailable", retryAfter(defaultRetryAfter))
			return
		}
		result = "relayed"
		return
	}

	pool := rule.Pool
	for _, m := range pool.Order(c.p.state) {
		var u sip.Uri
		uri := rule.RequestURI
		if uri == "" {
			uri = "sip:" + c.req.Recipient.User + "@${selected_host}:${selected_port}"
		}
		if err := sip.ParseUri(Expand(uri, c.caps, m), &u); err != nil {
			result = "bad request_uri"
			respond(c.tx, c.req, 500, "Server Internal Error")
			return
		}
		c.tried = append(c.tried, m.Key)
		if c.attempt(c.build(m, u), m, pool.AttemptTimeout, pool.SetupDeadline, pool.FailoverOn) == done {
			result = "relayed"
			return
		}
		if c.canceled || time.Since(c.start) >= pool.SetupDeadline {
			break
		}
	}
	if c.canceled {
		result = "cancelled"
		return
	}
	result = "503"
	respond(c.tx, c.req, 503, "Service Unavailable", retryAfter(pool.RetryAfter))
}

// build makes the request for one attempt: the rewritten Request-URI, one
// Max-Forwards less, Record-Route, and the rule's extra headers. The Via is
// added when it's sent.
func (c *call) build(m *Member, ruri sip.Uri) *sip.Request {
	out := c.req.Clone()
	out.Recipient = ruri
	out.SetDestination(m.Key)
	decMaxForwards(out)
	rr := func(s Socket) *sip.RecordRouteHeader {
		return &sip.RecordRouteHeader{Address: sip.Uri{Host: s.Advertise, Port: s.Port,
			UriParams: sip.HeaderParams{{K: "lr", V: ""}}}}
	}
	if c.rt.DoubleRR {
		// RFC 5658: the LAN/outgoing side on top, the peer-facing one below.
		out.PrependHeader(rr(c.rt.Peer))
		out.PrependHeader(rr(c.rt.LAN))
	} else {
		out.PrependHeader(rr(c.rt.Peer))
	}
	for _, h := range c.rule.AddHeaders {
		out.AppendHeader(sip.NewHeader(h[0], h[1]))
	}
	return out
}

// attempt sends one INVITE and follows it to an outcome: done (a final
// response was relayed or the call ended) or next (fail over).
func (c *call) attempt(out *sip.Request, m *Member, attemptTimeout, deadline time.Duration, failoverOn map[string]bool) outcome {
	s := c.p.lan
	ctx, err := s.client.TransactionRequest(context.Background(), out, sipgo.ClientRequestAddVia, s.laddrOpt())
	if err != nil {
		c.p.log.Warn("attempt not sent", "member", m.Key, "error", err)
		if failoverOn["transport_error"] {
			return next
		}
		respond(c.tx, c.req, 503, "Service Unavailable")
		return done
	}
	ctx.OnRetransmission(func(r *sip.Response) {
		if r.IsSuccess() {
			_ = c.tx.Respond(stripVia(r))
		}
	})

	attemptT := time.NewTimer(attemptTimeout)
	defer attemptT.Stop()
	remaining := deadline - time.Since(c.start)
	if remaining < time.Millisecond {
		remaining = time.Millisecond
	}
	deadlineT := time.NewTimer(remaining)
	defer deadlineT.Stop()
	var ringT <-chan time.Time
	cancelled := c.cancelled
	provisional, cancelPending := false, false

	sendCancel := func() { c.p.sendCancel(s, out) }

	for {
		select {
		case res := <-ctx.Responses():
			attemptT.Stop()
			code := int(res.StatusCode)
			switch {
			case code < 200:
				provisional = true
				if cancelPending {
					cancelPending = false
					sendCancel()
				}
				if code >= 180 && !c.canceled {
					if !c.ringing {
						c.ringing = true
						deadlineT.Stop()
						ringT = time.After(ringingTimeout)
					}
					_ = c.tx.Respond(stripVia(res))
				}
			case code < 300:
				c.p.state.CallStarted(callID(c.req), m.Key)
				_ = c.tx.Respond(stripVia(res))
				return done
			default:
				if c.canceled {
					return done // the caller already has its 487
				}
				if code >= 500 && code < 600 && !c.ringing && failoverOn["5xx"] {
					return next
				}
				_ = c.tx.Respond(stripVia(res))
				return done
			}

		case <-ctx.Done():
			// Timer B or a transport error, with no final response.
			if c.canceled {
				return done
			}
			if !c.ringing && failoverOn["transport_error"] {
				return next
			}
			respond(c.tx, c.req, 408, "Request Timeout")
			return done

		case <-attemptT.C:
			// Not even a 100 Trying: abandon quietly (no CANCEL before a
			// provisional, RFC 3261 §9.1) and try the next member.
			ctx.Terminate()
			if c.canceled {
				return done
			}
			if failoverOn["timeout"] {
				return next
			}
			respond(c.tx, c.req, 408, "Request Timeout")
			return done

		case <-deadlineT.C:
			// No 18x within the setup deadline: end the call here.
			if provisional {
				sendCancel()
			} else {
				ctx.Terminate()
			}
			respond(c.tx, c.req, 503, "Service Unavailable", retryAfter(c.retryAfter()))
			return done

		case <-ringT:
			sendCancel()
			respond(c.tx, c.req, 408, "Request Timeout")
			return done

		case <-cancelled:
			// The caller cancelled; the server transaction has already
			// answered the CANCEL and sent 487. Stop this attempt and don't
			// start another.
			cancelled = nil
			c.canceled = true
			if provisional {
				sendCancel()
			} else {
				cancelPending = true
			}
		}
	}
}

func (c *call) retryAfter() time.Duration {
	if c.rule.Pool != nil {
		return c.rule.Pool.RetryAfter
	}
	return defaultRetryAfter
}

// sendCancel CANCELs an attempt (RFC 3261 §9.1): same Request-URI, top Via,
// Call-ID, From, To and CSeq number as the INVITE.
func (p *Proxy) sendCancel(s side, inv *sip.Request) {
	cancel := sip.NewRequest(sip.CANCEL, inv.Recipient)
	if v := inv.Via(); v != nil {
		cancel.AppendHeader(v.Clone())
	}
	for _, h := range inv.GetHeaders("Route") {
		cancel.AppendHeader(sip.HeaderClone(h))
	}
	for _, h := range []sip.Header{inv.From(), inv.To(), inv.CallID()} {
		if h != nil {
			cancel.AppendHeader(sip.HeaderClone(h))
		}
	}
	mf := sip.MaxForwardsHeader(70)
	cancel.AppendHeader(&mf)
	cancel.AppendHeader(&sip.CSeqHeader{SeqNo: inv.CSeq().SeqNo, MethodName: sip.CANCEL})
	cancel.SetBody(nil)
	cancel.SetDestination(inv.Destination())
	cancel.Laddr = s.laddr
	ctx, err := s.client.TransactionRequest(context.Background(), cancel, func(*sipgo.Client, *sip.Request) error { return nil })
	if err != nil {
		p.log.Warn("CANCEL not sent", "error", err)
		return
	}
	go func() {
		select {
		case <-ctx.Responses():
		case <-ctx.Done():
		case <-time.After(32 * time.Second):
		}
		ctx.Terminate()
	}()
}
