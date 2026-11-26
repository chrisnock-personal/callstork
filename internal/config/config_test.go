package config

import (
	"fmt"
	"strings"
	"testing"
)

// base is a small valid config: the first %s is the rules section, the
// second is extra top-level YAML after the pools.
const base = `version: 1
defaults:
  attempt_timeout: 2s
  setup_deadline: 8s
listen:
  peer: { address: 0.0.0.0, port: 5060 }
  lan:  { address: 0.0.0.0, port: 5060 }
peers:
  - name: itsp
    allow: [198.51.100.0/24]
    match_field: request_uri_user
rules:
%s
pools:
  dids:
    strategy: least-calls
    members:
      - uri: 'sip:192.0.2.10:5060'
%s`

const okRule = `  - name: inbound-dids
    match: { methods: [INVITE], number: '^44(\d{10})$' }
    action:
      pool: dids
      rewrite: { request_uri: 'sip:0${1}@${selected_host}:${selected_port}' }`

func check(t *testing.T, yaml string) []Diag {
	t.Helper()
	cfg, diags := Parse([]byte(yaml))
	if cfg != nil {
		diags = append(diags, Check(cfg)...)
	}
	return diags
}

func lineOf(yaml, needle string) int {
	for i, l := range strings.Split(yaml, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	return -1
}

type want struct {
	sev  Severity
	msg  string // substring of the message
	line string // substring of the line it must point at ("" = don't check)
}

func expect(t *testing.T, yaml string, diags []Diag, wants ...want) {
	t.Helper()
	if len(diags) != len(wants) {
		t.Errorf("got %d problem(s), want %d:\n%s", len(diags), len(wants), dump(diags))
	}
	for _, w := range wants {
		found := false
		for _, d := range diags {
			if d.Severity == w.sev && strings.Contains(d.Msg, w.msg) {
				found = true
				if w.line != "" && d.Line != lineOf(yaml, w.line) {
					t.Errorf("%q reported on line %d, want line %d (%q)", w.msg, d.Line, lineOf(yaml, w.line), w.line)
				}
			}
		}
		if !found {
			t.Errorf("missing %s containing %q; got:\n%s", w.sev, w.msg, dump(diags))
		}
	}
}

func dump(diags []Diag) string {
	var b strings.Builder
	for _, d := range diags {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	return b.String()
}

func TestExampleConfigIsClean(t *testing.T) {
	cfg, diags, err := Load("../../config/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	diags = append(diags, Check(cfg)...)
	if len(diags) != 0 {
		t.Errorf("config/example.yaml should have no problems:\n%s", dump(diags))
	}
}

func TestBaseIsClean(t *testing.T) {
	y := fmt.Sprintf(base, okRule, "")
	expect(t, y, check(t, y))
}

// The lab-failover rule once rewrote with ${1} from ^447700900999$, which has
// no capture group: the PBX would have been sent sip:0@host.
func TestCaptureReferenceWithoutGroup(t *testing.T) {
	rules := `  - name: lab-failover
    match: { number: '^447700900999$' }
    action:
      pool: dids
      rewrite:
        request_uri: 'sip:0${1}@${selected_host}:${selected_port}'`
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y), want{Error, "uses ${1} but its number pattern has no capture group", "request_uri:"})
}

func TestCaptureReferenceBeyondGroups(t *testing.T) {
	rules := `  - name: two
    match: { number: '^44(\d{4})(\d{6})$' }
    action:
      pool: dids
      rewrite: { request_uri: 'sip:${3}@${selected_host}' }`
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y), want{Error, "uses ${3} but its number pattern has only 2 capture group(s)", "{ request_uri:"})
}

// test-line (an exact number) sat after the broad DID rule and could never match.
func TestExactRuleShadowed(t *testing.T) {
	rules := okRule + `
  - name: test-line
    match: { number: '^447700900123$' }
    action: { target: 'sip:127.0.0.1:5072' }`
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y), want{Warning, `rule "test-line" can never match: rule "inbound-dids"`, "'^447700900123$'"})
}

// The fix: the exact rule first, the broad one after. No warning.
func TestExactBeforeBroadIsFine(t *testing.T) {
	rules := `  - name: test-line
    match: { number: '^447700900123$' }
    action: { target: 'sip:127.0.0.1:5072' }
` + okRule
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y))
}

