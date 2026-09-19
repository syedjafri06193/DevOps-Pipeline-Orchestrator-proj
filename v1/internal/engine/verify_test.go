package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/verifier"
	fakeverifier "github.com/syedjafri06193/orch/internal/verifier/fake"
)

func f(v float64) *float64 { return &v }

// ------------------------------------------------- the criterion (7.2)

func TestFloorValuePreventsTheSpuriousRollback(t *testing.T) {
	// The single most valuable line in section 7.2:
	//
	//   "Without it, a service whose error rate moves from 0.001% to 0.004%
	//   trips a 2x ratio check and rolls back a perfectly good deploy."
	c := config.Criterion{
		Name: "error-rate", Direction: config.LowerIsBetter,
		MaxRatio: f(2.0), FloorValue: 0.001,
	}
	s := verifier.Sample{Value: 0.00004, Baseline: 0.00001} // a 4x "regression"
	ok, reason := EvaluateCriterion(c, s)
	if !ok {
		t.Fatalf("a 4x move below the floor must pass: %s", reason)
	}
	if !strings.Contains(reason, "floor") {
		t.Errorf("the reason should say why: %q", reason)
	}

	// And the same ratio above the floor must still fail, or the floor has
	// simply disabled the check.
	big := verifier.Sample{Value: 0.04, Baseline: 0.01}
	if ok, _ := EvaluateCriterion(c, big); ok {
		t.Error("a 4x move above the floor must fail")
	}
}

func TestTheFloorIsCheckedBeforeTheRatio(t *testing.T) {
	// Ordering matters: a floor applied after the ratio test is no floor.
	c := config.Criterion{
		Name: "e", Direction: config.LowerIsBetter,
		MaxRatio: f(1.1), FloorValue: 0.5,
	}
	// 100x the baseline, but below the floor.
	if ok, _ := EvaluateCriterion(c, verifier.Sample{Value: 0.1, Baseline: 0.001}); !ok {
		t.Error("the floor must win over the ratio")
	}
}

func TestAnAbsoluteCeilingAppliesRegardlessOfBaseline(t *testing.T) {
	c := config.Criterion{
		Name: "error-rate", Direction: config.LowerIsBetter,
		Max: f(0.01), MaxRatio: f(10), FloorValue: 0.0001,
	}
	// Well within the ratio tolerance, but over the absolute maximum.
	ok, reason := EvaluateCriterion(c, verifier.Sample{Value: 0.05, Baseline: 0.02})
	if ok {
		t.Fatal("an absolute ceiling must apply even when the ratio passes")
	}
	if !strings.Contains(reason, "maximum") {
		t.Errorf("reason = %q", reason)
	}
}

func TestAMissingBaselineDoesNotFailTheRatioCheck(t *testing.T) {
	// A first-ever deploy has no baseline. Treating that as a failure turns
	// every new service's first deploy into an automatic rollback.
	c := config.Criterion{Name: "e", Direction: config.LowerIsBetter, MaxRatio: f(1.2), FloorValue: 0.001}
	ok, reason := EvaluateCriterion(c, verifier.Sample{Value: 0.5, Baseline: 0})
	if !ok {
		t.Fatalf("a missing baseline must not fail: %s", reason)
	}
	if !strings.Contains(reason, "no baseline") {
		t.Errorf("say so: %q", reason)
	}
}

func TestHigherIsBetterInverts(t *testing.T) {
	// Availability, success rate: the comparison runs the other way.
	c := config.Criterion{Name: "success-rate", Direction: config.HigherIsBetter, MaxRatio: f(1.05)}
	// 99% against a 99.9% baseline is a ratio of 1.009 the wrong way.
	if ok, _ := EvaluateCriterion(c, verifier.Sample{Value: 0.90, Baseline: 0.999}); ok {
		t.Error("a drop in a higher-is-better metric must fail")
	}
	if ok, _ := EvaluateCriterion(c, verifier.Sample{Value: 0.999, Baseline: 0.998}); !ok {
		t.Error("an improvement must pass")
	}
}

func TestAMinimumOnHigherIsBetter(t *testing.T) {
	c := config.Criterion{Name: "availability", Direction: config.HigherIsBetter, Max: f(0.99)}
	if ok, _ := EvaluateCriterion(c, verifier.Sample{Value: 0.95}); ok {
		t.Error("below the minimum must fail")
	}
	if ok, _ := EvaluateCriterion(c, verifier.Sample{Value: 0.999}); !ok {
		t.Error("above the minimum must pass")
	}
}

