package store

import (
	"fmt"
	"time"
)

// State is a deployment state (design.md section 5).
type State string

const (
	StatePending        State = "PENDING"
	StatePreflight      State = "PREFLIGHT"
	StateAwaitApproval  State = "AWAIT_APPROVAL"
	StateDeploying      State = "DEPLOYING"
	StateVerifying      State = "VERIFYING"
	StatePromoting      State = "PROMOTING"
	StateSucceeded      State = "SUCCEEDED"
	StateRollingBack    State = "ROLLING_BACK"
	StateRolledBack     State = "ROLLED_BACK"
	StateRollbackFailed State = "ROLLBACK_FAILED"
	StateFailed         State = "FAILED"
	StateAborted        State = "ABORTED"
	// StateUnknown is reached by reconciliation when the provider reports a
	// version that matches neither what we deployed nor what was there
	// before. Section 17.2: "When reality doesn't match either expected
	// state, the correct action is to stop and tell a human."
	StateUnknown State = "UNKNOWN"
)

// IsTerminal reports the absorbing states. Nothing transitions out of them.
func (s State) IsTerminal() bool {
	switch s {
	case StateSucceeded, StateRolledBack, StateRollbackFailed, StateAborted,
		StateFailed, StateUnknown:
		return true
	}
	return false
}

// Pages reports whether reaching this state should wake someone up.
//
// Section 5: "ROLLBACK_FAILED is the only state that pages. Everything else
// is recoverable by the system; this one means production is in an unknown
// state and a human must look." UNKNOWN is here for the same reason -- it is
// reconciliation's version of the same finding.
func (s State) Pages() bool {
	return s == StateRollbackFailed || s == StateUnknown
}

// RequiresLease reports whether work happens in this state, and therefore
// whether the executor must be holding the lease to be in it.
func (s State) RequiresLease() bool {
	switch s {
	case StatePreflight, StateAwaitApproval, StateDeploying, StateVerifying,
		StatePromoting, StateRollingBack:
		return true
	}
	return false
}

// Active reports a non-terminal state, which is what reconciliation looks for.
func (s State) Active() bool { return !s.IsTerminal() }

