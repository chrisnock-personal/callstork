// Package proxy is CallStork's transaction-stateful SIP proxy: it matches
// inbound calls to rules and sends each one to a member of a pool, failing
// over serially, and stays in the signalling path with Record-Route.
package proxy

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chrisnock-personal/callstork/internal/config"
	"github.com/emiago/sipgo/sip"
)

// Socket is one listening address and what to advertise for it.
type Socket struct {
	Listen    string // ip:port to bind
	Advertise string // ip written into Via and Record-Route
	Port      int
}

func (s Socket) hostPort() string { return net.JoinHostPort(s.Advertise, strconv.Itoa(s.Port)) }

// Runtime is a checked config compiled for routing. A new one is built on
// every reload; calls in progress keep the one they started with.
type Runtime struct {
	Peer, LAN        Socket
	DoubleRR         bool
	StripPlus        bool
	StripLeadingZero bool
	MaxForwards      int
	Peers            []Peer
	Rules            []Rule
	Pools            map[string]*Pool
}

type Peer struct {
	Name        string
	Prefixes    []netip.Prefix
	MatchToUser bool
}

type Rule struct {
	Name       string
	Methods    map[string]bool // nil = every method
	Number     *regexp.Regexp
	ToHost     *regexp.Regexp
	FromUser   *regexp.Regexp
	Pool       *Pool
	Target     string
	RequestURI string
	AddHeaders [][2]string
}

type Pool struct {
	Name            string
	Strategy        string
	FailoverOn      map[string]bool
	AttemptTimeout  time.Duration
	SetupDeadline   time.Duration
	RetryAfter      time.Duration
	OptionsInterval time.Duration
	OptionsURI      string
	Accept          map[int]bool // empty = any final response means up
	Members         []*Member
}

type Member struct {
	URI      sip.Uri
	Key      string // host:port, identifies the member across reloads
	Priority int
	Weight   int
}

const (
	defaultAttempt    = 2 * time.Second
	defaultDeadline   = 8 * time.Second
	defaultRetryAfter = 30 * time.Second
	defaultOptions    = 30 * time.Second
	defaultMaxFwd     = 70
)

// Compile builds a Runtime from a config that passed config.Check without
// errors. It still returns an error for what the proxy can't do yet.
func Compile(c *config.Config) (*Runtime, error) {
	rt := &Runtime{Pools: map[string]*Pool{}, MaxForwards: defaultMaxFwd}
	d := c.Defaults
	rt.StripPlus = !d.StripPlus.Set || d.StripPlus.V
	rt.StripLeadingZero = d.StripLeadingZero.V
	if d.MaxForwards.Set {
		rt.MaxForwards = d.MaxForwards.V
	}
	var err error
	if rt.Peer, err = socket("peer", c.Listen.Peer); err != nil {
		return nil, err
	}
	if rt.LAN, err = socket("lan", c.Listen.LAN); err != nil {
		return nil, err
	}
	// One socket for both sides: one Record-Route. Two: RFC 5658 pair.
	rt.DoubleRR = rt.Peer.Listen != rt.LAN.Listen || rt.Peer.Advertise != rt.LAN.Advertise

	for _, p := range c.Peers {
		peer := Peer{Name: p.Name.V, MatchToUser: p.MatchField.V == "to_user"}
		for _, a := range p.Allow {
			pfx, err := parsePrefix(a.V)
			if err != nil {
				return nil, fmt.Errorf("peer %q: %w", p.Name.V, err)
			}
			peer.Prefixes = append(peer.Prefixes, pfx)
		}
		rt.Peers = append(rt.Peers, peer)
	}

	for _, p := range c.Pools {
		pool := &Pool{
			Name: p.Name.V, Strategy: p.Strategy.V, FailoverOn: map[string]bool{},
			AttemptTimeout:  pick(p.AttemptTimeout, d.AttemptTimeout, defaultAttempt),
			SetupDeadline:   pick(p.SetupDeadline, d.SetupDeadline, defaultDeadline),
			RetryAfter:      pick(p.RetryAfter, d.RetryAfter, defaultRetryAfter),
			OptionsInterval: pick(p.Health.OptionsInterval, config.Val[time.Duration]{}, defaultOptions),
			OptionsURI:      p.Health.OptionsURI.V, Accept: map[int]bool{},
		}
		switch pool.Strategy {
		case "least-calls", "first-available":
		default:
			return nil, fmt.Errorf("pool %q: strategy %q is not implemented yet (#9)", pool.Name, pool.Strategy)
		}
		if len(p.FailoverOn) == 0 {
			pool.FailoverOn = map[string]bool{"transport_error": true, "timeout": true, "5xx": true}
		}
		for _, f := range p.FailoverOn {
			pool.FailoverOn[f.V] = true
		}
		for _, a := range p.Health.Accept {
			pool.Accept[a.V] = true
		}
		for _, m := range p.Members {
			var u sip.Uri
			if err := sip.ParseUri(m.URI.V, &u); err != nil {
				return nil, fmt.Errorf("pool %q: member %q: %w", pool.Name, m.URI.V, err)
			}
			port := u.Port
			if port == 0 {
				port = 5060
			}
			pool.Members = append(pool.Members, &Member{
				URI: u, Key: net.JoinHostPort(u.Host, strconv.Itoa(port)),
				Priority: m.Priority.V, Weight: m.Weight.V,
			})
		}
		rt.Pools[pool.Name] = pool
	}

	for _, r := range c.Rules {
		rule := Rule{Name: r.Name.V, Target: r.Target.V, RequestURI: r.RequestURI.V}
		if len(r.Methods) > 0 {
			rule.Methods = map[string]bool{}
			for _, m := range r.Methods {
				rule.Methods[m.V] = true
			}
		}
		if rule.Number, err = regexp.Compile(r.Number.V); err != nil {
			return nil, err
		}
		if r.ToHost.Set {
			rule.ToHost = regexp.MustCompile(r.ToHost.V)
		}
		if r.FromUser.Set {
			rule.FromUser = regexp.MustCompile(r.FromUser.V)
		}
		if r.Pool.Set {
			rule.Pool = rt.Pools[r.Pool.V]
		}
		for _, h := range r.AddHeaders {
			rule.AddHeaders = append(rule.AddHeaders, [2]string{h.Name.V, h.Value.V})
		}
		rt.Rules = append(rt.Rules, rule)
	}
	return rt, nil
}

