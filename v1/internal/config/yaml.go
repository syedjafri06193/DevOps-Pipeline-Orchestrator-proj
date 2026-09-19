// Package config loads and validates orch.yaml (design.md section 13).
//
// The design document is unusually specific about what this layer has to do:
//
//	"Validate the config exhaustively at load time. Unknown keys are errors,
//	not warnings — a typo'd `failure_threshhold` silently falling back to a
//	default is exactly the bug that causes a spurious 3am rollback."
//
// and section 16's acceptance criterion for M0:
//
//	"Done when: a malformed config produces a message a stranger could act on."
//
// Both of those are about error quality, which is why the parser here is
// hand-written rather than pulled in. It is not a general YAML implementation
// and does not try to be — it covers block mappings, block sequences, flow
// mappings and sequences, block scalars, comments and quoting, which is the
// whole of what section 13's schema uses. Anchors, aliases, multi-document
// streams, tags and merge keys are rejected with a message saying so.
//
// What a hand-written parser buys, and a general one would not: every node
// carries the line and column it came from, so an unknown key reports
//
//	orch.yaml:42:9: unknown key "failure_threshhold" in apps.web.environments.prod.verify
//	  did you mean "failure_threshold"?
//
// rather than silently defaulting. That error is the feature.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind of a parsed node.
type Kind int

const (
	KindScalar Kind = iota
	KindMapping
	KindSequence
)

// Node is one parsed YAML value with its source position.
//
// Position is carried on every node, not just on errors, because the decoder
// reports the position of the *key* it could not handle, which it only has if
// every node kept one.
type Node struct {
	Kind Kind
	Line int
	Col  int

	// Scalar
	Value string
	// Quoted records that the scalar was written in quotes, so that "true"
	// and true can be told apart when decoding into a string.
	Quoted bool

	// Mapping. Keys are kept in file order so that error messages and
	// re-serialisation follow the file rather than Go's map iteration.
	Keys   []string
	Values map[string]*Node
	// KeyPos is where each key appeared, for "unknown key" errors.
	KeyPos map[string]Pos

	// Sequence
	Items []*Node
}

// Pos is a file position. Columns are 1-based, as every editor displays them.
type Pos struct {
	Line int
	Col  int
}

// Error is a config error carrying enough to act on: where, what, and often
// what to do instead.
type Error struct {
	File   string
	Line   int
	Col    int
	Msg    string
	Hint   string
	Path   string
	nested error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		b.WriteByte(':')
	}
	if e.Line > 0 {
		fmt.Fprintf(&b, "%d:%d: ", e.Line, e.Col)
	}
	b.WriteString(e.Msg)
	if e.Path != "" {
		fmt.Fprintf(&b, " (at %s)", e.Path)
	}
	if e.Hint != "" {
		b.WriteString("\n  ")
		b.WriteString(e.Hint)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.nested }

func errorAt(n *Node, format string, args ...any) *Error {
	e := &Error{Msg: fmt.Sprintf(format, args...)}
	if n != nil {
		e.Line, e.Col = n.Line, n.Col
	}
	return e
}

// ---------------------------------------------------------------- scanning

type line struct {
	num    int
	indent int
	text   string // with indentation stripped and comments removed
	raw    string
}

// Parse turns YAML source into a Node tree.
//
// Errors name the construct that was rejected rather than the token, because
// "anchors are not supported" is actionable and "unexpected '&'" is not.
func Parse(src string) (*Node, error) {
	lines, err := scan(src)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &Node{Kind: KindMapping, Values: map[string]*Node{}, KeyPos: map[string]Pos{}, Line: 1, Col: 1}, nil
	}
	p := &parser{lines: lines}
	n, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		return nil, &Error{
			Line: p.lines[p.i].num, Col: p.lines[p.i].indent + 1,
			Msg:  "unexpected content after the end of the document",
			Hint: "check the indentation of this line against the block it belongs to",
		}
	}
	return n, nil
}

