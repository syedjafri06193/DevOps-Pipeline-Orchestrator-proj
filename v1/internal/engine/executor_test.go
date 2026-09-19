package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/provider"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/verifier"
)

func bg() context.Context { return context.Background() }

// --------------------------------------------------------- the happy path

func TestADeploySucceedsAndReachesTheProvider(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")

	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.state(h.dep.ID); got != store.StateSucceeded {
		t.Fatalf("state = %s", got)
	}
	if got := h.provider.CurrentVersion(provider.Target{App: "web", Environment: "staging"}); got != "v2" {
		t.Errorf("the provider is on %q, want v2", got)
	}
}

func TestThePreviousVersionComesFromTheProviderNotTheDatabase(t *testing.T) {
	// Section 2.2: "Reconcile actual state from the provider on startup;
	// never trust the DB alone." The rollback target and the verification
	// baseline both depend on this being what is *actually* running.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1-actually-running")

	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	d, _ := h.store.Deployment(bg(), h.dep.ID)
	if d.PreviousVersion != "v1-actually-running" {
		t.Errorf("previous = %q; it must come from the environment", d.PreviousVersion)
	}
}

func TestTheFenceTokenReachesTheProvider(t *testing.T) {
	// Section 6.2: "Providers that support it should carry the fence into the
	// target system too... Then even a resumed zombie process can't clobber a
	// newer deploy."
	h := newHarness(t)
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	reqs := h.provider.Requests()
	if len(reqs) == 0 {
		t.Fatal("the provider was never called")
	}
	if reqs[0].Fence <= 0 {
		t.Errorf("fence = %d; the provider cannot fence without it", reqs[0].Fence)
	}
}

func TestEveryTransitionIsAuditedAndTheChainVerifies(t *testing.T) {
	h := newHarness(t)
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.VerifyAudit(bg()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	entries, _ := h.store.AuditEntries(bg(), 0)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{
		"deployment.created", "deployment.preflight", "deployment.deploying",
		"deployment.verifying", "deployment.succeeded",
	} {
		if !seen[want] {
			t.Errorf("no audit entry for %s", want)
		}
	}
}

func TestEveryTransitionEnqueuesANotification(t *testing.T) {
	h := newHarness(t)
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ := h.store.PendingOutbox(bg())
	if pending == 0 {
		t.Fatal("no notifications were enqueued; a silent deploy is the failure mode of section 9.5")
	}
}

// ------------------------------------------------------------- the lease

func TestASecondDeployToTheSameTargetIsRefused(t *testing.T) {
	h := newHarness(t)
	first := h.newDeployment("web", "staging", "v2")
	if _, err := h.store.AcquireLease(bg(), first.Resource(), first.ID, "other-node"); err != nil {
		t.Fatal(err)
	}
	second := h.newDeployment("web", "staging", "v3")
	err := h.exec.Run(bg(), second.ID)
	var held *store.LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("want LeaseHeldError, got %v", err)
	}
}

func TestTheLeaseIsReleasedAfterwardsSoTheNextDeployCanRun(t *testing.T) {
	// A lease leaked on the success path wedges the target for the TTL. A
	// lease leaked on the failure path wedges it forever, which is the
	// "nobody can deploy again" row of section 2.2's table.
	h := newHarness(t)
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	if held, _ := h.store.LeaseHeld(bg(), h.dep.Resource()); held {
		t.Fatal("the lease was not released after success")
	}

	h.provider.FailOn["deploy"] = errors.New("boom")
	failing := h.newDeployment("web", "staging", "v3")
	_ = h.exec.Run(bg(), failing.ID)
	if held, _ := h.store.LeaseHeld(bg(), failing.Resource()); held {
		t.Fatal("the lease was not released after failure")
	}
}

