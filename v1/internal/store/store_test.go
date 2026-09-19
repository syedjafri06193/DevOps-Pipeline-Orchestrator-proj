package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/ulid"
)

// testClock lets the tests control lease expiry and backoff without sleeping.
// Section 19.4 makes time a correctness concern here, and a correctness
// concern that can only be tested by waiting is a correctness concern that is
// not tested.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStore(t *testing.T) (*Store, *testClock, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "orch.journal")
	clk := newClock()
	s, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk, path
}

func newDeployment(app, env, version string) *Deployment {
	return &Deployment{
		ID:            ulid.New().String(),
		App:           app,
		Environment:   env,
		Version:       version,
		Strategy:      "rolling",
		State:         StatePending,
		TriggeredBy:   "jordan",
		TriggerSource: TriggerCLI,
		Provider:      "fake",
	}
}

func ctx() context.Context { return context.Background() }

// ------------------------------------------------------- the state machine

func TestTerminalStatesAreAbsorbing(t *testing.T) {
	// Invariant 1 from section 18.2.
	for _, from := range AllStates() {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range AllStates() {
			if CanTransition(from, to) {
				t.Errorf("terminal state %s must not transition to %s", from, to)
			}
		}
	}
}

func TestOnlyRollbackFailedAndUnknownPage(t *testing.T) {
	// Section 5: "ROLLBACK_FAILED is the only state that pages." UNKNOWN is
	// reconciliation's version of the same finding.
	for _, s := range AllStates() {
		want := s == StateRollbackFailed || s == StateUnknown
		if s.Pages() != want {
			t.Errorf("%s.Pages() = %v, want %v", s, s.Pages(), want)
		}
	}
}

func TestARollbackNeverReachesSucceeded(t *testing.T) {
	// ROLLING_BACK goes to ROLLED_BACK or ROLLBACK_FAILED. It cannot loop
	// back into the deploy path, which is what would make an infinite
	// rollback possible.
	for _, to := range AllStates() {
		if to == StateRolledBack || to == StateRollbackFailed || to == StateUnknown {
			continue
		}
		if CanTransition(StateRollingBack, to) {
			t.Errorf("ROLLING_BACK must not transition to %s", to)
		}
	}
}

func TestEveryNonTerminalStateCanReachATerminalOne(t *testing.T) {
	// A state with no way out is a deployment that wedges forever.
	for _, from := range AllStates() {
		if from.IsTerminal() {
			continue
		}
		if !reaches(from, map[State]bool{}) {
			t.Errorf("no path from %s to any terminal state", from)
		}
	}
}

func reaches(s State, seen map[State]bool) bool {
	if s.IsTerminal() {
		return true
	}
	if seen[s] {
		return false
	}
	seen[s] = true
	for _, next := range allowedTransitions[s] {
		if reaches(next, seen) {
			return true
		}
	}
	return false
}

func TestIllegalTransitionsAreRefused(t *testing.T) {
	s, _, _ := newStore(t)
	d, _, err := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	if err != nil {
		t.Fatal(err)
	}
	fence, err := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	// PENDING cannot jump straight to SUCCEEDED.
	err = s.TransitionState(ctx(), d.ID, fence, StatePending, StateSucceeded)
	var ill *IllegalTransitionError
	if !errors.As(err, &ill) {
		t.Fatalf("want IllegalTransitionError, got %v", err)
	}
}

func TestATransitionFromTheWrongStateIsRefused(t *testing.T) {
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	if err := s.TransitionState(ctx(), d.ID, fence, StatePending, StatePreflight); err != nil {
		t.Fatal(err)
	}
	// Something else already moved it; this caller's view is stale.
	err := s.TransitionState(ctx(), d.ID, fence, StatePending, StatePreflight)
	var unexpected *UnexpectedStateError
	if !errors.As(err, &unexpected) {
		t.Fatalf("want UnexpectedStateError, got %v", err)
	}
}

// ------------------------------------------------------------ idempotency

