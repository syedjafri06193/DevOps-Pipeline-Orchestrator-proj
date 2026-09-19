package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/syedjafri06193/orch/internal/store"
)

// ReconcileResult reports what startup recovery did, so it can be logged,
// counted (`orch_orphans_recovered_total`), and asserted in tests.
type ReconcileResult struct {
	Examined int
	Skipped  int // owned by a live lease on another node
	Resumed  int
	Failed   int
	Unknown  int
}

func (r ReconcileResult) String() string {
	return fmt.Sprintf("examined %d, skipped %d (live elsewhere), resumed %d, failed %d, unknown %d",
		r.Examined, r.Skipped, r.Resumed, r.Failed, r.Unknown)
}

// Reconcile recovers deployments left in non-terminal states by a crash
// (design.md section 17.2).
//
//	"It never assumes the database is right — it asks the provider what
//	happened."
//
// The three-way branch at the bottom is the whole function, and the `default`
// case is the one that matters:
//
//	"When reality doesn't match either expected state, the correct action is
//	to stop and tell a human. A tool that 'helpfully' reconciles toward what
//	it thinks should be true is how automation causes outages."
func (e *Executor) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	orphans, err := e.store.NonTerminalDeployments(ctx)
	if err != nil {
		return res, err
	}

	for _, dep := range orphans {
		res.Examined++

		held, err := e.store.LeaseHeld(ctx, dep.Resource())
		if err != nil {
			return res, err
		}
		if held {
			// Another node owns it and is alive. Touching it would be the
			// two-executors-at-once failure the lease exists to prevent.
			res.Skipped++
			continue
		}

		e.log.Warn("recovering orphaned deployment",
			"id", dep.ID, "state", dep.State, "app", dep.App, "environment", dep.Environment)

		cfgEnv, err := e.cfg.AppEnv(dep.App, dep.Environment)
		if err != nil {
			// The config no longer describes this target. We cannot ask the
			// provider anything, so we cannot know what is running.
			res.Unknown++
			_ = e.markUnknown(ctx, dep, fmt.Errorf(
				"the configuration no longer defines %s/%s, so what is deployed cannot be checked: %w",
				dep.App, dep.Environment, err))
			continue
		}

		// A deployment that never got past PENDING has no side effect to
		// reconcile: nothing was fetched and nothing was deployed.
		if dep.State == store.StatePending || dep.State == store.StatePreflight {
			res.Failed++
			_ = e.reconcileTransition(ctx, dep, store.StateFailed,
				"interrupted before any deployment was attempted")
			continue
		}

		p, err := e.providers.Get(cfgEnv.Provider)
		if err != nil {
			res.Unknown++
			_ = e.markUnknown(ctx, dep, err)
			continue
		}

		actual, err := p.Current(ctx, targetOf(cfgEnv))
		if err != nil {
			// Can't tell what's real. Don't guess about production.
			res.Unknown++
			_ = e.markUnknown(ctx, dep, fmt.Errorf("could not read the current version: %w", err))
			continue
		}
		if actual.Unknown {
			res.Unknown++
			_ = e.markUnknown(ctx, dep, errors.New("the provider cannot determine what is deployed"))
			continue
		}

		switch {
		case actual.ID == dep.Version:
			// The deploy landed; we crashed before recording it. Resume at
			// verification rather than assuming success -- the deploy
			// happening and the deploy being good are different claims, and
			// only one of them has been established.
			res.Resumed++
			_ = e.reconcileTransition(ctx, dep, store.StateVerifying,
				"recovered after a restart: the new version is live but was never verified")

		case actual.ID == dep.PreviousVersion:
			// Never took effect. Safe to fail cleanly.
			res.Failed++
			_ = e.reconcileTransition(ctx, dep, store.StateFailed,
				"interrupted before the deploy took effect; the previous version is still live")

		default:
			// Something else is deployed. Do not touch it.
			res.Unknown++
			_ = e.markUnknown(ctx, dep, fmt.Errorf(
				"unexpected version %s in %s: expected either %s (the deploy landed) or %s (it did not). "+
					"Something outside this orchestrator changed the environment",
				actual.ID, dep.Environment, dep.Version, dep.PreviousVersion))
		}
	}

	if res.Resumed+res.Failed+res.Unknown > 0 {
		e.log.Info("reconciliation complete", "result", res.String())
	}
	return res, nil
}

// reconcileTransition moves a recovered deployment, taking the lease first.
//
// Recovery is itself a mutation of a resource, so it goes through the same
// lease and fence machinery as a deploy. Skipping that here would mean
// reconciliation on two nodes could fight over the same orphan, which is the
// exact failure the lease exists to prevent.
func (e *Executor) reconcileTransition(ctx context.Context, dep *store.Deployment,
	to store.State, reason string) error {

	fence, err := e.store.AcquireLease(ctx, dep.Resource(), dep.ID, e.nodeID)
	if err != nil {
		// Someone took it between the check and here. Leave it to them.
		e.log.Info("skipping recovery; the lease was taken",
			"deployment", dep.ID, "error", err)
		return err
	}
	defer func() { _ = e.store.ReleaseLease(ctx, dep.Resource(), dep.ID) }()
	dep.Fence = fence

	ev := e.eventFor(dep, to, reason)
	ev.Actor = "reconciler"
	ev.Detail = map[string]string{"recovered_from": string(dep.State)}
	if to.Pages() {
		ev.Pages = true
	}

	if !store.CanTransition(dep.State, to) {
		// The state machine does not allow this jump. Rather than forcing it,
		// record the fact and stop -- a reconciler that can move a deployment
		// anywhere is not constrained by the state machine at all.
		return e.markUnknown(ctx, dep, fmt.Errorf(
			"cannot recover from %s to %s: the state machine does not allow it", dep.State, to))
	}
	return e.transitionAndNotify(ctx, dep, to, ev)
}

// markUnknown is the "stop and tell a human" path.
//
// UNKNOWN is terminal and pages. It is deliberately not recoverable by the
// system: the whole reason to be here is that the system does not know what
// is true, and a system that does not know what is true must not act.
func (e *Executor) markUnknown(ctx context.Context, dep *store.Deployment, cause error) error {
	e.log.Error("deployment is in an unknown state; a human must look",
		"deployment", dep.ID, "app", dep.App, "environment", dep.Environment,
		"state", dep.State, "reason", cause)

	fence, err := e.store.AcquireLease(ctx, dep.Resource(), dep.ID, e.nodeID)
	if err != nil {
		return err
	}
	defer func() { _ = e.store.ReleaseLease(ctx, dep.Resource(), dep.ID) }()
	dep.Fence = fence
	e.recordError(ctx, dep, cause)

	if !store.CanTransition(dep.State, store.StateUnknown) {
		// From PENDING, say. Record it in the audit log even though the state
		// cannot move, so the event is not lost.
		return e.store.AppendAudit(ctx, store.AuditEntry{
			At: e.clock.Now(), Actor: "reconciler",
			Action: "deployment.unknown", Resource: dep.Resource(),
			Detail: map[string]string{
				"deployment": dep.ID,
				"state":      string(dep.State),
				"reason":     cause.Error(),
			},
		})
	}

	ev := e.eventFor(dep, store.StateUnknown, cause.Error())
	ev.Actor = "reconciler"
	ev.Pages = true
	ev.Detail = map[string]string{
		"recovered_from": string(dep.State),
		"action":         "PRODUCTION STATE IS UNKNOWN. Check the environment by hand before deploying again.",
	}
	return e.transitionAndNotify(ctx, dep, store.StateUnknown, ev)
}
