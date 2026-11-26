// Package config loads the CallStork YAML config (see config/example.yaml)
// and checks it. Loading keeps the line and column of every value, so every
// problem can be reported where it is in the file.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pos is a 1-based line and column in the config file.
type Pos struct {
	Line int
	Col  int
}

// Val is a scalar from the file: its value, whether it was present, and where.
type Val[T any] struct {
	V   T
	Set bool
	Pos Pos
}

// Severity of a Diag.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Diag is one problem found while loading or checking a config.
type Diag struct {
	Severity Severity `json:"severity"`
	Line     int      `json:"line"`
	Col      int      `json:"column"`
	Path     string   `json:"path"`
	Msg      string   `json:"message"`
}

func (d Diag) String() string {
	return fmt.Sprintf("%d:%d: %s: %s", d.Line, d.Col, d.Severity, d.Msg)
}

// Config is the whole file.
type Config struct {
	Version  Val[int]
	Defaults Defaults
	Listen   Listen
	Peers    []Peer
	Rules    []Rule
	Pools    []Pool // in file order
	PoolsPos Pos
}

type Defaults struct {
	StripPlus        Val[bool]
	StripLeadingZero Val[bool]
	MaxForwards      Val[int]
	AttemptTimeout   Val[time.Duration]
	SetupDeadline    Val[time.Duration]
	RetryAfter       Val[time.Duration]
}

type Listen struct {
	Set  bool
	Pos  Pos
	Peer Socket
	LAN  Socket
}

type Socket struct {
	Set     bool
	Pos     Pos
	Address Val[string]
	Port    Val[int]
}

type Peer struct {
	Pos        Pos
	Name       Val[string]
	Allow      []Val[string]
	MatchField Val[string]
	Register   *Register
}

type Register struct {
	Pos         Pos
	Registrar   Val[string]
	Username    Val[string]
	PasswordEnv Val[string]
	Expires     Val[int]
}

type Rule struct {
	Pos      Pos
	Name     Val[string]
	Methods  []Val[string]
	Number   Val[string]
	ToHost   Val[string]
	FromUser Val[string]

	ActionPos     Pos
	Pool          Val[string]
	Target        Val[string]
	RequestURI    Val[string]
	AddHeaders    []Header
	RemoveHeaders []Val[string]
}

type Header struct {
	Name  Val[string]
	Value Val[string]
}

type Pool struct {
	Pos            Pos
	Name           Val[string]
	Strategy       Val[string]
	FailoverOn     []Val[string]
	AttemptTimeout Val[time.Duration]
	SetupDeadline  Val[time.Duration]
	RetryAfter     Val[time.Duration]
	Health         Health
	Members        []Member
}

type Health struct {
	Set             bool
	Pos             Pos
	Passive         Val[bool]
	OptionsInterval Val[time.Duration]
	OptionsURI      Val[string]
	Accept          []Val[int]
	CoolDown        Val[time.Duration]
}

type Member struct {
	Pos      Pos
	URI      Val[string]
	Weight   Val[int]
	Priority Val[int]
}

// Load reads and parses a config file. A nil Config means the file could not
// be parsed at all; the diagnostics say why.
func Load(path string) (*Config, []Diag, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, diags := Parse(data)
	return cfg, diags, nil
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// Parse parses config YAML from memory.
func Parse(data []byte) (*Config, []Diag) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		line := 1
		if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
			// yaml.v3 counts lines from 0 in its error messages.
			n, _ := strconv.Atoi(m[1])
			line = n + 1
		}
		msg := strings.TrimPrefix(err.Error(), "yaml: ")
		return nil, []Diag{{Severity: Error, Line: line, Col: 1, Msg: "not valid YAML: " + msg}}
	}
	p := &parser{}
	if len(doc.Content) == 0 {
		p.errf(Pos{1, 1}, "", "the file is empty")
		return nil, p.diags
	}
	cfg := p.config(doc.Content[0])
	return cfg, p.diags
}

