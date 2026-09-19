package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
)

const testConfig = `
version: 1
apps:
  web:
    environments:
      staging:
        provider: fake
        strategy: { type: rolling }
      prod:
        provider: fake
        strategy: { type: blue-green }
        promote_from: staging
notifiers:
  slack:
    signing_secret: super-secret-value
    bot_token: xoxb-super-secret
    app_token: xapp-super-secret
roles:
  developer:
    - deploy:trigger    on: ["*"]
identities:
  jordan:
    slack_id: U01JORDAN
    roles: [developer]
`

// testClock is settable, so a seeded deployment can be given a real duration:
// the store stamps FinishedAt from its own clock, which is the right thing for
// production and means a test cannot simply assign the field.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newTestServer(t *testing.T) (*Server, *testClock, http.Handler) {
	t.Helper()

	cfg, err := config.Decode(testConfig, "orch.yaml")
	if err != nil {
		t.Fatalf("test config: %v", err)
	}
	cfg.SourceFile = "orch.yaml"
	cfg.SourceCommit = "0123456789abcdef0123456789abcdef01234567"

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clk := &testClock{t: now}
	st, err := store.Open(filepath.Join(t.TempDir(), "orch.journal"), clk)
	if err != nil {
		t.Fatal(err)
	}
	// Seeding walks the state machine across simulated minutes; a 60-second
	// production lease would expire between transitions for reasons that have
	// nothing to do with what is under test.
	st.SetLeaseTTL(24 * time.Hour)
	t.Cleanup(func() { _ = st.Close() })

	s := &Server{
		Store: st, Config: cfg, Bus: NewBus(),
		Version: "test", NodeID: "node-1",
		Now: func() time.Time { return now },
	}
	h, err := s.Routes()
	if err != nil {
		t.Fatalf("building routes: %v", err)
	}
	return s, clk, h
}

// seed drives a deployment through the real state machine to a target state.
//
// Assigning the state directly is refused by the store, and rightly: every
// transition in production is validated, fenced and journalled, so a fixture
// that skipped that would be testing pages against data the system cannot
// actually produce.
func seed(t *testing.T, s *Server, clk *testClock, app, env, version string,
	state store.State, created time.Time, dur time.Duration) *store.Deployment {
	t.Helper()
	ctx := context.Background()

	clk.Set(created)
	d, _, err := s.Store.CreateDeployment(ctx, &store.Deployment{
		ID: ulid.New().String(), App: app, Environment: env, Version: version,
		Strategy: "rolling", Provider: "fake", TriggeredBy: "jordan",
		TriggerSource: store.TriggerCLI, State: store.StatePending,
	})
	if err != nil {
		t.Fatal(err)
	}

	fence, err := s.Store.AcquireLease(ctx, d.Resource(), d.ID, "node-1")
	if err != nil {
		t.Fatal(err)
	}

	var path []store.State
	switch state {
	case store.StatePending:
		path = nil
	case store.StateSucceeded:
		path = []store.State{store.StatePreflight, store.StateDeploying,
			store.StateVerifying, store.StatePromoting, store.StateSucceeded}
	case store.StateFailed:
		path = []store.State{store.StatePreflight, store.StateDeploying, store.StateFailed}
	case store.StateRolledBack:
		path = []store.State{store.StatePreflight, store.StateDeploying,
			store.StateRollingBack, store.StateRolledBack}
	case store.StateRollbackFailed:
		path = []store.State{store.StatePreflight, store.StateDeploying,
			store.StateRollingBack, store.StateRollbackFailed}
	default:
		t.Fatalf("seed does not know how to reach %s", state)
	}

	from := store.StatePending
	for i, to := range path {
		if i == len(path)-1 {
			// The last hop stamps FinishedAt from the store's clock, so the
			// deployment ends up with the duration the caller asked for.
			clk.Set(created.Add(dur))
		}
		if err := s.Store.TransitionState(ctx, d.ID, fence, from, to); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		from = to
	}

	if err := s.Store.ReleaseLease(ctx, d.Resource(), d.ID); err != nil {
		t.Fatal(err)
	}

	d, err = s.Store.Deployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == store.StateFailed || state == store.StateRolledBack {
		d.Error = "verification failed: error-rate above threshold"
		if err := s.Store.UpdateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Section 11's pages exist to be opened during an incident. A template that
// only fails when rendered is a 500 at the worst possible moment, so these
// tests render every page rather than merely constructing the server.

func TestEveryPageRenders(t *testing.T) {
	s, clk, h := newTestServer(t)
	base := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)
	d := seed(t, s, clk, "web", "prod", "v1.2.3", store.StateSucceeded, base, 4*time.Minute)

	for _, path := range []string{"/", "/deployments", "/deployments/" + d.ID, "/audit", "/config"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d\n%s", path, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q", ct)
			}
			// A template that fails halfway leaves a truncated page with a 200
			// already written, so the status code alone proves nothing.
			if !strings.Contains(rec.Body.String(), "</html>") {
				t.Errorf("page is truncated -- the template errored mid-render:\n%s", rec.Body.String())
			}
		})
	}
}

// Templates are parsed in Routes, not per request, so a broken one is a
// refusal to start rather than a 500 during an incident. This test asserts the
// parsing actually happens there -- if it moved back to render time, the map
// would be empty after a successful Routes call.
func TestTemplatesAreParsedAtStartup(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, page := range []string{"overview", "deployment", "history", "audit", "config"} {
		if s.tmpl[page] == nil {
			t.Errorf("%s was not parsed by Routes", page)
		}
	}
}

