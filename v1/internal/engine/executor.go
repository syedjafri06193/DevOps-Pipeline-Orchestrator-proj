package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/provider"
	"github.com/syedjafri06193/orch/internal/redact"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/verifier"
)

// Event is what goes into the outbox and out to notifiers.
type Event struct {
	Type         string      `json:"type"`
	DeploymentID string      `json:"deployment_id"`
	App          string      `json:"app"`
	Environment  string      `json:"environment"`
	Version      string      `json:"version"`
	Previous     string      `json:"previous,omitempty"`
	State        store.State `json:"state"`
	Actor        string      `json:"actor,omitempty"`
	Strategy     string      `json:"strategy,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	// Pages marks the events that should break through the noise.
	Pages  bool              `json:"pages,omitempty"`
	Detail map[string]string `json:"detail,omitempty"`
	At     time.Time         `json:"at"`
}

func (e Event) Resource() string { return "app:" + e.App + "/env:" + e.Environment }

// Executor runs one deployment plan through the state machine.
type Executor struct {
	store     *store.Store
	cfg       *config.Config
	providers *provider.Registry
	verifiers *verifier.Registry
	manifests Manifests
	clock     Clock
	log       *slog.Logger

	nodeID string
	// notifiers are the outbox destinations every event is addressed to.
	notifiers []string
	// sampleWindow is how far back a verifier looks for each observation.
	sampleWindow time.Duration
	// secrets are literal values redacted out of provider output.
	secrets []string

	// hooks let the chaos tests kill the process at a chosen state.
	hooks Hooks
}

// Hooks are test seams. In production every field is nil.
type Hooks struct {
	// BeforeSideEffect runs after a state has been persisted and before the
	// work for that state begins. Returning an error simulates a crash at
	// exactly the point section 5 says matters: "Persist the transition, then
	// do the thing."
	BeforeSideEffect func(state store.State, dep *store.Deployment) error
	// AfterSideEffect runs once the work is done and before the next
	// transition, which is the other half of the crash window.
	AfterSideEffect func(state store.State, dep *store.Deployment) error
}

// Options configure an Executor.
type Options struct {
	Store     *store.Store
	Config    *config.Config
	Providers *provider.Registry
	Verifiers *verifier.Registry
	Manifests Manifests
	Clock     Clock
	Logger    *slog.Logger
	NodeID    string
	// SampleWindow defaults to 2 minutes, matching the rate() windows in the
	// document's example queries.
	SampleWindow time.Duration
	Secrets      []string
	Hooks        Hooks
	// Notifiers are the outbox destinations events are written for. Which
	// ones exist depends on what is configured, and an entry addressed to a
	// notifier the server does not have is discarded on the way out -- so a
	// hardcoded list here means the dashboard silently receives nothing the
	// moment Slack is not set up.
	Notifiers []string
}

func New(o Options) *Executor {
	if o.Clock == nil {
		o.Clock = RealClock
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.NodeID == "" {
		o.NodeID = "node-1"
	}
	if o.SampleWindow == 0 {
		o.SampleWindow = 2 * time.Minute
	}
	if len(o.Notifiers) == 0 {
		// The dashboard is always there: it is compiled into the same binary,
		// so there is no configuration under which it is absent.
		o.Notifiers = []string{"dashboard"}
	}
	return &Executor{
		store: o.Store, cfg: o.Config, providers: o.Providers,
		verifiers: o.Verifiers, manifests: o.Manifests, clock: o.Clock,
		log: o.Logger, nodeID: o.NodeID, sampleWindow: o.SampleWindow,
		secrets: o.Secrets, hooks: o.Hooks, notifiers: o.Notifiers,
	}
}

// ErrLeaseLost is returned when the heartbeat could not renew.
var ErrLeaseLost = errors.New("engine: lease lost; aborting rather than operating on a resource we no longer own")

// Run drives one deployment to a terminal state (section 17.1).
//
// The ordering rules from section 5 are all here and all load bearing:
//
//   - the lease is taken first, and lost at any point means abort
//   - every transition is persisted BEFORE the side effect it describes
//   - failure handling runs on a context.WithoutCancel, because the deploy
//     context is usually already cancelled by the time we get here
func (e *Executor) Run(ctx context.Context, depID string) error {
	dep, err := e.store.Deployment(ctx, depID)
	if err != nil {
		return err
	}
	if dep.State.IsTerminal() {
		return fmt.Errorf("engine: deployment %s is already %s", depID, dep.State)
	}

	cfgEnv, err := e.cfg.AppEnv(dep.App, dep.Environment)
	if err != nil {
		return err
	}

	fence, err := e.store.AcquireLease(ctx, dep.Resource(), dep.ID, e.nodeID)
	if err != nil {
		return err
	}
	dep.Fence = fence

	// Renew in the background; cancel the work if we lose the lease.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := make(chan error, 1)
	go e.heartbeat(ctx, dep, cancel, lost)

	// context.WithoutCancel: releasing the lease has to happen even when the
	// deploy context is already dead, or the resource stays locked until the
	// TTL expires and nobody can deploy in the meantime.
	defer func() {
		if err := e.store.ReleaseLease(context.WithoutCancel(ctx), dep.Resource(), dep.ID); err != nil {
			e.log.Error("releasing lease", "resource", dep.Resource(), "error", err)
		}
	}()

	steps := []struct {
		state store.State
		fn    func(context.Context, *store.Deployment, *config.AppEnv) error
	}{
		{store.StatePreflight, e.preflight},
		{store.StateAwaitApproval, e.awaitGates},
		{store.StateDeploying, e.deploy},
		{store.StateVerifying, e.verify},
		{store.StatePromoting, e.promote},
	}

	for _, s := range steps {
		if s.state == store.StateAwaitApproval && !needsApproval(cfgEnv) {
			continue
		}
		// PERSIST THE TRANSITION BEFORE PERFORMING THE SIDE EFFECT.
		if err := e.transitionAndNotify(ctx, dep, s.state, e.eventFor(dep, s.state, "")); err != nil {
			return e.handleFailure(context.WithoutCancel(ctx), dep, cfgEnv, err)
		}
		if e.hooks.BeforeSideEffect != nil {
			if err := e.hooks.BeforeSideEffect(s.state, dep); err != nil {
				return err
			}
		}
		if err := s.fn(ctx, dep, cfgEnv); err != nil {
			if lostErr := drain(lost); lostErr != nil {
				err = fmt.Errorf("%w (%v)", ErrLeaseLost, err)
			}
			return e.handleFailure(context.WithoutCancel(ctx), dep, cfgEnv, err)
		}
		if e.hooks.AfterSideEffect != nil {
			if err := e.hooks.AfterSideEffect(s.state, dep); err != nil {
				return err
			}
		}
	}

	return e.transitionAndNotify(ctx, dep, store.StateSucceeded, e.eventFor(dep, store.StateSucceeded, ""))
}

func drain(ch chan error) error {
	select {
	case err := <-ch:
		return err
	default:
		return nil
	}
}

func needsApproval(cfg *config.AppEnv) bool {
	for _, g := range cfg.Gates {
		if g.Type == config.GateApproval {
			return true
		}
	}
	return false
}

// heartbeat renews the lease and cancels the work when renewal fails.
//
// Section 6.1: "If renewal fails (the process was partitioned, or someone
// force-broke the lease), the executor must abort rather than keep operating
// on a resource it no longer owns."
func (e *Executor) heartbeat(ctx context.Context, dep *store.Deployment, cancel context.CancelFunc, lost chan<- error) {
	ticker := e.clock.NewTicker(store.LeaseHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := e.store.RenewLease(ctx, dep.Resource(), dep.ID, dep.Fence); err != nil {
				if ctx.Err() != nil {
					return
				}
				e.log.Error("lost the lease; aborting the deployment",
					"deployment", dep.ID, "resource", dep.Resource(), "error", err)
				select {
				case lost <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}

// ------------------------------------------------------------------ steps

// preflight validates everything that can be checked before touching
// production (section 5: "config valid, artifact exists, provider reachable,
// prior deploy clean").
func (e *Executor) preflight(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	e.step(ctx, dep, 1, "preflight", func() error {
		return nil
	})

	if f, frozen := e.store.Freeze(ctx, dep.Resource()); frozen {
		return &BlockedError{Reason: fmt.Sprintf(
			"deploys to %s are frozen: %s (set by %s at %s)",
			dep.Resource(), f.Reason, f.By, f.At.Format(time.RFC3339))}
	}

	p, err := e.providers.Get(cfg.Provider)
	if err != nil {
		return err
	}
	target := targetOf(cfg)
	if err := p.Validate(target); err != nil {
		return fmt.Errorf("provider config is invalid: %w", err)
	}

	// A canary or blue-green strategy against a provider that cannot shift
	// traffic is caught here rather than at step three of a rollout, with
	// production half-shifted.
	if cfg.Strategy.Type == config.StrategyCanary || cfg.Strategy.Type == config.StrategyBlueGreen {
		if !provider.SupportsTrafficShifting(p) {
			return fmt.Errorf(
				"strategy %q needs traffic shifting and provider %q does not support it",
				cfg.Strategy.Type, cfg.Provider)
		}
	}

	// What is actually running, read from the environment. This is the
	// rollback target and the verification baseline, and taking it from our
	// own database instead is how the two drift apart.
	current, err := p.Current(ctx, target)
	if err != nil {
		return fmt.Errorf("could not read the current version from %s: %w", cfg.Provider, err)
	}
	if current.Unknown {
		return fmt.Errorf("provider %q cannot determine what is currently deployed to %s; refusing to deploy over an unknown state",
			cfg.Provider, target)
	}

	if current.ID == dep.Version && !dep.IsRollback {
		return &BlockedError{Reason: fmt.Sprintf(
			"%s is already running %s; nothing to do", target, dep.Version)}
	}

	// The circuit breaker: a version rolled back twice is not deployed again
	// without an override.
	if !dep.IsRollback {
		if err := e.CheckRollbackBudget(ctx, dep.App, dep.Environment, dep.Version, cfg.Rollback.MaxAttempts); err != nil {
			return err
		}
	}

	// promote_from enforces build-once-deploy-many: this exact artifact must
	// have succeeded in the upstream environment.
	if cfg.PromoteFrom != "" && !dep.IsRollback {
		if err := e.checkPromotion(ctx, dep, cfg); err != nil {
			return err
		}
	}

	dep.PreviousVersion = current.ID
	return e.store.UpdateDeployment(ctx, dep)
}

func (e *Executor) checkPromotion(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	upstream, err := e.store.ListDeployments(ctx, store.ListFilter{
		App: dep.App, Environment: cfg.PromoteFrom,
		States: []store.State{store.StateSucceeded}, Limit: 200,
	})
	if err != nil {
		return err
	}
	for _, u := range upstream {
		if u.Version == dep.Version {
			return nil
		}
	}
	return &BlockedError{Reason: fmt.Sprintf(
		"version %s has not succeeded in %s, which %s promotes from. "+
			"Rebuilding per environment means prod runs a binary that was never tested "+
			"(design.md section 13). Deploy it to %s first.",
		dep.Version, cfg.PromoteFrom, dep.Environment, cfg.PromoteFrom)}
}

// awaitGates blocks until every gate is satisfied (section 10.3).
func (e *Executor) awaitGates(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	for _, g := range cfg.Gates {
		switch g.Type {
		case config.GateSchedule:
			if err := e.checkSchedule(g); err != nil {
				return err
			}
		case config.GateFreeze:
			if f, frozen := e.store.Freeze(ctx, dep.Resource()); frozen {
				return &BlockedError{Reason: "frozen: " + f.Reason}
			}
		case config.GateApproval:
			if err := e.waitForApproval(ctx, dep, cfg, g); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Executor) checkSchedule(g config.Gate) error {
	loc, err := time.LoadLocation(g.Timezone)
	if err != nil {
		return fmt.Errorf("schedule gate: %w", err)
	}
	now := e.clock.Now().In(loc)
	for _, w := range g.Deny {
		if w.Contains(now) {
			return &BlockedError{Reason: fmt.Sprintf(
				"deploys are not allowed during %q (%s); it is currently %s",
				w.Raw, g.Timezone, now.Format("Mon 15:04"))}
		}
	}
	return nil
}

// waitForApproval blocks until someone with the right permission approves, or
// the approval window expires.
func (e *Executor) waitForApproval(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv, g config.Gate) error {
	deadline := dep.CreatedAt.Add(g.TTL)

	for {
		approvals, err := e.store.Approvals(ctx, dep.ID)
		if err != nil {
			return err
		}
		for _, a := range approvals {
			if err := e.ApprovalIsValid(dep, cfg, g, a.User, a.At); err == nil {
				e.log.Info("approved", "deployment", dep.ID, "by", a.User)
				return nil
			}
		}

		if !e.clock.Now().Before(deadline) {
			// Section 10.3's fourth check: "Approvals expire. A stale button
			// click is not consent."
			return &BlockedError{Reason: fmt.Sprintf(
				"no valid approval within %s; this request has expired", g.TTL)}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.clock.After(2 * time.Second):
		}
	}
}

// ApprovalIsValid applies section 10.3's four checks.
//
// Exported because the Slack handler applies exactly the same checks before
// recording an approval. One implementation, called from both places: if the
// two drifted, the button and the gate would disagree about who may approve.
func (e *Executor) ApprovalIsValid(dep *store.Deployment, cfg *config.AppEnv, g config.Gate,
	user string, at time.Time) error {

	// 1. The user must exist in our identity table. A Slack display name is
	// not an identity.
	id, ok := e.cfg.Identities[user]
	if !ok || id.Disabled {
		return fmt.Errorf("%q is not a known orchestrator user", user)
	}

	// 2. Check the real permission in the real RBAC system.
	if !e.cfg.Can(id, "deploy:approve", dep.Environment) {
		return fmt.Errorf("%q does not have deploy:approve on %s", user, dep.Environment)
	}

	// The gate may narrow further to specific roles.
	if len(g.Roles) > 0 && !holdsAnyRole(id, g.Roles) {
		return fmt.Errorf("%q does not hold any of the roles this gate requires (%s)",
			user, strings.Join(g.Roles, ", "))
	}

	// 3. No self-approval where the environment or the gate requires a peer.
	requirePeer := g.RequirePeer || e.cfg.EnvPolicyFor(dep.Environment).RequirePeerApproval
	if requirePeer && dep.TriggeredBy == user {
		return fmt.Errorf("%s requires approval from someone other than the person who triggered it", dep.Environment)
	}

	// 4. Approvals expire.
	if at.After(dep.CreatedAt.Add(g.TTL)) {
		return fmt.Errorf("this approval request expired at %s",
			dep.CreatedAt.Add(g.TTL).Format(time.RFC3339))
	}
	return nil
}

func holdsAnyRole(id *config.Identity, roles []string) bool {
	for _, want := range roles {
		for _, have := range id.Roles {
			if want == have {
				return true
			}
		}
	}
	return false
}

// deploy runs the strategy.
func (e *Executor) deploy(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	p, err := e.providers.Get(cfg.Provider)
	if err != nil {
		return err
	}

	var raw bytes.Buffer
	out := redact.New(&raw, e.secrets...)

	req := provider.DeployRequest{
		Target:         targetOf(cfg),
		Version:        dep.Version,
		Previous:       dep.PreviousVersion,
		IdempotencyKey: dep.ID, // stable across retries of this deployment
		Fence:          dep.Fence,
		Strategy:       string(cfg.Strategy.Type),
		BatchSize:      cfg.Strategy.BatchSize,
		IsRollback:     dep.IsRollback,
	}

	result, deployErr := p.Deploy(ctx, req, out)
	_ = out.Flush()

	step := &store.Step{
		DeploymentID: dep.ID, Seq: 2, Name: "deploy",
		Output: raw.String(),
	}
	started := e.clock.Now()
	step.StartedAt = &started
	finished := e.clock.Now()
	step.FinishedAt = &finished

	if deployErr != nil {
		step.State = "failed"
		step.Error = redact.String(deployErr.Error(), e.secrets...)
		_ = e.store.RecordStep(ctx, step)
		return deployErr
	}
	step.State = "ok"
	if err := e.store.RecordStep(ctx, step); err != nil {
		return err
	}

	dep.ProviderHandle = result.Handle
	if err := e.store.UpdateDeployment(ctx, dep); err != nil {
		return err
	}

	// Blue-green and canary need traffic moved; recreate and rolling do not.
	switch cfg.Strategy.Type {
	case config.StrategyBlueGreen:
		// The flip is a single atomic operation, which is why section 9 calls
		// this the best rollback story.
		if err := p.Shift(ctx, targetOf(cfg), 100); err != nil && !errors.Is(err, provider.ErrUnsupported) {
			return fmt.Errorf("shifting traffic to the new version: %w", err)
		}
	case config.StrategyCanary:
		// The canary's own steps run during verification, below.
	}
	return nil
}

// verify runs the bake, or the canary's stepped rollout.
func (e *Executor) verify(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	if cfg.Strategy.Type == config.StrategyCanary {
		return e.runCanary(ctx, dep, cfg)
	}
	result := e.Verify(ctx, dep, cfg.Verify)
	return e.actOnVerdict(dep, result)
}

func (e *Executor) actOnVerdict(dep *store.Deployment, r VerifyResult) error {
	switch r.Verdict {
	case VerdictHealthy:
		return nil
	case VerdictUnhealthy:
		return &VerificationFailed{Result: r}
	case VerdictInconclusive:
		// Section 7.3: "for prod, that should mean 'hold and ask a human',
		// not 'roll back'." An inconclusive verdict is a failure that does
		// NOT trigger an automatic rollback -- handleFailure checks for it.
		return &VerificationInconclusive{Result: r}
	default:
		return &VerificationFailed{Result: r}
	}
}

// VerificationFailed means the SLIs said the deploy is bad.
type VerificationFailed struct{ Result VerifyResult }

func (e *VerificationFailed) Error() string { return "verification failed: " + e.Result.Reason }

// VerificationInconclusive means we could not tell.
//
// A distinct type because it must not be treated as a failing deploy: section
// 7.3, "Rolling back on missing data is how a monitoring outage becomes a
// deployment outage."
type VerificationInconclusive struct{ Result VerifyResult }

func (e *VerificationInconclusive) Error() string {
	return "verification inconclusive: " + e.Result.Reason
}

// runCanary walks the configured steps (section 9).
func (e *Executor) runCanary(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	p, err := e.providers.Get(cfg.Provider)
	if err != nil {
		return err
	}
	target := targetOf(cfg)

	for i, step := range cfg.Strategy.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case step.SetWeight != nil:
			if err := p.Shift(ctx, target, *step.SetWeight); err != nil {
				return fmt.Errorf("canary step %d: shifting to %d%%: %w", i+1, *step.SetWeight, err)
			}
			e.log.Info("canary weight", "deployment", dep.ID, "weight", *step.SetWeight)

		case step.Verify != nil:
			policy := cfg.Verify
			if step.Verify.Bake > 0 {
				policy.Bake = step.Verify.Bake
			}
			result := e.Verify(ctx, dep, policy)
			if err := e.actOnVerdict(dep, result); err != nil {
				return fmt.Errorf("canary step %d: %w", i+1, err)
			}
		}
	}
	return nil
}

// promote finalises the deployment.
func (e *Executor) promote(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) error {
	if cfg.Strategy.Type == config.StrategyCanary {
		// The last canary step already set the weight to 100, which the
		// config loader enforces.
		return nil
	}
	return nil
}

// ------------------------------------------------------------- failure

// handleFailure is section 17.1's failure path.
func (e *Executor) handleFailure(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv, cause error) error {
	current, err := e.store.Deployment(ctx, dep.ID)
	if err == nil {
		dep.State = current.State
	}
	e.recordError(ctx, dep, cause)

	// A FAILED ROLLBACK NEVER TRIGGERS ANOTHER ROLLBACK.
	//
	// Section 5: "The alternative is an infinite loop that thrashes
	// production." This is the first check, before any policy lookup, so no
	// configuration can turn it off.
	if dep.IsRollback {
		_ = e.transitionAndNotify(ctx, dep, store.StateRollbackFailed,
			e.pageEvent(dep, cause))
		return cause
	}

	// A blocked deploy never happened, so there is nothing to roll back.
	var blocked *BlockedError
	if errors.As(cause, &blocked) {
		_ = e.transitionAndNotify(ctx, dep, store.StateFailed, e.failedEvent(dep, cause))
		return cause
	}

	// An inconclusive verification is not evidence of a bad deploy. Hold and
	// ask a human rather than rolling back on missing data.
	var inconclusive *VerificationInconclusive
	if errors.As(cause, &inconclusive) {
		_ = e.transitionAndNotify(ctx, dep, store.StateFailed, e.inconclusiveEvent(dep, cause))
		return cause
	}

	if !cfg.Rollback.Automatic {
		_ = e.transitionAndNotify(ctx, dep, store.StateFailed, e.failedEvent(dep, cause))
		return cause
	}

	plan, planErr := e.PlanRollback(ctx, dep, cfg)
	if planErr != nil {
		// UNSAFE ROLLBACK -> DO NOT ATTEMPT IT. ESCALATE.
		_ = e.transitionAndNotify(ctx, dep, store.StateRollbackFailed,
			e.unsafeRollbackEvent(dep, cause, planErr))
		return fmt.Errorf("%w (and rollback was refused: %v)", cause, planErr)
	}
	return e.executeRollback(ctx, dep, cfg, plan, cause)
}

// executeRollback deploys the previous version.
func (e *Executor) executeRollback(ctx context.Context, dep *store.Deployment,
	cfg *config.AppEnv, plan *Plan, cause error) error {

	if err := e.transitionAndNotify(ctx, dep, store.StateRollingBack,
		e.eventFor(dep, store.StateRollingBack, plan.Reason)); err != nil {
		return err
	}

	p, err := e.providers.Get(cfg.Provider)
	if err != nil {
		_ = e.transitionAndNotify(ctx, dep, store.StateRollbackFailed, e.pageEvent(dep, err))
		return err
	}

	var raw bytes.Buffer
	out := redact.New(&raw, e.secrets...)
	_, deployErr := p.Deploy(ctx, provider.DeployRequest{
		Target:         targetOf(cfg),
		Version:        plan.Version,
		Previous:       dep.Version,
		IdempotencyKey: dep.ID + ":rollback",
		Fence:          dep.Fence,
		Strategy:       string(cfg.Strategy.Type),
		BatchSize:      cfg.Strategy.BatchSize,
		IsRollback:     true,
	}, out)
	_ = out.Flush()

	started := e.clock.Now()
	_ = e.store.RecordStep(ctx, &store.Step{
		DeploymentID: dep.ID, Seq: 3, Name: "rollback",
		State: stepState(deployErr), StartedAt: &started, FinishedAt: &started,
		Output: raw.String(), Error: errString(deployErr, e.secrets),
	})

	if deployErr != nil {
		_ = e.transitionAndNotify(ctx, dep, store.StateRollbackFailed, e.pageEvent(dep, deployErr))
		return fmt.Errorf("rollback failed: %w (original failure: %v)", deployErr, cause)
	}

	// For a traffic-shifting strategy, put the traffic back too.
	if cfg.Strategy.Type == config.StrategyCanary || cfg.Strategy.Type == config.StrategyBlueGreen {
		if err := p.Shift(ctx, targetOf(cfg), 0); err != nil && !errors.Is(err, provider.ErrUnsupported) {
			_ = e.transitionAndNotify(ctx, dep, store.StateRollbackFailed, e.pageEvent(dep, err))
			return fmt.Errorf("rollback deployed but traffic was not shifted back: %w", err)
		}
	}

	dep.IsRollback = true
	ev := e.eventFor(dep, store.StateRolledBack, "rolled back to "+plan.Version+": "+cause.Error())
	if err := e.transitionAndNotify(ctx, dep, store.StateRolledBack, ev); err != nil {
		return err
	}

	// A successful rollback is still a failed deployment, and Run's error is
	// what `orch deploy --wait` turns into an exit code. Returning nil here
	// would make a rolled-back deploy report success to CI, which is the
	// "reports success the moment the deploy is queued" failure from section
	// 17.3 wearing a different hat.
	return fmt.Errorf("deployment failed and was rolled back to %s: %w", plan.Version, cause)
}

// recordError persists the failure reason on the deployment row.
//
// Without this the reason lives only in the log line: `orch status` shows a
// FAILED deployment with an empty error, and the dashboard shows a red box
// with nothing in it. The state says what happened; this says why.
func (e *Executor) recordError(ctx context.Context, dep *store.Deployment, cause error) {
	dep.Error = redact.String(cause.Error(), e.secrets...)
	if err := e.store.UpdateDeployment(ctx, dep); err != nil {
		e.log.Error("could not record the failure reason",
			"deployment", dep.ID, "error", err)
	}
}

func stepState(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

func errString(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	return redact.String(err.Error(), secrets...)
}

// ------------------------------------------------------------ transitions

// transitionAndNotify is section 6.4: the state change, the audit row, and
// the outbox entry, all in one transaction.
func (e *Executor) transitionAndNotify(ctx context.Context, dep *store.Deployment,
	to store.State, ev Event) error {

	from := dep.State
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	// One entry per destination, so a Slack outage retries Slack without
	// replaying the event to the dashboard, and an unconfigured Slack does not
	// swallow the dashboard's copy.
	notes := make([]store.OutboxEntry, 0, len(e.notifiers))
	for _, name := range e.notifiers {
		notes = append(notes, store.OutboxEntry{
			Notifier: name,
			Payload:  string(payload),
			// The dedupe key makes the consumer idempotent: at-least-once
			// delivery plus an idempotent consumer is the achievable goal.
			DedupeKey: dep.ID + ":" + string(to) + ":" + name,
		})
	}

	audit := store.AuditEntry{
		At:       e.clock.Now(),
		Actor:    dep.TriggeredBy,
		Action:   "deployment." + strings.ToLower(string(to)),
		Resource: dep.Resource(),
		Detail: map[string]string{
			"deployment": dep.ID,
			"version":    dep.Version,
			"from":       string(from),
			"to":         string(to),
		},
	}
	if ev.Reason != "" {
		audit.Detail["reason"] = redact.String(ev.Reason, e.secrets...)
	}

	if err := e.store.TransitionStateAndNotify(ctx, dep.ID, dep.Fence, from, to, audit, notes); err != nil {
		return err
	}
	dep.State = to
	e.log.Info("deployment state",
		"deployment", dep.ID, "app", dep.App, "environment", dep.Environment,
		"from", from, "to", to)
	return nil
}

// ---------------------------------------------------------------- events

// EventType maps a state to the name used in a config `notify.on` list.
//
// The two vocabularies are separate on purpose (see config.NotifyEvents), and
// this is the single place they meet. Without it, an `on: [started]`
// subscription silently matches nothing, and the first anyone knows is that
// deploys stopped appearing in Slack.
func EventType(s store.State) string {
	switch s {
	case store.StatePending, store.StatePreflight:
		return "started"
	case store.StateAwaitApproval:
		return "awaiting_approval"
	case store.StateRollingBack:
		return "rolling_back"
	case store.StateRolledBack:
		return "rolled_back"
	case store.StateRollbackFailed:
		return "rollback_failed"
	default:
		return strings.ToLower(string(s))
	}
}

func (e *Executor) eventFor(dep *store.Deployment, state store.State, reason string) Event {
	return Event{
		Type:         EventType(state),
		DeploymentID: dep.ID,
		App:          dep.App,
		Environment:  dep.Environment,
		Version:      dep.Version,
		Previous:     dep.PreviousVersion,
		State:        state,
		Actor:        dep.TriggeredBy,
		Strategy:     dep.Strategy,
		Reason:       reason,
		Pages:        state.Pages(),
		At:           e.clock.Now(),
	}
}

func (e *Executor) failedEvent(dep *store.Deployment, cause error) Event {
	ev := e.eventFor(dep, store.StateFailed, redact.String(cause.Error(), e.secrets...))
	return ev
}

func (e *Executor) inconclusiveEvent(dep *store.Deployment, cause error) Event {
	ev := e.eventFor(dep, store.StateFailed, redact.String(cause.Error(), e.secrets...))
	ev.Detail = map[string]string{
		"verdict": "inconclusive",
		"action":  "held for a human; no automatic rollback on missing data",
	}
	return ev
}

// pageEvent marks the one thing that wakes someone up.
func (e *Executor) pageEvent(dep *store.Deployment, cause error) Event {
	ev := e.eventFor(dep, store.StateRollbackFailed, redact.String(cause.Error(), e.secrets...))
	ev.Pages = true
	ev.Detail = map[string]string{
		"action": "PRODUCTION IS IN AN UNKNOWN STATE. A human must look at this.",
	}
	return ev
}

func (e *Executor) unsafeRollbackEvent(dep *store.Deployment, cause, refusal error) Event {
	ev := e.pageEvent(dep, cause)
	ev.Detail["rollback_refused"] = refusal.Error()
	ev.Detail["action"] = "the deploy failed AND rollback was refused as unsafe; roll forward with a fix"
	return ev
}

// ---------------------------------------------------------------- helpers

func targetOf(cfg *config.AppEnv) provider.Target {
	return provider.Target{
		App:         cfg.App,
		Environment: cfg.Name,
		Provider:    cfg.Provider,
		Config:      cfg.ProviderCfg,
	}
}

func (e *Executor) step(ctx context.Context, dep *store.Deployment, seq int, name string, fn func() error) {
	started := e.clock.Now()
	err := fn()
	finished := e.clock.Now()
	_ = e.store.RecordStep(ctx, &store.Step{
		DeploymentID: dep.ID, Seq: seq, Name: name,
		State: stepState(err), StartedAt: &started, FinishedAt: &finished,
		Error: errString(err, e.secrets),
	})
}

// Abort stops an in-flight deployment on request.
func (e *Executor) Abort(ctx context.Context, depID, actor string) error {
	dep, err := e.store.Deployment(ctx, depID)
	if err != nil {
		return err
	}
	if dep.State.IsTerminal() {
		return fmt.Errorf("deployment %s is already %s", depID, dep.State)
	}

	if dep.ProviderHandle != "" {
		if cfgEnv, err := e.cfg.AppEnv(dep.App, dep.Environment); err == nil {
			if p, err := e.providers.Get(cfgEnv.Provider); err == nil {
				if err := p.Abort(ctx, targetOf(cfgEnv), dep.ProviderHandle); err != nil {
					e.log.Error("provider abort failed", "deployment", depID, "error", err)
				}
			}
		}
	}

	// Break the lease so the aborting caller can transition without holding
	// it, then record the abort. Both are audited.
	lease, err := e.store.Lease(ctx, dep.Resource())
	if err == nil && lease.Holder == dep.ID {
		if err := e.store.BreakLease(ctx, dep.Resource(), actor); err != nil {
			return err
		}
	}
	fence, err := e.store.AcquireLease(ctx, dep.Resource(), dep.ID, e.nodeID)
	if err != nil {
		return err
	}
	dep.Fence = fence
	defer func() { _ = e.store.ReleaseLease(ctx, dep.Resource(), dep.ID) }()

	ev := e.eventFor(dep, store.StateAborted, "aborted by "+actor)
	ev.Actor = actor
	return e.transitionAndNotify(ctx, dep, store.StateAborted, ev)
}
