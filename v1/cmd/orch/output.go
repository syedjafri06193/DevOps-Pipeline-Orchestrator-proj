package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/store"
)

// Output rules from section 17.4:
//
//	"Respect NO_COLOR. Detect non-TTY and drop the progress animation.
//	`--output=json` on everything that lists or shows, because the first thing
//	anyone does with a deployment tool is script it."
//
// The third is the one people forget, and it is the one that decides whether
// the tool can be used in a pipeline at all.

// colorize is decided once, at startup.
var useColor = wantColor()

func wantColor() bool {
	// NO_COLOR is honoured whatever its value, including empty: the spec says
	// presence is what counts.
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(os.Stdout)
}

// isTerminal reports whether w is a character device.
//
// Done with os.Stat rather than an ioctl so this stays pure stdlib and works
// the same on every platform the binary is built for. A pipe or a file is not
// a character device, which is exactly the distinction that matters.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	blue   = "\x1b[34m"
)

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + reset
}

func stateColor(st store.State) string {
	switch st {
	case store.StateSucceeded:
		return green
	case store.StateFailed, store.StateRollbackFailed, store.StateUnknown:
		return red
	case store.StateRolledBack, store.StateAborted:
		return yellow
	default:
		return blue
	}
}

func renderState(st store.State) string { return paint(stateColor(st), string(st)) }

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints aligned columns without a dependency.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table { return &table{headers: headers} }

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) write(w io.Writer) {
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = visibleLen(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			if i < len(widths) && visibleLen(c) > widths[i] {
				widths[i] = visibleLen(c)
			}
		}
	}

	var b strings.Builder
	for i, h := range t.headers {
		b.WriteString(pad(paint(dim, h), widths[i], visibleLen(h)))
		if i < len(t.headers)-1 {
			b.WriteString("  ")
		}
	}
	fmt.Fprintln(w, strings.TrimRight(b.String(), " "))

	for _, row := range t.rows {
		b.Reset()
		for i, c := range row {
			b.WriteString(pad(c, widths[i], visibleLen(c)))
			if i < len(row)-1 {
				b.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

func pad(s string, width, visible int) string {
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// visibleLen ignores ANSI escapes, so a coloured cell still lines up. Without
// this, colour breaks every column to its right.
func visibleLen(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
		default:
			n++
		}
	}
	return n
}

func humanAgo(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// shortID is enough to identify a deployment by eye. ULIDs sort by time, so
// the tail is the part that differs between deployments made minutes apart.
func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[len(id)-10:]
}
