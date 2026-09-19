package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Section 6.1's values. Defaults rather than constants: a single-node
// deployment on a fast network wants a shorter TTL so a crash frees the
// resource sooner, and section 19.4's clock-skew discussion means a
// multi-node deployment may want a longer one. The invariant that must hold
// is TTL > heartbeat by a comfortable margin, checked in SetLeaseTiming.
const (
	DefaultLeaseTTL       = 60 * time.Second
	DefaultLeaseHeartbeat = 15 * time.Second
)

// LeaseTTL and LeaseHeartbeat are the process-wide defaults.
var (
	LeaseTTL       = DefaultLeaseTTL
	LeaseHeartbeat = DefaultLeaseHeartbeat
)

// GenesisHash is the previous-hash of the first audit entry (section 12.4).
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

var (
	ErrNotFound   = errors.New("store: not found")
	ErrStaleFence = errors.New("store: stale fence token")
	ErrFrozen     = errors.New("store: deploys are frozen for this target")
)

// Clock is injected so that lease expiry, approval TTLs and backoff can be
// tested without sleeping. Section 19.4 makes time a correctness concern in
// this system, and correctness concerns need to be controllable in tests.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// RealClock is the production clock.
var RealClock Clock = realClock{}

// Store is the system of record.
//
// The in-memory maps are the index; the journal is the truth. Every mutation
// appends to the journal and then updates the index, never the reverse, so a
// crash between the two loses the index update and recovery rebuilds it from
// the journal.
type Store struct {
	mu sync.RWMutex

	j     *journal
	clock Clock

	deployments map[string]*Deployment
	order       []string // deployment ids, ULID order
	byIdemKey   map[string]string
	steps       map[string][]*Step
	leases      map[string]*Lease
	samples     map[string][]*HealthSample
	approvals   map[string][]*Approval
	freezes     map[string]*Freeze

	leaseTTL time.Duration

	audit     []*AuditEntry
	auditHead string

	outbox    []*OutboxEntry
	outboxSeq int64
}

// Open opens or creates a store at path and replays the journal.
func Open(path string, clock Clock) (*Store, error) {
	if clock == nil {
		clock = RealClock
	}
	j, err := openJournal(path)
	if err != nil {
		return nil, err
	}
	s := &Store{
		j:           j,
		clock:       clock,
		deployments: map[string]*Deployment{},
		byIdemKey:   map[string]string{},
		steps:       map[string][]*Step{},
		leases:      map[string]*Lease{},
		samples:     map[string][]*HealthSample{},
		approvals:   map[string][]*Approval{},
		freezes:     map[string]*Freeze{},
		auditHead:   GenesisHash,
		leaseTTL:    LeaseTTL,
	}
	if err := j.replay(s.applyRecord); err != nil {
		_ = j.close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.j.close() }

// SetLeaseTTL overrides the lease duration for this store.
//
// Exported for tests running on a virtual clock, where a deploy's simulated
// ten-minute bake would otherwise outlive a sixty-second lease, and for
// deployments that have measured their own failover time.
func (s *Store) SetLeaseTTL(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseTTL = d
}

// LeaseTTL reports the configured lease duration.
func (s *Store) LeaseTTL() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaseTTL
}

// Now exposes the store's clock so callers share one notion of time.
func (s *Store) Now() time.Time { return s.clock.Now() }