type parser struct {
	diags []Diag
}

func posOf(n *yaml.Node) Pos { return Pos{n.Line, n.Column} }

func (p *parser) add(sev Severity, pos Pos, path, format string, args ...any) {
	p.diags = append(p.diags, Diag{Severity: sev, Line: pos.Line, Col: pos.Col, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (p *parser) errf(pos Pos, path, format string, args ...any) {
	p.add(Error, pos, path, format, args...)
}

// mapping walks a mapping node, calling the handler for each known key and
// reporting unknown and repeated keys.
func (p *parser) mapping(n *yaml.Node, path string, handlers map[string]func(*yaml.Node, string)) bool {
	if n.Kind != yaml.MappingNode {
		p.errf(posOf(n), path, "%s must be a mapping (key: value)", describe(path))
		return false
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		sub := join(path, k.Value)
		if seen[k.Value] {
			p.errf(posOf(k), sub, "%q appears more than once", k.Value)
			continue
		}
		seen[k.Value] = true
		h, ok := handlers[k.Value]
		if !ok {
			keys := make([]string, 0, len(handlers))
			for key := range handlers {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			p.errf(posOf(k), sub, "unknown key %q in %s (expected one of: %s)", k.Value, describe(path), strings.Join(keys, ", "))
			continue
		}
		h(v, sub)
	}
	return true
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describe(path string) string {
	if path == "" {
		return "the top level"
	}
	return path
}

func (p *parser) str(n *yaml.Node, path string) Val[string] {
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		p.errf(posOf(n), path, "%s must be a single value", path)
		return Val[string]{Pos: posOf(n)}
	}
	return Val[string]{V: n.Value, Set: true, Pos: posOf(n)}
}

func (p *parser) integer(n *yaml.Node, path string) Val[int] {
	v, err := strconv.Atoi(n.Value)
	if n.Kind != yaml.ScalarNode || n.Tag != "!!int" || err != nil {
		p.errf(posOf(n), path, "%s must be a whole number, not %q", path, n.Value)
		return Val[int]{Pos: posOf(n)}
	}
	return Val[int]{V: v, Set: true, Pos: posOf(n)}
}

func (p *parser) boolean(n *yaml.Node, path string) Val[bool] {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		p.errf(posOf(n), path, "%s must be true or false, not %q", path, n.Value)
		return Val[bool]{Pos: posOf(n)}
	}
	return Val[bool]{V: n.Value == "true", Set: true, Pos: posOf(n)}
}

func (p *parser) duration(n *yaml.Node, path string) Val[time.Duration] {
	d, err := time.ParseDuration(n.Value)
	if n.Kind != yaml.ScalarNode || err != nil || d <= 0 {
		p.errf(posOf(n), path, "%s must be a positive duration such as 2s or 500ms, not %q", path, n.Value)
		return Val[time.Duration]{Pos: posOf(n)}
	}
	return Val[time.Duration]{V: d, Set: true, Pos: posOf(n)}
}

func (p *parser) list(n *yaml.Node, path string, item func(*yaml.Node, string)) {
	if n.Kind != yaml.SequenceNode {
		p.errf(posOf(n), path, "%s must be a list", path)
		return
	}
	for i, c := range n.Content {
		item(c, fmt.Sprintf("%s[%d]", path, i))
	}
}

func (p *parser) strList(n *yaml.Node, path string) []Val[string] {
	var out []Val[string]
	p.list(n, path, func(c *yaml.Node, sub string) { out = append(out, p.str(c, sub)) })
	return out
}

func (p *parser) config(n *yaml.Node) *Config {
	c := &Config{}
	p.mapping(n, "", map[string]func(*yaml.Node, string){
		"version":  func(v *yaml.Node, path string) { c.Version = p.integer(v, path) },
		"defaults": func(v *yaml.Node, path string) { p.defaults(v, path, &c.Defaults) },
		"listen":   func(v *yaml.Node, path string) { p.listen(v, path, &c.Listen) },
		"peers": func(v *yaml.Node, path string) {
			p.list(v, path, func(item *yaml.Node, sub string) { c.Peers = append(c.Peers, p.peer(item, sub)) })
		},
		"rules": func(v *yaml.Node, path string) {
			p.list(v, path, func(item *yaml.Node, sub string) { c.Rules = append(c.Rules, p.rule(item, sub)) })
		},
		"pools": func(v *yaml.Node, path string) {
			c.PoolsPos = posOf(v)
			if v.Kind != yaml.MappingNode {
				p.errf(posOf(v), path, "pools must be a mapping of pool name to pool")
				return
			}
			for i := 0; i+1 < len(v.Content); i += 2 {
				k := v.Content[i]
				pool := p.pool(v.Content[i+1], join(path, k.Value))
				pool.Name = Val[string]{V: k.Value, Set: true, Pos: posOf(k)}
				c.Pools = append(c.Pools, pool)
			}
		},
	})
	return c
}

func (p *parser) defaults(n *yaml.Node, path string, d *Defaults) {
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"strip_plus":         func(v *yaml.Node, s string) { d.StripPlus = p.boolean(v, s) },
		"strip_leading_zero": func(v *yaml.Node, s string) { d.StripLeadingZero = p.boolean(v, s) },
		"max_forwards":       func(v *yaml.Node, s string) { d.MaxForwards = p.integer(v, s) },
		"attempt_timeout":    func(v *yaml.Node, s string) { d.AttemptTimeout = p.duration(v, s) },
		"setup_deadline":     func(v *yaml.Node, s string) { d.SetupDeadline = p.duration(v, s) },
		"retry_after":        func(v *yaml.Node, s string) { d.RetryAfter = p.duration(v, s) },
	})
}

func (p *parser) listen(n *yaml.Node, path string, l *Listen) {
	l.Set, l.Pos = true, posOf(n)
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"peer": func(v *yaml.Node, s string) { l.Peer = p.socket(v, s) },
		"lan":  func(v *yaml.Node, s string) { l.LAN = p.socket(v, s) },
	})
}