// scan strips comments and blank lines, records indentation, and handles
// block scalars by folding them into a single synthetic line.
func scan(src string) ([]line, error) {
	raw := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var out []line

	for i := 0; i < len(raw); i++ {
		text := raw[i]
		if strings.HasPrefix(strings.TrimSpace(text), "---") || strings.HasPrefix(strings.TrimSpace(text), "...") {
			if strings.TrimSpace(text) == "---" && len(out) == 0 {
				continue // a leading document marker is harmless
			}
			return nil, &Error{
				Line: i + 1, Col: 1,
				Msg:  "multi-document YAML is not supported",
				Hint: "put one configuration per file",
			}
		}
		if strings.ContainsRune(text, '\t') && strings.TrimSpace(text) != "" {
			// Tabs in YAML indentation are invalid and the resulting parse
			// errors are famously confusing, so reject them by name.
			if lead := text[:len(text)-len(strings.TrimLeft(text, " \t"))]; strings.ContainsRune(lead, '\t') {
				return nil, &Error{
					Line: i + 1, Col: strings.IndexRune(text, '\t') + 1,
					Msg:  "tab character in indentation",
					Hint: "YAML indentation must be spaces",
				}
			}
		}

		stripped := stripComment(text)
		if strings.TrimSpace(stripped) == "" {
			continue
		}
		indent := len(stripped) - len(strings.TrimLeft(stripped, " "))
		body := strings.TrimRight(strings.TrimLeft(stripped, " "), " ")

		if err := rejectUnsupported(body, i+1); err != nil {
			return nil, err
		}

		// Block scalar: "key: |" or "key: >" swallows the indented lines
		// beneath it verbatim.
		if style, ok := blockScalarStyle(body); ok {
			key := strings.TrimSpace(body[:strings.LastIndex(body, ":")])
			var buf []string
			j := i + 1
			for ; j < len(raw); j++ {
				cand := raw[j]
				if strings.TrimSpace(cand) == "" {
					buf = append(buf, "")
					continue
				}
				candIndent := len(cand) - len(strings.TrimLeft(cand, " "))
				if candIndent <= indent {
					break
				}
				buf = append(buf, cand)
			}
			joined := dedent(buf)
			var value string
			if style == '>' {
				value = strings.Join(strings.Fields(strings.Join(joined, " ")), " ")
			} else {
				value = strings.TrimRight(strings.Join(joined, "\n"), "\n")
			}
			out = append(out, line{
				num:    i + 1,
				indent: indent,
				text:   key + ": " + quoteScalar(value),
				raw:    text,
			})
			i = j - 1
			continue
		}

		out = append(out, line{num: i + 1, indent: indent, text: body, raw: text})
	}
	return out, nil
}

func rejectUnsupported(body string, num int) error {
	switch {
	case strings.HasPrefix(body, "&"), strings.Contains(body, ": &"):
		return &Error{Line: num, Col: 1,
			Msg:  "YAML anchors are not supported",
			Hint: "write the value out in full; this config is meant to be read by people on call"}
	case strings.HasPrefix(body, "*"), strings.Contains(body, ": *"):
		return &Error{Line: num, Col: 1,
			Msg:  "YAML aliases are not supported",
			Hint: "write the value out in full"}
	case strings.HasPrefix(body, "<<:"):
		return &Error{Line: num, Col: 1,
			Msg:  "YAML merge keys are not supported",
			Hint: "write the merged keys out in full"}
	}
	return nil
}

func blockScalarStyle(body string) (byte, bool) {
	idx := strings.LastIndex(body, ":")
	if idx < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(body[idx+1:])
	if rest == "|" || rest == "|-" || rest == "|+" {
		return '|', true
	}
	if rest == ">" || rest == ">-" || rest == ">+" {
		return '>', true
	}
	return 0, false
}

func dedent(lines []string) []string {
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if min < 0 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= min {
			out[i] = l[min:]
		} else {
			out[i] = strings.TrimLeft(l, " ")
		}
	}
	return out
}

func quoteScalar(s string) string {
	return "\x00BLOCK\x00" + s
}

func isBlockScalar(s string) (string, bool) {
	if strings.HasPrefix(s, "\x00BLOCK\x00") {
		return strings.TrimPrefix(s, "\x00BLOCK\x00"), true
	}
	return "", false
}

// stripComment removes a trailing comment, respecting quotes.
func stripComment(s string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '\\':
			if inDouble {
				i++
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				return s[:i]
			}
		}
	}
	return s
}

// ----------------------------------------------------------------- parsing

type parser struct {
	lines []line
	i     int
}

func (p *parser) cur() (line, bool) {
	if p.i >= len(p.lines) {
		return line{}, false
	}
	return p.lines[p.i], true
}

// parseBlock parses every line at exactly `indent` as one mapping or sequence.
func (p *parser) parseBlock(indent int) (*Node, error) {
	l, ok := p.cur()
	if !ok {
		return &Node{Kind: KindScalar, Value: ""}, nil
	}
	if strings.HasPrefix(l.text, "- ") || l.text == "-" {
		return p.parseSequence(indent)
	}
	return p.parseMapping(indent)
}

