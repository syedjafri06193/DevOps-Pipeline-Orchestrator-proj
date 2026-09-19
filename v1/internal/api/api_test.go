package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/auth"
	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
)

const apiConfig = `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
        strategy: { type: rolling }
      staging:
        provider: fake
        strategy: { type: rolling }
      prod:
        provider: fake
        strategy: { type: blue-green }
        promote_from: staging
        gates:
          - type: approval
            require_peer: true
            roles: [release-manager]
            ttl: 1h
environments:
  prod:
    require_peer_approval: true
    protected: true
notifiers:
  slack:
    signing_secret: super-secret-value
    bot_token: xoxb-super-secret
    app_token: xapp-super-secret
roles:
  developer:
    - deploy:trigger    on: [dev, staging]
    - deploy:view       on: ["*"]
  release-manager:
    - deploy:trigger    on: ["*"]
    - deploy:approve    on: ["*"]
    - deploy:rollback   on: ["*"]
    - deploy:freeze     on: ["*"]
    - deploy:abort      on: ["*"]
    - deploy:view       on: ["*"]
  auditor:
    - deploy:view       on: ["*"]
identities:
  jordan:
    slack_id: U01JORDAN
    roles: [developer]
  sam:
    slack_id: U02SAM
    roles: [release-manager]
  quinn:
    slack_id: U03QUINN
    roles: [auditor]
`

type apiClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *apiClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

type fixture struct {
	t       *testing.T
	server  *Server
	handler http.Handler
	store   *store.Store
	cfg     *config.Config
	tokens  *auth.Store
	clock   *apiClock
	started []string
	mu      sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	cfg, err := config.Decode(apiConfig, "orch.yaml")
	if err != nil {
		t.Fatalf("test config: %v", err)
	}

	clk := &apiClock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "orch.journal"), clk)
	if err != nil {
		t.Fatal(err)
	}
	st.SetLeaseTTL(24 * time.Hour)
	t.Cleanup(func() { _ = st.Close() })

	tokens := auth.NewStore(clk.Now)
	f := &fixture{t: t, store: st, cfg: cfg, tokens: tokens, clock: clk}

	f.server = &Server{
		Store:  st,
		Config: cfg,
		Tokens: tokens,
		Authz:  &auth.Authorizer{Config: cfg},
		Executor: engine.New(engine.Options{
			Store: st, Config: cfg, Clock: engine.RealClock,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         clk.Now,
		RequireAuth: true,
		Run: func(id string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.started = append(f.started, id)
		},
	}
	f.handler = f.server.Routes()
	return f
}

func (f *fixture) token(user string, apps, envs []string) string {
	f.t.Helper()
	plain, _, err := f.tokens.Issue(user, "test", time.Hour, apps, envs)
	if err != nil {
		f.t.Fatal(err)
	}
	return plain
}

func (f *fixture) do(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) deploy(token, app, env, version, key string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do("POST", "/v1/deployments", token,
		CreateDeploymentRequest{App: app, Environment: env, Version: version},
		map[string]string{"Idempotency-Key": key})
}

func (f *fixture) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// ------------------------------------------------------------- idempotency

// Section 6.3: the CLI reuses one key across retries, so a flaky network must
// not produce two deployments.
func TestARepeatedIdempotencyKeyReturnsTheOriginalDeployment(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil)

	first := f.deploy(tok, "web", "dev", "v1.0.0", "key-1")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first deploy = %d: %s", first.Code, first.Body.String())
	}
	second := f.deploy(tok, "web", "dev", "v1.0.0", "key-1")
	if second.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", second.Code, second.Body.String())
	}

	var a, b store.Deployment
	decode(t, first, &a)
	decode(t, second, &b)
	if a.ID != b.ID {
		t.Fatalf("the retry created a second deployment: %s then %s", a.ID, b.ID)
	}
	if n := f.startedCount(); n != 1 {
		t.Fatalf("the executor was started %d times, want 1", n)
	}
}

// A retry carrying a different version is a client bug, but returning the
// original silently would deploy the wrong thing and report success. The
// original is what the key identifies, and the response says so plainly.
func TestARepeatedKeyWithADifferentVersionStillReturnsTheOriginal(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil)

	first := f.deploy(tok, "web", "dev", "v1.0.0", "key-1")
	second := f.deploy(tok, "web", "dev", "v2.0.0", "key-1")

	var a, b store.Deployment
	decode(t, first, &a)
	decode(t, second, &b)
	if b.Version != "v1.0.0" {
		t.Fatalf("the retry reported version %q; the key names the first request", b.Version)
	}
	if a.ID != b.ID {
		t.Fatal("a second deployment was created")
	}
}

func TestAMutatingCallWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil)

	rec := f.do("POST", "/v1/deployments", tok,
		CreateDeploymentRequest{App: "web", Environment: "dev", Version: "v1"}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Idempotency-Key") {
		t.Errorf("the error does not name the missing header: %s", rec.Body.String())
	}
}

