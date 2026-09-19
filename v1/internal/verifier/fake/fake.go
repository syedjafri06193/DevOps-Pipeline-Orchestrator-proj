// Package fake is a scripted verifier for tests.
//
// It exists to make the three cases in section 7.3's acceptance criterion
// testable without a metrics stack:
//
//	"a deliberately broken deploy is detected, a deliberate single-sample blip
//	is *not*, and killing Prometheus produces Inconclusive rather than a
//	rollback."
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/syedjafri06193/orch/internal/verifier"
)

// Verifier replays a scripted series of samples.
type Verifier struct {
	mu sync.Mutex

	// Values is the series returned, one per Sample call. When it runs out,
	// the last value repeats -- so a test can say "two bad samples then
	// steady" without padding the slice to the length of the bake window.
	Values []float64
	// Baseline is returned with every sample. Zero means "no baseline",
	// which disables ratio checks.
	Baseline float64
	// FailAt makes the Nth call (1-based) return an error, which the engine
	// must treat as Inconclusive rather than as a failing sample.
	FailAt map[int]error
	// ValidateErr is returned by Validate, for config tests.
	ValidateErr error

	calls int
}

func New(values ...float64) *Verifier {
	return &Verifier{Values: values, FailAt: map[int]error{}}
}

func (v *Verifier) Name() string { return "fake" }

func (v *Verifier) Validate(settings map[string]string) error { return v.ValidateErr }

func (v *Verifier) Sample(ctx context.Context, t verifier.Target, window time.Duration) (verifier.Sample, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.calls++
	if err, ok := v.FailAt[v.calls]; ok {
		return verifier.Sample{}, err
	}
	if len(v.Values) == 0 {
		return verifier.Sample{}, verifier.Inconclusive("fake: no values scripted")
	}
	idx := v.calls - 1
	if idx >= len(v.Values) {
		idx = len(v.Values) - 1
	}
	return verifier.Sample{
		Value:    v.Values[idx],
		Baseline: v.Baseline,
		At:       time.Now().UTC(),
		Note:     fmt.Sprintf("fake sample %d", v.calls),
	}, nil
}

// Calls reports how many observations were taken, so a test can assert that
// the engine stopped sampling when it reached a verdict rather than running
// the full bake window.
func (v *Verifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}