func TestUnknownDeploymentIs404(t *testing.T) {
	_, _, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/deployments/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	_, _, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: the CSS is embedded, so this cannot depend on the working directory", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "{") {
		t.Error("the response does not look like CSS")
	}
}

// Section 11.2 puts anything that pages at the top of the overview, because it
// is the only thing on the page that needs acting on right now.
func TestTheOverviewSurfacesWhatPages(t *testing.T) {
	s, clk, h := newTestServer(t)
	base := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)
	seed(t, s, clk, "web", "staging", "v1.0.0", store.StateSucceeded, base, time.Minute)
	bad := seed(t, s, clk, "web", "prod", "v2.0.0", store.StateRollbackFailed, base, 2*time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, bad.ID) {
		t.Fatalf("the paging deployment is missing from the overview:\n%s", body)
	}
	// It has to come before the recent-deployments table, or an operator has
	// to scroll past healthy noise to find the thing that woke them.
	if i, j := strings.Index(body, bad.ID), strings.Index(body, "v1.0.0"); i > j {
		t.Error("the paging deployment is listed below the healthy ones")
	}
}

func TestAFrozenEnvironmentSaysSoAndWhy(t *testing.T) {
	s, _, h := newTestServer(t)
	err := s.Store.SetFreeze(context.Background(), &store.Freeze{
		Resource: "app:web/env:prod", Reason: "release train paused for the audit",
		By: "sam", At: s.now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "release train paused for the audit") {
		t.Fatalf("the freeze reason is not shown; an operator would retry and be refused:\n%s", body)
	}
}

// Section 19.1: the DORA metrics fall out of the data already stored.
func TestDORAMetrics(t *testing.T) {
	s, clk, _ := newTestServer(t)
	base := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	// Three successes and one failure: a 25% change failure rate.
	seed(t, s, clk, "web", "prod", "v1", store.StateSucceeded, base, 2*time.Minute)
	seed(t, s, clk, "web", "prod", "v2", store.StateSucceeded, base.Add(time.Hour), 4*time.Minute)
	seed(t, s, clk, "web", "prod", "v3", store.StateSucceeded, base.Add(2*time.Hour), 6*time.Minute)
	seed(t, s, clk, "web", "prod", "v4", store.StateFailed, base.Add(3*time.Hour), 3*time.Minute)

	got := s.dora(context.Background(), 30*24*time.Hour)

	if !strings.Contains(got.ChangeFailureRate, "25") {
		t.Errorf("change failure rate = %q, want 25%%", got.ChangeFailureRate)
	}
	// Median of 2m, 4m, 6m is 4m -- the failed deploy is not a delivery.
	if !strings.Contains(got.MedianDuration, "4m") {
		t.Errorf("median duration = %q, want 4m", got.MedianDuration)
	}
	if got.Frequency == "—" {
		t.Error("deployment frequency was not computed")
	}
}

func TestDORAWithNoDataSaysSoRatherThanClaimingZero(t *testing.T) {
	s, _, _ := newTestServer(t)
	got := s.dora(context.Background(), 30*24*time.Hour)

	// A change failure rate of "0%" with no deployments reads as a perfect
	// record rather than an empty one, which is the more dangerous of the two
	// things to show someone.
	if got.ChangeFailureRate == "0%" || got.ChangeFailureRate == "0.0%" {
		t.Errorf("change failure rate = %q with no deployments; want an explicit no-data marker", got.ChangeFailureRate)
	}
}

// Section 12.3: "never log a secret". The config page renders the live config,
// which is where the secrets are.
func TestTheConfigPageNeverRendersASecret(t *testing.T) {
	_, _, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/config", nil))
	body := rec.Body.String()

	for _, secret := range []string{"super-secret-value", "xoxb-super-secret", "xapp-super-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("the config page rendered %q verbatim", secret)
		}
	}
	// It should still say a secret is configured -- "not set" and "set to
	// something I will not show you" are different operational facts.
	if !strings.Contains(strings.ToLower(body), "set") {
		t.Error("the page does not indicate whether the secret is configured")
	}
}

func TestTheConfigPageShowsWhereTheConfigCameFrom(t *testing.T) {
	_, _, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/config", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "orch.yaml") {
		t.Error("the source file is not shown")
	}
	if !strings.Contains(body, "0123456789ab") {
		t.Error("the source commit is not shown; 'which config is this' is the first question during an incident")
	}
}

func TestMask(t *testing.T) {
	if got := mask(""); strings.Contains(got, "*") {
		t.Errorf("an unset secret rendered as %q, which implies one is configured", got)
	}
	got := mask("xoxb-1234567890")
	if strings.Contains(got, "1234567890") {
		t.Errorf("mask leaked the secret: %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{4 * time.Minute, "4m"},
		{90 * time.Minute, "1h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); !strings.Contains(got, c.want) {
			t.Errorf("humanDuration(%s) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestDetailRenderingIsStable(t *testing.T) {
	// A map iterates in random order, so an audit row would reshuffle on every
	// refresh and two screenshots of the same event would not match.
	d := map[string]string{"version": "v1.2.3", "app": "web", "environment": "prod"}
	first := renderDetail(d)
	for i := 0; i < 20; i++ {
		if got := renderDetail(d); got != first {
			t.Fatalf("detail rendering is not deterministic: %q then %q", first, got)
		}
	}
}

func TestStateClassesCoverEveryState(t *testing.T) {
	for _, st := range store.AllStates() {
		if stateClass(st) == "" {
			t.Errorf("state %s has no CSS class, so it renders unstyled", st)
		}
	}
}