// applyRecord rebuilds the index from one journal record. It must be a pure
// function of the record: anything that consults the wall clock here would
// make recovery depend on when it happened.
func (s *Store) applyRecord(rec record) error {
	switch rec.Kind {
	case kindDeployment:
		var d Deployment
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		if _, exists := s.deployments[d.ID]; !exists {
			s.order = append(s.order, d.ID)
		}
		cp := d
		s.deployments[d.ID] = &cp
		if d.IdempotencyKey != "" {
			s.byIdemKey[d.IdempotencyKey] = d.ID
		}

	case kindStep:
		var st Step
		if err := json.Unmarshal(rec.Data, &st); err != nil {
			return err
		}
		s.putStep(&st)

	case kindLease:
		var l Lease
		if err := json.Unmarshal(rec.Data, &l); err != nil {
			return err
		}
		cp := l
		s.leases[l.Resource] = &cp

	case kindLeaseRelease:
		var l Lease
		if err := json.Unmarshal(rec.Data, &l); err != nil {
			return err
		}
		// Release keeps the row so the fence token stays monotonic across
		// releases. A released lease is one whose expiry is in the past.
		if cur, ok := s.leases[l.Resource]; ok && cur.Holder == l.Holder {
			cur.ExpiresAt = l.ExpiresAt
			cur.Holder = ""
		}

	case kindAudit:
		var a AuditEntry
		if err := json.Unmarshal(rec.Data, &a); err != nil {
			return err
		}
		cp := a
		s.audit = append(s.audit, &cp)
		s.auditHead = a.Hash

	case kindOutbox:
		var o OutboxEntry
		if err := json.Unmarshal(rec.Data, &o); err != nil {
			return err
		}
		cp := o
		s.outbox = append(s.outbox, &cp)
		if o.ID > s.outboxSeq {
			s.outboxSeq = o.ID
		}

	case kindOutboxState:
		var o OutboxEntry
		if err := json.Unmarshal(rec.Data, &o); err != nil {
			return err
		}
		for _, e := range s.outbox {
			if e.ID == o.ID {
				e.Attempts = o.Attempts
				e.NextAttempt = o.NextAttempt
				e.DeliveredAt = o.DeliveredAt
				e.LastError = o.LastError
			}
		}

	case kindHealthSample:
		var h HealthSample
		if err := json.Unmarshal(rec.Data, &h); err != nil {
			return err
		}
		cp := h
		s.samples[h.DeploymentID] = append(s.samples[h.DeploymentID], &cp)

	case kindApproval:
		var a Approval
		if err := json.Unmarshal(rec.Data, &a); err != nil {
			return err
		}
		cp := a
		s.approvals[a.DeploymentID] = append(s.approvals[a.DeploymentID], &cp)

	case kindFreeze:
		var f Freeze
		if err := json.Unmarshal(rec.Data, &f); err != nil {
			return err
		}
		cp := f
		s.freezes[f.Resource] = &cp

	default:
		return fmt.Errorf("%w: unknown record kind %q", ErrCorrupt, rec.Kind)
	}
	return nil
}

func (s *Store) putStep(st *Step) {
	list := s.steps[st.DeploymentID]
	for i, existing := range list {
		if existing.Seq == st.Seq {
			cp := *st
			list[i] = &cp
			return
		}
	}
	cp := *st
	s.steps[st.DeploymentID] = append(list, &cp)
}

// ------------------------------------------------------------ transactions

// Tx batches mutations so they commit together.
//
// This is the transactional-outbox requirement made structural (section 6.4):
// "Write the notification into the outbox table in the same transaction as
// the state change." A caller cannot enqueue a notification outside a
// transaction, because Enqueue is a method on Tx.
type Tx struct {
	s    *Store
	recs []record
	// applyAfter runs against the index once the journal write succeeds.
	applyAfter []func()
}

func (s *Store) begin() *Tx { return &Tx{s: s} }

func (t *Tx) add(kind recordKind, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.recs = append(t.recs, record{Kind: kind, Data: data})
	return nil
}

// commit writes the whole group durably, then updates the index.
//
// The order is the point. If the process dies during the append, replay
// discards the group and the index is rebuilt without it. If it dies after
// the fsync but before the index update, the next open replays the group and
// the index contains it. There is no ordering that loses a committed record.
func (t *Tx) commit() error {
	if len(t.recs) == 0 {
		return nil
	}
	if err := t.s.j.appendGroup(t.recs); err != nil {
		return err
	}
	for _, fn := range t.applyAfter {
		fn()
	}
	return nil
}

// InTx runs fn inside a transaction.
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.begin()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.commit()
}

// ------------------------------------------------------------- deployments