func TestLosingTheLeaseAbortsTheDeploy(t *testing.T) {
	// Section 6.1: "If renewal fails, the executor must abort rather than
	// keep operating on a resource it no longer owns."
	h := newHarness(t)
	// The provider hangs long enough for a heartbeat to run.
	h.provider.HangOn["deploy"] = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- h.exec.Run(bg(), h.dep.ID) }()

	// Break the lease out from under the running executor.
	deadline := time.After(2 * time.Second)
	for {
		if held, _ := h.store.LeaseHeld(bg(), h.dep.Resource()); held {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the executor never took the lease")
		case <-time.After(time.Millisecond):
		}
	}
	if err := h.store.BreakLease(bg(), h.dep.Resource(), "alex"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the deploy should not have succeeded after losing the lease")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the executor did not abort after losing the lease")
	}
}

// ------------------------------------------------------------ preflight

func TestAFrozenTargetIsRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetFreeze(bg(), &store.Freeze{
		Resource: h.dep.Resource(), Reason: "incident 4471", By: "alex",
	}); err != nil {
		t.Fatal(err)
	}
	err := h.exec.Run(bg(), h.dep.ID)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("want BlockedError, got %v", err)
	}
	if !strings.Contains(err.Error(), "incident 4471") {
		t.Errorf("the reason should say who froze it and why: %v", err)
	}
	if h.provider.Counts()["app:web/env:staging@v2"] != 0 {
		t.Error("a frozen target must not be deployed to")
	}
}

func TestDeployingAVersionThatIsAlreadyRunningIsBlocked(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v2")
	err := h.exec.Run(bg(), h.dep.ID)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("want a clean refusal, got %v", err)
	}
}

func TestAnUnknownCurrentVersionRefusesToDeployOverIt(t *testing.T) {
	// Deploying over a state you cannot read is the same class of mistake as
	// reconciling toward what you think should be true.
	h := newHarness(t)
	h.provider.CurrentUnknown = true
	err := h.exec.Run(bg(), h.dep.ID)
	if err == nil || !strings.Contains(err.Error(), "unknown state") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestACanaryAgainstAProviderWithoutTrafficControlIsRefusedAtPreflight(t *testing.T) {
	// Section 9: caught before the rollout starts, not at step three with
	// production half-shifted.
	h := newHarnessWith(t, strings.Replace(harnessConfig,
		"        strategy: { type: blue-green }\n        verify:",
		"        strategy:\n          type: canary\n          steps:\n            - { setWeight: 10 }\n            - { verify: { bake: 1m } }\n            - { setWeight: 100 }\n        verify:", 1))
	h.provider.NoTrafficControl = true

	dep := h.newDeployment("web", "staging", "v2")
	err := h.exec.Run(bg(), dep.ID)
	if err == nil || !strings.Contains(err.Error(), "traffic shifting") {
		t.Fatalf("want a preflight refusal, got %v", err)
	}
	if len(h.provider.Requests()) != 0 {
		t.Error("nothing should have been deployed")
	}
}

func TestPromoteFromEnforcesBuildOnceDeployMany(t *testing.T) {
	// Section 13: "Rebuilding per environment means prod runs a binary that
	// was never tested, and it's a shockingly common mistake."
	h := newHarness(t)
	prod := h.newDeployment("web", "prod", "v2-never-tested")
	h.approve(prod, "sam")

	err := h.exec.Run(bg(), prod.ID)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "has not succeeded in staging") {
		t.Errorf("the error should name the upstream environment: %v", err)
	}
}

func TestPromotionSucceedsOnceTheArtifactPassedUpstream(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	if err := h.exec.Run(bg(), h.dep.ID); err != nil { // web/staging v2
		t.Fatal(err)
	}

	prod := h.newDeployment("web", "prod", "v2")
	h.approve(prod, "sam")
	if err := h.exec.Run(bg(), prod.ID); err != nil {
		t.Fatalf("the same artifact should be promotable: %v", err)
	}
	if h.state(prod.ID) != store.StateSucceeded {
		t.Errorf("state = %s", h.state(prod.ID))
	}
}

// -------------------------------------------------------------- approval