func TestBroadRuleMayBeShadowed(t *testing.T) {
	rules := `  - name: everything-44
    match: { number: '^44' }
    action: { pool: dids, rewrite: { request_uri: 'sip:${selected_host}' } }
  - name: mobiles
    match: { number: '^447(\d+)$' }
    action: { pool: dids, rewrite: { request_uri: 'sip:0${1}@${selected_host}' } }`
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y), want{Warning, `rule "mobiles" may be shadowed: rule "everything-44"`, "'^447("})
}

// An earlier rule for INVITE only doesn't shadow a later rule's OPTIONS.
func TestShadowingRespectsMethods(t *testing.T) {
	rules := okRule + `
  - name: options-probe
    match: { methods: [OPTIONS], number: '^447700900123$' }
    action: { target: 'sip:127.0.0.1:5072' }`
	y := fmt.Sprintf(base, rules, "")
	expect(t, y, check(t, y))
}

func TestRuleProblems(t *testing.T) {
	cases := []struct {
		name  string
		rules string
		extra string
		wants []want
	}{
		{"unknown top-level key", okRule, "rulez: []\n",
			[]want{{Error, `unknown key "rulez"`, "rulez:"}}},
		{"invalid regex", `  - name: bad
    match: { number: '^44(' }
    action: { pool: dids }`, "",
			[]want{{Error, "not a valid regular expression: missing closing )", "'^44('"}}},
		{"missing pool", `  - name: r
    match: { number: '^44' }
    action: { pool: nope }`, "",
			[]want{{Error, `there is no pool "nope"`, "pool: nope"}, {Warning, `pool "dids" is not used`, "dids:"}}},
		{"pool and target", `  - name: r
    match: { number: '^44' }
    action: { pool: dids, target: 'sip:192.0.2.9' }`, "",
			[]want{{Error, "has both pool and target", ""}}},
		{"no action", `  - name: r
    match: { number: '^44' }`, "",
			[]want{{Error, `rule "r" has no action`, "- name: r"}, {Warning, "is not used", ""}}},
		{"managed header", `  - name: r
    match: { number: '^44' }
    action:
      pool: dids
      rewrite:
        request_uri: 'sip:x@${selected_host}'
        add_headers: { Via: 'SIP/2.0/UDP 192.0.2.1' }`, "",
			[]want{{Error, "can't add Via", "add_headers"}}},
		{"bad header name", `  - name: r
    match: { number: '^44' }
    action: { pool: dids, rewrite: { remove_headers: ['P Asserted'] } }`, "",
			[]want{{Error, `"P Asserted" is not a valid header name`, ""}}},
		{"unknown template variable", `  - name: r
    match: { number: '^44' }
    action: { pool: dids, rewrite: { request_uri: 'sip:${foo}@${selected_host}' } }`, "",
			[]want{{Error, "unknown variable ${foo}", ""}}},
		{"selected_host with target", `  - name: r
    match: { number: '^44' }
    action: { target: 'sip:${selected_host}' }`, "",
			[]want{{Error, "unknown variable ${selected_host}", ""}, {Warning, "is not used", ""}}},
		{"request_uri ignores the member", `  - name: r
    match: { number: '^44' }
    action: { pool: dids, rewrite: { request_uri: 'sip:x@192.0.2.50' } }`, "",
			[]want{{Warning, "doesn't use ${selected_host}", ""}}},
		{"target not a SIP URI", `  - name: r
    match: { number: '^44' }
    action: { target: 'http://192.0.2.9/' }`, "",
			[]want{{Error, "must be a sip: or sips: URI", ""}, {Warning, "is not used", ""}}},
		{"bad method", `  - name: r
    match: { methods: [INVITE, invite], number: '^44' }
    action: { pool: dids }`, "",
			[]want{{Error, `"invite" is not a SIP method`, ""}}},
		{"duplicate rule names", okRule + "\n" + okRule, "",
			[]want{{Error, `rule name "inbound-dids" is already used`, ""}, {Warning, "may be shadowed", ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := fmt.Sprintf(base, c.rules, c.extra)
			expect(t, y, check(t, y), c.wants...)
		})
	}
}

func TestStructureProblems(t *testing.T) {
	ok := fmt.Sprintf(base, okRule, "")
	const peerEnd = "    match_field: request_uri_user\n"
	cases := []struct {
		name     string
		from, to string
		wants    []want
	}{
		{"duration without unit", "attempt_timeout: 2s", "attempt_timeout: 2",
			[]want{{Error, "must be a positive duration", "attempt_timeout"}}},
		{"attempt not shorter than deadline", "attempt_timeout: 2s", "attempt_timeout: 9s",
			[]want{{Error, "must be shorter than setup_deadline", "attempt_timeout"}}},
		{"wrong type", "  setup_deadline: 8s\n", "  setup_deadline: 8s\n  strip_plus: yes please\n",
			[]want{{Error, "must be true or false", "strip_plus"}}},
		{"version 2", "version: 1", "version: 2",
			[]want{{Error, "version 2 is not supported", "version"}}},
		{"bad listen address", "peer: { address: 0.0.0.0", "peer: { address: localhost",
			[]want{{Error, `"localhost" is not an IP address`, "peer:"}}},
		{"bad port", "lan:  { address: 0.0.0.0, port: 5060 }", "lan:  { address: 0.0.0.0, port: 70000 }",
			[]want{{Error, "port must be 1 to 65535", "lan:"}}},
		{"bad CIDR", "allow: [198.51.100.0/24]", "allow: [198.51.100.0/33]",
			[]want{{Error, "is not an IP address or CIDR range", "allow"}}},
		{"bad match_field", "match_field: request_uri_user", "match_field: ruri",
			[]want{{Error, "match_field must be request_uri_user or to_user", "match_field"}}},
		{"unknown strategy", "strategy: least-calls", "strategy: random",
			[]want{{Error, `unknown strategy "random"`, "strategy"}}},
		{"weighted without weights", "strategy: least-calls", "strategy: weighted",
			[]want{{Error, "uses weighted but this member has no weight", "- uri"}}},
		{"weight ignored", "- uri: 'sip:192.0.2.10:5060'", "- uri: 'sip:192.0.2.10:5060'\n        weight: 10",
			[]want{{Warning, `weight is ignored by strategy "least-calls"`, "weight"}}},
		{"member not a SIP URI", "- uri: 'sip:192.0.2.10:5060'", "- uri: '192.0.2.10:5060'",
			[]want{{Error, `member uri "192.0.2.10:5060" must be a sip: or sips: URI`, "- uri"}}},
		{"overlapping peers", peerEnd,
			peerEnd + "  - name: pbx\n    allow: [198.51.100.7]\n    match_field: to_user\n",
			[]want{{Warning, `198.51.100.7/32 overlaps 198.51.100.0/24 of peer "itsp"`, "198.51.100.7"}}},
		{"register not in v1", peerEnd,
			peerEnd + "    register: { registrar: 'sip:sip.example', username: u, password_env: P }\n",
			[]want{{Warning, "register is not implemented in v1", "register"}}},
		{"register without password_env", peerEnd,
			peerEnd + "    register: { registrar: 'sip:sip.example', username: u }\n",
			[]want{{Warning, "not implemented in v1", ""}, {Error, "register needs password_env", ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(ok, c.from) {
				t.Fatalf("base has no %q", c.from)
			}
			y := strings.Replace(ok, c.from, c.to, 1)
			expect(t, y, check(t, y), c.wants...)
		})
	}
}

func TestInvalidYAML(t *testing.T) {
	cfg, diags := Parse([]byte("version: 1\nrules:\n  - name: [unclosed\n"))
	if cfg != nil || len(diags) != 1 || diags[0].Severity != Error || !strings.Contains(diags[0].Msg, "not valid YAML") {
		t.Fatalf("want one 'not valid YAML' error, got cfg=%v diags=%s", cfg != nil, dump(diags))
	}
	if diags[0].Line < 3 {
		t.Errorf("error reported on line %d, want 3 or later", diags[0].Line)
	}
}

func TestLiteralNumber(t *testing.T) {
	for pattern, want := range map[string]string{
		`^447700900123$`:   "447700900123",
		`^44(7700900999)$`: "447700900999",
	} {
		if got, ok := literalNumber(pattern); !ok || got != want {
			t.Errorf("literalNumber(%q) = %q, %v; want %q", pattern, got, ok, want)
		}
	}
	for _, pattern := range []string{`^44(\d{10})$`, `447700900123`, `^4477|^4478$`, `(?i)^abc$`} {
		if got, ok := literalNumber(pattern); ok {
			t.Errorf("literalNumber(%q) = %q, want not literal", pattern, got)
		}
	}
}

func TestExampleNumber(t *testing.T) {
	for pattern, want := range map[string]string{
		`^44(\d{10})$`: "440000000000",
		`^447(\d+)$`:   "4470",
		`^(\+)?44`:     "44",
		`^[1-9]\d{2}$`: "100",
	} {
		got, ok := exampleNumber(pattern)
		if !ok || got != want {
			t.Errorf("exampleNumber(%q) = %q, %v; want %q", pattern, got, ok, want)
		}
	}
}