// CreateDeployment inserts a deployment, honouring the idempotency key.
//
// Section 6.3: "Same key, same request → return the original result, don't
// re-run." Returning the existing row rather than an error is what makes a
// CI retry safe.
func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) (*Deployment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d.IdempotencyKey != "" {
		if id, ok := s.byIdemKey[d.IdempotencyKey]; ok {
			return clone(s.deployments[id]), false, nil
		}
	}
	if _, exists := s.deployments[d.ID]; exists {
		return nil, false, fmt.Errorf("store: deployment %s already exists", d.ID)
	}

	now := s.clock.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	if d.State == "" {
		d.State = StatePending
	}

	tx := s.begin()
	if err := tx.add(kindDeployment, d); err != nil {
		return nil, false, err
	}
	if err := s.appendAuditTx(tx, AuditEntry{
		At:       now,
		Actor:    d.TriggeredBy,
		Action:   "deployment.created",
		Resource: d.Resource(),
		Detail: map[string]string{
			"deployment": d.ID,
			"version":    d.Version,
			"source":     string(d.TriggerSource),
		},
	}); err != nil {
		return nil, false, err
	}
	cp := *d
	tx.applyAfter = append(tx.applyAfter, func() {
		s.order = append(s.order, cp.ID)
		s.deployments[cp.ID] = &cp
		if cp.IdempotencyKey != "" {
			s.byIdemKey[cp.IdempotencyKey] = cp.ID
		}
	})
	if err := tx.commit(); err != nil {
		return nil, false, err
	}
	return clone(&cp), true, nil
}

func (s *Store) Deployment(ctx context.Context, id string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.deployments[id]
	if !ok {
		return nil, fmt.Errorf("%w: deployment %s", ErrNotFound, id)
	}
	return clone(d), nil
}

func (s *Store) DeploymentByIdempotencyKey(ctx context.Context, key string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byIdemKey[key]
	if !ok {
		return nil, fmt.Errorf("%w: idempotency key", ErrNotFound)
	}
	return clone(s.deployments[id]), nil
}

// TransitionState moves a deployment, checking the fence token.
//
// Section 6.2's UPDATE, with its three conditions preserved: the row must be
// in the expected state, the caller must hold the lease, and the fence token
// must not be stale. Any of the three failing means someone else is now in
// charge of this resource and this caller must stop.
func (s *Store) TransitionState(ctx context.Context, depID string, fence int64, from, to State) error {
	return s.transition(ctx, depID, fence, from, to, nil, nil)
}

// TransitionStateAndNotify is section 6.4's `transitionAndNotify`: the state
// change, the audit row, and the outbox entry in one transaction.
func (s *Store) TransitionStateAndNotify(ctx context.Context, depID string, fence int64,
	from, to State, entry AuditEntry, notes []OutboxEntry) error {
	return s.transition(ctx, depID, fence, from, to, &entry, notes)
}

func (s *Store) transition(ctx context.Context, depID string, fence int64,
	from, to State, entry *AuditEntry, notes []OutboxEntry) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.deployments[depID]
	if !ok {
		return fmt.Errorf("%w: deployment %s", ErrNotFound, depID)
	}
	if d.State != from {
		return &UnexpectedStateError{DeploymentID: depID, Want: from, Got: d.State}
	}
	if !CanTransition(from, to) {
		return &IllegalTransitionError{From: from, To: to}
	}
	if to.RequiresLease() || from.RequiresLease() {
		if err := s.checkFenceLocked(d.Resource(), depID, fence); err != nil {
			return err
		}
	}

	now := s.clock.Now()
	next := *d
	next.State = to
	next.UpdatedAt = now
	if to.IsTerminal() {
		t := now
		next.FinishedAt = &t
	}

	tx := s.begin()
	if err := tx.add(kindDeployment, &next); err != nil {
		return err
	}
	if entry != nil {
		e := *entry
		if e.At.IsZero() {
			e.At = now
		}
		if e.Resource == "" {
			e.Resource = d.Resource()
		}
		if err := s.appendAuditTx(tx, e); err != nil {
			return err
		}
	}
	for i := range notes {
		if err := s.enqueueTx(tx, &notes[i]); err != nil {
			return err
		}
	}
	tx.applyAfter = append(tx.applyAfter, func() { s.deployments[depID] = &next })
	return tx.commit()
}