func (p *parser) socket(n *yaml.Node, path string) Socket {
	s := Socket{Set: true, Pos: posOf(n)}
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"address": func(v *yaml.Node, sub string) { s.Address = p.str(v, sub) },
		"port":    func(v *yaml.Node, sub string) { s.Port = p.integer(v, sub) },
	})
	return s
}

func (p *parser) peer(n *yaml.Node, path string) Peer {
	pe := Peer{Pos: posOf(n)}
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"name":        func(v *yaml.Node, s string) { pe.Name = p.str(v, s) },
		"allow":       func(v *yaml.Node, s string) { pe.Allow = p.strList(v, s) },
		"match_field": func(v *yaml.Node, s string) { pe.MatchField = p.str(v, s) },
		"register": func(v *yaml.Node, s string) {
			r := &Register{Pos: posOf(v)}
			p.mapping(v, s, map[string]func(*yaml.Node, string){
				"registrar":    func(v *yaml.Node, s string) { r.Registrar = p.str(v, s) },
				"username":     func(v *yaml.Node, s string) { r.Username = p.str(v, s) },
				"password_env": func(v *yaml.Node, s string) { r.PasswordEnv = p.str(v, s) },
				"expires":      func(v *yaml.Node, s string) { r.Expires = p.integer(v, s) },
			})
			pe.Register = r
		},
	})
	return pe
}

