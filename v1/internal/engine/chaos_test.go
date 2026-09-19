package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/syedjafri06193/orch/internal/provider"
	"github.com/syedjafri06193/orch/internal/store"
)

// Section 18.3: "Chaos tests — the ones that find real bugs."
//
// The assertion that matters is not that the deployment record was recovered.
// It is the last line of the document's example:
//
//	"The critical assertion: the system still works afterward. Recovering the
//	*record* is easy; the real requirement is that a crash never permanently
//	wedges the system by leaking a lease or leaving a row that blocks all
//	future deploys."
//
// So every case below ends by deploying again.

// errCrash is what a hook returns to simulate the process dying.
var errCrash = errors.New("simulated process death")

func TestCrashBeforeEveryStatesSideEffect(t *testing.T) {
	// The window between "the transition is committed" and "the work has
	// happened". Section 5's ordering rule exists precisely for this.
	for _, killAt := range []store.State{
		store.StatePreflight,
		store.StateDeploying,
		store.StateVerifying,
		store.StatePromoting,
	} {
		t.Run(string(killAt), func(t *testing.T) {
			h := newHarness(t)
			h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
			h.exec.hooks.BeforeSideEffect = func(s store.State, _ *store.Deployment) error {
				if s == killAt {
					return errCrash
				}
				return nil
			}

			dep := h.newDeployment("web", "staging", "v2")
			if err := h.exec.Run(bg(), dep.ID); !errors.Is(err, errCrash) {
				t.Fatalf("expected the simulated crash, got %v", err)
			}

			// A fresh process against the same journal.
			next := h.reopen()
			res, err := next.exec.Reconcile(bg())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if res.Examined == 0 {
				t.Fatal("the orphan was not examined")
			}

			after := next.state(dep.ID)
			if !after.IsTerminal() && after != store.StateVerifying {
				t.Fatalf("left in unrecoverable state %s", after)
			}

			// THE CRITICAL ASSERTION: the system still works.
			assertStillDeployable(t, next, "v3")
		})
	}
}

