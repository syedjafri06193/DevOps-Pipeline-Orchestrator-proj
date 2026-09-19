// Package fake is the provider the tests use (design.md section 18.1).
//
//	"The fake provider is your most important test fixture."
//
//	"Being able to say 'fail on the third instance of a rolling deploy, then
//	hang for 90 seconds on abort' in a unit test is worth more than any number
//	of integration tests against real cloud APIs."
//
// So the failure injection is the point of this package, not an afterthought:
// deploys that fail, deploys that hang, deploys that succeed on some
// instances and fail on others, environments that drift underneath us, and
// providers that cannot shift traffic at all.
//
// DeployCount is what makes idempotency assertable. A test that says "retry
// this and check it did not deploy twice" needs a provider that counts.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/syedjafri06193/orch/internal/provider"
)

// Provider is an in-memory deployment target.
type Provider struct {
	mu sync.Mutex

	// versions maps a target resource to what is "running" on it.
	versions map[string]string
	// weights maps a target resource to the traffic weight on the new version.
	weights map[string]int
	// handles maps an idempotency key to the handle returned for it, which is
	// how Deploy becomes idempotent.
	handles map[string]provider.DeployResult

	// ---- failure injection ----

	// FailOn maps a step name to the error to return. "deploy", "shift",
	// "current" and "abort" are the step names.
	FailOn map[string]error
	// HangOn makes a step block for a duration, so a test can exercise
	// context cancellation and lease loss.
	HangOn map[string]time.Duration
	// PartialDeploy makes Deploy report that it updated some instances and
	// then failed, which is the case that leaves an environment in a mixed
	// state and is the hardest one to reconcile.
	PartialDeploy bool
	// InstanceCount is how many instances a rolling deploy touches.
	InstanceCount int
	// FailAfterInstance fails partway through, after this many instances.
	FailAfterInstance int
	// NoTrafficControl makes Shift return ErrUnsupported, like an SSH or
	// recreate target.
	NoTrafficControl bool
	// DriftTo makes Current report a version nobody deployed, which is what
	// reconciliation's "stop and tell a human" branch is for.
	DriftTo string
	// CurrentUnknown makes Current succeed but report that it cannot tell.
	CurrentUnknown bool
	// SecretOutput is written to the deploy log, so the redaction tests have
	// a provider that leaks.
	SecretOutput string

	// ---- observation ----

	// DeployCount counts calls per (target, version), for idempotency
	// assertions.
	DeployCount map[string]int
	// ShiftLog records every traffic weight in order, so a canary's shape can
	// be asserted.
	ShiftLog []int
	// AbortCount counts Abort calls.
	AbortCount int
	// Deployed records every DeployRequest, for asserting the fence token
	// reached the provider.
	Deployed []provider.DeployRequest
}

func New() *Provider {
	return &Provider{
		versions:      map[string]string{},
		weights:       map[string]int{},
		handles:       map[string]provider.DeployResult{},
		FailOn:        map[string]error{},
		HangOn:        map[string]time.Duration{},
		DeployCount:   map[string]int{},
		InstanceCount: 3,
	}
}

func (p *Provider) Name() string { return "fake" }

func (p *Provider) Capabilities() provider.Capabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return provider.Capabilities{TrafficShifting: !p.NoTrafficControl, Abort: true}
}

func (p *Provider) Validate(t provider.Target) error {
	if v, ok := t.Config["invalid"]; ok {
		return fmt.Errorf("fake: config says invalid=%s", v)
	}
	return nil
}

// SetCurrent seeds what is running, without recording a deploy.
func (p *Provider) SetCurrent(t provider.Target, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.versions[t.Resource()] = version
}

// CurrentVersion reads the fake environment directly, for assertions.
func (p *Provider) CurrentVersion(t provider.Target) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.versions[t.Resource()]
}

func (p *Provider) Weight(t provider.Target) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.weights[t.Resource()]
}