func TestAProdDeployWaitsForApproval(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID) // get v2 through staging

	prod := h.newDeployment("web", "prod", "v2")
	// No approval recorded. The gate's TTL is an hour and the fake clock
	// advances on every After(), so the wait ends rather than hanging.
	err := h.exec.Run(bg(), prod.ID)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expiry refusal, got %v", err)
	}
	if h.state(prod.ID) != store.StateFailed {
		t.Errorf("state = %s", h.state(prod.ID))
	}
}

func TestSelfApprovalIsRefusedOnAPeerRequiredEnvironment(t *testing.T) {
	// Section 10.3's third check.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID)

	prod := h.newDeployment("web", "prod", "v2")
	prod.TriggeredBy = "sam"
	if err := h.store.UpdateDeployment(bg(), prod); err != nil {
		t.Fatal(err)
	}
	h.approve(prod, "sam") // the same person who triggered it

	err := h.exec.Run(bg(), prod.ID)
	if err == nil {
		t.Fatal("self-approval must not satisfy a peer-required gate")
	}
}

func TestApprovalBySomeoneWithoutThePermissionDoesNotCount(t *testing.T) {
	// Section 10.3's second check: "Check the real permission, in the real
	// RBAC system." jordan is a developer and cannot approve prod.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID)

	prod := h.newDeployment("web", "prod", "v2")
	h.approve(prod, "jordan")

	err := h.exec.Run(bg(), prod.ID)
	if err == nil {
		t.Fatal("an unauthorised approval must not open the gate")
	}
}