func (p *parser) parseMapping(indent int) (*Node, error) {
	n := &Node{
		Kind:   KindMapping,
		Values: map[string]*Node{},
		KeyPos: map[string]Pos{},
	}
	first := true

	for {
		l, ok := p.cur()
		if !ok || l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, &Error{
				Line: l.num, Col: l.indent + 1,
				Msg:  "unexpected indentation",
				Hint: fmt.Sprintf("this line is indented %d spaces; the block it is in uses %d", l.indent, indent),
			}
		}
		if first {
			n.Line, n.Col = l.num, l.indent+1
			first = false
		}

		key, rest, err := splitKey(l)
		if err != nil {
			return nil, err
		}
		if _, dup := n.Values[key]; dup {
			prev := n.KeyPos[key]
			return nil, &Error{
				Line: l.num, Col: l.indent + 1,
				Msg:  fmt.Sprintf("duplicate key %q", key),
				Hint: fmt.Sprintf("already set at line %d; a silently-overridden key is how the wrong value reaches production", prev.Line),
			}
		}
		p.i++

		var val *Node
		if rest != "" {
			val, err = parseInline(rest, l.num, l.indent+len(key)+3)
			if err != nil {
				return nil, err
			}
		} else {
			next, has := p.cur()
			if !has || next.indent <= indent {
				// "key:" with nothing under it is an explicit null.
				val = &Node{Kind: KindScalar, Value: "", Line: l.num, Col: l.indent + 1}
			} else {
				val, err = p.parseBlock(next.indent)
				if err != nil {
					return nil, err
				}
			}
		}

		n.Keys = append(n.Keys, key)
		n.Values[key] = val
		n.KeyPos[key] = Pos{Line: l.num, Col: l.indent + 1}
	}
	if first {
		n.Line, n.Col = 1, 1
	}
	return n, nil
}

func (p *parser) parseSequence(indent int) (*Node, error) {
	n := &Node{Kind: KindSequence}
	first := true

	for {
		l, ok := p.cur()
		if !ok || l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, &Error{
				Line: l.num, Col: l.indent + 1,
				Msg: "unexpected indentation inside a list",
			}
		}
		if !strings.HasPrefix(l.text, "- ") && l.text != "-" {
			break
		}
		if first {
			n.Line, n.Col = l.num, l.indent+1
			first = false
		}

		body := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		p.i++

		switch {
		case body == "":
			next, has := p.cur()
			if !has || next.indent <= indent {
				n.Items = append(n.Items, &Node{Kind: KindScalar, Line: l.num, Col: l.indent + 1})
				continue
			}
			item, err := p.parseBlock(next.indent)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, item)

		case isInlineKey(body):
			// "- key: value" starts a mapping whose remaining keys are
			// indented to where the key began, which is the column `body`
			// starts at within the original line -- not a fixed two spaces,
			// because "-   key:" is legal and aligns differently.
			itemIndent := l.indent + (len(l.text) - len(body))
			synthetic := line{num: l.num, indent: itemIndent, text: body, raw: l.raw}
			p.lines = append(p.lines[:p.i], append([]line{synthetic}, p.lines[p.i:]...)...)
			item, err := p.parseMapping(itemIndent)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, item)

		default:
			item, err := parseInline(body, l.num, l.indent+2)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, item)
		}
	}
	return n, nil
}

func isInlineKey(body string) bool {
	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		return false
	}
	_, _, err := splitKey(line{text: body})
	return err == nil
}

func splitKey(l line) (key, rest string, err error) {
	s := l.text
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if inSingle || inDouble {
				continue
			}
			if i+1 < len(s) && s[i+1] != ' ' {
				continue // part of a URL or a time, not a key separator
			}
			key = strings.TrimSpace(s[:i])
			rest = strings.TrimSpace(s[i+1:])
			if key == "" {
				return "", "", &Error{Line: l.num, Col: l.indent + 1, Msg: "empty key"}
			}
			return unquote(key), rest, nil
		}
	}
	return "", "", &Error{
		Line: l.num, Col: l.indent + 1,
		Msg:  fmt.Sprintf("expected a \"key: value\" pair, got %q", s),
		Hint: "a mapping key needs a colon followed by a space",
	}
}

// parseInline handles everything that can appear after "key: " on one line:
// a scalar, a flow mapping, or a flow sequence.
func parseInline(s string, lineNum, col int) (*Node, error) {
	s = strings.TrimSpace(s)
	if body, ok := isBlockScalar(s); ok {
		return &Node{Kind: KindScalar, Value: body, Quoted: true, Line: lineNum, Col: col}, nil
	}
	switch {
	case strings.HasPrefix(s, "{"):
		return parseFlowMapping(s, lineNum, col)
	case strings.HasPrefix(s, "["):
		return parseFlowSequence(s, lineNum, col)
	default:
		v, quoted := unquoteQ(s)
		return &Node{Kind: KindScalar, Value: v, Quoted: quoted, Line: lineNum, Col: col}, nil
	}
}

