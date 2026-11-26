package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// Check runs every check on a parsed config and returns the problems found,
// in file order. Errors mean the config must not be used; warnings mean it
// works but probably not as intended.
func Check(c *Config) []Diag {
	k := &checker{c: c, pools: map[string]*Pool{}, used: map[string]bool{}}
	k.version()
	k.defaults()
	k.listen()
	k.peers()
	k.poolsIndex()
	k.rules()
	k.poolChecks()
	k.shadowing()
	sort.SliceStable(k.diags, func(i, j int) bool {
		a, b := k.diags[i], k.diags[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return k.diags
}

type checker struct {
	c       *Config
	diags   []Diag
	pools   map[string]*Pool
	used    map[string]bool
	numbers []*regexp.Regexp // compiled rules[i].Number, nil if invalid
}

func (k *checker) errf(pos Pos, path, format string, args ...any) {
	k.diags = append(k.diags, Diag{Severity: Error, Line: pos.Line, Col: pos.Col, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (k *checker) warnf(pos Pos, path, format string, args ...any) {
	k.diags = append(k.diags, Diag{Severity: Warning, Line: pos.Line, Col: pos.Col, Path: path, Msg: fmt.Sprintf(format, args...)})
}

var top = Pos{1, 1}

func (k *checker) version() {
	switch {
	case !k.c.Version.Set:
		k.errf(top, "version", "version is missing; this checker understands version: 1")
	case k.c.Version.V != 1:
		k.errf(k.c.Version.Pos, "version", "version %d is not supported; this checker understands version: 1", k.c.Version.V)
	}
}

func (k *checker) defaults() {
	d := k.c.Defaults
	if d.MaxForwards.Set && (d.MaxForwards.V < 1 || d.MaxForwards.V > 255) {
		k.errf(d.MaxForwards.Pos, "defaults.max_forwards", "max_forwards must be 1 to 255, not %d", d.MaxForwards.V)
	}
	if d.AttemptTimeout.Set && d.SetupDeadline.Set && d.AttemptTimeout.V >= d.SetupDeadline.V {
		k.errf(d.AttemptTimeout.Pos, "defaults.attempt_timeout",
			"attempt_timeout (%s) must be shorter than setup_deadline (%s), or no second member is ever tried",
			d.AttemptTimeout.V, d.SetupDeadline.V)
	}
}

func (k *checker) listen() {
	l := k.c.Listen
	if !l.Set {
		k.errf(top, "listen", "listen is missing: give the peer-facing and LAN-facing addresses and ports")
		return
	}
	for _, s := range []struct {
		name string
		sock Socket
	}{{"peer", l.Peer}, {"lan", l.LAN}} {
		path := "listen." + s.name
		if !s.sock.Set {
			k.errf(l.Pos, path, "listen.%s is missing", s.name)
			continue
		}
		if !s.sock.Address.Set {
			k.errf(s.sock.Pos, path+".address", "listen.%s.address is missing", s.name)
		} else if _, err := netip.ParseAddr(s.sock.Address.V); err != nil {
			k.errf(s.sock.Address.Pos, path+".address", "listen.%s.address %q is not an IP address", s.name, s.sock.Address.V)
		}
		if !s.sock.Port.Set {
			k.errf(s.sock.Pos, path+".port", "listen.%s.port is missing", s.name)
		} else if s.sock.Port.V < 1 || s.sock.Port.V > 65535 {
			k.errf(s.sock.Port.Pos, path+".port", "listen.%s.port must be 1 to 65535, not %d", s.name, s.sock.Port.V)
		}
	}
}

var matchFields = map[string]bool{"request_uri_user": true, "to_user": true}

func (k *checker) peers() {
	if len(k.c.Peers) == 0 {
		k.errf(top, "peers", "no peers: every call would be refused with 403")
		return
	}
	type prefix struct {
		peer string
		p    netip.Prefix
		pos  Pos
	}
	var prefixes []prefix
	names := map[string]Pos{}
	for i, pe := range k.c.Peers {
		path := fmt.Sprintf("peers[%d]", i)
		name := pe.Name.V
		switch {
		case !pe.Name.Set || name == "":
			k.errf(pe.Pos, path+".name", "peer has no name")
			name = path
		default:
			if first, dup := names[name]; dup {
				k.errf(pe.Name.Pos, path+".name", "peer name %q is already used on line %d", name, first.Line)
			}
			names[name] = pe.Name.Pos
		}
		if len(pe.Allow) == 0 {
			k.errf(pe.Pos, path+".allow", "peer %q has no allow list, so no call can come from it", name)
		}
		for j, a := range pe.Allow {
			if !a.Set {
				continue
			}
			p, err := parsePrefix(a.V)
			if err != nil {
				k.errf(a.Pos, fmt.Sprintf("%s.allow[%d]", path, j), "peer %q: %q is not an IP address or CIDR range", name, a.V)
				continue
			}
			for _, other := range prefixes {
				if other.peer != name && other.p.Overlaps(p) {
					k.warnf(a.Pos, fmt.Sprintf("%s.allow[%d]", path, j),
						"peer %q: %s overlaps %s of peer %q (line %d); calls from the overlap are matched to %q, the first",
						name, p, other.p, other.peer, other.pos.Line, other.peer)
				}
			}
			prefixes = append(prefixes, prefix{name, p, a.Pos})
		}
		switch {
		case !pe.MatchField.Set:
			k.errf(pe.Pos, path+".match_field", "peer %q has no match_field (request_uri_user or to_user)", name)
		case !matchFields[pe.MatchField.V]:
			k.errf(pe.MatchField.Pos, path+".match_field", "peer %q: match_field must be request_uri_user or to_user, not %q", name, pe.MatchField.V)
		}
		if r := pe.Register; r != nil {
			k.warnf(r.Pos, path+".register", "peer %q: register is not implemented in v1 and will be ignored (#11)", name)
			if r.Registrar.Set {
				k.sipURI(r.Registrar, path+".register.registrar", "registrar", nil)
			}
			if !r.PasswordEnv.Set {
				k.errf(r.Pos, path+".register.password_env", "peer %q: register needs password_env, the environment variable holding the password", name)
			}
			if r.Expires.Set && r.Expires.V < 60 {
				k.errf(r.Expires.Pos, path+".register.expires", "peer %q: expires must be at least 60 seconds", name)
			}
		}
	}
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

var strategies = map[string]bool{"round-robin": true, "least-calls": true, "weighted": true, "first-available": true}
var failoverReasons = map[string]bool{"transport_error": true, "timeout": true, "5xx": true}

func (k *checker) poolsIndex() {
	for i := range k.c.Pools {
		p := &k.c.Pools[i]
		if first, dup := k.pools[p.Name.V]; dup {
			k.errf(p.Name.Pos, "pools."+p.Name.V, "pool %q is defined twice (also on line %d)", p.Name.V, first.Name.Pos.Line)
			continue
		}
		k.pools[p.Name.V] = p
	}
}

func (k *checker) poolChecks() {
	d := k.c.Defaults
	for i := range k.c.Pools {
		p := &k.c.Pools[i]
		name, path := p.Name.V, "pools."+p.Name.V
		if !k.used[name] {
			k.warnf(p.Name.Pos, path, "pool %q is not used by any rule", name)
		}
		switch {
		case !p.Strategy.Set:
			k.errf(p.Name.Pos, path+".strategy", "pool %q has no strategy (round-robin, least-calls, weighted or first-available)", name)
		case !strategies[p.Strategy.V]:
			k.errf(p.Strategy.Pos, path+".strategy", "pool %q: unknown strategy %q (round-robin, least-calls, weighted or first-available)", name, p.Strategy.V)
		}
		for j, f := range p.FailoverOn {
			if f.Set && !failoverReasons[f.V] {
				k.errf(f.Pos, fmt.Sprintf("%s.failover_on[%d]", path, j), "pool %q: failover_on %q is not one of transport_error, timeout, 5xx", name, f.V)
			}
		}
		attempt, deadline := p.AttemptTimeout, p.SetupDeadline
		if !attempt.Set {
			attempt = d.AttemptTimeout
		}
		if !deadline.Set {
			deadline = d.SetupDeadline
		}
		if attempt.Set && deadline.Set && attempt.V >= deadline.V && (p.AttemptTimeout.Set || p.SetupDeadline.Set) {
			pos := p.AttemptTimeout.Pos
			if !p.AttemptTimeout.Set {
				pos = p.SetupDeadline.Pos
			}
			k.errf(pos, path, "pool %q: attempt_timeout (%s) must be shorter than setup_deadline (%s), or no second member is ever tried",
				name, attempt.V, deadline.V)
		}
		if h := p.Health; h.Set {
			for j, a := range h.Accept {
				if a.Set && (a.V < 100 || a.V > 699) {
					k.errf(a.Pos, fmt.Sprintf("%s.health.accept[%d]", path, j), "pool %q: %d is not a SIP status code", name, a.V)
				}
			}
			if h.OptionsURI.Set {
				k.sipURI(h.OptionsURI, path+".health.options_uri", "options_uri", map[string]string{"member_host": "192.0.2.1"})
			}
			if h.CoolDown.Set && h.OptionsInterval.Set && h.CoolDown.V < h.OptionsInterval.V {
				k.warnf(h.CoolDown.Pos, path+".health.cool_down",
					"pool %q: cool_down (%s) is shorter than options_interval (%s), so a member can come back before it has been probed",
					name, h.CoolDown.V, h.OptionsInterval.V)
			}
		}
		if len(p.Members) == 0 {
			k.errf(p.Name.Pos, path+".members", "pool %q has no members", name)
		}
		for j, m := range p.Members {
			mpath := fmt.Sprintf("%s.members[%d]", path, j)
			if !m.URI.Set {
				k.errf(m.Pos, mpath+".uri", "pool %q: member has no uri", name)
			} else {
				k.sipURI(m.URI, mpath+".uri", "member uri", nil)
			}
			if m.Weight.Set {
				switch {
				case m.Weight.V < 0:
					k.errf(m.Weight.Pos, mpath+".weight", "pool %q: weight can't be negative", name)
				case p.Strategy.V != "weighted":
					k.warnf(m.Weight.Pos, mpath+".weight", "pool %q: weight is ignored by strategy %q (only weighted uses it)", name, p.Strategy.V)
				}
			} else if p.Strategy.V == "weighted" {
				k.errf(m.Pos, mpath+".weight", "pool %q uses weighted but this member has no weight", name)
			}
			if m.Priority.Set && p.Strategy.V != "first-available" {
				k.warnf(m.Priority.Pos, mpath+".priority", "pool %q: priority is ignored by strategy %q (only first-available uses it)", name, p.Strategy.V)
			}
		}
	}
}

var sipMethods = map[string]bool{
	"INVITE": true, "ACK": true, "BYE": true, "CANCEL": true, "OPTIONS": true, "REGISTER": true,
	"PRACK": true, "SUBSCRIBE": true, "NOTIFY": true, "PUBLISH": true, "INFO": true, "REFER": true,
	"MESSAGE": true, "UPDATE": true,
}

// Headers the leaf itself maintains, or that v1 never rewrites, with their
// compact forms. Rules may not add or remove them.
var managedHeaders = map[string]bool{
	"via": true, "v": true, "route": true, "record-route": true, "max-forwards": true,
	"call-id": true, "i": true, "cseq": true, "from": true, "f": true, "to": true, "t": true,
	"contact": true, "m": true, "content-length": true, "l": true, "content-type": true, "c": true,
}

var tokenRE = regexp.MustCompile("^[A-Za-z0-9.!%*_+`'~-]+$")

func (k *checker) rules() {
	if len(k.c.Rules) == 0 {
		k.warnf(top, "rules", "no rules: every call gets 404")
	}
	names := map[string]Pos{}
	k.numbers = make([]*regexp.Regexp, len(k.c.Rules))
	for i, r := range k.c.Rules {
		path := fmt.Sprintf("rules[%d]", i)
		name := r.Name.V
		if !r.Name.Set || name == "" {
			k.errf(r.Pos, path+".name", "rule has no name")
			name = path
		} else {
			if first, dup := names[name]; dup {
				k.errf(r.Name.Pos, path+".name", "rule name %q is already used on line %d", name, first.Line)
			}
			names[name] = r.Name.Pos
		}

		var captures int
		if !r.Number.Set {
			k.errf(r.Pos, path+".match.number", "rule %q has no match.number", name)
		} else if re, err := regexp.Compile(r.Number.V); err != nil {
			k.errf(r.Number.Pos, path+".match.number", "rule %q: number %q is not a valid regular expression: %s", name, r.Number.V, reErr(err))
		} else {
			k.numbers[i] = re
			captures = re.NumSubexp()
		}
		for j, m := range r.Methods {
			if m.Set && !sipMethods[m.V] {
				k.errf(m.Pos, fmt.Sprintf("%s.match.methods[%d]", path, j), "rule %q: %q is not a SIP method", name, m.V)
			}
		}
		for _, extra := range []struct {
			key string
			v   Val[string]
		}{{"to_host", r.ToHost}, {"from_user", r.FromUser}} {
			if extra.v.Set {
				if _, err := regexp.Compile(extra.v.V); err != nil {
					k.errf(extra.v.Pos, path+".match."+extra.key, "rule %q: %s %q is not a valid regular expression: %s", name, extra.key, extra.v.V, reErr(err))
				}
			}
		}

		if r.ActionPos == (Pos{}) {
			k.errf(r.Pos, path+".action", "rule %q has no action", name)
			continue
		}
		switch {
		case r.Pool.Set && r.Target.Set:
			k.errf(r.Target.Pos, path+".action", "rule %q has both pool and target; give one", name)
		case !r.Pool.Set && !r.Target.Set:
			k.errf(r.ActionPos, path+".action", "rule %q: action needs a pool or a target", name)
		}
		if r.Pool.Set {
			k.used[r.Pool.V] = true
			if _, ok := k.pools[r.Pool.V]; !ok {
				k.errf(r.Pool.Pos, path+".action.pool", "rule %q: there is no pool %q", name, r.Pool.V)
			}
		}
		if r.Target.Set {
			k.templates(r.Target, path+".action.target", name, captures, nil)
			k.sipURI(r.Target, path+".action.target", "target", captureSamples(captures))
		}
		if r.RequestURI.Set {
			if r.Target.Set {
				k.errf(r.RequestURI.Pos, path+".action.rewrite.request_uri",
					"rule %q: rewrite.request_uri can't be used with target, which already sets the Request-URI", name)
			} else {
				vars := map[string]string{"selected_host": "192.0.2.1", "selected_port": "5060"}
				if k.templates(r.RequestURI, path+".action.rewrite.request_uri", name, captures, vars) {
					samples := captureSamples(captures)
					for v, s := range vars {
						samples[v] = s
					}
					k.sipURI(r.RequestURI, path+".action.rewrite.request_uri", "request_uri", samples)
				}
				if !strings.Contains(r.RequestURI.V, "${selected_host}") {
					k.warnf(r.RequestURI.Pos, path+".action.rewrite.request_uri",
						"rule %q: request_uri doesn't use ${selected_host}, so it names a different host from the pool member the call is sent to", name)
				}
			}
		}
		for _, h := range r.AddHeaders {
			hp := path + ".action.rewrite.add_headers." + h.Name.V
			k.headerName(h.Name, hp, name, "add")
			if h.Value.Set && strings.ContainsAny(h.Value.V, "\r\n") {
				k.errf(h.Value.Pos, hp, "rule %q: header %s's value contains a line break", name, h.Name.V)
			}
		}
		for j, h := range r.RemoveHeaders {
			k.headerName(h, fmt.Sprintf("%s.action.rewrite.remove_headers[%d]", path, j), name, "remove")
		}
	}
}

func reErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i >= 0 && strings.HasPrefix(msg, "error parsing regexp") {
		return msg[i+2:]
	}
	return msg
}

func (k *checker) headerName(h Val[string], path, rule, verb string) {
	switch {
	case !tokenRE.MatchString(h.V):
		k.errf(h.Pos, path, "rule %q: %q is not a valid header name", rule, h.V)
	case managedHeaders[strings.ToLower(h.V)]:
		k.errf(h.Pos, path, "rule %q: can't %s %s; the leaf manages it or v1 never rewrites it", rule, verb, h.V)
	}
}

var templateRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// templates checks the ${...} variables in a value: captures must exist in the
// rule's number pattern, named variables must be in vars. Reports whether the
// value is well-formed enough to test as a URI.
func (k *checker) templates(v Val[string], path, rule string, captures int, vars map[string]string) bool {
	ok := true
	rest := templateRE.ReplaceAllString(v.V, "")
	if strings.Contains(rest, "${") {
		k.errf(v.Pos, path, "rule %q: unterminated ${ in %q", rule, v.V)
		ok = false
	}
	for _, m := range templateRE.FindAllStringSubmatch(v.V, -1) {
		name := m[1]
		if n, err := strconv.Atoi(name); err == nil {
			if n > captures {
				switch captures {
				case 0:
					k.errf(v.Pos, path, "rule %q uses ${%d} but its number pattern has no capture group", rule, n)
				default:
					k.errf(v.Pos, path, "rule %q uses ${%d} but its number pattern has only %d capture group(s)", rule, n, captures)
				}
				ok = false
			}
			continue
		}
		if _, known := vars[name]; !known {
			allowed := []string{"${1}…"}
			for vname := range vars {
				allowed = append(allowed, "${"+vname+"}")
			}
			sort.Strings(allowed)
			k.errf(v.Pos, path, "rule %q: unknown variable ${%s} (allowed here: %s)", rule, name, strings.Join(allowed, ", "))
			ok = false
		}
	}
	return ok
}

func captureSamples(n int) map[string]string {
	out := map[string]string{}
	for i := 0; i <= n; i++ {
		out[strconv.Itoa(i)] = "0"
	}
	return out
}

// sipURI checks that a value is a sip: or sips: URI with a host, after
// replacing template variables with sample values.
func (k *checker) sipURI(v Val[string], path, what string, vars map[string]string) {
	s := templateRE.ReplaceAllStringFunc(v.V, func(m string) string {
		if r, ok := vars[m[2:len(m)-1]]; ok {
			return r
		}
		return "0"
	})
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "sip:") && !strings.HasPrefix(low, "sips:") {
		k.errf(v.Pos, path, "%s %q must be a sip: or sips: URI", what, v.V)
		return
	}
	var u sip.Uri
	if err := sip.ParseUri(s, &u); err != nil || u.Host == "" {
		reason := "it has no host"
		if err != nil {
			reason = err.Error()
		}
		k.errf(v.Pos, path, "%s %q is not a valid SIP URI: %s", what, v.V, reason)
	}
}

// methodSet returns the methods a rule matches; nil means every method.
func methodSet(r Rule) map[string]bool {
	if len(r.Methods) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, m := range r.Methods {
		out[m.V] = true
	}
	return out
}

// covered returns the methods of later that earlier also matches, in order,
// or nil when it covers none of them.
func covered(earlier, later map[string]bool) []string {
	var out []string
	switch {
	case earlier == nil && later == nil:
		return []string{"every method"}
	case earlier == nil:
		for m := range later {
			out = append(out, m)
		}
	case later == nil:
		for m := range earlier {
			out = append(out, m)
		}
	default:
		for m := range later {
			if earlier[m] {
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// shadowing warns when an earlier rule catches the numbers of a later one.
// For an exact number (^447700900123$) that is certain: the later rule can
// never match. Otherwise one example number of the later rule is tried
// against the earlier ones.
func (k *checker) shadowing() {
	rules := k.c.Rules
	for j := range rules {
		later := rules[j]
		if k.numbers[j] == nil {
			continue
		}
		literal, exact := literalNumber(later.Number.V)
		sample := literal
		if !exact {
			var ok bool
			if sample, ok = exampleNumber(later.Number.V); !ok {
				continue
			}
		}
		for i := 0; i < j; i++ {
			earlier := rules[i]
			if k.numbers[i] == nil || earlier.ToHost.Set || earlier.FromUser.Set {
				continue // extra terms make the earlier rule narrower than its number
			}
			if !k.numbers[i].MatchString(sample) {
				continue
			}
			methods := covered(methodSet(earlier), methodSet(later))
			if methods == nil {
				continue
			}
			scope := ""
			if methods[0] != "every method" && len(methods) < len(later.Methods) {
				scope = " for " + strings.Join(methods, ", ")
			}
			if exact {
				k.warnf(later.Number.Pos, fmt.Sprintf("rules[%d].match.number", j),
					"rule %q can never match%s: rule %q (line %d) comes first and also matches %s; move the more specific rule up",
					later.Name.V, scope, earlier.Name.V, earlier.Name.Pos.Line, literal)
			} else {
				k.warnf(later.Number.Pos, fmt.Sprintf("rules[%d].match.number", j),
					"rule %q may be shadowed%s: rule %q (line %d) comes first and also matches numbers like %s",
					later.Name.V, scope, earlier.Name.V, earlier.Name.Pos.Line, sample)
			}
			break
		}
	}
}
