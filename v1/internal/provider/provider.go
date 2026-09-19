// Package provider is the plugin point for "put this version onto this
// environment" (design.md section 4).
//
//	"Three plugin points. Keep them small — a small interface is the
//	difference between 'anyone can add a provider' and 'only you understand
//	this.'"
//
// Note what this interface does NOT have: no `DownloadMixedRecording`-style
// escape hatch, no `Rollback`, and no health check. The absences are load
// bearing:
//
//   - There is no Rollback method because a rollback is a deploy of an older
//     version, planned and checked by the engine (section 8). A provider with
//     its own rollback path would bypass the migration floor.
//   - There is no health check because "a verifier never decides to roll
//     back" (section 4). Scattering that decision into providers is how these
//     tools become unpredictable.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrUnsupported is returned by Shift for providers with no traffic control.
// It is a normal answer, not a failure: the engine uses it to reject a canary
// strategy at preflight rather than halfway through a rollout.
var ErrUnsupported = errors.New("provider: operation not supported")

// Target names what is being deployed to.
type Target struct {
	App         string
	Environment string
	Provider    string
	// Config is the provider block from orch.yaml, already validated.
	Config map[string]string
}

func (t Target) Resource() string { return "app:" + t.App + "/env:" + t.Environment }

func (t Target) String() string { return t.App + "/" + t.Environment }

// Version identifies an artifact.
type Version struct {
	ID string
	// Unknown is set when the provider could reach the environment but could
	// not determine what is running. Distinct from an error, and distinct
	// from a version: reconciliation treats it as "do not guess about
	// production" (section 17.2).
	Unknown bool
}

func (v Version) String() string {
	if v.Unknown {
		return "unknown"
	}
	return v.ID
}

// DeployRequest is one deployment as the provider sees it.
type DeployRequest struct {
	Target  Target
	Version string
	// Previous is what was running before, so a provider that needs the old
	// identity (to keep a blue-green slot, say) has it without querying.
	Previous string
	// IdempotencyKey makes Deploy safe to call twice.
	//
	//	"Deploy is idempotent: calling it twice with the same
	//	(target, version, idempotencyKey) must not deploy twice."
	IdempotencyKey string
	// Fence is the lease fence token. Providers that can carry it into the
	// target system should -- an ECS task-definition tag, a Kubernetes
	// annotation, an SSH lock file -- so that even a resumed zombie process
	// cannot clobber a newer deploy (section 6.2).
	Fence int64
	// Strategy and BatchSize describe how to roll, for providers that care.
	Strategy  string
	BatchSize string
	// IsRollback tells a provider it is going backwards, which some platforms
	// can do more cheaply (a Lambda alias flip, a blue-green swap back).
	IsRollback bool
}

// DeployResult is what the provider did.
type DeployResult struct {
	// Handle is opaque to the orchestrator and meaningful to the provider: an
	// ECS deployment id, a Lambda version, an SSH run id. It is what
	// reconciliation asks about after a crash.
	Handle string
	// AlreadyDone is true when the idempotency key matched a previous call.
	// The engine records this rather than treating it as a fresh deploy, so
	// a retried CI job does not double-count in the metrics.
	AlreadyDone bool
	// Instances is how many targets were updated, for the log.
	Instances int
}

// Capabilities describes what a provider can do, so the engine can refuse an
// impossible configuration at preflight rather than discovering it halfway
// through a rollout.
//
// This is a separate method rather than a probe call for a reason worth
// stating: the obvious implementation of "can this provider shift traffic?"
// is to call Shift and see whether it returns ErrUnsupported -- and that
// shifts traffic. On a canary target it moves 100% of production onto a
// version that has not been deployed yet. Asking a question must not be an
// action.
type Capabilities struct {
	// TrafficShifting is required by the canary and blue-green strategies.
	TrafficShifting bool
	// Abort reports whether an in-flight deploy can be stopped.
	Abort bool
}

// Provider knows how to put a version onto an environment.
type Provider interface {
	Name() string

	// Capabilities is a pure query. It must have no side effects.
	Capabilities() Capabilities

	// Validate checks config at load time, before any deploy runs.
	//
	// Called by `orch config validate`, so a missing cluster name is a CI
	// failure rather than a preflight failure at 2am.
	Validate(t Target) error

	// Current returns the version currently deployed, read from the real
	// environment -- never from our database.
	//
	// This is the method reconciliation depends on, and the reason the
	// interface exists at all: "never trust the DB alone" (section 2.2).
	Current(ctx context.Context, t Target) (Version, error)

	// Deploy is idempotent on (target, version, idempotencyKey).
	//
	// `out` receives provider output. It is already wrapped in a redactor by
	// the engine, so anything the target prints is scrubbed before it reaches
	// the database (section 12.3).
	Deploy(ctx context.Context, req DeployRequest, out io.Writer) (DeployResult, error)

	// Shift moves traffic for strategies that support it. Providers without
	// traffic control return ErrUnsupported.
	Shift(ctx context.Context, t Target, weight int) error

	// Abort stops an in-flight deploy as cleanly as the platform allows.
	Abort(ctx context.Context, t Target, handle string) error
}

// Registry maps names to providers.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{providers: map[string]Provider{}}
	for _, p := range ps {
		r.providers[p.Name()] = p
	}
	return r
}

func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider: %q is not registered (have: %s)", name, r.namesString())
	}
	return p, nil
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}

func (r *Registry) namesString() string {
	names := r.Names()
	if len(names) == 0 {
		return "none"
	}
	s := names[0]
	for _, n := range names[1:] {
		s += ", " + n
	}
	return s
}

// SupportsTrafficShifting reports whether a provider can do canary or
// blue-green.
//
// Checked at preflight rather than discovered at step three of a canary,
// which is the difference between a clean refusal and a half-shifted
// production environment.
func SupportsTrafficShifting(p Provider) bool {
	return p.Capabilities().TrafficShifting
}

// Timeout is a convenience for providers that need a deadline and were not
// given one.
func Timeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