func (p *Provider) Current(ctx context.Context, t provider.Target) (provider.Version, error) {
	if err := p.step(ctx, "current"); err != nil {
		return provider.Version{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.CurrentUnknown {
		return provider.Version{Unknown: true}, nil
	}
	if p.DriftTo != "" {
		return provider.Version{ID: p.DriftTo}, nil
	}
	return provider.Version{ID: p.versions[t.Resource()]}, nil
}

func (p *Provider) Deploy(ctx context.Context, req provider.DeployRequest, out io.Writer) (provider.DeployResult, error) {
	p.mu.Lock()
	if req.IdempotencyKey != "" {
		if prev, ok := p.handles[req.IdempotencyKey]; ok {
			p.mu.Unlock()
			fmt.Fprintf(out, "deploy %s: already applied under key %s\n", req.Version, req.IdempotencyKey)
			prev.AlreadyDone = true
			return prev, nil
		}
	}
	p.Deployed = append(p.Deployed, req)
	key := req.Target.Resource() + "@" + req.Version
	p.DeployCount[key]++
	instances := p.InstanceCount
	failAfter := p.FailAfterInstance
	partial := p.PartialDeploy
	secret := p.SecretOutput
	p.mu.Unlock()

	if err := p.step(ctx, "deploy"); err != nil {
		return provider.DeployResult{}, err
	}

	fmt.Fprintf(out, "deploying %s to %s (strategy=%s fence=%d)\n",
		req.Version, req.Target, req.Strategy, req.Fence)
	if secret != "" {
		// A real target prints things it should not. This is what the
		// redacting writer is for.
		fmt.Fprintf(out, "connecting with token %s\n", secret)
	}

	for i := 1; i <= instances; i++ {
		if err := ctx.Err(); err != nil {
			return provider.DeployResult{}, err
		}
		if partial && failAfter > 0 && i > failAfter {
			fmt.Fprintf(out, "instance %d/%d: FAILED\n", i, instances)
			return provider.DeployResult{Instances: i - 1},
				fmt.Errorf("fake: instance %d of %d failed; %d instances are on %s and %d are on %s",
					i, instances, i-1, req.Version, instances-i+1, req.Previous)
		}
		fmt.Fprintf(out, "instance %d/%d: ok\n", i, instances)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.versions[req.Target.Resource()] = req.Version
	result := provider.DeployResult{
		Handle:    fmt.Sprintf("fake-%s-%s", req.Target.App, req.Version),
		Instances: instances,
	}
	if req.IdempotencyKey != "" {
		p.handles[req.IdempotencyKey] = result
	}
	fmt.Fprintf(out, "deployed %s\n", req.Version)
	return result, nil
}

func (p *Provider) Shift(ctx context.Context, t provider.Target, weight int) error {
	p.mu.Lock()
	unsupported := p.NoTrafficControl
	p.mu.Unlock()
	if unsupported {
		return provider.ErrUnsupported
	}
	if err := p.step(ctx, "shift"); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.weights[t.Resource()] = weight
	p.ShiftLog = append(p.ShiftLog, weight)
	return nil
}

func (p *Provider) Abort(ctx context.Context, t provider.Target, handle string) error {
	if err := p.step(ctx, "abort"); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.AbortCount++
	return nil
}

// step applies the configured hang and failure for a named step.
//
// The hang respects context cancellation, so a test can assert that a lost
// lease actually interrupts a provider call rather than waiting for it.
func (p *Provider) step(ctx context.Context, name string) error {
	p.mu.Lock()
	hang := p.HangOn[name]
	err := p.FailOn[name]
	p.mu.Unlock()

	if hang > 0 {
		select {
		case <-time.After(hang):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// Counts returns a copy of the deploy counter, for assertions.
func (p *Provider) Counts() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.DeployCount))
	for k, v := range p.DeployCount {
		out[k] = v
	}
	return out
}

// Shifts returns a copy of the traffic weight log.
func (p *Provider) Shifts() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.ShiftLog...)
}

// Requests returns a copy of every DeployRequest received.
func (p *Provider) Requests() []provider.DeployRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.DeployRequest(nil), p.Deployed...)
}