func TestTheSameIdempotencyKeyReturnsTheOriginal(t *testing.T) {
	// Section 6.3: a CI retry must not deploy twice.
	s, _, _ := newStore(t)
	first := newDeployment("web", "prod", "v2")
	first.IdempotencyKey = "ci-run-1234"

	a, created, err := s.CreateDeployment(ctx(), first)
	if err != nil || !created {
		t.Fatalf("first create: %v created=%v", err, created)
	}

	second := newDeployment("web", "prod", "v2")
	second.IdempotencyKey = "ci-run-1234"
	b, created, err := s.CreateDeployment(ctx(), second)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second create must not make a new deployment")
	}
	if b.ID != a.ID {
		t.Errorf("got a different deployment: %s vs %s", b.ID, a.ID)
	}
}

func TestConcurrentDuplicatesCollapseToOne(t *testing.T) {
	// The UNIQUE constraint in the document's schema; here, the lock.
	s, _, _ := newStore(t)
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := newDeployment("web", "prod", "v2")
			d.IdempotencyKey = "same-key"
			got, _, err := s.CreateDeployment(ctx(), d)
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids[i] = got.ID
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("goroutine %d got %s, first got %s", i, id, ids[0])
		}
	}
	all, _ := s.ListDeployments(ctx(), ListFilter{})
	if len(all) != 1 {
		t.Fatalf("created %d deployments, want 1", len(all))
	}
}

// ----------------------------------------------------------------- leases

func TestALeaseExcludesOtherHolders(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.AcquireLease(ctx(), "app:web/env:prod", "dep-1", "node-1"); err != nil {
		t.Fatal(err)
	}
	_, err := s.AcquireLease(ctx(), "app:web/env:prod", "dep-2", "node-1")
	var held *LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("want LeaseHeldError, got %v", err)
	}
	// The error has to name the holder and the expiry, or the person reading
	// it has nowhere to go.
	if !strings.Contains(held.Error(), "dep-1") || !strings.Contains(held.Error(), "until") {
		t.Errorf("unhelpful error: %v", held)
	}
}

func TestExactlyOneOfTwentyConcurrentDeploysRuns(t *testing.T) {
	// Section 18.3: "Fire 20 goroutines at the same app+env and assert
	// exactly one ran, the other 19 got a clean LeaseHeldError."
	s, _, _ := newStore(t)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
		refused  int
		other    []error
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AcquireLease(ctx(), "app:web/env:prod", fmt.Sprintf("dep-%d", i), "node-1")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				acquired++
			case errors.As(err, new(*LeaseHeldError)):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()

	if acquired != 1 {
		t.Errorf("%d goroutines acquired the lease, want exactly 1", acquired)
	}
	if refused != 19 {
		t.Errorf("%d goroutines got LeaseHeldError, want 19", refused)
	}
	if len(other) > 0 {
		t.Errorf("unexpected errors: %v", other)
	}
}

func TestAnExpiredLeaseCanBeTakenOver(t *testing.T) {
	// The whole reason for a lease rather than a lock: a crashed holder does
	// not block the resource forever.
	s, clk, _ := newStore(t)
	if _, err := s.AcquireLease(ctx(), "r", "dep-1", "node-1"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(LeaseTTL + time.Second)
	fence, err := s.AcquireLease(ctx(), "r", "dep-2", "node-2")
	if err != nil {
		t.Fatalf("an expired lease must be takeable: %v", err)
	}
	if fence != 2 {
		t.Errorf("fence = %d, want 2", fence)
	}
}

func TestFenceTokensAreMonotonicAcrossReleases(t *testing.T) {
	// Not just across expiries. A zombie that held the lease before a clean
	// release, on an idle resource, must still be fenced out.
	s, _, _ := newStore(t)
	f1, _ := s.AcquireLease(ctx(), "r", "dep-1", "node-1")
	if err := s.ReleaseLease(ctx(), "r", "dep-1"); err != nil {
		t.Fatal(err)
	}
	f2, _ := s.AcquireLease(ctx(), "r", "dep-2", "node-1")
	if f2 <= f1 {
		t.Fatalf("fence did not advance across a release: %d then %d", f1, f2)
	}
}

func TestAZombieWithAStaleFenceCannotMutateState(t *testing.T) {
	// Section 6.2: "A lease expiring doesn't stop the old holder — it may be
	// paused, or partitioned, and will resume believing it still holds the
	// lease."
	s, clk, _ := newStore(t)
	zombie, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	staleFence, _ := s.AcquireLease(ctx(), zombie.Resource(), zombie.ID, "node-1")

	// The zombie is paused here. The lease expires and someone else takes it.
	clk.Advance(LeaseTTL + time.Second)
	successor, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v3"))
	if _, err := s.AcquireLease(ctx(), successor.Resource(), successor.ID, "node-2"); err != nil {
		t.Fatal(err)
	}

	// The zombie wakes up and tries to carry on.
	err := s.TransitionState(ctx(), zombie.ID, staleFence, StatePending, StatePreflight)
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("a resumed zombie must be fenced out, got %v", err)
	}
}

