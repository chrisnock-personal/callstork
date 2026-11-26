package proxy

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/chrisnock-personal/callstork/internal/config"
	"github.com/emiago/sipgo/sip"
)

func compile(t *testing.T, yaml string) *Runtime {
	t.Helper()
	cfg, diags := config.Parse([]byte(yaml))
	if cfg == nil {
		t.Fatalf("parse: %v", diags)
	}
	for _, d := range append(diags, config.Check(cfg)...) {
		if d.Severity == config.Error {
			t.Fatalf("config error: %s", d)
		}
	}
	rt, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

const lab = `version: 1
listen:
  peer: { address: 127.0.0.1, port: 6060 }
  lan:  { address: 127.0.0.1, port: %LAN% }
peers:
  - { name: itsp, allow: [198.51.100.0/24], match_field: to_user }
  - { name: lab, allow: [127.0.0.0/8], match_field: request_uri_user }
rules:
  - name: exact
    match: { number: '^447700900123$' }
    action: { target: 'sip:127.0.0.1:5072' }
  - name: dids
    match: { methods: [INVITE], number: '^44(\d{10})$' }
    action:
      pool: dids
      rewrite:
        request_uri: 'sip:0${1}@${selected_host}:${selected_port}'
        add_headers: { X-Inbound-Rule: dids }
pools:
  dids:
    strategy: first-available
    members:
      - { uri: 'sip:192.0.2.10', priority: 1 }
      - { uri: 'sip:192.0.2.11:5080', priority: 9 }
`

func labRuntime(t *testing.T, lanPort string) *Runtime {
	return compile(t, strings.Replace(lab, "%LAN%", lanPort, 1))
}

func TestRecordRouteMode(t *testing.T) {
	if labRuntime(t, "6060").DoubleRR {
		t.Error("one socket for both sides should mean one Record-Route")
	}
	if !labRuntime(t, "6061").DoubleRR {
		t.Error("two sockets should mean the RFC 5658 pair")
	}
}

func TestPeerFor(t *testing.T) {
	rt := labRuntime(t, "6060")
	for addr, want := range map[string]string{"198.51.100.7": "itsp", "127.0.0.1": "lab", "192.0.2.1": ""} {
		p := rt.PeerFor(netip.MustParseAddr(addr))
		got := ""
		if p != nil {
			got = p.Name
		}
		if got != want {
			t.Errorf("PeerFor(%s) = %q, want %q", addr, got, want)
		}
	}
	if !rt.PeerFor(netip.MustParseAddr("198.51.100.7")).MatchToUser {
		t.Error("itsp should take the number from the To user")
	}
}

func TestNormaliseAndMatch(t *testing.T) {
	rt := labRuntime(t, "6060")
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "+447700900123", Host: "127.0.0.1"})
	number := rt.Normalise(req.Recipient.User)
	if number != "447700900123" {
		t.Fatalf("Normalise = %q", number)
	}
	if r, _ := rt.Match("INVITE", number, req); r == nil || r.Name != "exact" {
		t.Errorf("447700900123 should match the exact rule first, got %v", r)
	}
	r, caps := rt.Match("INVITE", "447700900999", req)
	if r == nil || r.Name != "dids" || caps[1] != "7700900999" {
		t.Fatalf("447700900999: rule %v, captures %v", r, caps)
	}
	if r, _ := rt.Match("OPTIONS", "447700900999", req); r != nil {
		t.Errorf("the dids rule is INVITE only, but OPTIONS matched %q", r.Name)
	}
	if r, _ := rt.Match("INVITE", "12345", req); r != nil {
		t.Errorf("12345 should match nothing, got %q", r.Name)
	}
}

func TestExpandAndOrder(t *testing.T) {
	rt := labRuntime(t, "6060")
	pool := rt.Pools["dids"]
	state := NewState()
	order := pool.Order(state)
	if len(order) != 2 || order[0].Key != "192.0.2.11:5080" || order[1].Key != "192.0.2.10:5060" {
		t.Fatalf("first-available should try the highest priority first: %v, %v", order[0].Key, order[1].Key)
	}
	got := Expand(rt.Rules[1].RequestURI, []string{"447700900999", "7700900999"}, order[0])
	if got != "sip:07700900999@192.0.2.11:5080" {
		t.Errorf("Expand = %q", got)
	}
	if got := Expand(rt.Rules[1].RequestURI, []string{"x", "1"}, order[1]); got != "sip:01@192.0.2.10:5060" {
		t.Errorf("a member without a port should expand to 5060, got %q", got)
	}

	state.setUp("192.0.2.11:5080", false)
	if order := pool.Order(state); len(order) != 1 || order[0].Key != "192.0.2.10:5060" {
		t.Errorf("a member marked down should be skipped: %v", order)
	}
}

func TestLeastCalls(t *testing.T) {
	rt := compile(t, strings.Replace(strings.Replace(lab, "%LAN%", "6060", 1), "strategy: first-available", "strategy: least-calls", 1))
	state := NewState()
	pool := rt.Pools["dids"]
	first := pool.Order(state)[0].Key
	state.CallStarted("call-1", first)
	if pool.Order(state)[0].Key == first {
		t.Error("the member with a call should move behind the idle one")
	}
	state.CallEnded("call-1")
	if pool.Order(state)[0].Key != first {
		t.Error("after BYE the original order should return")
	}
	state.CallEnded("call-1") // a second BYE must not go negative
	if state.Calls(first) != 0 {
		t.Errorf("calls = %d", state.Calls(first))
	}
}

func TestUnspecifiedAddressNeedsAdvertise(t *testing.T) {
	y := strings.Replace(strings.Replace(lab, "%LAN%", "6060", 1),
		"peer: { address: 127.0.0.1, port: 6060 }", "peer: { address: 0.0.0.0, port: 6060 }", 1)
	cfg, _ := config.Parse([]byte(y))
	if _, err := Compile(cfg); err == nil || !strings.Contains(err.Error(), "advertise") {
		t.Errorf("want an error asking for advertise, got %v", err)
	}
	y = strings.Replace(y, "address: 0.0.0.0, port: 6060 }", "address: 0.0.0.0, port: 6060, advertise: 192.0.2.5 }", 1)
	cfg, _ = config.Parse([]byte(y))
	rt, err := Compile(cfg)
	if err != nil || rt.Peer.Advertise != "192.0.2.5" {
		t.Errorf("advertise not used: %v %v", rt, err)
	}
}

func TestUnimplementedStrategyRefused(t *testing.T) {
	y := strings.Replace(strings.Replace(lab, "%LAN%", "6060", 1), "strategy: first-available", "strategy: round-robin", 1)
	cfg, _ := config.Parse([]byte(y))
	if _, err := Compile(cfg); err == nil || !strings.Contains(err.Error(), "#9") {
		t.Errorf("round-robin should be refused until #9, got %v", err)
	}
}