func pick(v, def config.Val[time.Duration], fallback time.Duration) time.Duration {
	switch {
	case v.Set:
		return v.V
	case def.Set:
		return def.V
	}
	return fallback
}

func socket(name string, s config.Socket) (Socket, error) {
	addr, err := netip.ParseAddr(s.Address.V)
	if err != nil {
		return Socket{}, fmt.Errorf("listen.%s.address: %w", name, err)
	}
	adv := s.Advertise.V
	if adv == "" {
		if addr.IsUnspecified() {
			return Socket{}, fmt.Errorf("listen.%s.address is %s: set listen.%s.advertise to the IP other hosts reach it on", name, addr, name)
		}
		adv = addr.String()
	}
	return Socket{Listen: net.JoinHostPort(addr.String(), strconv.Itoa(s.Port.V)), Advertise: adv, Port: s.Port.V}, nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// PeerFor returns the first peer whose allow list covers the address.
func (rt *Runtime) PeerFor(ip netip.Addr) *Peer {
	ip = ip.Unmap()
	for i := range rt.Peers {
		for _, p := range rt.Peers[i].Prefixes {
			if p.Contains(ip) {
				return &rt.Peers[i]
			}
		}
	}
	return nil
}

// Normalise applies strip_plus and strip_leading_zero to a dialled number.
func (rt *Runtime) Normalise(n string) string {
	if rt.StripPlus {
		n = strings.TrimPrefix(n, "+")
	}
	if rt.StripLeadingZero {
		n = strings.TrimPrefix(n, "0")
	}
	return n
}

// Match returns the first rule matching the request, with the number
// pattern's captures.
func (rt *Runtime) Match(method, number string, req *sip.Request) (*Rule, []string) {
	for i := range rt.Rules {
		r := &rt.Rules[i]
		if r.Methods != nil && !r.Methods[method] {
			continue
		}
		caps := r.Number.FindStringSubmatch(number)
		if caps == nil {
			continue
		}
		if r.ToHost != nil && (req.To() == nil || !r.ToHost.MatchString(req.To().Address.Host)) {
			continue
		}
		if r.FromUser != nil && (req.From() == nil || !r.FromUser.MatchString(req.From().Address.User)) {
			continue
		}
		return r, caps
	}
	return nil, nil
}

var templateRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// Expand fills ${n}, ${selected_host} and ${selected_port} in a template.
func Expand(tmpl string, caps []string, m *Member) string {
	return templateRE.ReplaceAllStringFunc(tmpl, func(t string) string {
		name := t[2 : len(t)-1]
		if n, err := strconv.Atoi(name); err == nil {
			if n < len(caps) {
				return caps[n]
			}
			return ""
		}
		if m != nil {
			switch name {
			case "selected_host":
				return m.URI.Host
			case "selected_port":
				port := m.URI.Port
				if port == 0 {
					port = 5060
				}
				return strconv.Itoa(port)
			case "member_host":
				return m.URI.Host
			}
		}
		return t
	})
}

// Order returns a pool's members in the order to try them, skipping the
// ones health checks have marked down.
func (p *Pool) Order(state *State) []*Member {
	var up []*Member
	for _, m := range p.Members {
		if state.Up(m.Key) {
			up = append(up, m)
		}
	}
	switch p.Strategy {
	case "first-available":
		sort.SliceStable(up, func(i, j int) bool { return up[i].Priority > up[j].Priority })
	case "least-calls":
		sort.SliceStable(up, func(i, j int) bool { return state.Calls(up[i].Key) < state.Calls(up[j].Key) })
	}
	return up
}