func TestRenewalFailsOnceTheLeaseIsLost(t *testing.T) {
	// The executor's abort signal (section 6.1).
	s, clk, _ := newStore(t)
	fence, _ := s.AcquireLease(ctx(), "r", "dep-1", "node-1")
	if err := s.RenewLease(ctx(), "r", "dep-1", fence); err != nil {
		t.Fatalf("renewing a live lease should work: %v", err)
	}
	clk.Advance(LeaseTTL + time.Second)
	if _, err := s.AcquireLease(ctx(), "r", "dep-2", "node-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewLease(ctx(), "r", "dep-1", fence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("renewal must fail once the lease is gone, got %v", err)
	}
}

func TestAnExpiredLeaseCannotBeResurrectedByItsHolder(t *testing.T) {
	// Someone else may be part-way through acquiring. A lease that can be
	// un-expired is not a lease.
	s, clk, _ := newStore(t)
	fence, _ := s.AcquireLease(ctx(), "r", "dep-1", "node-1")
	clk.Advance(LeaseTTL + time.Second)
	if err := s.RenewLease(ctx(), "r", "dep-1", fence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestBreakingALeaseIsAudited(t *testing.T) {
	// It is the operation that can cause two executors to act at once.
	s, _, _ := newStore(t)
	if _, err := s.AcquireLease(ctx(), "r", "dep-1", "node-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.BreakLease(ctx(), "r", "alex"); err != nil {
		t.Fatal(err)
	}
	held, _ := s.LeaseHeld(ctx(), "r")
	if held {
		t.Error("the lease should be gone")
	}
	entries, _ := s.AuditEntries(ctx(), 0)
	var found bool
	for _, e := range entries {
		if e.Action == "lease.broken" && e.Actor == "alex" && e.Detail["previous_holder"] == "dep-1" {
			found = true
		}
	}
	if !found {
		t.Error("breaking a lease must be audited, naming who and whose it was")
	}
}

// --------------------------------------------------------------- recovery

func TestEverythingCommittedSurvivesAReopen(t *testing.T) {
	s, clk, path := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	if err := s.TransitionState(ctx(), d.ID, fence, StatePending, StatePreflight); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSample(ctx(), &HealthSample{
		DeploymentID: d.ID, Verifier: "fake", Criterion: "error-rate",
		At: clk.Now(), Value: 0.01, Baseline: 0.01, Healthy: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got, err := reopened.Deployment(ctx(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePreflight {
		t.Errorf("state = %s, want PREFLIGHT", got.State)
	}
	samples, _ := reopened.Samples(ctx(), d.ID)
	if len(samples) != 1 {
		t.Errorf("samples = %d, want 1", len(samples))
	}
	lease, err := reopened.Lease(ctx(), d.Resource())
	if err != nil {
		t.Fatal(err)
	}
	if lease.FenceToken != fence {
		t.Errorf("fence = %d, want %d", lease.FenceToken, fence)
	}
}

func TestATornTailIsDiscardedAndTheStoreStillWorks(t *testing.T) {
	// The crash-during-write case. Everything before the tear must survive,
	// the torn record must vanish, and the store must be usable afterwards --
	// which is the assertion that actually matters (section 18.3).
	s, clk, path := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Append a plausible but incomplete record, as a kill -9 mid-write would.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{'O', 'R', 'J', '1', 0, 0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reopened, err := Open(path, clk)
	if err != nil {
		t.Fatalf("a torn tail must not stop the store from opening: %v", err)
	}
	defer reopened.Close()

	if _, err := reopened.Deployment(ctx(), d.ID); err != nil {
		t.Errorf("the committed deployment should have survived: %v", err)
	}
	// And the system still works.
	if _, _, err := reopened.CreateDeployment(ctx(), newDeployment("web", "prod", "v3")); err != nil {
		t.Errorf("the store must be usable after recovery: %v", err)
	}
}

func TestAnUncommittedGroupIsDiscardedWholesale(t *testing.T) {
	// A transaction is all-or-nothing: the commit marker is written last, so
	// a crash before it discards every record in the group.
	s, clk, path := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")

	// The next commit will write only part of its bytes and no marker.
	s.j.failWrite = func(n int) error { return &partialWriteError{Bytes: n / 2} }
	err := s.TransitionStateAndNotify(ctx(), d.ID, fence, StatePending, StatePreflight,
		AuditEntry{Actor: "jordan", Action: "deployment.preflight"},
		[]OutboxEntry{{Notifier: "slack", Payload: `{"text":"started"}`}})
	if err == nil {
		t.Fatal("expected the simulated write failure to surface")
	}
	s.Close()

	reopened, err := Open(path, clk)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, _ := reopened.Deployment(ctx(), d.ID)
	if got.State != StatePending {
		t.Errorf("state = %s; the uncommitted transition must be gone", got.State)
	}
	entries, _ := reopened.AuditEntries(ctx(), 0)
	for _, e := range entries {
		if e.Action == "deployment.preflight" {
			t.Error("the audit row from the uncommitted group must be gone too")
		}
	}
	pending, _ := reopened.PendingOutbox(ctx())
	if pending != 0 {
		t.Errorf("outbox = %d; the notification must not survive its transaction", pending)
	}
}

func TestACorruptedPayloadIsRefusedNotTruncated(t *testing.T) {
	// A flipped bit inside a complete record is a different fault from a torn
	// tail. Silently discarding everything after it would hide data loss.
	s, clk, path := newStore(t)
	if _, _, err := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v3")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first record's payload.
	data[headerSize+10] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, clk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestACommitIsDurableNotBuffered(t *testing.T) {
	s, _, _ := newStore(t)
	before := s.Syncs()
	if _, _, err := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2")); err != nil {
		t.Fatal(err)
	}
	if s.Syncs() != before+1 {
		t.Errorf("a commit must fsync exactly once: %d -> %d", before, s.Syncs())
	}
}

func TestOneTransactionIsOneSync(t *testing.T) {
	// Section 6.4 puts the state change, the audit row and the outbox entry in
	// one transaction. If that were three syncs it would also be three
	// windows in which a crash splits them.
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	before := s.Syncs()
	if err := s.TransitionStateAndNotify(ctx(), d.ID, fence, StatePending, StatePreflight,
		AuditEntry{Actor: "jordan", Action: "deployment.preflight"},
		[]OutboxEntry{{Notifier: "slack", Payload: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Syncs() - before; got != 1 {
		t.Errorf("syncs = %d, want 1", got)
	}
}

// ------------------------------------------------------------------ audit

func TestTheAuditChainVerifies(t *testing.T) {
	s, _, _ := newStore(t)
	for i := 0; i < 5; i++ {
		d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", fmt.Sprintf("v%d", i)))
		_ = s.AppendAudit(ctx(), AuditEntry{
			Actor: "jordan", Action: "deployment.viewed", Resource: d.Resource(),
		})
	}
	if err := s.VerifyAudit(ctx()); err != nil {
		t.Fatalf("a freshly written chain must verify: %v", err)
	}
	if s.AuditHead() == GenesisHash {
		t.Error("the head should have moved")
	}
}

func TestTamperingWithAnAuditEntryIsDetected(t *testing.T) {
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	_ = s.AppendAudit(ctx(), AuditEntry{Actor: "jordan", Action: "deploy.override", Resource: d.Resource()})

	// An attacker with database access rewrites who did it.
	s.mu.Lock()
	s.audit[1].Actor = "someone-else"
	s.mu.Unlock()

	err := s.VerifyAudit(ctx())
	if err == nil {
		t.Fatal("a modified entry must break verification")
	}
	if !strings.Contains(err.Error(), "modified") {
		t.Errorf("say what happened: %v", err)
	}
}

func TestTheDetailMapIsCoveredByTheHash(t *testing.T) {
	// The document's payload is prev|at|actor|action|resource and leaves the
	// detail map out -- which lets an attacker change which version was
	// deployed, or which environment, without breaking the chain. The
	// interesting facts live in `detail`.
	s, _, _ := newStore(t)
	_ = s.AppendAudit(ctx(), AuditEntry{
		Actor: "jordan", Action: "deployment.created", Resource: "app:web/env:prod",
		Detail: map[string]string{"version": "v2-safe"},
	})
	s.mu.Lock()
	s.audit[0].Detail["version"] = "v2-malicious"
	s.mu.Unlock()

	if err := s.VerifyAudit(ctx()); err == nil {
		t.Fatal("changing the detail map must break the chain")
	}
}

func TestDeletingAnAuditEntryIsDetected(t *testing.T) {
	s, _, _ := newStore(t)
	for i := 0; i < 4; i++ {
		_ = s.AppendAudit(ctx(), AuditEntry{Actor: "a", Action: "x", Resource: "r"})
	}
	s.mu.Lock()
	s.audit = append(s.audit[:1], s.audit[2:]...) // excise entry 2
	s.mu.Unlock()

	err := s.VerifyAudit(ctx())
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("a deletion must break the chain: %v", err)
	}
}

func TestSeveralAuditEntriesInOneTransactionChainCorrectly(t *testing.T) {
	// If two entries in one transaction both chained onto the pre-transaction
	// head, the chain would fork and verification would fail on a perfectly
	// honest write.
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	if err := s.TransitionStateAndNotify(ctx(), d.ID, fence, StatePending, StatePreflight,
		AuditEntry{Actor: "jordan", Action: "deployment.preflight"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyAudit(ctx()); err != nil {
		t.Fatalf("chain broken by a multi-entry transaction: %v", err)
	}
}

func TestTheAuditChainSurvivesAReopen(t *testing.T) {
	s, clk, path := newStore(t)
	for i := 0; i < 3; i++ {
		_, _, _ = s.CreateDeployment(ctx(), newDeployment("web", "prod", fmt.Sprintf("v%d", i)))
	}
	head := s.AuditHead()
	s.Close()

	reopened, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.AuditHead() != head {
		t.Error("the chain head must survive a restart, or external anchoring is useless")
	}
	if err := reopened.VerifyAudit(ctx()); err != nil {
		t.Fatal(err)
	}
}

// ----------------------------------------------------------------- outbox

func TestTheOutboxCommitsWithTheStateChange(t *testing.T) {
	// Section 6.4: never call Slack inside the transaction; never commit the
	// state change without the notification.
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	fence, _ := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-1")
	if err := s.TransitionStateAndNotify(ctx(), d.ID, fence, StatePending, StatePreflight,
		AuditEntry{Actor: "jordan", Action: "deployment.preflight"},
		[]OutboxEntry{{Notifier: "slack", Payload: `{"text":"started"}`}}); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueOutbox(ctx(), 0)
	if len(due) != 1 {
		t.Fatalf("outbox = %d, want 1", len(due))
	}
}

func TestFailedDeliveryBacksOff(t *testing.T) {
	s, clk, _ := newStore(t)
	e := &OutboxEntry{Notifier: "slack", Payload: "{}"}
	if err := s.Enqueue(ctx(), e); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueOutbox(ctx(), 0)
	if len(due) != 1 {
		t.Fatal("should be due immediately")
	}
	if err := s.MarkFailed(ctx(), due[0].ID, errors.New("slack is down")); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DueOutbox(ctx(), 0); len(again) != 0 {
		t.Error("a failed entry must not be retried immediately")
	}
	clk.Advance(10 * time.Second)
	if again, _ := s.DueOutbox(ctx(), 0); len(again) != 1 {
		t.Error("it must come back after the backoff")
	}
}

func TestBackoffIsCapped(t *testing.T) {
	// A Slack outage should not schedule the next attempt for next week.
	if got := backoff(30); got > 5*time.Minute {
		t.Errorf("backoff(30) = %v, want <= 5m", got)
	}
	if backoff(1) >= backoff(4) {
		t.Error("backoff should grow")
	}
}

func TestDeliveredEntriesStopBeingDue(t *testing.T) {
	s, _, _ := newStore(t)
	e := &OutboxEntry{Notifier: "slack", Payload: "{}"}
	_ = s.Enqueue(ctx(), e)
	due, _ := s.DueOutbox(ctx(), 0)
	if err := s.MarkDelivered(ctx(), due[0].ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DueOutbox(ctx(), 0); len(again) != 0 {
		t.Error("a delivered entry must not be sent again")
	}
	if n, _ := s.PendingOutbox(ctx()); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

func TestOutboxSurvivesARestartUndelivered(t *testing.T) {
	// "Slack is down when a deploy finishes → notification lost silently" is
	// in section 2.2's table of things that separate a real tool from a script.
	s, clk, path := newStore(t)
	_ = s.Enqueue(ctx(), &OutboxEntry{Notifier: "slack", Payload: `{"text":"deploy finished"}`})
	s.Close()

	reopened, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if n, _ := reopened.PendingOutbox(ctx()); n != 1 {
		t.Errorf("pending after restart = %d, want 1", n)
	}
}

// --------------------------------------------------------------- queries

func TestListIsNewestFirstWithoutSorting(t *testing.T) {
	s, _, _ := newStore(t)
	var ids []string
	for i := 0; i < 5; i++ {
		d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", fmt.Sprintf("v%d", i)))
		ids = append(ids, d.ID)
	}
	got, _ := s.ListDeployments(ctx(), ListFilter{App: "web", Environment: "prod"})
	if len(got) != 5 {
		t.Fatalf("got %d", len(got))
	}
	for i := range got {
		if got[i].ID != ids[len(ids)-1-i] {
			t.Fatalf("position %d = %s, want %s", i, got[i].ID, ids[len(ids)-1-i])
		}
	}
}

func TestLastSuccessfulBeforeSkipsFailures(t *testing.T) {
	// A deployment that failed and rolled back is not somewhere to roll back
	// to, which is the bug in taking "the previous row".
	s, _, _ := newStore(t)
	good := mustSucceed(t, s, "web", "prod", "v1")
	bad, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	driveTo(t, s, bad, StateFailed)
	current, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v3"))

	target, err := s.LastSuccessfulBefore(ctx(), "web", "prod", current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != good.ID {
		t.Errorf("rollback target = %s (%s), want %s (v1)", target.ID, target.Version, good.ID)
	}
}

func TestCountRollbacksPowersTheCircuitBreaker(t *testing.T) {
	// Section 7.4: two rollbacks of a version in 24h and further deploys of
	// it are blocked.
	s, clk, _ := newStore(t)
	for i := 0; i < 2; i++ {
		d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2-bad"))
		driveTo(t, s, d, StateRolledBack)
	}
	n, err := s.CountRollbacks(ctx(), "web", "prod", "v2-bad", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rollbacks = %d, want 2", n)
	}
	// And the window is real.
	clk.Advance(25 * time.Hour)
	n, _ = s.CountRollbacks(ctx(), "web", "prod", "v2-bad", 24*time.Hour)
	if n != 0 {
		t.Errorf("rollbacks outside the window = %d, want 0", n)
	}
}

func TestNonTerminalDeploymentsAreWhatReconcileWalks(t *testing.T) {
	s, _, _ := newStore(t)
	stuck, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	driveTo(t, s, stuck, StateDeploying)
	done := mustSucceed(t, s, "api", "prod", "v9")

	orphans, _ := s.NonTerminalDeployments(ctx())
	if len(orphans) != 1 || orphans[0].ID != stuck.ID {
		t.Fatalf("orphans = %v, want just %s", orphans, stuck.ID)
	}
	_ = done
}

// --------------------------------------------------------------- freezes

func TestAFreezeIsActiveUntilLifted(t *testing.T) {
	s, clk, _ := newStore(t)
	if err := s.SetFreeze(ctx(), &Freeze{
		Resource: "app:web/env:prod", Reason: "incident 4471", By: "alex",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Freeze(ctx(), "app:web/env:prod"); !ok {
		t.Fatal("the freeze should be active")
	}
	if err := s.SetFreeze(ctx(), &Freeze{
		Resource: "app:web/env:prod", Reason: "incident 4471", By: "alex",
		Lifted: true, LiftedBy: "alex", At: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Freeze(ctx(), "app:web/env:prod"); ok {
		t.Error("the freeze should be lifted")
	}
}

func TestATimedFreezeExpires(t *testing.T) {
	s, clk, _ := newStore(t)
	until := clk.Now().Add(time.Hour)
	_ = s.SetFreeze(ctx(), &Freeze{Resource: "r", Reason: "change window", By: "alex", Until: &until})
	if _, ok := s.Freeze(ctx(), "r"); !ok {
		t.Fatal("should be frozen")
	}
	clk.Advance(2 * time.Hour)
	if _, ok := s.Freeze(ctx(), "r"); ok {
		t.Error("a timed freeze should expire on its own")
	}
}

// --------------------------------------------------------------- approvals

func TestClickingApproveTwiceIsOneApproval(t *testing.T) {
	// Otherwise a require_peer check counting approvals is satisfied by one
	// person clicking twice.
	s, _, _ := newStore(t)
	d, _, _ := s.CreateDeployment(ctx(), newDeployment("web", "prod", "v2"))
	for i := 0; i < 3; i++ {
		if err := s.RecordApproval(ctx(), &Approval{
			DeploymentID: d.ID, User: "sam", Source: "slack",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Approvals(ctx(), d.ID)
	if len(got) != 1 {
		t.Fatalf("approvals = %d, want 1", len(got))
	}
}

// ---------------------------------------------------------------- helpers

func driveTo(t *testing.T, s *Store, d *Deployment, target State) {
	t.Helper()
	path := map[State][]State{
		StateDeploying:  {StatePreflight, StateDeploying},
		StateFailed:     {StatePreflight, StateFailed},
		StateSucceeded:  {StatePreflight, StateDeploying, StateVerifying, StatePromoting, StateSucceeded},
		StateRolledBack: {StatePreflight, StateDeploying, StateRollingBack, StateRolledBack},
	}[target]
	if path == nil {
		t.Fatalf("no path defined to %s", target)
	}
	fence, err := s.AcquireLease(ctx(), d.Resource(), d.ID, "node-test")
	if err != nil {
		t.Fatal(err)
	}
	from := d.State
	for _, to := range path {
		if err := s.TransitionState(ctx(), d.ID, fence, from, to); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		from = to
	}
	if err := s.ReleaseLease(ctx(), d.Resource(), d.ID); err != nil {
		t.Fatal(err)
	}
}

func mustSucceed(t *testing.T, s *Store, app, env, version string) *Deployment {
	t.Helper()
	d, _, err := s.CreateDeployment(ctx(), newDeployment(app, env, version))
	if err != nil {
		t.Fatal(err)
	}
	driveTo(t, s, d, StateSucceeded)
	got, _ := s.Deployment(ctx(), d.ID)
	return got
}