func TestConcurrentRetriesOfOneKeyProduceOneDeployment(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.deploy(tok, "web", "dev", "v1.0.0", "same-key")
		}()
	}
	wg.Wait()

	deps, err := f.store.ListDeployments(context.Background(), store.ListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("20 concurrent retries created %d deployments, want 1", len(deps))
	}
	if n := f.startedCount(); n != 1 {
		t.Fatalf("the executor was started %d times, want 1", n)
	}
}

// ------------------------------------------------------------------- authn

func TestNoTokenIs401(t *testing.T) {
	f := newFixture(t)
	rec := f.deploy("", "web", "dev", "v1", "key-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestAnExpiredTokenSaysSoRatherThanJustFailing(t *testing.T) {
	f := newFixture(t)
	plain, _, err := f.tokens.Issue("jordan", "test", time.Minute, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.mu.Lock()
	f.clock.t = f.clock.t.Add(time.Hour)
	f.clock.mu.Unlock()

	rec := f.deploy(plain, "web", "dev", "v1", "key-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	// "Your token expired at 12:01" and "that token does not exist" send
	// someone to very different places.
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("the error does not distinguish expiry: %s", rec.Body.String())
	}
}

func TestARevokedTokenIsRefused(t *testing.T) {
	f := newFixture(t)
	plain, tok, err := f.tokens.Issue("jordan", "test", time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tokens.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.deploy(plain, "web", "dev", "v1", "key-1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestTheTokenIsNeverStoredInPlaintext(t *testing.T) {
	f := newFixture(t)
	plain := f.token("jordan", nil, nil)
	for _, tok := range f.tokens.List() {
		if strings.Contains(tok.Hash, plain) || tok.Hash == plain {
			t.Fatal("the token table holds the plaintext; a stolen table would be replayable")
		}
	}
}

// ------------------------------------------------------------------- authz

// Section 12.1's second threat is a malicious insider, and the mitigation is
// real RBAC rather than "is this person in the channel".
func TestADeveloperCannotDeployToProd(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil) // developer: dev and staging only

	rec := f.deploy(tok, "web", "prod", "v1.0.0", "key-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if n := f.startedCount(); n != 0 {
		t.Fatal("a denied deploy still started the executor")
	}
}

func TestAReleaseManagerCanDeployToProd(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)
	if rec := f.deploy(tok, "web", "prod", "v1.0.0", "key-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

// A CI token scoped to staging is a capability, not a login: it must not carry
// its owner's production rights.
func TestAScopedTokenCannotReachOutsideItsScope(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", []string{"web"}, []string{"staging"})

	if rec := f.deploy(tok, "web", "staging", "v1", "key-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("in-scope deploy = %d: %s", rec.Code, rec.Body.String())
	}
	rec := f.deploy(tok, "web", "prod", "v1", "key-2")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope deploy = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "scoped") {
		t.Errorf("the error does not explain the scope: %s", rec.Body.String())
	}
}

func TestAnAuditorCanReadButNotDeploy(t *testing.T) {
	f := newFixture(t)
	tok := f.token("quinn", nil, nil)

	if rec := f.do("GET", "/v1/status", tok, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("status read = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := f.deploy(tok, "web", "dev", "v1", "key-1"); rec.Code != http.StatusForbidden {
		t.Fatalf("deploy = %d, want 403", rec.Code)
	}
}

func TestAnUnknownUserIsRefusedEvenWithAValidToken(t *testing.T) {
	f := newFixture(t)
	// A token for someone who has since been removed from the config. The
	// token is cryptographically fine; the identity is gone.
	tok := f.token("someone-who-left", nil, nil)
	if rec := f.deploy(tok, "web", "dev", "v1", "key-1"); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------------- audit

// Section 12.4: a denied attempt is the row someone looks for after an
// incident, so it must be there.
func TestADeniedDeployIsAudited(t *testing.T) {
	f := newFixture(t)
	tok := f.token("jordan", nil, nil)

	f.deploy(tok, "web", "prod", "v1.0.0", "key-1")

	entries, err := f.store.AuditEntries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.AuditEntry
	for _, e := range entries {
		if e.Action == "deploy.denied" {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("no deploy.denied row in %d audit entries", len(entries))
	}
	if found.Actor != "jordan" {
		t.Errorf("the denied row names %q, not the person who tried", found.Actor)
	}
	if found.Detail["reason"] == "" {
		t.Error("the denied row does not say why")
	}
}

func TestTheAuditChainVerifiesAfterRealTraffic(t *testing.T) {
	f := newFixture(t)
	jordan := f.token("jordan", nil, nil)
	sam := f.token("sam", nil, nil)

	f.deploy(jordan, "web", "dev", "v1", "k1")
	f.deploy(jordan, "web", "prod", "v1", "k2") // denied, audited
	f.deploy(sam, "web", "prod", "v1", "k3")

	if err := f.store.VerifyAudit(context.Background()); err != nil {
		t.Fatalf("the audit chain does not verify: %v", err)
	}
	if f.store.AuditHead() == store.GenesisHash {
		t.Fatal("the audit head never moved")
	}
}

// ----------------------------------------------------------------- dry run

// Section 17.3: --dry-run should be "safe enough that people run it
// reflexively", which means it must not leave anything behind.
func TestADryRunCreatesNothing(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)

	rec := f.do("POST", "/v1/deployments", tok,
		CreateDeploymentRequest{App: "web", Environment: "prod", Version: "v9", DryRun: true},
		map[string]string{"Idempotency-Key": "dry-1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	deps, _ := f.store.ListDeployments(context.Background(), store.ListFilter{Limit: 10})
	if len(deps) != 0 {
		t.Fatalf("a dry run created %d deployments", len(deps))
	}
	if n := f.startedCount(); n != 0 {
		t.Fatal("a dry run started the executor")
	}

	var plan map[string]any
	decode(t, rec, &plan)
	// The plan is the point: it has to say what would have happened, including
	// the gates, or "run it reflexively" buys nothing.
	if plan["gates"] == nil {
		t.Error("the plan does not describe the gates")
	}
	if plan["strategy"] != "blue-green" {
		t.Errorf("the plan says strategy %v, want the configured blue-green", plan["strategy"])
	}
}

// The same key used for a dry run must not block the real deploy that follows,
// since the dry run stored nothing to return.
func TestADryRunDoesNotBurnItsIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)

	f.do("POST", "/v1/deployments", tok,
		CreateDeploymentRequest{App: "web", Environment: "prod", Version: "v9", DryRun: true},
		map[string]string{"Idempotency-Key": "shared-key"})

	rec := f.deploy(tok, "web", "prod", "v9", "shared-key")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the real deploy after a dry run = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------------ misc

func TestUnknownAppIs404NotAnEmptyDeployment(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)
	rec := f.deploy(tok, "nope", "prod", "v1", "key-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/health", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: health is what a load balancer polls", rec.Code)
	}
}

func TestStatusReportsAFreeze(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)

	rec := f.do("POST", "/v1/freezes", tok, FreezeRequest{
		App: "web", Environment: "prod", Reason: "release train paused",
	}, map[string]string{"Idempotency-Key": "freeze-1"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("setting a freeze = %d: %s", rec.Code, rec.Body.String())
	}

	rec = f.do("GET", "/v1/status?app=web&environment=prod", tok, nil, nil)
	var body struct{ Status []Status }
	decode(t, rec, &body)
	if len(body.Status) != 1 {
		t.Fatalf("got %d rows, want 1", len(body.Status))
	}
	if !body.Status[0].Frozen {
		t.Error("status does not report the environment as frozen")
	}
	if body.Status[0].FreezeReason != "release train paused" {
		t.Errorf("freeze reason = %q", body.Status[0].FreezeReason)
	}
}

func TestTheConfigEndpointRedactsSecrets(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)

	rec := f.do("GET", "/v1/config", tok, nil, nil)
	for _, secret := range []string{"super-secret-value", "xoxb-super-secret", "xapp-super-secret"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("GET /v1/config returned %q verbatim", secret)
		}
	}
}

// Approval is the one place where a bug hands someone else's authority away,
// so the API path is held to the same four checks as the Slack button.
func TestSelfApprovalIsRefusedThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	sam := f.token("sam", nil, nil)

	rec := f.deploy(sam, "web", "prod", "v1.0.0", "key-1")
	var dep store.Deployment
	decode(t, rec, &dep)

	rec = f.do("POST", "/v1/deployments/"+dep.ID+"/approve", sam, nil,
		map[string]string{"Idempotency-Key": "approve-1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	entries, _ := f.store.AuditEntries(context.Background(), 100)
	var denied bool
	for _, e := range entries {
		if e.Action == "approval.denied" {
			denied = true
		}
	}
	if !denied {
		t.Error("a refused approval was not audited")
	}
}

func TestApprovingAnEnvironmentWithNoGateIsARequestError(t *testing.T) {
	f := newFixture(t)
	sam := f.token("sam", nil, nil)

	rec := f.deploy(sam, "web", "dev", "v1.0.0", "key-1")
	var dep store.Deployment
	decode(t, rec, &dep)

	rec = f.do("POST", "/v1/deployments/"+dep.ID+"/approve", sam, nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownDeploymentIs404(t *testing.T) {
	f := newFixture(t)
	tok := f.token("sam", nil, nil)
	if rec := f.do("GET", "/v1/deployments/"+ulid.New().String(), tok, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestErrorsAreJSONWithAMessage(t *testing.T) {
	f := newFixture(t)
	rec := f.deploy("", "web", "dev", "v1", "key-1")

	var e ErrorResponse
	decode(t, rec, &e)
	if e.Error == "" {
		t.Fatalf("an error response with no message: %s", rec.Body.String())
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
}
