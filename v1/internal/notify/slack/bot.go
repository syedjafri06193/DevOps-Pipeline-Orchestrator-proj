package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/store"
)

// Transport is the Slack API, narrowed to what this package uses.
//
// An interface because Socket Mode needs a live WebSocket, which this build
// cannot dial (no network, no slack-go). Everything that *decides* anything
// sits on this side of it and is tested against a fake, which is what section
// 18.5 asks for: "Mock the Slack API at the HTTP layer."
type Transport interface {
	// PostMessage returns the message ts, which is stored so later updates go
	// to chat.update rather than posting again.
	PostMessage(ctx context.Context, m Message) (ts string, err error)
	UpdateMessage(ctx context.Context, m Message) error
	// PostEphemeral replies to one person only. Every refusal uses this: a
	// public "you are not allowed to do that" is a poor way to tell someone.
	PostEphemeral(ctx context.Context, channel, user, text string) error
}

// RateLimitedError is what a transport returns on a 429.
//
// Section 10.4: "chat.postMessage is tightly limited per channel (roughly one
// message per second, with small bursts)." The retry-after is carried so the
// outbox worker can back off by the amount Slack asked for rather than
// guessing.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("slack: rate limited, retry after %s", e.RetryAfter)
}

// InteractionCallback is the payload Slack posts when a button is clicked.
type InteractionCallback struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		// Name is display text the user controls. It is never used for
		// authorization -- see Handler.Approve.
		Name string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS string `json:"ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// Handler applies section 10.3's rules to an interaction.
type Handler struct {
	Store     *store.Store
	Config    *config.Config
	Executor  *engine.Executor
	Transport Transport
	Log       *slog.Logger
	Now       func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

// ErrNotLinked means the Slack account maps to no internal user.
var ErrNotLinked = errors.New("slack: this Slack account is not linked to an orchestrator user")

// Handle dispatches one interaction.
//
// The caller has already acknowledged to Slack -- section 2.3's three-second
// deadline means the ack happens before any of this runs, and this function
// is what the worker does afterwards.
func (h *Handler) Handle(ctx context.Context, cb InteractionCallback) error {
	if len(cb.Actions) == 0 {
		return nil
	}
	action := cb.Actions[0]
	switch action.ActionID {
	case ActionApprove:
		return h.decide(ctx, cb, action.Value, true)
	case ActionReject:
		return h.decide(ctx, cb, action.Value, false)
	case ActionAbort:
		return h.abort(ctx, cb, action.Value)
	}
	return nil
}

// decide applies every check in section 10.3 before recording an approval.
func (h *Handler) decide(ctx context.Context, cb InteractionCallback, depID string, approve bool) error {
	// 1. MAP SLACK IDENTITY -> INTERNAL IDENTITY. Never trust the display
	// name: it is text the user controls, and two people can set the same one.
	user, ok := h.Config.IdentityBySlackID(cb.User.ID)
	if !ok {
		h.ephemeral(ctx, cb, "Your Slack account isn't linked to an orchestrator user, so I can't act on this.")
		h.audit(ctx, cb.User.ID, "approval.denied", "app:?/env:?", map[string]string{
			"deployment": depID,
			"reason":     "unlinked slack account",
			"slack_id":   cb.User.ID,
		})
		return ErrNotLinked
	}

	dep, err := h.Store.Deployment(ctx, depID)
	if err != nil {
		h.ephemeral(ctx, cb, "I can't find that deployment any more.")
		return err
	}

	cfgEnv, err := h.Config.AppEnv(dep.App, dep.Environment)
	if err != nil {
		h.ephemeral(ctx, cb, "That deployment's configuration has changed; I can't evaluate the gate.")
		return err
	}

	gate, ok := approvalGate(cfgEnv)
	if !ok {
		h.ephemeral(ctx, cb, "That environment has no approval gate.")
		return errors.New("slack: no approval gate on " + dep.Environment)
	}

	if !approve {
		h.audit(ctx, user.User, "approval.rejected", dep.Resource(),
			map[string]string{"deployment": depID})
		if err := h.Executor.Abort(ctx, depID, user.User); err != nil {
			h.ephemeral(ctx, cb, "I couldn't abort that deployment: "+err.Error())
			return err
		}
		return h.replaceButtons(ctx, cb, dep, user.User, false)
	}

	// 2-4. Permission, self-approval, and expiry. One implementation, shared
	// with the gate itself -- if the button and the gate disagreed about who
	// may approve, the button would be the weaker of the two.
	if err := h.Executor.ApprovalIsValid(dep, cfgEnv, gate, user.User, h.now()); err != nil {
		h.ephemeral(ctx, cb, "I can't accept that approval: "+err.Error())
		h.audit(ctx, user.User, "approval.denied", dep.Resource(), map[string]string{
			"deployment": depID,
			"reason":     err.Error(),
		})
		return err
	}

	if err := h.Store.RecordApproval(ctx, &store.Approval{
		DeploymentID: depID, User: user.User, At: h.now(), Source: "slack",
	}); err != nil {
		return err
	}
	return h.replaceButtons(ctx, cb, dep, user.User, true)
}

func (h *Handler) abort(ctx context.Context, cb InteractionCallback, depID string) error {
	user, ok := h.Config.IdentityBySlackID(cb.User.ID)
	if !ok {
		h.ephemeral(ctx, cb, "Your Slack account isn't linked to an orchestrator user.")
		return ErrNotLinked
	}
	dep, err := h.Store.Deployment(ctx, depID)
	if err != nil {
		return err
	}
	// Aborting is its own permission. Someone who can look at a deploy is not
	// thereby someone who can stop one.
	if !h.Config.Can(user, "deploy:abort", dep.Environment) &&
		!h.Config.Can(user, "deploy:rollback", dep.Environment) {
		h.ephemeral(ctx, cb, "You don't have permission to abort deploys in "+dep.Environment+".")
		h.audit(ctx, user.User, "abort.denied", dep.Resource(),
			map[string]string{"deployment": depID})
		return fmt.Errorf("slack: %s cannot abort in %s", user.User, dep.Environment)
	}
	return h.Executor.Abort(ctx, depID, user.User)
}

func (h *Handler) replaceButtons(ctx context.Context, cb InteractionCallback,
	dep *store.Deployment, user string, approved bool) error {

	ev := engine.Event{
		App: dep.App, Environment: dep.Environment, Version: dep.Version,
		Actor: dep.TriggeredBy, State: dep.State, DeploymentID: dep.ID,
	}
	return h.Transport.UpdateMessage(ctx, Message{
		Channel: cb.Channel.ID,
		TS:      cb.Message.TS,
		Text:    fmt.Sprintf("%s %s → %s", stateIcon(dep.State), dep.App, dep.Environment),
		Blocks:  ApprovedBlocks(ev, user, approved),
	})
}

func (h *Handler) ephemeral(ctx context.Context, cb InteractionCallback, text string) {
	if err := h.Transport.PostEphemeral(ctx, cb.Channel.ID, cb.User.ID, text); err != nil && h.Log != nil {
		h.Log.Error("posting ephemeral reply", "error", err)
	}
}

func (h *Handler) audit(ctx context.Context, actor, action, resource string, detail map[string]string) {
	if err := h.Store.AppendAudit(ctx, store.AuditEntry{
		At: h.now(), Actor: actor, Action: action, Resource: resource, Detail: detail,
	}); err != nil && h.Log != nil {
		h.Log.Error("appending audit entry", "error", err)
	}
}

func approvalGate(cfg *config.AppEnv) (config.Gate, bool) {
	for _, g := range cfg.Gates {
		if g.Type == config.GateApproval {
			return g, true
		}
	}
	return config.Gate{}, false
}

// ---------------------------------------------------------- the notifier

// Notifier drains outbox entries to Slack.
//
// Two behaviours from section 10.4 live here:
//
//   - one message per deployment, updated in place, with the ts stored on the
//     deployment row;
//   - update coalescing, because a canary sampling every fifteen seconds and
//     updating on every sample gets throttled.
type Notifier struct {
	Store     *store.Store
	Config    *config.Config
	Transport Transport
	Log       *slog.Logger
	// DashboardURL is used for the "View dashboard" button.
	DashboardURL string
	// MinUpdateInterval coalesces updates. Slack allows roughly one message
	// per second per channel; this is deliberately slower than that, because
	// a progress bar that moves every five seconds is no less useful than one
	// that moves every second and is much less likely to be throttled.
	MinUpdateInterval time.Duration
	Now               func() time.Time

	mu         sync.Mutex
	lastUpdate map[string]time.Time
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now().UTC()
}

func (n *Notifier) Name() string { return "slack" }

// Notify delivers one outbox entry.
//
// Returning an error leaves the entry in the outbox for the worker to retry,
// which is how at-least-once delivery is achieved. The dedupe key and the
// stored message ts are what make the consumer idempotent, so at-least-once
// does not become several-times-visible.
func (n *Notifier) Notify(ctx context.Context, payload string) error {
	var ev engine.Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		// A malformed payload will never become well-formed. Returning an
		// error here would retry it forever and block the queue behind it.
		if n.Log != nil {
			n.Log.Error("discarding unparseable notification", "error", err)
		}
		return nil
	}

	cfgEnv, err := n.Config.AppEnv(ev.App, ev.Environment)
	if err != nil {
		return nil // the config changed; nothing to notify about
	}
	if cfgEnv.Notify.Slack == nil {
		return nil
	}
	sn := cfgEnv.Notify.Slack
	if !subscribed(sn.On, ev.Type) {
		return nil
	}

	dep, err := n.Store.Deployment(ctx, ev.DeploymentID)
	if err != nil {
		return err
	}

	// A terminal failure gets its own message, so it breaks through.
	if ev.State == store.StateFailed || ev.State == store.StateRollbackFailed ||
		ev.State == store.StateUnknown || ev.State == store.StateRolledBack {
		_, err := n.Transport.PostMessage(ctx, FailureMessage(sn.Channel, ev, sn.MentionOnFailure))
		return err
	}

	samples, _ := n.Store.Samples(ctx, ev.DeploymentID)
	msg := DeployMessage(sn.Channel, ev, samples, n.DashboardURL)

	if dep.SlackTS == "" {
		ts, err := n.Transport.PostMessage(ctx, msg)
		if err != nil {
			return err
		}
		dep.SlackTS = ts
		return n.Store.UpdateDeployment(ctx, dep)
	}

	if n.throttled(ev.DeploymentID, ev.State) {
		// Deliberately not an error: the update was skipped on purpose, and
		// retrying it would defeat the coalescing.
		return nil
	}
	msg.TS = dep.SlackTS
	return n.Transport.UpdateMessage(ctx, msg)
}

// throttled reports whether this update should be skipped.
//
// A state change always goes through -- those are the updates people are
// actually waiting for. Only same-state refreshes (a new health sample during
// a bake) are coalesced.
func (n *Notifier) throttled(depID string, state store.State) bool {
	if n.MinUpdateInterval <= 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastUpdate == nil {
		n.lastUpdate = map[string]time.Time{}
	}
	key := depID + ":" + string(state)
	now := n.now()
	if last, ok := n.lastUpdate[key]; ok && now.Sub(last) < n.MinUpdateInterval {
		return true
	}
	n.lastUpdate[key] = now
	return false
}

func subscribed(events []string, t string) bool {
	if len(events) == 0 {
		return true
	}
	for _, e := range events {
		if e == t {
			return true
		}
	}
	return false
}
