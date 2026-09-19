// Package metrics exposes the counters from design.md section 19.1.
//
//	"Observe the orchestrator itself."
//
// A tiny in-process registry rendering the Prometheus text exposition format,
// rather than prometheus/client_golang, because this build has no module
// proxy (see docs/notes-on-the-spec.md). The metric names, label sets and
// semantics are the document's, so swapping in the real client is a
// find-and-replace rather than a redesign.
//
// The one the document singles out is `orch_outbox_pending`:
//
//	"A growing value means Slack delivery is broken and people are not being
//	told about deploys. Alert on it."
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds every metric.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	order      []string
}

func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*Counter{},
		gauges:     map[string]*Gauge{},
		histograms: map[string]*Histogram{},
	}
}

type metricMeta struct {
	name string
	help string
	typ  string
}

// Counter only goes up.
type Counter struct {
	metricMeta
	mu     sync.Mutex
	labels []string
	values map[string]float64
}

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{
		metricMeta: metricMeta{name: name, help: help, typ: "counter"},
		labels:     labels,
		values:     map[string]float64{},
	}
	r.counters[name] = c
	r.order = append(r.order, name)
	return c
}

func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *Counter) Add(v float64, labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[joinLabels(labelValues)] += v
}

func (c *Counter) Value(labelValues ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[joinLabels(labelValues)]
}

// Gauge goes up and down.
type Gauge struct {
	metricMeta
	mu     sync.Mutex
	labels []string
	values map[string]float64
}

func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{
		metricMeta: metricMeta{name: name, help: help, typ: "gauge"},
		labels:     labels,
		values:     map[string]float64{},
	}
	r.gauges[name] = g
	r.order = append(r.order, name)
	return g
}

func (g *Gauge) Set(v float64, labelValues ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values[joinLabels(labelValues)] = v
}

func (g *Gauge) Value(labelValues ...string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.values[joinLabels(labelValues)]
}

// Histogram records a distribution in exponential buckets.
type Histogram struct {
	metricMeta
	mu      sync.Mutex
	labels  []string
	buckets []float64
	counts  map[string][]uint64
	sums    map[string]float64
	totals  map[string]uint64
}

// ExponentialBuckets matches the document's
// `prometheus.ExponentialBuckets(10, 2, 10)`.
func ExponentialBuckets(start, factor float64, count int) []float64 {
	out := make([]float64, count)
	v := start
	for i := range out {
		out[i] = v
		v *= factor
	}
	return out
}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return h
	}
	h := &Histogram{
		metricMeta: metricMeta{name: name, help: help, typ: "histogram"},
		labels:     labels,
		buckets:    buckets,
		counts:     map[string][]uint64{},
		sums:       map[string]float64{},
		totals:     map[string]uint64{},
	}
	r.histograms[name] = h
	r.order = append(r.order, name)
	return h
}

func (h *Histogram) Observe(v float64, labelValues ...string) {
	key := joinLabels(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.counts[key]; !ok {
		h.counts[key] = make([]uint64, len(h.buckets))
	}
	for i, b := range h.buckets {
		if v <= b {
			h.counts[key][i]++
		}
	}
	h.sums[key] += v
	h.totals[key]++
}

func (h *Histogram) ObserveDuration(d time.Duration, labelValues ...string) {
	h.Observe(d.Seconds(), labelValues...)
}

// Gather renders the Prometheus text exposition format.
func (r *Registry) Gather() string {
	r.mu.RLock()
	names := append([]string(nil), r.order...)
	counters, gauges, histograms := r.counters, r.gauges, r.histograms
	r.mu.RUnlock()

	var b strings.Builder
	for _, name := range names {
		switch {
		case counters[name] != nil:
			writeSimple(&b, counters[name].metricMeta, counters[name].labels, counters[name].snapshot())
		case gauges[name] != nil:
			writeSimple(&b, gauges[name].metricMeta, gauges[name].labels, gauges[name].snapshot())
		case histograms[name] != nil:
			histograms[name].write(&b)
		}
	}
	return b.String()
}

func (c *Counter) snapshot() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]float64, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}

func (g *Gauge) snapshot() map[string]float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]float64, len(g.values))
	for k, v := range g.values {
		out[k] = v
	}
	return out
}