func TestApprovalValidityChecksAreOneImplementation(t *testing.T) {
	// The Slack handler and the gate must agree about who may approve. They
	// call the same function, and this asserts each of section 10.3's four
	// checks through it.
	h := newHarness(t)
	cfg := h.appEnv("web", "prod")
	gate := cfg.Gates[0]
	dep := h.newDeployment("web", "prod", "v2")
	now := h.clock.Now()

	for _, tc := range []struct {
		name, user string
		at         time.Time
		wantErr    string
	}{
		{"unknown user", "nobody", now, "not a known orchestrator user"},
		{"no permission", "jordan", now, "does not have deploy:approve"},
		{"self approval", "jordan", now, "does not have"}, // caught by RBAC first
		{"expired", "sam", now.Add(2 * time.Hour), "expired"},
		{"valid", "sam", now, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := h.exec.ApprovalIsValid(dep, cfg, gate, tc.user, tc.at)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSelfApprovalIsRefusedEvenWithThePermission(t *testing.T) {
	h := newHarness(t)
	cfg := h.appEnv("web", "prod")
	dep := h.newDeployment("web", "prod", "v2")
	dep.TriggeredBy = "sam" // sam is a release-manager and may approve prod
	err := h.exec.ApprovalIsValid(dep, cfg, cfg.Gates[0], "sam", h.clock.Now())
	if err == nil || !strings.Contains(err.Error(), "someone other than") {
		t.Fatalf("want a peer-approval refusal, got %v", err)
	}
}

// ------------------------------------------------------------- rollback

func TestAFailedDeployRollsBackAutomatically(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	// Succeed once so there is something to roll back to.
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}

	// v3's manifest says the rollback to v2 is safe. Without a manifest the
	// rollback is refused, which TestAMissingManifestIsNotPermissionToRollBack
	// covers separately.
	h.manifests.Add("web", &Manifest{Version: "v3", SchemaVersion: 47, RollbackFloor: "v1"})
	h.manifests.Add("web", &Manifest{Version: "v2", SchemaVersion: 47})

	h.verifier.Values = []float64{0.9, 0.9, 0.9} // unhealthy
	bad := h.newDeployment("web", "staging", "v3")
	err := h.exec.Run(bg(), bad.ID)
	if err == nil {
		t.Fatal("expected the deploy to fail")
	}
	if got := h.state(bad.ID); got != store.StateRolledBack {
		t.Fatalf("state = %s, want ROLLED_BACK", got)
	}
	if got := h.provider.CurrentVersion(provider.Target{App: "web", Environment: "staging"}); got != "v2" {
		t.Errorf("the environment is on %q, want v2 after the rollback", got)
	}
}

func TestARollbackIsNeverAutomaticallyRolledBack(t *testing.T) {
	// Section 5: "The alternative is an infinite loop that thrashes
	// production." This is checked before any policy lookup, so no
	// configuration can turn it off.
	h := newHarness(t)
	dep := h.newDeployment("web", "staging", "v2")
	dep.IsRollback = true
	if err := h.store.UpdateDeployment(bg(), dep); err != nil {
		t.Fatal(err)
	}
	h.provider.FailOn["deploy"] = errors.New("the rollback itself failed")

	err := h.exec.Run(bg(), dep.ID)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if got := h.state(dep.ID); got != store.StateRollbackFailed {
		t.Fatalf("state = %s, want ROLLBACK_FAILED", got)
	}
	// Exactly one deploy attempt: no second rollback was tried.
	if n := h.provider.Counts()["app:web/env:staging@v2"]; n != 1 {
		t.Errorf("provider was called %d times; a failed rollback must not retry", n)
	}
}

func TestRollbackFailedIsTheStateThatPages(t *testing.T) {
	h := newHarness(t)
	dep := h.newDeployment("web", "staging", "v2")
	dep.IsRollback = true
	_ = h.store.UpdateDeployment(bg(), dep)
	h.provider.FailOn["deploy"] = errors.New("boom")
	_ = h.exec.Run(bg(), dep.ID)

	if !store.StateRollbackFailed.Pages() {
		t.Fatal("ROLLBACK_FAILED must page")
	}
	due, _ := h.store.DueOutbox(bg(), 0)
	var paged bool
	for _, e := range due {
		if strings.Contains(e.Payload, `"pages":true`) {
			paged = true
		}
	}
	if !paged {
		t.Error("the notification must be marked as paging")
	}
}

func TestAnUnsafeRollbackIsRefusedNotAttempted(t *testing.T) {
	// Section 8.3: "When rollback is unsafe, the tool must refuse and say
	// why. Not warn -- refuse."
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	if err := h.exec.Run(bg(), h.dep.ID); err != nil { // v2 succeeds
		t.Fatal(err)
	}

	// v3 ran an irreversible migration, so it cannot go back to v2.
	h.manifests.Add("web", &Manifest{
		Version: "v3", SchemaVersion: 47, RollbackFloor: "v3",
		Migrations: []Migration{{ID: "0046_backfill_uuid", Reversible: false}},
	})
	h.verifier.Values = []float64{0.9, 0.9, 0.9}

	bad := h.newDeployment("web", "staging", "v3")
	err := h.exec.Run(bg(), bad.ID)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if got := h.state(bad.ID); got != store.StateRollbackFailed {
		t.Fatalf("state = %s, want ROLLBACK_FAILED (escalated, not rolled back)", got)
	}
	// The environment is still on the bad version, which is correct: the tool
	// refused to make it worse.
	if got := h.provider.CurrentVersion(provider.Target{App: "web", Environment: "staging"}); got != "v3" {
		t.Errorf("the environment is on %q; an unsafe rollback must not have run", got)
	}
	if !strings.Contains(err.Error(), "rollback floor") && !strings.Contains(err.Error(), "irreversible") {
		t.Errorf("the error must explain why: %v", err)
	}
	if !strings.Contains(err.Error(), "roll forward") {
		t.Errorf("the error must say what to do instead: %v", err)
	}
}

func TestAMissingManifestIsNotPermissionToRollBack(t *testing.T) {
	// The whole point of the check is that an unsafe rollback looks exactly
	// like a safe one.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID)

	// No manifest registered for v3 at all.
	h.verifier.Values = []float64{0.9, 0.9, 0.9}
	bad := h.newDeployment("web", "staging", "v3")
	err := h.exec.Run(bg(), bad.ID)

	if got := h.state(bad.ID); got != store.StateRollbackFailed {
		t.Fatalf("state = %s; an unreadable manifest must escalate", got)
	}
	if !strings.Contains(err.Error(), "rollback floor is unknown") {
		t.Errorf("say why: %v", err)
	}
}

func TestASafeRollbackProceeds(t *testing.T) {
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID)

	h.manifests.Add("web", &Manifest{
		Version: "v3", SchemaVersion: 47, RollbackFloor: "v1",
		Migrations: []Migration{{ID: "0047_add_column", Reversible: true, ExpandPhase: true}},
	})
	h.manifests.Add("web", &Manifest{Version: "v2", SchemaVersion: 47})
	h.verifier.Values = []float64{0.9, 0.9, 0.9}

	bad := h.newDeployment("web", "staging", "v3")
	_ = h.exec.Run(bg(), bad.ID)
	if got := h.state(bad.ID); got != store.StateRolledBack {
		t.Fatalf("state = %s, want ROLLED_BACK", got)
	}
}

func TestTheCircuitBreakerBlocksAKnownBadVersion(t *testing.T) {
	// Section 7.4: two rollbacks in 24h and the version is blocked.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID) // v2 good

	h.manifests.Add("web", &Manifest{Version: "v3-bad", SchemaVersion: 47, RollbackFloor: "v1"})
	h.manifests.Add("web", &Manifest{Version: "v2", SchemaVersion: 47})
	h.verifier.Values = []float64{0.9, 0.9, 0.9}

	for i := 0; i < 2; i++ {
		d := h.newDeployment("web", "staging", "v3-bad")
		_ = h.exec.Run(bg(), d.ID)
		if got := h.state(d.ID); got != store.StateRolledBack {
			t.Fatalf("attempt %d: state = %s", i, got)
		}
		// Put the environment back so the next attempt is not "already
		// running".
		h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v2")
	}

	third := h.newDeployment("web", "staging", "v3-bad")
	err := h.exec.Run(bg(), third.ID)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("the third attempt must be blocked, got %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("say how to override: %v", err)
	}
}

func TestAnInconclusiveVerdictHoldsRatherThanRollingBack(t *testing.T) {
	// Section 7.3: "for prod, that should mean 'hold and ask a human', not
	// 'roll back'." A monitoring outage must not undo a good deploy.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	_ = h.exec.Run(bg(), h.dep.ID)

	next := h.newDeployment("web", "staging", "v3")
	for i := 1; i <= 20; i++ {
		h.verifier.FailAt[i] = verifier.Inconclusive("prometheus: connection refused")
	}
	err := h.exec.Run(bg(), next.ID)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if got := h.state(next.ID); got != store.StateFailed {
		t.Fatalf("state = %s, want FAILED (held, not rolled back)", got)
	}
	// The new version is still live: nothing was undone on missing data.
	if got := h.provider.CurrentVersion(provider.Target{App: "web", Environment: "staging"}); got != "v3" {
		t.Errorf("the environment is on %q; an inconclusive verdict must not roll back", got)
	}
}

func TestABlockedDeployDoesNotRollBack(t *testing.T) {
	// Nothing was deployed, so there is nothing to undo. Rolling back here
	// would redeploy the previous version for no reason.
	h := newHarness(t)
	_ = h.store.SetFreeze(bg(), &store.Freeze{Resource: h.dep.Resource(), Reason: "freeze", By: "alex"})
	_ = h.exec.Run(bg(), h.dep.ID)

	if got := h.state(h.dep.ID); got != store.StateFailed {
		t.Fatalf("state = %s, want FAILED", got)
	}
	if len(h.provider.Requests()) != 0 {
		t.Error("nothing should have been deployed")
	}
}

func TestRollbackIsDisabledWhenThePolicySaysSo(t *testing.T) {
	h := newHarnessWith(t, strings.Replace(harnessConfig,
		"      prod:\n        provider: fake",
		"      manual:\n        provider: fake\n        rollback: { automatic: false }\n      prod:\n        provider: fake", 1))
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "manual"}, "v1")
	h.provider.FailOn["deploy"] = errors.New("boom")

	d := h.newDeployment("web", "manual", "v2")
	_ = h.exec.Run(bg(), d.ID)
	if got := h.state(d.ID); got != store.StateFailed {
		t.Fatalf("state = %s, want FAILED with no rollback", got)
	}
}

