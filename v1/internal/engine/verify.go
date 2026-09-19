// Package engine drives the deployment state machine.
//
// verify.go is design.md section 7 -- "the intellectual core of the project,
// and where most homegrown deployment tools are weakest."
//
// The three things the document says matter more than they look, all of which
// are separate tests below:
//
//  1. A verifier error is not a deploy failure. "Rolling back on missing data
//     is how a monitoring outage becomes a deployment outage."
//  2. Consecutive failures, not cumulative. "One bad sample in a ten-minute
//     bake is noise. Three in a row is a signal."
//  3. MinSamples guards against a fast early verdict on a metric that has not
//     had time to move.
//
// And one that the document states in a single sentence and is the single
// most valuable line in the file: the `FloorValue` check. "Without it, a
// service whose error rate moves from 0.001% to 0.004% trips a 2x ratio check
// and rolls back a perfectly good deploy."
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/verifier"
)

// Verdict is the outcome of a verification window.
type Verdict string

const (
	VerdictHealthy      Verdict = "healthy"
	VerdictUnhealthy    Verdict = "unhealthy"
	VerdictInconclusive Verdict = "inconclusive"
	VerdictAborted      Verdict = "aborted"
)

// EvaluateCriterion applies one criterion to one sample (section 7.2).
//
// Returns whether it passed and why, because "unhealthy" with no reason
// produces a rollback nobody can explain afterwards.
func EvaluateCriterion(c config.Criterion, s verifier.Sample) (bool, string) {
	// THE FLOOR CHECK, FIRST.
	//
	// Section 7.2: "That FloorValue field looks like a detail and is not."
	// A service whose error rate moves from 0.001% to 0.004% has quadrupled
	// and is fine. This single comparison prevents a large fraction of
	// spurious rollbacks, and it has to come before the ratio test rather
	// than after it.
	if c.Direction == config.LowerIsBetter && s.Value <= c.FloorValue {
		return true, fmt.Sprintf("%.6g is at or below the floor of %.6g; too small to matter",
			s.Value, c.FloorValue)
	}
	if c.Direction == config.HigherIsBetter && c.FloorValue > 0 && s.Value >= c.FloorValue {
		// The mirror case: a success rate above the floor is fine whatever
		// the baseline did.
		return true, fmt.Sprintf("%.6g is at or above the floor of %.6g", s.Value, c.FloorValue)
	}

	// Absolute ceiling, applied regardless of baseline.
	if c.Max != nil {
		if c.Direction == config.LowerIsBetter && s.Value > *c.Max {
			return false, fmt.Sprintf("%.6g exceeds the maximum of %.6g", s.Value, *c.Max)
		}
		if c.Direction == config.HigherIsBetter && s.Value < *c.Max {
			return false, fmt.Sprintf("%.6g is below the minimum of %.6g", s.Value, *c.Max)
		}
	}

	// Relative tolerance against the baseline.
	if c.MaxRatio != nil {
		if !s.HasBaseline() {
			// No baseline means the ratio check cannot be evaluated. Passing
			// it is the right answer: the absolute checks above already ran,
			// and treating a missing baseline as a failure turns a
			// first-ever deploy into an automatic rollback.
			return true, "no baseline available; ratio check skipped"
		}
		ratio := s.Value / s.Baseline
		if c.Direction == config.HigherIsBetter {
			ratio = s.Baseline / s.Value
		}
		if ratio > *c.MaxRatio {
			return false, fmt.Sprintf("%.6g is %.2fx the baseline of %.6g, over the %.2fx tolerance",
				s.Value, ratio, s.Baseline, *c.MaxRatio)
		}
		return true, fmt.Sprintf("%.6g is %.2fx the baseline of %.6g, within %.2fx",
			s.Value, ratio, s.Baseline, *c.MaxRatio)
	}

	if c.Max != nil {
		return true, fmt.Sprintf("%.6g is within the maximum of %.6g", s.Value, *c.Max)
	}
	return true, fmt.Sprintf("%.6g passed", s.Value)
}

// VerifyResult carries the verdict plus the evidence for it.
type VerifyResult struct {
	Verdict Verdict
	// Reason is shown in Slack and on the dashboard. A rollback whose reason
	// is "verification failed" teaches nobody anything.
	Reason  string
	Samples int
	// Inconclusive counts verifier errors, which are reported separately from
	// failures because they mean something different.
	InconclusiveCount int
	ConsecutiveFail   int
	Cause             error
}

// sampler is the boundary the verification loop pulls observations through,
// so the loop can be tested without a clock or a metrics backend.
type sampler interface {
	sampleAll(ctx context.Context, dep *store.Deployment, criteria []config.Criterion) (bool, string, error)
}

