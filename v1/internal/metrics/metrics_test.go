package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestCounterRendersWithItsLabels(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("orch_deployments_total", "Deployments by outcome.", "app", "environment", "result")
	c.Inc("web", "prod", "succeeded")
	c.Inc("web", "prod", "succeeded")
	c.Inc("web", "prod", "failed")

	out := r.Gather()
	if !strings.Contains(out, `orch_deployments_total{app="web",environment="prod",result="succeeded"} 2`) {
		t.Errorf("missing or wrong succeeded series:\n%s", out)
	}
	if !strings.Contains(out, `orch_deployments_total{app="web",environment="prod",result="failed"} 1`) {
		t.Errorf("missing failed series:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE orch_deployments_total counter") {
		t.Errorf("missing TYPE line:\n%s", out)
	}
}

// Regression: an empty labelled counter used to emit `name 0`, putting a child
// with no labels under a name whose every other child has three. A strict
// scraper reading a freshly started server -- which is when anyone is testing
// the integration -- sees a metric whose label dimensions change under it.
func TestAnEmptyLabelledMetricEmitsNoSample(t *testing.T) {
	r := NewRegistry()
	r.Counter("orch_rollbacks_total", "Rollbacks.", "app", "environment", "outcome")

	out := r.Gather()
	if !strings.Contains(out, "# TYPE orch_rollbacks_total counter") {
		t.Fatal("the metric should still be declared")
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "orch_rollbacks_total 0" {
			t.Fatalf("emitted an unlabelled sample for a labelled counter:\n%s", out)
		}
	}
}

func TestAnEmptyUnlabelledMetricEmitsZero(t *testing.T) {
	r := NewRegistry()
	r.Gauge("orch_outbox_pending", "Undelivered notifications.")

	// This one does get a zero: an alert on it needs a series to exist before
	// the first notification, and there are no label values to invent.
	if !strings.Contains(r.Gather(), "orch_outbox_pending 0") {
		t.Fatalf("an unlabelled gauge with no value should render 0:\n%s", r.Gather())
	}
}

func TestGaugeTakesTheLastValue(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("orch_outbox_pending", "Undelivered notifications.")
	g.Set(5)
	g.Set(2)
	if got := g.Value(); got != 2 {
		t.Fatalf("gauge = %v, want 2", got)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("orch_deployment_duration_seconds", "Duration.",
		[]float64{1, 10, 100}, "app")
	h.ObserveDuration(500*time.Millisecond, "web")
	h.ObserveDuration(5*time.Second, "web")
	h.ObserveDuration(50*time.Second, "web")

	out := r.Gather()
	// A histogram bucket counts everything at or below its bound, so the
	// counts must not decrease as the bound grows.
	for _, want := range []string{
		`le="1"} 1`,
		`le="10"} 2`,
		`le="100"} 3`,
		`le="+Inf"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing bucket %s:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "orch_deployment_duration_seconds_count") {
		t.Error("missing _count")
	}
	if !strings.Contains(out, "orch_deployment_duration_seconds_sum") {
		t.Error("missing _sum")
	}
}

func TestExponentialBuckets(t *testing.T) {
	got := ExponentialBuckets(10, 2, 4)
	want := []float64{10, 20, 40, 80}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("orch_test_total", "Test.", "reason")
	// A reason string reaches this from an error message, and an unescaped
	// quote in one would make the whole scrape unparseable.
	c.Inc(`he said "no"`)

	out := r.Gather()
	if strings.Contains(out, `reason="he said "no""`) {
		t.Fatalf("an unescaped quote made it into the output:\n%s", out)
	}
	if !strings.Contains(out, `\"no\"`) {
		t.Fatalf("the quote was not escaped:\n%s", out)
	}
}

func TestRegisteringTheSameMetricTwiceReturnsTheSameOne(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("orch_test_total", "Test.", "x")
	b := r.Counter("orch_test_total", "Test.", "x")
	a.Inc("one")
	if got := b.Value("one"); got != 1 {
		t.Fatalf("the second registration returned a different counter (value %v)", got)
	}
}

func TestConcurrentObservations(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("orch_test_total", "Test.", "app")
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				c.Inc("web")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := c.Value("web"); got != 1600 {
		t.Fatalf("counter = %v, want 1600", got)
	}
}

func TestTheDocumentsMetricsAreAllPresent(t *testing.T) {
	m := New()
	out := m.Gather()
	// Section 19.1's list. A missing one is a dashboard panel that renders
	// empty and an alert that never fires.
	for _, name := range []string{
		"orch_deployments_total",
		"orch_deployment_duration_seconds",
		"orch_lease_wait_seconds",
		"orch_outbox_pending",
		"orch_orphans_recovered_total",
		"orch_rollbacks_total",
		"orch_verification_verdicts_total",
		"orch_approvals_denied_total",
		"orch_notify_failures_total",
	} {
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s is not exposed", name)
		}
	}
}
