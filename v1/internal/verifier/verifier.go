// Package verifier answers "is the thing we just deployed healthy?"
// (design.md section 7).
//
// The single most important line in the interface is a comment:
//
//	"A verifier never decides to roll back. It produces observations. The
//	rollback decision lives in one place in the engine, where it can be
//	tested, tuned, and reasoned about. Scattering rollback logic into
//	provider-specific health checks is how these tools become unpredictable."
//
// So `Sample` returns a number and a baseline. It does not return a boolean,
// it does not return a verdict, and it has no access to the policy. Whether
// that number is acceptable is decided in engine/verify.go, once, for every
// verifier.
package verifier

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sample is one observation.
type Sample struct {
	Value float64
	// Baseline is what the same metric reads for the thing we are comparing
	// against. Zero means "no baseline available", which the criterion
	// treats as "ratio checks do not apply" rather than as "baseline is 0"
	// -- dividing by a missing baseline is how a ratio check becomes
	// infinite and rolls back a healthy deploy.
	Baseline float64
	At       time.Time
	// Note explains where the number came from, so a health chart is
	// interpretable six weeks later during a postmortem.
	Note string
}

// HasBaseline reports whether a ratio comparison is meaningful.
func (s Sample) HasBaseline() bool { return s.Baseline > 0 }

// Target is what to observe. Version is included because a canary criterion
// has to be able to query the *new version's* metrics specifically -- section
// 9's warning that untagged metrics make canary analysis meaningless.
type Target struct {
	App         string
	Environment string
	Version     string
	Previous    string
	// Settings are the criterion's verifier-specific keys from orch.yaml.
	Settings map[string]string
}

// Verifier produces observations.
type Verifier interface {
	Name() string
	// Validate checks a criterion's settings at config load time, so a
	// missing URL or an unparseable query is a CI failure rather than an
	// Inconclusive verdict during a deploy.
	Validate(settings map[string]string) error
	// Sample returns one observation over the given window.
	Sample(ctx context.Context, t Target, window time.Duration) (Sample, error)
}

// ErrInconclusive means the verifier could not produce an observation.
//
// This is the distinction section 7.3 insists on: "A broken Prometheus is NOT
// a failing deploy." A verifier returning this must not be counted as a
// failing sample, because "we do not know" and "it is bad" call for opposite
// actions.
var ErrInconclusive = errors.New("verifier: no observation available")

// Inconclusive wraps a cause as an inconclusive result.
func Inconclusive(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInconclusive, fmt.Sprintf(format, args...))
}

// Registry maps names to verifiers.
type Registry struct {
	verifiers map[string]Verifier
}

func NewRegistry(vs ...Verifier) *Registry {
	r := &Registry{verifiers: map[string]Verifier{}}
	for _, v := range vs {
		r.verifiers[v.Name()] = v
	}
	return r
}

func (r *Registry) Get(name string) (Verifier, error) {
	v, ok := r.verifiers[name]
	if !ok {
		return nil, fmt.Errorf("verifier: %q is not registered", name)
	}
	return v, nil
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.verifiers))
	for n := range r.verifiers {
		out = append(out, n)
	}
	return out
}