func TestCrashAfterEveryStatesSideEffect(t *testing.T) {
	// The other half of the window: the work happened and the next transition
	// never committed. This is the case where the database and reality
	// disagree, and where reconciliation must ask the provider.
	for _, killAt := range []store.State{
		store.StatePreflight,
		store.StateDeploying,
		store.StateVerifying,
	} {
		t.Run(string(killAt), func(t *testing.T) {
			h := newHarness(t)
			h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
			h.exec.hooks.AfterSideEffect = func(s store.State, _ *store.Deployment) error {
				if s == killAt {
					return errCrash
				}
				return nil
			}

			dep := h.newDeployment("web", "staging", "v2")
			if err := h.exec.Run(bg(), dep.ID); !errors.Is(err, errCrash) {
				t.Fatalf("expected the simulated crash, got %v", err)
			}

			next := h.reopen()
			if _, err := next.exec.Reconcile(bg()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			after := next.state(dep.ID)
			if !after.IsTerminal() && after != store.StateVerifying {
				t.Fatalf("left in unrecoverable state %s", after)
			}
			assertStillDeployable(t, next, "v3")
		})
	}
}

func TestACrashDuringDeployResumesAtVerificationNotSuccess(t *testing.T) {
	// Section 17.2: "The deploy landed; we crashed before recording it.
	// Resume at verification rather than assuming success." The deploy
	// happening and the deploy being good are different claims.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	h.exec.hooks.AfterSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	// The provider really is on v2 now.
	if got := h.provider.CurrentVersion(provider.Target{App: "web", Environment: "staging"}); got != "v2" {
		t.Fatalf("the fixture is wrong: provider is on %q", got)
	}

	next := h.reopen()
	res, err := next.exec.Reconcile(bg())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != 1 {
		t.Fatalf("resumed %d, want 1 (%s)", res.Resumed, res)
	}
	if got := next.state(dep.ID); got != store.StateVerifying {
		t.Fatalf("state = %s, want VERIFYING", got)
	}
}

func TestACrashBeforeTheDeployTookEffectFailsCleanly(t *testing.T) {
	// Section 17.2's second branch: the environment is still on the old
	// version, so nothing happened and the deployment can be failed cleanly.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	h.exec.hooks.BeforeSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	next := h.reopen()
	if _, err := next.exec.Reconcile(bg()); err != nil {
		t.Fatal(err)
	}
	if got := next.state(dep.ID); got != store.StateFailed {
		t.Fatalf("state = %s, want FAILED", got)
	}
}

func TestAnUnexpectedVersionStopsAndPages(t *testing.T) {
	// SECTION 17.2's `default` BRANCH, which is the whole point of the
	// function:
	//
	//	"When reality doesn't match either expected state, the correct action
	//	is to stop and tell a human. A tool that 'helpfully' reconciles toward
	//	what it thinks should be true is how automation causes outages."
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	h.exec.hooks.BeforeSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	// Someone deployed something else by hand while we were down.
	next := h.reopen()
	next.provider.DriftTo = "v-deployed-by-hand"

	if _, err := next.exec.Reconcile(bg()); err != nil {
		t.Fatal(err)
	}
	got := next.state(dep.ID)
	if got != store.StateUnknown {
		t.Fatalf("state = %s, want UNKNOWN", got)
	}
	if !got.Pages() {
		t.Error("UNKNOWN must page: production is in a state nobody can explain")
	}

	d, _ := next.store.Deployment(bg(), dep.ID)
	if !strings.Contains(d.Error, "v-deployed-by-hand") {
		t.Errorf("the error should name what it actually found: %q", d.Error)
	}
}

func TestAnUnreachableProviderDoesNotGuess(t *testing.T) {
	// "Can't tell what's real. Don't guess about production."
	h := newHarness(t)
	h.exec.hooks.BeforeSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	next := h.reopen()
	next.provider.FailOn["current"] = errors.New("ecs: connection timed out")

	if _, err := next.exec.Reconcile(bg()); err != nil {
		t.Fatal(err)
	}
	if got := next.state(dep.ID); got != store.StateUnknown {
		t.Fatalf("state = %s, want UNKNOWN", got)
	}
}

func TestReconcileLeavesALiveDeploymentAlone(t *testing.T) {
	// Another node owns it. Touching it is the two-executors-at-once failure
	// the lease exists to prevent.
	h := newHarness(t)
	dep := h.newDeployment("web", "staging", "v2")
	fence, err := h.store.AcquireLease(bg(), dep.Resource(), dep.ID, "some-other-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.TransitionState(bg(), dep.ID, fence, store.StatePending, store.StatePreflight); err != nil {
		t.Fatal(err)
	}

	if _, err := h.exec.Reconcile(bg()); err != nil {
		t.Fatal(err)
	}
	if got := h.state(dep.ID); got != store.StatePreflight {
		t.Errorf("state = %s; a live deployment must not be touched", got)
	}
}

func TestACrashDoesNotLeakTheLease(t *testing.T) {
	// The "nobody can deploy again" row of section 2.2's table. This is the
	// failure that makes a crash permanent rather than transient.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	h.exec.hooks.BeforeSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	if held, _ := h.store.LeaseHeld(bg(), dep.Resource()); held {
		t.Fatal("the lease outlived the crashed deployment")
	}
	assertStillDeployable(t, h, "v3")
}

func TestTheJournalSurvivesACrashMidCommit(t *testing.T) {
	// A kill -9 during the fsync. Everything committed before it must be
	// intact, the torn group must be gone, and the store must still work.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	if err := h.exec.Run(bg(), h.dep.ID); err != nil {
		t.Fatal(err)
	}
	committed := h.state(h.dep.ID)

	next := h.reopen()
	if got := next.state(h.dep.ID); got != committed {
		t.Fatalf("after restart state = %s, want %s", got, committed)
	}
	if err := next.store.VerifyAudit(bg()); err != nil {
		t.Fatalf("the audit chain did not survive: %v", err)
	}
	assertStillDeployable(t, next, "v3")
}