func writeSimple(b *strings.Builder, m metricMeta, labels []string, values map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		// An unlabelled metric with no observations still gets a line, so an
		// alert on rate(x[5m]) has something to evaluate before the first
		// event.
		//
		// A labelled one does not, and must not: emitting `orch_rollbacks_total 0`
		// for a counter declared with app/environment/outcome puts a child with
		// no labels under a name whose every other child has three, which is
		// not something the exposition format allows. There is no honest
		// zero to emit here, because the label values are not known until
		// something happens.
		if len(labels) == 0 {
			fmt.Fprintf(b, "%s 0\n", m.name)
		}
		return
	}
	for _, k := range keys {
		fmt.Fprintf(b, "%s%s %s\n", m.name, renderLabels(labels, k), formatFloat(values[k]))
	}
}

func (h *Histogram) write(b *strings.Builder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)

	keys := make([]string, 0, len(h.counts))
	for k := range h.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		base := renderLabels(h.labels, k)
		for i, bucket := range h.buckets {
			fmt.Fprintf(b, "%s_bucket%s %d\n", h.name,
				withLabel(base, "le", formatFloat(bucket)), h.counts[k][i])
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", h.name, withLabel(base, "le", "+Inf"), h.totals[k])
		fmt.Fprintf(b, "%s_sum%s %s\n", h.name, base, formatFloat(h.sums[k]))
		fmt.Fprintf(b, "%s_count%s %d\n", h.name, base, h.totals[k])
	}
}

const labelSep = "\x1f"

func joinLabels(values []string) string { return strings.Join(values, labelSep) }

func renderLabels(names []string, joined string) string {
	if len(names) == 0 || joined == "" {
		return ""
	}
	values := strings.Split(joined, labelSep)
	var parts []string
	for i, n := range names {
		if i < len(values) {
			parts = append(parts, fmt.Sprintf("%s=%q", n, values[i]))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func withLabel(base, name, value string) string {
	pair := fmt.Sprintf("%s=%q", name, value)
	if base == "" {
		return "{" + pair + "}"
	}
	return base[:len(base)-1] + "," + pair + "}"
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ------------------------------------------------- the orchestrator's own

// Metrics is the set from section 19.1.
type Metrics struct {
	Registry *Registry

	DeploymentsTotal   *Counter
	DeploymentDuration *Histogram
	LeaseWaitSeconds   *Histogram
	OutboxPending      *Gauge
	OrphansRecovered   *Counter
	// RollbacksTotal is not in the document's list but is half of the change
	// failure rate, which section 19.1 says you get "essentially for free".
	// Free only if you record it.
	RollbacksTotal  *Counter
	VerdictsTotal   *Counter
	ApprovalsDenied *Counter
	NotifyFailures  *Counter
}

func New() *Metrics {
	r := NewRegistry()
	return &Metrics{
		Registry: r,
		DeploymentsTotal: r.Counter("orch_deployments_total",
			"Deployments by outcome.", "app", "environment", "result"),
		DeploymentDuration: r.Histogram("orch_deployment_duration_seconds",
			"Deployment duration.", ExponentialBuckets(10, 2, 10),
			"app", "environment", "strategy"),
		LeaseWaitSeconds: r.Histogram("orch_lease_wait_seconds",
			"Time spent waiting for a lease.", ExponentialBuckets(0.1, 2, 12), "resource"),
		OutboxPending: r.Gauge("orch_outbox_pending",
			"Undelivered notifications. Growing means people are not being told about deploys."),
		OrphansRecovered: r.Counter("orch_orphans_recovered_total",
			"Deployments recovered by startup reconciliation.", "outcome"),
		RollbacksTotal: r.Counter("orch_rollbacks_total",
			"Rollbacks, for the change failure rate.", "app", "environment", "outcome"),
		VerdictsTotal: r.Counter("orch_verification_verdicts_total",
			"Verification verdicts.", "app", "environment", "verdict"),
		ApprovalsDenied: r.Counter("orch_approvals_denied_total",
			"Approval attempts refused by RBAC. A rising rate is a signal.", "environment", "reason"),
		NotifyFailures: r.Counter("orch_notify_failures_total",
			"Notification delivery failures by notifier.", "notifier"),
	}
}

func (m *Metrics) Gather() string { return m.Registry.Gather() }