// Verify runs section 7.3's decision loop.
//
// The loop samples on a ticker until it reaches a verdict or the bake window
// expires. It is written against injected time so that a ten-minute bake is a
// millisecond in a test -- a verification engine that can only be tested in
// real time is a verification engine that is not tested.
func (e *Executor) Verify(ctx context.Context, dep *store.Deployment, p config.VerifyPolicy) VerifyResult {
	if len(p.Criteria) == 0 {
		// Nothing to check is not the same as healthy, but it is what the
		// operator asked for, and refusing here would make a bake-less
		// environment undeployable.
		return VerifyResult{Verdict: VerdictHealthy, Reason: "no verification criteria configured"}
	}

	var (
		consecutiveFail int
		consecutivePass int
		inconclusive    int
		total           int
		lastReason      string
	)

	deadline := e.clock.Now().Add(p.Bake)
	ticker := e.clock.NewTicker(p.SampleInterval)
	defer ticker.Stop()

	for e.clock.Now().Before(deadline) {
		// Checked before the select, because a select with two ready cases
		// picks at random and a cancelled context must win every time.
		if err := ctx.Err(); err != nil {
			return VerifyResult{
				Verdict: VerdictAborted,
				Reason:  "verification was cancelled: " + err.Error(),
				Samples: total, Cause: err,
			}
		}
		select {
		case <-ctx.Done():
			return VerifyResult{
				Verdict: VerdictAborted,
				Reason:  "verification was cancelled: " + ctx.Err().Error(),
				Samples: total, Cause: ctx.Err(),
			}
		case <-ticker.C():
		}

		ok, reason, err := e.sampleAll(ctx, dep, p.Criteria)
		if err != nil {
			// A BROKEN PROMETHEUS IS NOT A FAILING DEPLOY.
			//
			// Section 7.3. Counting this as a failure means a monitoring
			// outage becomes a deployment outage, which is a much larger
			// incident than the one it was trying to prevent.
			inconclusive++
			lastReason = err.Error()
			if inconclusive > p.MaxInconclusive {
				return VerifyResult{
					Verdict: VerdictInconclusive,
					Reason: fmt.Sprintf("%d verifier errors (limit %d); the last was: %s",
						inconclusive, p.MaxInconclusive, err),
					Samples: total, InconclusiveCount: inconclusive, Cause: err,
				}
			}
			continue
		}
		total++
		lastReason = reason

		if ok {
			consecutivePass++
			consecutiveFail = 0
			// MinSamples guards against a fast early verdict on a metric that
			// has not had time to move.
			if consecutivePass >= p.SuccessThreshold && total >= p.MinSamples {
				return VerifyResult{
					Verdict: VerdictHealthy,
					Reason: fmt.Sprintf("%d consecutive healthy samples of %d taken: %s",
						consecutivePass, total, reason),
					Samples: total, InconclusiveCount: inconclusive,
				}
			}
		} else {
			consecutiveFail++
			consecutivePass = 0
			// CONSECUTIVE, NOT CUMULATIVE. One bad sample in a ten-minute
			// bake is noise; three in a row is a signal.
			if consecutiveFail >= p.FailureThreshold {
				return VerifyResult{
					Verdict: VerdictUnhealthy,
					Reason: fmt.Sprintf("%d consecutive failing samples: %s",
						consecutiveFail, reason),
					Samples: total, InconclusiveCount: inconclusive,
					ConsecutiveFail: consecutiveFail,
				}
			}
		}
	}

	// The bake window ended without tripping either threshold.
	if total < p.MinSamples {
		return VerifyResult{
			Verdict: VerdictInconclusive,
			Reason: fmt.Sprintf("only %d samples in the bake window, need %d",
				total, p.MinSamples),
			Samples: total, InconclusiveCount: inconclusive,
		}
	}
	return VerifyResult{
		Verdict: VerdictHealthy,
		Reason:  fmt.Sprintf("survived the %s bake across %d samples: %s", p.Bake, total, lastReason),
		Samples: total, InconclusiveCount: inconclusive,
	}
}

// sampleAll takes one observation per criterion and records it.
//
// All criteria must pass for the sample to pass. A single criterion erroring
// makes the whole sample inconclusive rather than failing, for the same
// reason as above: partial data is not evidence of a bad deploy.
func (e *Executor) sampleAll(ctx context.Context, dep *store.Deployment, criteria []config.Criterion) (bool, string, error) {
	allOK := true
	var firstFailure string
	var summary string

	for _, c := range criteria {
		v, err := e.verifiers.Get(c.Verifier)
		if err != nil {
			return false, "", verifier.Inconclusive("criterion %q: %v", c.Name, err)
		}
		target := verifier.Target{
			App:         dep.App,
			Environment: dep.Environment,
			Version:     dep.Version,
			Previous:    dep.PreviousVersion,
			Settings:    c.Settings,
		}
		s, err := v.Sample(ctx, target, e.sampleWindow)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return false, "", err
			}
			return false, "", verifier.Inconclusive("criterion %q: %v", c.Name, err)
		}

		ok, reason := EvaluateCriterion(c, s)
		if err := e.store.RecordSample(ctx, &store.HealthSample{
			DeploymentID: dep.ID,
			Verifier:     c.Verifier,
			Criterion:    c.Name,
			At:           e.clock.Now(),
			Value:        s.Value,
			Baseline:     s.Baseline,
			Healthy:      ok,
			Note:         reason,
		}); err != nil {
			return false, "", err
		}

		if !ok && allOK {
			allOK = false
			firstFailure = c.Name + ": " + reason
		}
		if summary == "" {
			summary = c.Name + " " + reason
		}
	}

	if !allOK {
		return false, firstFailure, nil
	}
	return true, summary, nil
}

// Clock abstracts time for the verification loop and the lease heartbeat.
//
// Section 19.4 makes clock handling a correctness concern in this system; a
// correctness concern that can only be exercised by sleeping is one that does
// not get exercised.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
	After(d time.Duration) <-chan time.Time
}

// Ticker is the subset of time.Ticker the loop uses.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// RealClock is the production clock.
var RealClock Clock = realClock{}