// ------------------------------------------------ the decision loop (7.3)

func policy(criteria ...config.Criterion) config.VerifyPolicy {
	return config.VerifyPolicy{
		Bake:             10 * time.Minute,
		SampleInterval:   30 * time.Second,
		MinSamples:       3,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		MaxInconclusive:  3,
		Criteria:         criteria,
	}
}

func errorRate() config.Criterion {
	return config.Criterion{
		Name: "error-rate", Verifier: "fake", Direction: config.LowerIsBetter,
		Max: f(0.01), FloorValue: 0.0001,
	}
}

func TestASingleBlipDoesNotRollBack(t *testing.T) {
	// "One bad sample in a ten-minute bake is noise. Three in a row is a
	// signal." This is the specific case section 16's M5 acceptance criterion
	// names: "a deliberate single-sample blip is *not*" detected as a failure.
	h := newHarness(t)
	h.verifier.Values = []float64{0.001, 0.001, 0.5, 0.001, 0.001, 0.001}

	res := h.exec.Verify(context.Background(), h.dep, policy(errorRate()))
	if res.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %s (%s); a single blip must not roll back", res.Verdict, res.Reason)
	}
}

func TestThreeConsecutiveFailuresRollBack(t *testing.T) {
	h := newHarness(t)
	h.verifier.Values = []float64{0.001, 0.5, 0.5, 0.5}

	res := h.exec.Verify(context.Background(), h.dep, policy(errorRate()))
	if res.Verdict != VerdictUnhealthy {
		t.Fatalf("verdict = %s (%s)", res.Verdict, res.Reason)
	}
	if res.ConsecutiveFail != 3 {
		t.Errorf("consecutive failures = %d, want 3", res.ConsecutiveFail)
	}
	if !strings.Contains(res.Reason, "consecutive") {
		t.Errorf("the reason should say why: %q", res.Reason)
	}
}

func TestFailuresMustBeConsecutiveNotCumulative(t *testing.T) {
	// Alternating bad and good samples: three failures in total, never three
	// in a row. A cumulative counter would roll back here, which is the bug.
	h := newHarness(t)
	h.verifier.Values = []float64{0.5, 0.001, 0.5, 0.001, 0.5, 0.001, 0.001, 0.001}

	res := h.exec.Verify(context.Background(), h.dep, policy(errorRate()))
	if res.Verdict == VerdictUnhealthy {
		t.Fatalf("a cumulative counter rolled back on noise: %s", res.Reason)
	}
}

func TestABrokenVerifierIsInconclusiveNotUnhealthy(t *testing.T) {
	// Section 7.3: "A broken Prometheus is NOT a failing deploy. Rolling back
	// on missing data is how a monitoring outage becomes a deployment
	// outage."
	h := newHarness(t)
	h.verifier.Values = []float64{0.001}
	for i := 1; i <= 10; i++ {
		h.verifier.FailAt[i] = verifier.Inconclusive("prometheus: connection refused")
	}

	res := h.exec.Verify(context.Background(), h.dep, policy(errorRate()))
	if res.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s, want inconclusive", res.Verdict)
	}
	if res.Verdict == VerdictUnhealthy {
		t.Fatal("a monitoring outage must never cause a rollback")
	}
	if !strings.Contains(res.Reason, "verifier errors") {
		t.Errorf("reason = %q", res.Reason)
	}
}

func TestAFewVerifierErrorsAreToleratedWithinTheBudget(t *testing.T) {
	// A single scrape failure in a ten-minute bake is normal.
	h := newHarness(t)
	h.verifier.Values = []float64{0.001}
	h.verifier.FailAt[1] = verifier.Inconclusive("scrape timeout")
	h.verifier.FailAt[3] = verifier.Inconclusive("scrape timeout")

	res := h.exec.Verify(context.Background(), h.dep, policy(errorRate()))
	if res.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %s (%s)", res.Verdict, res.Reason)
	}
	if res.InconclusiveCount != 2 {
		t.Errorf("inconclusive = %d, want 2", res.InconclusiveCount)
	}
}