func (p *parser) rule(n *yaml.Node, path string) Rule {
	r := Rule{Pos: posOf(n)}
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"name": func(v *yaml.Node, s string) { r.Name = p.str(v, s) },
		"match": func(v *yaml.Node, s string) {
			p.mapping(v, s, map[string]func(*yaml.Node, string){
				"methods":   func(v *yaml.Node, s string) { r.Methods = p.strList(v, s) },
				"number":    func(v *yaml.Node, s string) { r.Number = p.str(v, s) },
				"to_host":   func(v *yaml.Node, s string) { r.ToHost = p.str(v, s) },
				"from_user": func(v *yaml.Node, s string) { r.FromUser = p.str(v, s) },
			})
		},
		"action": func(v *yaml.Node, s string) {
			r.ActionPos = posOf(v)
			p.mapping(v, s, map[string]func(*yaml.Node, string){
				"pool":   func(v *yaml.Node, s string) { r.Pool = p.str(v, s) },
				"target": func(v *yaml.Node, s string) { r.Target = p.str(v, s) },
				"rewrite": func(v *yaml.Node, s string) {
					p.mapping(v, s, map[string]func(*yaml.Node, string){
						"request_uri": func(v *yaml.Node, s string) { r.RequestURI = p.str(v, s) },
						"add_headers": func(v *yaml.Node, s string) {
							if v.Kind != yaml.MappingNode {
								p.errf(posOf(v), s, "add_headers must be a mapping of header name to value")
								return
							}
							for i := 0; i+1 < len(v.Content); i += 2 {
								k := v.Content[i]
								r.AddHeaders = append(r.AddHeaders, Header{
									Name:  Val[string]{V: k.Value, Set: true, Pos: posOf(k)},
									Value: p.str(v.Content[i+1], join(s, k.Value)),
								})
							}
						},
						"remove_headers": func(v *yaml.Node, s string) { r.RemoveHeaders = p.strList(v, s) },
					})
				},
			})
		},
	})
	return r
}

func (p *parser) pool(n *yaml.Node, path string) Pool {
	po := Pool{Pos: posOf(n)}
	p.mapping(n, path, map[string]func(*yaml.Node, string){
		"strategy":        func(v *yaml.Node, s string) { po.Strategy = p.str(v, s) },
		"failover_on":     func(v *yaml.Node, s string) { po.FailoverOn = p.strList(v, s) },
		"attempt_timeout": func(v *yaml.Node, s string) { po.AttemptTimeout = p.duration(v, s) },
		"setup_deadline":  func(v *yaml.Node, s string) { po.SetupDeadline = p.duration(v, s) },
		"retry_after":     func(v *yaml.Node, s string) { po.RetryAfter = p.duration(v, s) },
		"health": func(v *yaml.Node, s string) {
			h := &po.Health
			h.Set, h.Pos = true, posOf(v)
			p.mapping(v, s, map[string]func(*yaml.Node, string){
				"passive":          func(v *yaml.Node, s string) { h.Passive = p.boolean(v, s) },
				"options_interval": func(v *yaml.Node, s string) { h.OptionsInterval = p.duration(v, s) },
				"options_uri":      func(v *yaml.Node, s string) { h.OptionsURI = p.str(v, s) },
				"accept": func(v *yaml.Node, s string) {
					p.list(v, s, func(c *yaml.Node, sub string) { h.Accept = append(h.Accept, p.integer(c, sub)) })
				},
				"cool_down": func(v *yaml.Node, s string) { h.CoolDown = p.duration(v, s) },
			})
		},
		"members": func(v *yaml.Node, s string) {
			p.list(v, s, func(c *yaml.Node, sub string) {
				m := Member{Pos: posOf(c)}
				p.mapping(c, sub, map[string]func(*yaml.Node, string){
					"uri":      func(v *yaml.Node, s string) { m.URI = p.str(v, s) },
					"weight":   func(v *yaml.Node, s string) { m.Weight = p.integer(v, s) },
					"priority": func(v *yaml.Node, s string) { m.Priority = p.integer(v, s) },
				})
				po.Members = append(po.Members, m)
			})
		},
	})
	return po
}
