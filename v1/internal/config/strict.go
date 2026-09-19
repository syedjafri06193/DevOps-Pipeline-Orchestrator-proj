package config

import (
	"fmt"
	"sort"
	"strings"
)

// decoder walks a Node tree and records where it is, so that an error deep in
// the file says which app and environment it came from.
//
// The central rule (section 13):
//
//	"Unknown keys are errors, not warnings — a typo'd `failure_threshhold`
//	silently falling back to a default is exactly the bug that causes a
//	spurious 3am rollback."
//
// The mechanism is `mapping.done()`: a caller lists the keys it consumed, and
// anything left over is an error. It is deliberately the *last* call in every
// decode function, so forgetting it is visible in review as a missing line
// rather than as a silently permissive schema.
type decoder struct {
	file string
	path []string
}

func (d *decoder) at(seg string) *decoder {
	c := &decoder{file: d.file, path: append(append([]string{}, d.path...), seg)}
	return c
}

func (d *decoder) where() string { return strings.Join(d.path, ".") }

func (d *decoder) errf(n *Node, format string, args ...any) *Error {
	e := errorAt(n, format, args...)
	e.File = d.file
	e.Path = d.where()
	return e
}

func (d *decoder) errAtKey(m *mapping, key string, format string, args ...any) *Error {
	e := &Error{File: d.file, Msg: fmt.Sprintf(format, args...), Path: d.where()}
	if pos, ok := m.node.KeyPos[key]; ok {
		e.Line, e.Col = pos.Line, pos.Col
	} else if m.node != nil {
		e.Line, e.Col = m.node.Line, m.node.Col
	}
	return e
}

// mapping wraps a Node of KindMapping and tracks which keys have been read.
type mapping struct {
	d    *decoder
	node *Node
	seen map[string]bool
}

func (d *decoder) mapping(n *Node) (*mapping, error) {
	if n == nil || n.IsNull() {
		return &mapping{d: d, node: &Node{Kind: KindMapping, Values: map[string]*Node{}, KeyPos: map[string]Pos{}}, seen: map[string]bool{}}, nil
	}
	if n.Kind != KindMapping {
		return nil, d.errf(n, "expected a block of settings here, got %s", describe(n))
	}
	return &mapping{d: d, node: n, seen: map[string]bool{}}, nil
}

func describe(n *Node) string {
	switch n.Kind {
	case KindSequence:
		return "a list"
	case KindScalar:
		if n.Value == "" {
			return "nothing"
		}
		return fmt.Sprintf("the value %q", n.Value)
	default:
		return "a block"
	}
}

func (m *mapping) has(key string) bool {
	_, ok := m.node.Values[key]
	return ok
}

// get marks a key as consumed and returns it, or nil when absent.
func (m *mapping) get(key string) *Node {
	m.seen[key] = true
	return m.node.Values[key]
}

// req is get for a key that must be present.
func (m *mapping) req(key string) (*Node, error) {
	m.seen[key] = true
	n, ok := m.node.Values[key]
	if !ok || n.IsNull() {
		return nil, m.d.errAtKey(m, key, "missing required key %q", key)
	}
	return n, nil
}

func (m *mapping) str(key, def string) (string, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return def, nil
	}
	s, err := n.String()
	if err != nil {
		return "", m.wrap(key, err)
	}
	return s, nil
}

func (m *mapping) reqStr(key string) (string, error) {
	n, err := m.req(key)
	if err != nil {
		return "", err
	}
	s, err := n.String()
	if err != nil {
		return "", m.wrap(key, err)
	}
	return s, nil
}

func (m *mapping) boolean(key string, def bool) (bool, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return def, nil
	}
	v, err := n.Bool()
	if err != nil {
		return false, m.wrap(key, err)
	}
	return v, nil
}

func (m *mapping) integer(key string, def int) (int, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return def, nil
	}
	v, err := n.Int()
	if err != nil {
		return 0, m.wrap(key, err)
	}
	return v, nil
}

func (m *mapping) float(key string) (*float64, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return nil, nil
	}
	v, err := n.Float()
	if err != nil {
		return nil, m.wrap(key, err)
	}
	return &v, nil
}

func (m *mapping) floatDef(key string, def float64) (float64, error) {
	p, err := m.float(key)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return def, nil
	}
	return *p, nil
}

func (m *mapping) strings(key string) ([]string, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return nil, nil
	}
	if n.Kind != KindSequence {
		return nil, m.d.errf(n, "%q must be a list", key)
	}
	out := make([]string, 0, len(n.Items))
	for _, item := range n.Items {
		s, err := item.String()
		if err != nil {
			return nil, m.d.errf(item, "%q must be a list of strings", key)
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *mapping) wrap(key string, err error) error {
	if e, ok := err.(*Error); ok {
		e.File = m.d.file
		e.Path = m.d.where() + "." + key
		return e
	}
	return err
}

// done reports every key the caller did not consume.
//
// This is where a typo becomes a refusal. The suggestion comes from edit
// distance against the keys that *were* expected, which is what makes the
// error act on rather than merely report.
func (m *mapping) done() error {
	var unknown []string
	for _, k := range m.node.Keys {
		if !m.seen[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	key := unknown[0]

	known := make([]string, 0, len(m.seen))
	for k := range m.seen {
		known = append(known, k)
	}
	sort.Strings(known)

	err := m.d.errAtKey(m, key, "unknown key %q", key)
	if best, ok := closest(key, known); ok {
		err.Hint = fmt.Sprintf("did you mean %q?", best)
	} else if len(known) > 0 {
		err.Hint = "valid keys here: " + strings.Join(known, ", ")
	}
	if len(unknown) > 1 {
		err.Hint += fmt.Sprintf("\n  (%d other unknown keys here: %s)",
			len(unknown)-1, strings.Join(unknown[1:], ", "))
	}
	return err
}

// closest finds the nearest known key within a small edit distance.
//
// The threshold is deliberately tight. Suggesting "strategy" for "verify"
// is worse than suggesting nothing, because it sends the reader looking in
// the wrong place.
func closest(got string, known []string) (string, bool) {
	best, bestDist := "", 1<<30
	limit := len(got)/3 + 1
	if limit < 2 {
		limit = 2
	}
	for _, k := range known {
		d := levenshtein(got, k)
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	if best != "" && bestDist <= limit {
		return best, true
	}
	return "", false
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// oneOf validates an enum and lists the alternatives on failure, because
// "invalid strategy" without the list means opening the source.
func (m *mapping) oneOf(key, def string, allowed ...string) (string, error) {
	v, err := m.str(key, def)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	e := m.d.errAtKey(m, key, "%q is not a valid %s", v, key)
	if best, ok := closest(v, allowed); ok {
		e.Hint = fmt.Sprintf("did you mean %q? valid values: %s", best, strings.Join(allowed, ", "))
	} else {
		e.Hint = "valid values: " + strings.Join(allowed, ", ")
	}
	return "", e
}
