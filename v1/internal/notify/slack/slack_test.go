package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/provider"
	fakeprovider "github.com/syedjafri06193/orch/internal/provider/fake"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
	"github.com/syedjafri06193/orch/internal/verifier"
	fakeverifier "github.com/syedjafri06193/orch/internal/verifier/fake"
)

// Section 18.5 lists exactly what to test. Each item is a test here:
//
//	Valid signature accepted
//	Invalid signature rejected
//	Timestamp older than 5 minutes rejected (replay)
//	Body modified after signing rejected
//	Unknown Slack user rejected
//	Known user without the role rejected and audited
//	Self-approval rejected when require_peer is set
//	Expired approval rejected
//	Rate-limit response handled with backoff, not dropped

const secret = "8f742231b10e8888abcd99yyyzzz85a5"

func ts(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// ------------------------------------------------------------- signatures

func TestAValidSignatureIsAccepted(t *testing.T) {
	now := time.Now()
	body := []byte(`{"type":"block_actions"}`)
	sig := Sign(secret, ts(now), body)
	if err := VerifySignature(secret, sig, ts(now), body, now); err != nil {
		t.Fatalf("a correctly signed request must be accepted: %v", err)
	}
}

func TestAnInvalidSignatureIsRejected(t *testing.T) {
	now := time.Now()
	body := []byte(`{"type":"block_actions"}`)
	if err := VerifySignature(secret, "v0=deadbeef", ts(now), body, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestASignatureFromADifferentSecretIsRejected(t *testing.T) {
	now := time.Now()
	body := []byte(`{}`)
	sig := Sign("someone-elses-secret", ts(now), body)
	if err := VerifySignature(secret, sig, ts(now), body, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestAStaleRequestIsRejected(t *testing.T) {
	// Replay protection. Without it, a captured "approve" is a production
	// deploy, forever.
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	body := []byte(`{}`)
	sig := Sign(secret, ts(old), body)

	if err := VerifySignature(secret, sig, ts(old), body, now); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("want ErrStaleRequest, got %v", err)
	}
	// And the signature itself is still valid, which is the point: the
	// signature check alone would have accepted this.
	if err := VerifySignature(secret, sig, ts(old), body, old); err != nil {
		t.Fatalf("the signature really is valid: %v", err)
	}
}

func TestARequestFromTheFutureIsRejected(t *testing.T) {
	// Section 19.4: clock skew breaks all of this, and the failure mode is
	// subtle. A request timestamped ten minutes ahead is as suspicious as one
	// ten minutes behind.
	now := time.Now()
	future := now.Add(10 * time.Minute)
	body := []byte(`{}`)
	sig := Sign(secret, ts(future), body)
	if err := VerifySignature(secret, sig, ts(future), body, now); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("want ErrStaleRequest, got %v", err)
	}
}

func TestABodyModifiedAfterSigningIsRejected(t *testing.T) {
	// The reason the raw bytes must be read before any JSON decoding: the
	// signature covers exact bytes.
	now := time.Now()
	original := []byte(`{"actions":[{"value":"dep-safe"}]}`)
	sig := Sign(secret, ts(now), original)
	tampered := []byte(`{"actions":[{"value":"dep-prod"}]}`)

	if err := VerifySignature(secret, sig, ts(now), tampered, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestReEncodedJsonDoesNotVerify(t *testing.T) {
	// A decode-then-re-encode round trip changes whitespace and key order and
	// breaks the signature. This is the mistake the document warns about, and
	// it fails loudly rather than mysteriously.
	now := time.Now()
	original := []byte(`{"b":1, "a":2}`)
	sig := Sign(secret, ts(now), original)

	var v map[string]any
	_ = json.Unmarshal(original, &v)
	reencoded, _ := json.Marshal(v)

	if err := VerifySignature(secret, sig, ts(now), reencoded, now); err == nil {
		t.Fatal("re-encoded JSON must not verify")
	}
}

func TestAnUnsignedRequestIsRejected(t *testing.T) {
	now := time.Now()
	if err := VerifySignature(secret, "", ts(now), []byte(`{}`), now); !errors.Is(err, ErrNoSignature) {
		t.Fatalf("want ErrNoSignature, got %v", err)
	}
}

func TestAMissingSigningSecretIsRefusedNotSkipped(t *testing.T) {
	// The worst possible default: no secret configured, so verification
	// silently passes.
	now := time.Now()
	body := []byte(`{}`)
	if err := VerifySignature("", Sign(secret, ts(now), body), ts(now), body, now); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("want ErrNoSigningKey, got %v", err)
	}
}

// ----------------------------------------------------------- the handler

type fakeTransport struct {
	mu         sync.Mutex
	posted     []Message
	updated    []Message
	ephemerals []string
	postErr    error
	nextTS     int
}

func (f *fakeTransport) PostMessage(_ context.Context, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return "", f.postErr
	}
	f.posted = append(f.posted, m)
	f.nextTS++
	return "ts-" + strconv.Itoa(f.nextTS), nil
}

func (f *fakeTransport) UpdateMessage(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	f.updated = append(f.updated, m)
	return nil
}

func (f *fakeTransport) PostEphemeral(_ context.Context, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ephemerals = append(f.ephemerals, text)
	return nil
}

func (f *fakeTransport) lastEphemeral() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ephemerals) == 0 {
		return ""
	}
	return f.ephemerals[len(f.ephemerals)-1]
}

const testConfig = `
version: 1
apps:
  web:
    environments:
      staging:
        provider: fake
      prod:
        provider: fake
        gates:
          - type: approval
            require_peer: true
            roles: [release-manager]
            ttl: 1h
        notify:
          slack:
            channel: "#deploys"
            on: [started, awaiting_approval, deploying, verifying, promoting, succeeded, failed, rolled_back, rollback_failed]
            mention_on_failure: ["@oncall"]
environments:
  prod:
    require_peer_approval: true
    protected: true
roles:
  developer:
    - deploy:trigger   on: [staging]
    - deploy:view      on: ["*"]
  release-manager:
    - deploy:trigger   on: ["*"]
    - deploy:approve   on: ["*"]
    - deploy:abort     on: ["*"]
identities:
  jordan:
    slack_id: U01JORDAN
    roles: [developer]
  sam:
    slack_id: U02SAM
    roles: [release-manager]
  dana:
    slack_id: U03DANA
    roles: [release-manager]
`

type fixture struct {
	t         *testing.T
	store     *store.Store
	cfg       *config.Config
	transport *fakeTransport
	handler   *Handler
	exec      *engine.Executor
	now       time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Decode(testConfig, "orch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "j"), fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	exec := engine.New(engine.Options{
		Store:     st,
		Config:    cfg,
		Providers: provider.NewRegistry(fakeprovider.New()),
		Verifiers: verifier.NewRegistry(fakeverifier.New(0.0001)),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tr := &fakeTransport{}

	return &fixture{
		t: t, store: st, cfg: cfg, transport: tr, exec: exec, now: now,
		handler: &Handler{
			Store: st, Config: cfg, Executor: exec, Transport: tr,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now: func() time.Time { return now },
		},
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func (f *fixture) deployment(env, triggeredBy string) *store.Deployment {
	f.t.Helper()
	d, _, err := f.store.CreateDeployment(context.Background(), &store.Deployment{
		ID: ulid.NewAt(f.now).String(), App: "web", Environment: env,
		Version: "v2", Strategy: "blue-green", State: store.StatePending,
		TriggeredBy: triggeredBy, TriggerSource: store.TriggerCLI, Provider: "fake",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func callback(slackID, actionID, depID string) InteractionCallback {
	var cb InteractionCallback
	cb.Type = "block_actions"
	cb.User.ID = slackID
	cb.User.Name = "whatever-they-set"
	cb.Channel.ID = "C123"
	cb.Message.TS = "ts-1"
	cb.Actions = append(cb.Actions, struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}{ActionID: actionID, Value: depID})
	return cb
}

func TestAnUnknownSlackUserIsRejected(t *testing.T) {
	// Section 10.3's first rule: "Map Slack identity → internal identity.
	// Never trust the display name."
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")

	err := f.handler.Handle(context.Background(), callback("U-stranger", ActionApprove, dep.ID))
	if !errors.Is(err, ErrNotLinked) {
		t.Fatalf("want ErrNotLinked, got %v", err)
	}
	approvals, _ := f.store.Approvals(context.Background(), dep.ID)
	if len(approvals) != 0 {
		t.Fatal("an unlinked account must not record an approval")
	}
	if !strings.Contains(f.transport.lastEphemeral(), "isn't linked") {
		t.Errorf("the refusal should explain itself: %q", f.transport.lastEphemeral())
	}
}

func TestAKnownUserWithoutTheRoleIsRejectedAndAudited(t *testing.T) {
	// "Check the real permission, in the real RBAC system." jordan is a
	// developer; developers cannot approve prod.
	f := newFixture(t)
	dep := f.deployment("prod", "sam")

	if err := f.handler.Handle(context.Background(), callback("U01JORDAN", ActionApprove, dep.ID)); err == nil {
		t.Fatal("want a refusal")
	}
	approvals, _ := f.store.Approvals(context.Background(), dep.ID)
	if len(approvals) != 0 {
		t.Fatal("an unauthorised approval must not be recorded")
	}

	entries, _ := f.store.AuditEntries(context.Background(), 0)
	var audited bool
	for _, e := range entries {
		if e.Action == "approval.denied" && e.Actor == "jordan" {
			audited = true
			if e.Detail["reason"] == "" {
				t.Error("the audit entry should say why it was denied")
			}
		}
	}
	if !audited {
		t.Error("a denied approval must be audited: it is a signal, not noise")
	}
}

func TestSelfApprovalIsRejectedWhenRequirePeerIsSet(t *testing.T) {
	f := newFixture(t)
	dep := f.deployment("prod", "sam") // sam triggered it
	if err := f.handler.Handle(context.Background(), callback("U02SAM", ActionApprove, dep.ID)); err == nil {
		t.Fatal("sam must not approve sam's own deploy")
	}
	approvals, _ := f.store.Approvals(context.Background(), dep.ID)
	if len(approvals) != 0 {
		t.Fatal("no approval should have been recorded")
	}
	if !strings.Contains(f.transport.lastEphemeral(), "someone other than") {
		t.Errorf("the refusal should explain: %q", f.transport.lastEphemeral())
	}
}

func TestAPeerCanApprove(t *testing.T) {
	f := newFixture(t)
	dep := f.deployment("prod", "sam")
	if err := f.handler.Handle(context.Background(), callback("U03DANA", ActionApprove, dep.ID)); err != nil {
		t.Fatalf("dana is a release-manager and did not trigger it: %v", err)
	}
	approvals, _ := f.store.Approvals(context.Background(), dep.ID)
	if len(approvals) != 1 || approvals[0].User != "dana" {
		t.Fatalf("approvals = %+v", approvals)
	}
}

func TestAnExpiredApprovalIsRejected(t *testing.T) {
	// Section 10.3's fourth check, "the one people forget": a button posted
	// at 2pm should not be actionable at 11pm.
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")

	// The TTL is an hour; click nine hours later.
	f.handler.Now = func() time.Time { return f.now.Add(9 * time.Hour) }

	if err := f.handler.Handle(context.Background(), callback("U02SAM", ActionApprove, dep.ID)); err == nil {
		t.Fatal("a stale button click is not consent")
	}
	if !strings.Contains(f.transport.lastEphemeral(), "expired") {
		t.Errorf("say so: %q", f.transport.lastEphemeral())
	}
}

func TestTheDisplayNameIsNeverUsedForAuthorization(t *testing.T) {
	// A user can set their Slack display name to anything, including
	// "sam". Authorization must key on the account id.
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	cb := callback("U-impostor", ActionApprove, dep.ID)
	cb.User.Name = "sam"
	cb.User.Username = "sam"

	if err := f.handler.Handle(context.Background(), cb); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("want ErrNotLinked, got %v", err)
	}
}

func TestApprovingReplacesTheButtons(t *testing.T) {
	// So the same message cannot be clicked twice, and so the channel shows
	// who decided.
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	if err := f.handler.Handle(context.Background(), callback("U02SAM", ActionApprove, dep.ID)); err != nil {
		t.Fatal(err)
	}
	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(f.transport.updated))
	}
	body, _ := json.Marshal(f.transport.updated[0].Blocks)
	if strings.Contains(string(body), ActionApprove) {
		t.Error("the approve button should be gone after a decision")
	}
	if !strings.Contains(string(body), "sam") {
		t.Error("the message should name who approved")
	}
}

func TestAbortRequiresItsOwnPermission(t *testing.T) {
	// Being able to look at a deploy is not being able to stop one.
	f := newFixture(t)
	dep := f.deployment("staging", "sam")
	err := f.handler.Handle(context.Background(), callback("U01JORDAN", ActionAbort, dep.ID))
	if err == nil {
		t.Fatal("jordan has no deploy:abort")
	}
	if !strings.Contains(f.transport.lastEphemeral(), "permission") {
		t.Errorf("say so: %q", f.transport.lastEphemeral())
	}
}

// ------------------------------------------------------------- notifier

func TestOneMessagePerDeploymentUpdatedInPlace(t *testing.T) {
	// Section 10.4: "A deploy that posts eight messages to #deploys trains
	// people to mute the channel."
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	n := &Notifier{Store: f.store, Config: f.cfg, Transport: f.transport,
		Now: func() time.Time { return f.now }}

	for _, state := range []store.State{
		store.StatePreflight, store.StateAwaitApproval, store.StateDeploying,
	} {
		ev := engine.Event{
			Type: engine.EventType(state), DeploymentID: dep.ID,
			App: "web", Environment: "prod", Version: "v2", State: state,
			Actor: "jordan", At: f.now,
		}
		payload, _ := json.Marshal(ev)
		if err := n.Notify(context.Background(), string(payload)); err != nil {
			t.Fatal(err)
		}
	}

	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.posted) != 1 {
		t.Errorf("posted %d messages, want 1", len(f.transport.posted))
	}
	if len(f.transport.updated) == 0 {
		t.Error("later states should update the first message")
	}
}

func TestATerminalFailurePostsANewMessage(t *testing.T) {
	// "Post a *new* message only for terminal failure states, so failures
	// break through the noise."
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	dep.SlackTS = "ts-1"
	if err := f.store.UpdateDeployment(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	n := &Notifier{Store: f.store, Config: f.cfg, Transport: f.transport}

	ev := engine.Event{
		Type: engine.EventType(store.StateRollbackFailed), DeploymentID: dep.ID, App: "web",
		Environment: "prod", Version: "v2", State: store.StateRollbackFailed,
		Pages: true, Reason: "the rollback itself failed",
		Detail: map[string]string{"action": "PRODUCTION IS IN AN UNKNOWN STATE."},
	}
	payload, _ := json.Marshal(ev)
	if err := n.Notify(context.Background(), string(payload)); err != nil {
		t.Fatal(err)
	}

	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.posted) != 1 {
		t.Fatalf("posted %d, want a new message", len(f.transport.posted))
	}
	body, _ := json.Marshal(f.transport.posted[0])
	if !strings.Contains(string(body), "@oncall") {
		t.Error("mention_on_failure should reach the message")
	}
	if !strings.Contains(string(body), "UNKNOWN STATE") {
		t.Error("the action line should be prominent")
	}
}

func TestUpdatesAreCoalesced(t *testing.T) {
	// Section 10.4: "A canary with a 15-second sample interval updating a
	// message on every sample will get throttled."
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	dep.SlackTS = "ts-1"
	_ = f.store.UpdateDeployment(context.Background(), dep)

	now := f.now
	n := &Notifier{
		Store: f.store, Config: f.cfg, Transport: f.transport,
		MinUpdateInterval: 10 * time.Second,
		Now:               func() time.Time { return now },
	}

	ev := engine.Event{
		Type: engine.EventType(store.StateVerifying), DeploymentID: dep.ID, App: "web", Environment: "prod",
		Version: "v2", State: store.StateVerifying, At: now,
	}
	payload, _ := json.Marshal(ev)

	// Twenty samples' worth of updates, two seconds apart.
	for i := 0; i < 20; i++ {
		if err := n.Notify(context.Background(), string(payload)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
	}

	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.updated) > 5 {
		t.Errorf("sent %d updates for 40 seconds of verification; Slack allows about one per second and this should coalesce", len(f.transport.updated))
	}
	if len(f.transport.updated) == 0 {
		t.Error("coalescing must not mean sending nothing")
	}
}

func TestAStateChangeIsNeverCoalescedAway(t *testing.T) {
	// The updates people are waiting for must always go through.
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	dep.SlackTS = "ts-1"
	_ = f.store.UpdateDeployment(context.Background(), dep)

	now := f.now
	n := &Notifier{
		Store: f.store, Config: f.cfg, Transport: f.transport,
		MinUpdateInterval: time.Hour,
		Now:               func() time.Time { return now },
	}
	for _, state := range []store.State{
		store.StateDeploying, store.StateVerifying, store.StatePromoting,
	} {
		ev := engine.Event{
			Type: engine.EventType(state), DeploymentID: dep.ID, App: "web",
			Environment: "prod", Version: "v2", State: state, At: now,
		}
		payload, _ := json.Marshal(ev)
		if err := n.Notify(context.Background(), string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.updated) != 3 {
		t.Errorf("updates = %d, want 3; a state change must not be throttled", len(f.transport.updated))
	}
}

func TestARateLimitIsRetriedNotDropped(t *testing.T) {
	// The entry stays in the outbox, which is what at-least-once delivery
	// means in practice.
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	n := &Notifier{Store: f.store, Config: f.cfg, Transport: f.transport}
	f.transport.postErr = &RateLimitedError{RetryAfter: 3 * time.Second}

	ev := engine.Event{
		Type: engine.EventType(store.StatePreflight), DeploymentID: dep.ID, App: "web", Environment: "prod",
		Version: "v2", State: store.StatePreflight, At: f.now,
	}
	payload, _ := json.Marshal(ev)
	err := n.Notify(context.Background(), string(payload))
	if err == nil {
		t.Fatal("a rate limit must surface as an error so the entry is retried")
	}
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("want RateLimitedError, got %v", err)
	}
	if rl.RetryAfter != 3*time.Second {
		t.Errorf("the retry-after should be carried through: %v", rl.RetryAfter)
	}
}

func TestAnUnparseablePayloadIsDroppedNotRetriedForever(t *testing.T) {
	// It will never become well-formed, and retrying it blocks the queue
	// behind it.
	f := newFixture(t)
	n := &Notifier{Store: f.store, Config: f.cfg, Transport: f.transport,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := n.Notify(context.Background(), "{not json"); err != nil {
		t.Fatalf("want the entry discarded, got %v", err)
	}
}

func TestOnlySubscribedEventsAreSent(t *testing.T) {
	f := newFixture(t)
	dep := f.deployment("prod", "jordan")
	n := &Notifier{Store: f.store, Config: f.cfg, Transport: f.transport}

	// "aborted" is not in this environment's `on` list.
	ev := engine.Event{
		Type: "aborted", DeploymentID: dep.ID, App: "web", Environment: "prod",
		Version: "v2", State: store.StateAborted, At: f.now,
	}
	payload, _ := json.Marshal(ev)
	if err := n.Notify(context.Background(), string(payload)); err != nil {
		t.Fatal(err)
	}
	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.posted)+len(f.transport.updated) != 0 {
		t.Error("an unsubscribed event should not be sent")
	}
}

// ---------------------------------------------------------------- blocks

func TestEveryMessageHasFallbackText(t *testing.T) {
	// Block Kit messages without it show as "This content can't be
	// displayed" in notifications and to screen readers.
	ev := engine.Event{
		App: "web", Environment: "prod", Version: "v2",
		State: store.StateDeploying, Actor: "jordan", At: time.Now(),
	}
	for name, m := range map[string]Message{
		"deploy":  DeployMessage("#deploys", ev, nil, "https://orch.example.com"),
		"failure": FailureMessage("#deploys", ev, nil),
	} {
		if m.Text == "" {
			t.Errorf("%s message has no fallback text", name)
		}
	}
}

func TestApprovalButtonsAppearOnlyWhileWaiting(t *testing.T) {
	base := engine.Event{App: "web", Environment: "prod", Version: "v2", At: time.Now()}

	waiting := base
	waiting.State = store.StateAwaitApproval
	body, _ := json.Marshal(DeployMessage("#d", waiting, nil, ""))
	if !strings.Contains(string(body), ActionApprove) {
		t.Error("an awaiting-approval message needs an Approve button")
	}

	done := base
	done.State = store.StateSucceeded
	body, _ = json.Marshal(DeployMessage("#d", done, nil, ""))
	if strings.Contains(string(body), ActionApprove) {
		t.Error("a finished deploy must not offer an Approve button")
	}
}

func TestHealthSamplesAreSummarisedNotListed(t *testing.T) {
	// Twenty samples per criterion in a Slack message is how a channel gets
	// muted. One line per criterion, showing the latest.
	var samples []*store.HealthSample
	for i := 0; i < 20; i++ {
		samples = append(samples,
			&store.HealthSample{Criterion: "error-rate", Value: 0.0004, Baseline: 0.0003, Healthy: true},
			&store.HealthSample{Criterion: "p99-latency", Value: 181, Baseline: 174, Healthy: true})
	}
	ev := engine.Event{App: "web", Environment: "prod", Version: "v2",
		State: store.StateVerifying, At: time.Now()}
	body, _ := json.Marshal(DeployMessage("#d", ev, samples, ""))
	if n := strings.Count(string(body), "error-rate"); n != 1 {
		t.Errorf("error-rate appears %d times, want 1", n)
	}
	if !strings.Contains(string(body), "baseline") {
		t.Error("the baseline is what makes the number interpretable")
	}
}