// UpdateDeployment persists whole-row changes that are not state transitions
// (the provider handle, the Slack message ts, the error text).
func (s *Store) UpdateDeployment(ctx context.Context, d *Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.deployments[d.ID]
	if !ok {
		return fmt.Errorf("%w: deployment %s", ErrNotFound, d.ID)
	}
	if cur.State != d.State {
		// State only moves through TransitionState, where the fence and the
		// legality of the move are checked. Allowing it here would make the
		// state machine advisory.
		return fmt.Errorf("store: use TransitionState to change state (%s -> %s)", cur.State, d.State)
	}
	next := *d
	next.UpdatedAt = s.clock.Now()

	tx := s.begin()
	if err := tx.add(kindDeployment, &next); err != nil {
		return err
	}
	tx.applyAfter = append(tx.applyAfter, func() { s.deployments[d.ID] = &next })
	return tx.commit()
}

// ListDeployments returns deployments newest first.
type ListFilter struct {
	App         string
	Environment string
	States      []State
	Limit       int
}

func (s *Store) ListDeployments(ctx context.Context, f ListFilter) ([]*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	want := map[State]bool{}
	for _, st := range f.States {
		want[st] = true
	}
	var out []*Deployment
	// ULIDs sort by time, so walking the index backwards is newest-first
	// without a sort (section 5).
	for i := len(s.order) - 1; i >= 0; i-- {
		d := s.deployments[s.order[i]]
		if f.App != "" && d.App != f.App {
			continue
		}
		if f.Environment != "" && d.Environment != f.Environment {
			continue
		}
		if len(want) > 0 && !want[d.State] {
			continue
		}
		out = append(out, clone(d))
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// ActiveDeployment returns the non-terminal deployment for a target, if any.
func (s *Store) ActiveDeployment(ctx context.Context, app, env string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.order) - 1; i >= 0; i-- {
		d := s.deployments[s.order[i]]
		if d.App == app && d.Environment == env && d.State.Active() {
			return clone(d), nil
		}
	}
	return nil, fmt.Errorf("%w: no active deployment", ErrNotFound)
}

// NonTerminalDeployments is what startup reconciliation walks (section 17.2).
func (s *Store) NonTerminalDeployments(ctx context.Context) ([]*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Deployment
	for _, id := range s.order {
		if d := s.deployments[id]; d.State.Active() {
			out = append(out, clone(d))
		}
	}
	return out, nil
}

// LastSuccessfulBefore is the rollback target (section 8.3).
//
// "Successful" here means SUCCEEDED, not "the previous row": a deployment
// that failed and rolled back is not somewhere to roll back to.
func (s *Store) LastSuccessfulBefore(ctx context.Context, app, env, beforeID string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		if id >= beforeID {
			continue
		}
		d := s.deployments[id]
		if d.App == app && d.Environment == env && d.State == StateSucceeded {
			return clone(d), nil
		}
	}
	return nil, fmt.Errorf("%w: no previous successful deployment of %s/%s", ErrNotFound, app, env)
}

// CountRollbacks powers the circuit breaker (section 7.4).
func (s *Store) CountRollbacks(ctx context.Context, app, env, version string, within time.Duration) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := s.clock.Now().Add(-within)
	n := 0
	for _, id := range s.order {
		d := s.deployments[id]
		if d.App != app || d.Environment != env || d.Version != version {
			continue
		}
		if d.CreatedAt.Before(cutoff) {
			continue
		}
		// Count deployments of this version that ended in a rollback, not
		// rollback deployments themselves -- the question the breaker asks is
		// "has this version been tried and pulled back before".
		if d.State == StateRolledBack || d.State == StateRollbackFailed {
			n++
		}
	}
	return n, nil
}

// ------------------------------------------------------------------ steps

func (s *Store) RecordStep(ctx context.Context, st *Step) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.begin()
	if err := tx.add(kindStep, st); err != nil {
		return err
	}
	cp := *st
	tx.applyAfter = append(tx.applyAfter, func() { s.putStep(&cp) })
	return tx.commit()
}