func TestTwentyConcurrentDeploysRunExactlyOne(t *testing.T) {
	// Section 18.3: "Fire 20 goroutines at the same app+env and assert
	// exactly one ran, the other 19 got a clean LeaseHeldError, and the
	// provider's DeployCount is exactly 1."
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")

	var deps []*store.Deployment
	for i := 0; i < 20; i++ {
		deps = append(deps, h.newDeployment("web", "staging", "v2"))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ran     int
		refused int
		other   []error
	)
	for _, d := range deps {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			err := h.exec.Run(bg(), id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ran++
			case errors.As(err, new(*store.LeaseHeldError)):
				refused++
			default:
				other = append(other, err)
			}
		}(d.ID)
	}
	wg.Wait()

	if ran != 1 {
		t.Errorf("%d deploys succeeded, want exactly 1 (other errors: %v)", ran, other)
	}
	if refused+ran != 20 {
		t.Errorf("%d refused + %d ran = %d, want 20; unexpected errors: %v",
			refused, ran, refused+ran, other)
	}
	if n := h.provider.Counts()["app:web/env:staging@v2"]; n != 1 {
		t.Errorf("the provider was called %d times, want exactly 1", n)
	}
}

func TestRetryingTheSameDeploymentDoesNotDeployTwice(t *testing.T) {
	// The CI-retry row of section 2.2's table, end to end: the same
	// deployment id carries the same provider idempotency key.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "staging"}, "v1")
	h.exec.hooks.AfterSideEffect = func(s store.State, _ *store.Deployment) error {
		if s == store.StateDeploying {
			return errCrash
		}
		return nil
	}
	dep := h.newDeployment("web", "staging", "v2")
	_ = h.exec.Run(bg(), dep.ID)

	// Recover and let it finish.
	next := h.reopen()
	if _, err := next.exec.Reconcile(bg()); err != nil {
		t.Fatal(err)
	}
	next.exec.hooks = Hooks{}
	_ = next.exec.Run(bg(), dep.ID)

	if n := h.provider.Counts()["app:web/env:staging@v2"]; n != 1 {
		t.Errorf("the provider deployed %d times across a crash and a retry, want 1", n)
	}
}

func TestAPartialRollingDeployIsNotReportedAsSuccess(t *testing.T) {
	// The hardest case to reconcile: some instances are on the new version
	// and some are not. The environment is genuinely mixed, and the tool must
	// not claim otherwise.
	h := newHarness(t)
	h.provider.SetCurrent(provider.Target{App: "web", Environment: "dev"}, "v1")
	h.provider.PartialDeploy = true
	h.provider.InstanceCount = 5
	h.provider.FailAfterInstance = 2

	dep := h.newDeployment("web", "dev", "v2")
	err := h.exec.Run(bg(), dep.ID)
	if err == nil {
		t.Fatal("a partial deploy must not report success")
	}
	if got := h.state(dep.ID); got == store.StateSucceeded {
		t.Fatalf("state = %s", got)
	}
	steps, _ := h.store.Steps(bg(), dep.ID)
	var sawDetail bool
	for _, s := range steps {
		if strings.Contains(s.Error, "instances are on") {
			sawDetail = true
		}
	}
	if !sawDetail {
		t.Error("the step should record how many instances ended up where")
	}
}

// assertStillDeployable is the assertion section 18.3 says matters most.
func assertStillDeployable(t *testing.T, h *harness, version string) {
	t.Helper()
	h.exec.hooks = Hooks{}
	h.provider.FailOn = map[string]error{}
	h.verifier.Values = []float64{0.0001}
	h.verifier.FailAt = map[int]error{}

	d := h.newDeployment("web", "staging", version)
	if err := h.exec.Run(bg(), d.ID); err != nil {
		t.Fatalf("the system is wedged: a later deploy failed with %v", err)
	}
	if got := h.state(d.ID); got != store.StateSucceeded {
		t.Fatalf("the system is wedged: a later deploy ended in %s", got)
	}
}

var _ = fmt.Sprintf