func TestMinSamplesPreventsAFastEarlyVerdict(t *testing.T) {
	// A metric that has not had time to move must not be declared healthy on
	// the strength of two quick samples.
	h := newHarness(t)
	h.verifier.Values = []float64{0.0001, 0.0001, 0.0001, 0.0001, 0.0001}

	p := policy(errorRate())
	p.MinSamples = 5
	p.SuccessThreshold = 1 // would otherwise return after the first sample

	res := h.exec.Verify(context.Background(), h.dep, p)
	if res.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %s", res.Verdict)
	}
	if res.Samples < 5 {
		t.Errorf("returned after %d samples, MinSamples is 5", res.Samples)
	}
}

func TestTooFewSamplesInTheWindowIsInconclusive(t *testing.T) {
	h := newHarness(t)
	h.verifier.Values = []float64{0.0001}
	p := policy(errorRate())
	p.Bake = 60 * time.Second
	p.SampleInterval = 30 * time.Second
	p.MinSamples = 10 // cannot be reached

	res := h.exec.Verify(context.Background(), h.dep, p)
	if res.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s (%s)", res.Verdict, res.Reason)
	}
}

func TestCancellationAborts(t *testing.T) {
	h := newHarness(t)
	h.verifier.Values = []float64{0.0001}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := h.exec.Verify(ctx, h.dep, policy(errorRate()))
	if res.Verdict != VerdictAborted {
		t.Fatalf("verdict = %s", res.Verdict)
	}
	if !errors.Is(res.Cause, context.Canceled) {
		t.Errorf("cause = %v", res.Cause)
	}
}

func TestNoCriteriaIsHealthyNotABlock(t *testing.T) {
	// Refusing here would make a bake-less environment undeployable, which is
	// a worse answer than trusting the operator's configuration.
	h := newHarness(t)
	res := h.exec.Verify(context.Background(), h.dep, config.VerifyPolicy{})
	if res.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %s", res.Verdict)
	}
}

func TestEverySampleIsRecordedForTheChart(t *testing.T) {
	// A rollback the rep cannot see the evidence for is a rollback nobody
	// trusts. Section 11.2 puts a health-sample chart on the detail page.
	h := newHarness(t)
	h.verifier.Values = []float64{0.001, 0.5, 0.5, 0.5}

	h.exec.Verify(context.Background(), h.dep, policy(errorRate()))

	samples, err := h.store.Samples(context.Background(), h.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) < 4 {
		t.Fatalf("recorded %d samples, want at least 4", len(samples))
	}
	for _, s := range samples {
		if s.Note == "" {
			t.Error("every sample needs a note explaining the judgement")
		}
		if s.Criterion == "" {
			t.Error("every sample must name its criterion")
		}
	}
}

func TestAllCriteriaMustPassForASampleToPass(t *testing.T) {
	h := newHarness(t)
	h.verifier.Values = []float64{0.5} // fails the first criterion

	p := policy(
		errorRate(),
		config.Criterion{Name: "latency", Verifier: "fake", Direction: config.LowerIsBetter, Max: f(100)},
	)
	p.FailureThreshold = 1
	res := h.exec.Verify(context.Background(), h.dep, p)
	if res.Verdict != VerdictUnhealthy {
		t.Fatalf("verdict = %s", res.Verdict)
	}
	if !strings.Contains(res.Reason, "error-rate") {
		t.Errorf("the reason should name the criterion that failed: %q", res.Reason)
	}
}

func TestAnUnknownVerifierIsInconclusiveNotAFailure(t *testing.T) {
	// A misconfigured verifier is our fault, not the deploy's.
	h := newHarness(t)
	p := policy(config.Criterion{Name: "x", Verifier: "nonexistent", Max: f(1)})
	res := h.exec.Verify(context.Background(), h.dep, p)
	if res.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s (%s)", res.Verdict, res.Reason)
	}
}

func TestTheVerdictStopsSampling(t *testing.T) {
	// Once three consecutive failures are in, continuing to sample only
	// delays the rollback.
	h := newHarness(t)
	h.verifier.Values = []float64{0.5, 0.5, 0.5}
	p := policy(errorRate())
	p.Bake = time.Hour // would be 120 samples

	h.exec.Verify(context.Background(), h.dep, p)
	if got := h.verifier.Calls(); got > 5 {
		t.Errorf("took %d samples after reaching a verdict at 3", got)
	}
}

var _ = fakeverifier.New