func (s *Store) Steps(ctx context.Context, depID string) ([]*Step, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.steps[depID]
	out := make([]*Step, len(list))
	for i, st := range list {
		cp := *st
		out[i] = &cp
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// ----------------------------------------------------------------- leases

// AcquireLease takes the lease for a resource, or fails if held and unexpired.
//
// Section 6.1, with one addition the sketch leaves implicit: the fence token
// is monotonic across *releases* as well as expiries, so a zombie holding an
// old token is fenced out even if the resource was idle in between.
func (s *Store) AcquireLease(ctx context.Context, resource, holder, node string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	var fence int64
	if cur, ok := s.leases[resource]; ok {
		if cur.Holder != "" && !cur.Expired(now) {
			if cur.Holder == holder {
				// Re-acquiring our own live lease is a no-op, which makes
				// executor restart-in-place safe.
				return cur.FenceToken, nil
			}
			return 0, &LeaseHeldError{Resource: resource, Holder: cur.Holder, Until: cur.ExpiresAt}
		}
		fence = cur.FenceToken
	}
	fence++

	l := &Lease{
		Resource:   resource,
		Holder:     holder,
		OwnerNode:  node,
		AcquiredAt: now,
		ExpiresAt:  now.Add(s.leaseTTL),
		FenceToken: fence,
	}
	tx := s.begin()
	if err := tx.add(kindLease, l); err != nil {
		return 0, err
	}
	cp := *l
	tx.applyAfter = append(tx.applyAfter, func() { s.leases[resource] = &cp })
	if err := tx.commit(); err != nil {
		return 0, err
	}
	return fence, nil
}

// RenewLease extends the lease, failing if it was taken by someone else.
//
// A renewal failure is the executor's signal to abort: section 6.1, "If
// renewal fails the executor must abort rather than keep operating on a
// resource it no longer owns."
func (s *Store) RenewLease(ctx context.Context, resource, holder string, fence int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.leases[resource]
	if !ok || cur.Holder != holder || cur.FenceToken != fence {
		return fmt.Errorf("%w: renewing %s", ErrStaleFence, resource)
	}
	now := s.clock.Now()
	if cur.Expired(now) {
		// Expired but not yet taken: refuse to resurrect it. Someone else may
		// already be part-way through acquiring, and a lease that can be
		// un-expired is not a lease.
		return fmt.Errorf("%w: lease on %s expired at %s", ErrStaleFence, resource, cur.ExpiresAt.Format(time.RFC3339))
	}
	next := *cur
	next.ExpiresAt = now.Add(s.leaseTTL)

	tx := s.begin()
	if err := tx.add(kindLease, &next); err != nil {
		return err
	}
	tx.applyAfter = append(tx.applyAfter, func() { s.leases[resource] = &next })
	return tx.commit()
}

// ReleaseLease drops a lease the caller holds.
func (s *Store) ReleaseLease(ctx context.Context, resource, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.leases[resource]
	if !ok || cur.Holder != holder {
		return nil // already released, or taken over; either way we are done
	}
	released := *cur
	released.ExpiresAt = s.clock.Now()

	tx := s.begin()
	if err := tx.add(kindLeaseRelease, &released); err != nil {
		return err
	}
	tx.applyAfter = append(tx.applyAfter, func() {
		if l, ok := s.leases[resource]; ok && l.Holder == holder {
			l.ExpiresAt = released.ExpiresAt
			l.Holder = ""
		}
	})
	return tx.commit()
}

// BreakLease forcibly clears a lease. Used by `orch doctor --break-lease` and
// always audited, because it is the operation that can cause two executors to
// act at once if used carelessly.
func (s *Store) BreakLease(ctx context.Context, resource, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.leases[resource]
	if !ok {
		return fmt.Errorf("%w: lease on %s", ErrNotFound, resource)
	}
	now := s.clock.Now()
	broken := *cur
	broken.ExpiresAt = now
	prevHolder := cur.Holder

	tx := s.begin()
	if err := tx.add(kindLeaseRelease, &broken); err != nil {
		return err
	}
	if err := s.appendAuditTx(tx, AuditEntry{
		At: now, Actor: actor, Action: "lease.broken", Resource: resource,
		Detail: map[string]string{"previous_holder": prevHolder},
	}); err != nil {
		return err
	}
	tx.applyAfter = append(tx.applyAfter, func() {
		if l, ok := s.leases[resource]; ok {
			l.ExpiresAt = now
			l.Holder = ""
		}
	})
	return tx.commit()
}

// LeaseHeld reports whether a live lease exists for a resource.
func (s *Store) LeaseHeld(ctx context.Context, resource string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, ok := s.leases[resource]
	if !ok {
		return false, nil
	}
	return cur.Holder != "" && !cur.Expired(s.clock.Now()), nil
}

func (s *Store) Lease(ctx context.Context, resource string) (*Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, ok := s.leases[resource]
	if !ok {
		return nil, fmt.Errorf("%w: lease on %s", ErrNotFound, resource)
	}
	cp := *cur
	return &cp, nil
}

func (s *Store) checkFenceLocked(resource, holder string, fence int64) error {
	cur, ok := s.leases[resource]
	if !ok {
		return fmt.Errorf("%w: no lease on %s", ErrStaleFence, resource)
	}
	if cur.FenceToken > fence {
		return &staleFenceError{Resource: resource, Fence: fence, Current: cur.FenceToken}
	}
	if cur.Holder != holder {
		return fmt.Errorf("%w: %s is held by %q, not %q", ErrStaleFence, resource, cur.Holder, holder)
	}
	if cur.Expired(s.clock.Now()) {
		return fmt.Errorf("%w: lease on %s expired at %s", ErrStaleFence,
			resource, cur.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// ------------------------------------------------------------------ audit

func (s *Store) appendAuditTx(tx *Tx, e AuditEntry) error {
	prev := s.auditHead
	for _, r := range tx.recs {
		// Entries earlier in this same transaction have already extended the
		// chain in memory-but-not-yet-committed terms, so chain onto them.
		if r.Kind != kindAudit {
			continue
		}
		var earlier AuditEntry
		if err := json.Unmarshal(r.Data, &earlier); err != nil {
			return err
		}
		prev = earlier.Hash
	}

	e.ID = int64(len(s.audit)) + 1
	for _, r := range tx.recs {
		if r.Kind == kindAudit {
			e.ID++
		}
	}
	e.PrevHash = prev
	e.Hash = auditHash(prev, e)
	if err := tx.add(kindAudit, e); err != nil {
		return err
	}
	cp := e
	tx.applyAfter = append(tx.applyAfter, func() {
		s.audit = append(s.audit, &cp)
		s.auditHead = cp.Hash
	})
	return nil
}

// auditHash is section 12.4's payload, extended to cover the detail map.
//
// The document hashes prev|at|actor|action|resource and leaves `detail` out.
// Leaving it out means an attacker can rewrite the *contents* of an entry --
// which version was deployed, which environment -- without breaking the
// chain, and the detail map is exactly where the interesting facts live. So
// it is hashed too, with keys sorted for determinism.
func auditHash(prev string, e AuditEntry) string {
	var b strings.Builder
	b.WriteString(prev)
	b.WriteByte('|')
	b.WriteString(e.At.UTC().Format(time.RFC3339Nano))
	b.WriteByte('|')
	b.WriteString(e.Actor)
	b.WriteByte('|')
	b.WriteString(e.Action)
	b.WriteByte('|')
	b.WriteString(e.Resource)
	keys := make([]string, 0, len(e.Detail))
	for k := range e.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(e.Detail[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// AppendAudit writes a standalone audit entry.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = s.clock.Now()
	}
	tx := s.begin()
	if err := s.appendAuditTx(tx, e); err != nil {
		return err
	}
	return tx.commit()
}

// AuditEntries returns the chain, oldest first.
func (s *Store) AuditEntries(ctx context.Context, limit int) ([]*AuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	start := 0
	if limit > 0 && len(s.audit) > limit {
		start = len(s.audit) - limit
	}
	out := make([]*AuditEntry, 0, len(s.audit)-start)
	for _, e := range s.audit[start:] {
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}

// AuditHead is the current chain head, for external anchoring (section 12.4:
// "Periodically write the head hash somewhere the orchestrator can't modify").
func (s *Store) AuditHead() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auditHead
}

// VerifyAudit walks the chain and reports the first broken link.
//
// This is `orch audit verify`. It recomputes every hash rather than checking
// that prev_hash matches the previous row's hash: the weaker check passes for
// an attacker who edits a row and recomputes the chain from there, and the
// stronger one at least forces them to rewrite everything after it -- which
// the externally anchored head then catches.
func (s *Store) VerifyAudit(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	prev := GenesisHash
	for i, e := range s.audit {
		if e.PrevHash != prev {
			return fmt.Errorf("audit chain broken at entry %d (%s %s): prev_hash is %s, expected %s",
				e.ID, e.Action, e.Resource, short(e.PrevHash), short(prev))
		}
		want := auditHash(prev, *e)
		if want != e.Hash {
			return fmt.Errorf("audit entry %d (%s %s) has been modified: hash is %s, recomputes to %s",
				e.ID, e.Action, e.Resource, short(e.Hash), short(want))
		}
		if i > 0 && e.At.Before(s.audit[i-1].At) {
			return fmt.Errorf("audit entry %d is timestamped before entry %d", e.ID, s.audit[i-1].ID)
		}
		prev = e.Hash
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "..."
	}
	return h
}

// ----------------------------------------------------------------- outbox

func (s *Store) enqueueTx(tx *Tx, e *OutboxEntry) error {
	s.outboxSeq++
	e.ID = s.outboxSeq
	if e.CreatedAt.IsZero() {
		e.CreatedAt = s.clock.Now()
	}
	if e.NextAttempt.IsZero() {
		e.NextAttempt = e.CreatedAt
	}
	if err := tx.add(kindOutbox, e); err != nil {
		return err
	}
	cp := *e
	tx.applyAfter = append(tx.applyAfter, func() { s.outbox = append(s.outbox, &cp) })
	return nil
}

// Enqueue adds a notification outside a state transition. Prefer
// TransitionStateAndNotify, which puts it in the same transaction as the
// change it describes.
func (s *Store) Enqueue(ctx context.Context, e *OutboxEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.begin()
	if err := s.enqueueTx(tx, e); err != nil {
		return err
	}
	return tx.commit()
}

// DueOutbox returns undelivered entries whose backoff has elapsed.
func (s *Store) DueOutbox(ctx context.Context, limit int) ([]*OutboxEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.clock.Now()
	var out []*OutboxEntry
	for _, e := range s.outbox {
		if e.Delivered() || e.NextAttempt.After(now) {
			continue
		}
		cp := *e
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// MarkDelivered records a successful send.
func (s *Store) MarkDelivered(ctx context.Context, id int64) error {
	return s.updateOutbox(ctx, id, func(e *OutboxEntry) {
		now := s.clock.Now()
		e.DeliveredAt = &now
		e.LastError = ""
	})
}

// MarkFailed records a failed send and schedules the retry.
//
// Exponential with a cap: a Slack outage should not schedule the next attempt
// for next Tuesday, and it should not hammer either.
func (s *Store) MarkFailed(ctx context.Context, id int64, cause error) error {
	return s.updateOutbox(ctx, id, func(e *OutboxEntry) {
		e.Attempts++
		e.LastError = cause.Error()
		e.NextAttempt = s.clock.Now().Add(backoff(e.Attempts))
	})
}

func backoff(attempts int) time.Duration {
	d := time.Duration(1<<min(attempts, 8)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Store) updateOutbox(ctx context.Context, id int64, fn func(*OutboxEntry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.outbox {
		if e.ID != id {
			continue
		}
		next := *e
		fn(&next)
		tx := s.begin()
		if err := tx.add(kindOutboxState, &next); err != nil {
			return err
		}
		target := e
		tx.applyAfter = append(tx.applyAfter, func() { *target = next })
		return tx.commit()
	}
	return fmt.Errorf("%w: outbox entry %d", ErrNotFound, id)
}

// PendingOutbox is the `orch_outbox_pending` gauge (section 19.1). A growing
// value means Slack delivery is broken and people are not being told about
// deploys.
func (s *Store) PendingOutbox(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.outbox {
		if !e.Delivered() {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------- samples, approvals

func (s *Store) RecordSample(ctx context.Context, h *HealthSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.begin()
	if err := tx.add(kindHealthSample, h); err != nil {
		return err
	}
	cp := *h
	tx.applyAfter = append(tx.applyAfter, func() {
		s.samples[h.DeploymentID] = append(s.samples[h.DeploymentID], &cp)
	})
	return tx.commit()
}

func (s *Store) Samples(ctx context.Context, depID string) ([]*HealthSample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.samples[depID]
	out := make([]*HealthSample, len(list))
	for i, h := range list {
		cp := *h
		out[i] = &cp
	}
	return out, nil
}

func (s *Store) RecordApproval(ctx context.Context, a *Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.At.IsZero() {
		a.At = s.clock.Now()
	}
	for _, existing := range s.approvals[a.DeploymentID] {
		if existing.User == a.User {
			// Clicking Approve twice is not two approvals. Without this, a
			// require_peer check counting approvals could be satisfied by one
			// person clicking twice.
			return nil
		}
	}
	tx := s.begin()
	if err := tx.add(kindApproval, a); err != nil {
		return err
	}
	if err := s.appendAuditTx(tx, AuditEntry{
		At: a.At, Actor: a.User, Action: "approval.granted",
		Resource: s.deployments[a.DeploymentID].Resource(),
		Detail:   map[string]string{"deployment": a.DeploymentID, "source": a.Source},
	}); err != nil {
		return err
	}
	cp := *a
	tx.applyAfter = append(tx.applyAfter, func() {
		s.approvals[a.DeploymentID] = append(s.approvals[a.DeploymentID], &cp)
	})
	return tx.commit()
}

func (s *Store) Approvals(ctx context.Context, depID string) ([]*Approval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.approvals[depID]
	out := make([]*Approval, len(list))
	for i, a := range list {
		cp := *a
		out[i] = &cp
	}
	return out, nil
}

// --------------------------------------------------------------- freezes

func (s *Store) SetFreeze(ctx context.Context, f *Freeze) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.At.IsZero() {
		f.At = s.clock.Now()
	}
	action := "deploy.frozen"
	if f.Lifted {
		action = "deploy.unfrozen"
	}
	tx := s.begin()
	if err := tx.add(kindFreeze, f); err != nil {
		return err
	}
	actor := f.By
	if f.Lifted && f.LiftedBy != "" {
		actor = f.LiftedBy
	}
	if err := s.appendAuditTx(tx, AuditEntry{
		At: f.At, Actor: actor, Action: action, Resource: f.Resource,
		Detail: map[string]string{"reason": f.Reason},
	}); err != nil {
		return err
	}
	cp := *f
	tx.applyAfter = append(tx.applyAfter, func() { s.freezes[f.Resource] = &cp })
	return tx.commit()
}

func (s *Store) Freeze(ctx context.Context, resource string) (*Freeze, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.freezes[resource]
	if !ok || !f.Active(s.clock.Now()) {
		return nil, false
	}
	cp := *f
	return &cp, true
}

func (s *Store) Freezes(ctx context.Context) []*Freeze {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.clock.Now()
	var out []*Freeze
	for _, f := range s.freezes {
		if f.Active(now) {
			cp := *f
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// --------------------------------------------------------------- helpers

func clone(d *Deployment) *Deployment {
	if d == nil {
		return nil
	}
	cp := *d
	if d.FinishedAt != nil {
		t := *d.FinishedAt
		cp.FinishedAt = &t
	}
	return &cp
}

// Syncs reports how many times the journal has been fsynced, so a test can
// assert that a commit was durable rather than buffered.
func (s *Store) Syncs() int {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	return s.j.syncs
}