// allowedTransitions is the state machine from section 5, as data.
//
// Written out rather than implied by control flow so that the property test
// can enumerate it, and so that adding a state is a visible diff rather than
// a new branch somewhere in the executor.
var allowedTransitions = map[State][]State{
	StatePending:       {StatePreflight, StateAborted, StateFailed},
	StatePreflight:     {StateAwaitApproval, StateDeploying, StateFailed, StateAborted, StateRollbackFailed},
	StateAwaitApproval: {StateDeploying, StateFailed, StateAborted},

	// ROLLBACK_FAILED is reachable from every state in which work happens,
	// not only from ROLLING_BACK. The document's diagram draws the common
	// case -- a rollback that fails its own verification -- but a rollback
	// deployment can also fail while DEPLOYING, and it must reach the state
	// that pages rather than the state that says "we will try something
	// else". Only a deployment with IsRollback set takes these edges;
	// `handleFailure` checks that before any policy lookup.
	StateDeploying:   {StateVerifying, StateRollingBack, StateFailed, StateAborted, StateUnknown, StateRollbackFailed},
	StateVerifying:   {StatePromoting, StateRollingBack, StateFailed, StateAborted, StateUnknown, StateRollbackFailed},
	StatePromoting:   {StateSucceeded, StateRollingBack, StateFailed, StateAborted, StateUnknown, StateRollbackFailed},
	StateRollingBack: {StateRolledBack, StateRollbackFailed, StateUnknown},

	// Terminal states have no outgoing transitions. Present as empty slices
	// rather than absent, so "no entry" and "no transitions" are the same
	// thing and CanTransition needs no special case.
	StateSucceeded:      {},
	StateRolledBack:     {},
	StateRollbackFailed: {},
	StateFailed:         {},
	StateAborted:        {},
	StateUnknown:        {},
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to State) bool {
	if from == to {
		return false
	}
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// AllStates is every state, for exhaustive tests.
func AllStates() []State {
	return []State{
		StatePending, StatePreflight, StateAwaitApproval, StateDeploying,
		StateVerifying, StatePromoting, StateSucceeded, StateRollingBack,
		StateRolledBack, StateRollbackFailed, StateFailed, StateAborted,
		StateUnknown,
	}
}

// TriggerSource records how a deployment was started, for the audit trail.
type TriggerSource string

const (
	TriggerCLI       TriggerSource = "cli"
	TriggerSlack     TriggerSource = "slack"
	TriggerAPI       TriggerSource = "api"
	TriggerSchedule  TriggerSource = "schedule"
	TriggerReconcile TriggerSource = "reconcile"
)

// Deployment is one row of the `deployments` table in section 5's schema.
type Deployment struct {
	ID          string `json:"id"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Version     string `json:"version"`
	// PreviousVersion is captured at PREFLIGHT, from the provider rather than
	// from our own history: what is actually running is the only thing a
	// rollback can return to.
	PreviousVersion string        `json:"previous_version,omitempty"`
	Strategy        string        `json:"strategy"`
	State           State         `json:"state"`
	IdempotencyKey  string        `json:"idempotency_key,omitempty"`
	TriggeredBy     string        `json:"triggered_by"`
	TriggerSource   TriggerSource `json:"trigger_source"`
	Provider        string        `json:"provider"`
	// ProviderHandle is opaque to us and meaningful to the provider. It is
	// what reconciliation uses to ask "what happened to the thing I started".
	ProviderHandle string     `json:"provider_handle,omitempty"`
	IsRollback     bool       `json:"is_rollback"`
	RollbackOf     string     `json:"rollback_of,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	// SlackTS is the timestamp of the message posted for this deployment, so
	// updates go to chat.update rather than posting again (section 6.4).
	SlackTS string `json:"slack_ts,omitempty"`
	// Fence is the lease fence token held while this deployment runs. Not
	// persisted as authority -- the lease row is -- but carried so every
	// mutating call can present it.
	Fence int64 `json:"fence,omitempty"`
}

// Resource is the lease key and audit resource for this deployment's target.
func (d *Deployment) Resource() string {
	return "app:" + d.App + "/env:" + d.Environment
}

func (d *Deployment) Duration(now time.Time) time.Duration {
	if d.FinishedAt != nil {
		return d.FinishedAt.Sub(d.CreatedAt)
	}
	return now.Sub(d.CreatedAt)
}

// Step is one row of `deployment_steps`.
type Step struct {
	DeploymentID string     `json:"deployment_id"`
	Seq          int        `json:"seq"`
	Name         string     `json:"name"`
	State        string     `json:"state"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Output       string     `json:"output,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// Lease is a lock with an expiry (section 6.1).
type Lease struct {
	Resource   string    `json:"resource"`
	Holder     string    `json:"holder"`
	OwnerNode  string    `json:"owner_node"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	FenceToken int64     `json:"fence_token"`
}

func (l *Lease) Expired(now time.Time) bool { return !l.ExpiresAt.After(now) }

// HealthSample is one observation (section 7.2).
type HealthSample struct {
	DeploymentID string    `json:"deployment_id"`
	Verifier     string    `json:"verifier"`
	Criterion    string    `json:"criterion"`
	At           time.Time `json:"at"`
	Value        float64   `json:"value"`
	Baseline     float64   `json:"baseline"`
	Healthy      bool      `json:"healthy"`
	// Note carries why a sample was judged the way it was, which is the
	// difference between a chart nobody can interpret and one that explains
	// a rollback.
	Note string `json:"note,omitempty"`
}

// AuditEntry is one link of the hash chain (section 12.4).
type AuditEntry struct {
	ID       int64             `json:"id"`
	At       time.Time         `json:"at"`
	Actor    string            `json:"actor"`
	Action   string            `json:"action"`
	Resource string            `json:"resource"`
	Detail   map[string]string `json:"detail,omitempty"`
	PrevHash string            `json:"prev_hash"`
	Hash     string            `json:"hash"`
}

// OutboxEntry is a notification awaiting delivery (section 6.4).
type OutboxEntry struct {
	ID          int64      `json:"id"`
	CreatedAt   time.Time  `json:"created_at"`
	Notifier    string     `json:"notifier"`
	Payload     string     `json:"payload"`
	Attempts    int        `json:"attempts"`
	NextAttempt time.Time  `json:"next_attempt"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	// DedupeKey lets a consumer be idempotent: at-least-once delivery plus an
	// idempotent consumer is the achievable goal (section 6.4).
	DedupeKey string `json:"dedupe_key,omitempty"`
}

func (o *OutboxEntry) Delivered() bool { return o.DeliveredAt != nil }

// Approval records that a human said yes (section 10.3).
type Approval struct {
	DeploymentID string    `json:"deployment_id"`
	User         string    `json:"user"`
	At           time.Time `json:"at"`
	Source       string    `json:"source"`
}

// Freeze blocks deploys for a target (section 8.4's `orch freeze`).
type Freeze struct {
	Resource string     `json:"resource"`
	Reason   string     `json:"reason"`
	By       string     `json:"by"`
	At       time.Time  `json:"at"`
	Until    *time.Time `json:"until,omitempty"`
	Lifted   bool       `json:"lifted"`
	LiftedBy string     `json:"lifted_by,omitempty"`
}

func (f *Freeze) Active(now time.Time) bool {
	if f.Lifted {
		return false
	}
	if f.Until != nil && now.After(*f.Until) {
		return false
	}
	return true
}

// ------------------------------------------------------------------ errors

// LeaseHeldError is returned when another deployment owns the resource.
//
// It names the holder and the expiry, because "resource is locked" sends
// someone looking and "held by deployment X until 12:04:31" does not.
type LeaseHeldError struct {
	Resource string
	Holder   string
	Until    time.Time
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("%s is being deployed by %s until %s",
		e.Resource, e.Holder, e.Until.Format(time.RFC3339))
}

// ErrStaleFence means the caller lost the lease, or the state moved under it.
type staleFenceError struct {
	Resource string
	Fence    int64
	Current  int64
}

// Is makes errors.Is(err, ErrStaleFence) work for the structured form, so
// callers can test the class without knowing the concrete type.
func (e *staleFenceError) Is(target error) bool { return target == ErrStaleFence }

func (e *staleFenceError) Error() string {
	return fmt.Sprintf("stale fence token %d for %s (current %d): the lease was lost",
		e.Fence, e.Resource, e.Current)
}

// UnexpectedStateError means a transition was attempted from a state the
// deployment is no longer in -- usually because something else moved it.
type UnexpectedStateError struct {
	DeploymentID string
	Want, Got    State
}

func (e *UnexpectedStateError) Error() string {
	return fmt.Sprintf("deployment %s is %s, not %s", e.DeploymentID, e.Got, e.Want)
}

// IllegalTransitionError is a transition the state machine does not allow.
type IllegalTransitionError struct {
	From, To State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal transition %s -> %s", e.From, e.To)
}