func parseFlowMapping(s string, lineNum, col int) (*Node, error) {
	inner, err := flowBody(s, '{', '}', lineNum, col)
	if err != nil {
		return nil, err
	}
	n := &Node{Kind: KindMapping, Values: map[string]*Node{}, KeyPos: map[string]Pos{}, Line: lineNum, Col: col}
	for _, part := range splitFlow(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := flowColon(part)
		if idx < 0 {
			return nil, &Error{Line: lineNum, Col: col,
				Msg:  fmt.Sprintf("expected \"key: value\" inside { }, got %q", part),
				Hint: "inline mappings look like { type: rolling, batch_size: 50% }"}
		}
		key := unquote(strings.TrimSpace(part[:idx]))
		val, err := parseInline(strings.TrimSpace(part[idx+1:]), lineNum, col)
		if err != nil {
			return nil, err
		}
		if _, dup := n.Values[key]; dup {
			return nil, &Error{Line: lineNum, Col: col, Msg: fmt.Sprintf("duplicate key %q", key)}
		}
		n.Keys = append(n.Keys, key)
		n.Values[key] = val
		n.KeyPos[key] = Pos{Line: lineNum, Col: col}
	}
	return n, nil
}

func parseFlowSequence(s string, lineNum, col int) (*Node, error) {
	inner, err := flowBody(s, '[', ']', lineNum, col)
	if err != nil {
		return nil, err
	}
	n := &Node{Kind: KindSequence, Line: lineNum, Col: col}
	for _, part := range splitFlow(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		item, err := parseInline(part, lineNum, col)
		if err != nil {
			return nil, err
		}
		n.Items = append(n.Items, item)
	}
	return n, nil
}

func flowBody(s string, open, close byte, lineNum, col int) (string, error) {
	if len(s) < 2 || s[0] != open {
		return "", &Error{Line: lineNum, Col: col, Msg: "malformed inline value"}
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				if rest := strings.TrimSpace(s[i+1:]); rest != "" {
					return "", &Error{Line: lineNum, Col: col,
						Msg: fmt.Sprintf("unexpected %q after the closing %q", rest, string(close))}
				}
				return s[1:i], nil
			}
		}
	}
	return "", &Error{Line: lineNum, Col: col,
		Msg:  fmt.Sprintf("unclosed %q", string(open)),
		Hint: fmt.Sprintf("add a %q", string(close))}
}

// splitFlow splits on commas that are not inside nested brackets or quotes.
func splitFlow(s string) []string {
	var out []string
	var depth int
	var inSingle, inDouble bool
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{', '[':
			if !inSingle && !inDouble {
				depth++
			}
		case '}', ']':
			if !inSingle && !inDouble {
				depth--
			}
		case ',':
			if depth == 0 && !inSingle && !inDouble {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func flowColon(s string) int {
	var depth int
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ':':
			if depth == 0 && !inSingle && !inDouble {
				return i
			}
		}
	}
	return -1
}

func unquote(s string) string {
	v, _ := unquoteQ(s)
	return v
}

func unquoteQ(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			if v, err := strconv.Unquote(s); err == nil {
				return v, true
			}
			return s[1 : len(s)-1], true
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), true
		}
	}
	return s, false
}

// ---------------------------------------------------------------- accessors

// IsNull reports the YAML null forms plus an empty value.
func (n *Node) IsNull() bool {
	if n == nil {
		return true
	}
	if n.Kind != KindScalar || n.Quoted {
		return false
	}
	switch n.Value {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

func (n *Node) String() (string, error) {
	if n == nil || n.Kind != KindScalar {
		return "", errorAt(n, "expected a string")
	}
	return n.Value, nil
}

func (n *Node) Bool() (bool, error) {
	if n == nil || n.Kind != KindScalar {
		return false, errorAt(n, "expected true or false")
	}
	switch strings.ToLower(n.Value) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	return false, errorAt(n, "expected true or false, got %q", n.Value)
}

func (n *Node) Int() (int, error) {
	if n == nil || n.Kind != KindScalar {
		return 0, errorAt(n, "expected a whole number")
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil {
		return 0, errorAt(n, "expected a whole number, got %q", n.Value)
	}
	return v, nil
}

func (n *Node) Float() (float64, error) {
	if n == nil || n.Kind != KindScalar {
		return 0, errorAt(n, "expected a number")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(n.Value), 64)
	if err != nil {
		return 0, errorAt(n, "expected a number, got %q", n.Value)
	}
	return v, nil
}