// --------------------------------------------------------------- canary

func TestACanaryWalksItsStepsInOrder(t *testing.T) {
	h := newHarnessWith(t, strings.Replace(harnessConfig,
		"        strategy: { type: blue-green }\n        verify:",
		`        strategy:
          type: canary
          steps:
            - { setWeight: 5 }
            - { verify: { bake: 5m } }
            - { setWeight: 25 }
            - { verify: { bake: 5m } }
            - { setWeight: 100 }
        verify:`, 1))
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")

	dep := h.newDeployment("web", "staging", "v2")
	if err := h.exec.Run(bg(), dep.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []int{5, 25, 100}
	got := h.provider.Shifts()
	if len(got) != len(want) {
		t.Fatalf("weights = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("weights = %v, want %v", got, want)
		}
	}
}

func TestAFailingCanaryShiftsTrafficBackToZero(t *testing.T) {
	h := newHarnessWith(t, strings.Replace(harnessConfig,
		"        strategy: { type: blue-green }\n        verify:",
		`        strategy:
          type: canary
          steps:
            - { setWeight: 5 }
            - { verify: { bake: 5m } }
            - { setWeight: 100 }
        verify:`, 1))
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	if err := h.exec.Run(bg(), h.newDeployment("web", "staging", "v2").ID); err != nil {
		t.Fatal(err)
	}

	h.manifests.Add("web", &Manifest{Version: "v3", RollbackFloor: "v1"})
	h.manifests.Add("web", &Manifest{Version: "v2"})
	h.verifier.Values = []float64{0.9, 0.9, 0.9}

	bad := h.newDeployment("web", "staging", "v3")
	_ = h.exec.Run(bg(), bad.ID)

	shifts := h.provider.Shifts()
	if len(shifts) == 0 || shifts[len(shifts)-1] != 0 {
		t.Fatalf("weights = %v; a failed canary must end at 0", shifts)
	}
	if got := h.state(bad.ID); got != store.StateRolledBack {
		t.Errorf("state = %s", got)
	}
}

// ----------------------------------------------------------- redaction

func TestSecretsAreRedactedBeforeTheyReachTheStore(t *testing.T) {
	// Section 12.3: "Redact before persistence, not on display -- once a
	// secret is in the database it's leaked."
	h := newHarness(t)
	h.exec.secrets = []string{"super-secret-deploy-token"}
	h.provider.SecretOutput = "super-secret-deploy-token"

	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	steps, _ := h.store.Steps(bg(), h.dep.ID)
	for _, s := range steps {
		if strings.Contains(s.Output, "super-secret-deploy-token") {
			t.Fatalf("a secret reached the store: %q", s.Output)
		}
	}
	var sawPlaceholder bool
	for _, s := range steps {
		if strings.Contains(s.Output, "[REDACTED]") {
			sawPlaceholder = true
		}
	}
	if !sawPlaceholder {
		t.Error("the deploy log should show that something was redacted")
	}
}

// ---------------------------------------------------------------- abort

func TestAbortStopsAnInFlightDeploy(t *testing.T) {
	h := newHarness(t)
	dep := h.newDeployment("web", "staging", "v2")
	fence, _ := h.store.AcquireLease(bg(), dep.Resource(), dep.ID, "node-test")
	dep.Fence = fence
	if err := h.store.TransitionState(bg(), dep.ID, fence, store.StatePending, store.StatePreflight); err != nil {
		t.Fatal(err)
	}

	if err := h.exec.Abort(bg(), dep.ID, "alex"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := h.state(dep.ID); got != store.StateAborted {
		t.Fatalf("state = %s", got)
	}
	if held, _ := h.store.LeaseHeld(bg(), dep.Resource()); held {
		t.Error("abort must release the lease so the next deploy can run")
	}
}

func TestAbortingATerminalDeploymentIsRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.Abort(bg(), h.dep.ID, "alex"); err == nil {
		t.Fatal("aborting a finished deployment should be refused")
	}
}

var _ = config.StrategyCanary
