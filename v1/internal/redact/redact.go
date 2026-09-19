// Package redact scrubs secrets out of provider output (design.md 12.3).
//
//	"Provider output goes into logs that end up in a database and on a
//	dashboard. Anything the deploy target prints is a potential leak."
//
//	"Redact before persistence, not on display — once a secret is in the
//	database it's leaked."
//
// Two details from the document that are easy to get wrong and are both
// tested:
//
//   - Report `len(p)`, not the redacted length. A Writer that returns a
//     shorter count than it was given is signalling a short write, and
//     `io.Copy` and `fmt.Fprintf` will treat it as an error.
//
//   - A secret split across two Write calls must still be caught. Provider
//     output arrives in whatever chunks the network produced, so a redactor
//     that only looks at one call at a time misses exactly the secrets that
//     matter. This implementation holds back a tail.
package redact

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

// Placeholder is what replaces a secret.
const Placeholder = "[REDACTED]"

// Patterns are the shapes of credentials worth catching even when nobody
// registered the literal value.
//
// This list is not exhaustive and cannot be: it is the set a deploy target
// plausibly prints. The literals registered from the secret store are the
// primary defence; these catch the ones nobody told us about.
var Patterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),             // AWS access key id
	regexp.MustCompile(`ASIA[0-9A-Z]{16}`),             // AWS temporary key id
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`), // Slack tokens
	regexp.MustCompile(`xapp-[0-9]-[0-9A-Za-z-]{10,}`), // Slack app tokens
	regexp.MustCompile(`gh[pousr]_[0-9A-Za-z]{20,}`),   // GitHub tokens
	regexp.MustCompile(`glpat-[0-9A-Za-z_-]{20,}`),     // GitLab tokens
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWT
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)\s*[:=]\s*\S+`),
}

// maxHold is how many bytes are held back between writes so that a literal
// split across two calls is still caught. Long enough for the longest
// realistic credential, short enough that a dashboard tailing logs does not
// visibly lag.
const maxHold = 4096

// Redactor wraps a writer and removes secrets on the way through.
type Redactor struct {
	mu       sync.Mutex
	inner    io.Writer
	literals [][]byte
	patterns []*regexp.Regexp
	// held is the tail of the previous write, not yet emitted in case a
	// secret straddles the boundary.
	held []byte
	// Count is how many redactions were made, so a caller can log that
	// something was scrubbed without logging what.
	Count int
}

// New wraps w, redacting the given literal secrets and the standard patterns.
func New(w io.Writer, literals ...string) *Redactor {
	r := &Redactor{inner: w, patterns: Patterns}
	for _, l := range literals {
		// A one- or two-character "secret" would redact the whole log. This
		// mostly guards against an empty environment variable being
		// registered as a literal, which is a real and silent disaster.
		if len(l) >= 6 {
			r.literals = append(r.literals, []byte(l))
		}
	}
	return r
}

// WithPatterns replaces the pattern list, for tests and for deployments that
// know their own credential shapes.
func (r *Redactor) WithPatterns(ps ...*regexp.Regexp) *Redactor {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patterns = ps
	return r
}

func (r *Redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	buf := append(r.held, p...)
	r.held = nil

	// Hold back a tail so a secret spanning this write and the next is still
	// caught -- but never hold back a complete line, or a dashboard tailing
	// the log stalls. The split point is the last newline within the hold
	// window.
	emit := buf
	if len(buf) > maxHold {
		split := bytes.LastIndexByte(buf[len(buf)-maxHold:], '\n')
		if split >= 0 {
			cut := len(buf) - maxHold + split + 1
			emit, r.held = buf[:cut], append([]byte(nil), buf[cut:]...)
		}
	} else if idx := bytes.LastIndexByte(buf, '\n'); idx >= 0 && idx < len(buf)-1 {
		emit, r.held = buf[:idx+1], append([]byte(nil), buf[idx+1:]...)
	} else if idx < 0 {
		// No newline at all: hold everything, up to the cap.
		if len(buf) < maxHold {
			r.held = append([]byte(nil), buf...)
			return len(p), nil
		}
		emit = buf
	}

	if _, err := r.inner.Write(r.scrub(emit)); err != nil {
		return 0, err
	}
	// Report the original length. Returning the redacted length looks like a
	// short write to every caller in the standard library.
	return len(p), nil
}

// Flush emits any held tail. The engine calls this when a deploy finishes;
// without it the last partial line of output is lost.
func (r *Redactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.held) == 0 {
		return nil
	}
	out := r.scrub(r.held)
	r.held = nil
	_, err := r.inner.Write(out)
	return err
}

func (r *Redactor) scrub(p []byte) []byte {
	out := p
	for _, lit := range r.literals {
		if bytes.Contains(out, lit) {
			r.Count += bytes.Count(out, lit)
			out = bytes.ReplaceAll(out, lit, []byte(Placeholder))
		}
	}
	for _, re := range r.patterns {
		out = re.ReplaceAllFunc(out, func(m []byte) []byte {
			r.Count++
			return []byte(Placeholder)
		})
	}
	return out
}

// String redacts a single string, for error messages and notification
// payloads -- the other two places a secret reaches persistence.
func String(s string, literals ...string) string {
	var buf bytes.Buffer
	r := New(&buf, literals...)
	_, _ = r.Write([]byte(s))
	_ = r.Flush()
	return buf.String()
}
