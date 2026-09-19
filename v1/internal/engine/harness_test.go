package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/provider"
	fakeprovider "github.com/syedjafri06193/orch/internal/provider/fake"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
	"github.com/syedjafri06193/orch/internal/verifier"
	fakeverifier "github.com/syedjafri06193/orch/internal/verifier/fake"
)

// fakeClock drives the verification loop and the lease heartbeat.
//
// Its tickers fire immediately and advance the clock by the tick interval, so
// a ten-minute bake window with a thirty-second sample interval runs twenty
// samples in microseconds. Without this, every verification test would be a
// real ten-minute wait, which means in practice they would not exist.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) NewTicker(d time.Duration) Ticker { return &fakeTicker{clock: c, interval: d} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.Advance(d)
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

// fakeTicker fires on demand, advancing the clock as it does. The receive is
// what moves time forward, which keeps the loop's "have we passed the
// deadline" check meaningful.
type fakeTicker struct {
	clock    *fakeClock
	interval time.Duration
	stopped  bool
	mu       sync.Mutex
}

func (t *fakeTicker) C() <-chan time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan time.Time, 1)
	if t.stopped {
		return ch // never fires
	}
	t.clock.Advance(t.interval)
	ch <- t.clock.Now()
	return ch
}

func (t *fakeTicker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
}

// storeClock adapts the fake clock to the store's narrower Clock interface.
type storeClock struct{ c *fakeClock }

func (s storeClock) Now() time.Time { return s.c.Now() }

// harness wires an executor to fakes for every boundary.
type harness struct {
	t         *testing.T
	store     *store.Store
	cfg       *config.Config
	provider  *fakeprovider.Provider
	verifier  *fakeverifier.Verifier
	manifests *MapManifests
	clock     *fakeClock
	exec      *Executor
	dep       *store.Deployment
	path      string
}

const harnessConfig = `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
        strategy: { type: rolling }
      staging:
        provider: fake
        strategy: { type: blue-green }
        verify:
          bake: 10m
          sample_interval: 30s
          min_samples: 3
          criteria:
            - name: error-rate
              verifier: fake
              direction: lower-is-better
              max: 0.01
              floor_value: 0.0001
      prod:
        provider: fake
        strategy: { type: blue-green }
        promote_from: staging
        verify:
          bake: 10m
          sample_interval: 30s
          min_samples: 3
          criteria:
            - name: error-rate
              verifier: fake
              direction: lower-is-better
              max: 0.01
              floor_value: 0.0001
        gates:
          - type: approval
            require_peer: true
            roles: [release-manager]
            ttl: 1h
        rollback:
          automatic: true
          check_migration_floor: true
          max_attempts: 2
environments:
  prod:
    require_peer_approval: true
    protected: true
roles:
  developer:
    - deploy:trigger    on: [dev, staging]
    - deploy:view       on: ["*"]
  release-manager:
    - deploy:trigger    on: ["*"]
    - deploy:approve    on: ["*"]
    - deploy:rollback   on: ["*"]
identities:
  jordan:
    slack_id: U01JORDAN
    roles: [developer]
  sam:
    slack_id: U02SAM
    roles: [release-manager]
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, harnessConfig)
}

func newHarnessWith(t *testing.T, cfgSrc string) *harness {
	t.Helper()

	cfg, err := config.Decode(cfgSrc, "orch.yaml")
	if err != nil {
		t.Fatalf("harness config: %v", err)
	}

	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "orch.journal")
	st, err := store.Open(path, storeClock{clk})
	if err != nil {
		t.Fatal(err)
	}
	// The fake clock jumps by a sample interval on every verification tick,
	// so a ten-minute bake advances virtual time by ten minutes in
	// microseconds -- faster than any heartbeat goroutine can be scheduled.
	// A production-sized 60s lease would expire mid-bake for reasons that
	// have nothing to do with the code under test. The invariant that
	// matters (TTL comfortably exceeds the heartbeat interval) still holds.
	st.SetLeaseTTL(24 * time.Hour)
	t.Cleanup(func() { _ = st.Close() })

	p := fakeprovider.New()
	v := fakeverifier.New(0.0001)
	man := NewMapManifests()

	exec := New(Options{
		Store:     st,
		Config:    cfg,
		Providers: provider.NewRegistry(p),
		Verifiers: verifier.NewRegistry(v),
		Manifests: man,
		Clock:     clk,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeID:    "node-test",
	})

	h := &harness{
		t: t, store: st, cfg: cfg, provider: p, verifier: v,
		manifests: man, clock: clk, exec: exec, path: path,
	}
	h.dep = h.newDeployment("web", "staging", "v2")
	return h
}

// newDeployment creates a PENDING deployment ready to run.
func (h *harness) newDeployment(app, env, version string) *store.Deployment {
	h.t.Helper()
	d := &store.Deployment{
		ID:            ulid.NewAt(h.clock.Now()).String(),
		App:           app,
		Environment:   env,
		Version:       version,
		Strategy:      "blue-green",
		State:         store.StatePending,
		TriggeredBy:   "jordan",
		TriggerSource: store.TriggerCLI,
		Provider:      "fake",
	}
	got, _, err := h.store.CreateDeployment(context.Background(), d)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// reopen simulates a process restart against the same journal.
func (h *harness) reopen() *harness {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	st, err := store.Open(h.path, storeClock{h.clock})
	if err != nil {
		h.t.Fatal(err)
	}
	st.SetLeaseTTL(24 * time.Hour)
	h.t.Cleanup(func() { _ = st.Close() })

	next := &harness{
		t: h.t, store: st, cfg: h.cfg, provider: h.provider, verifier: h.verifier,
		manifests: h.manifests, clock: h.clock, path: h.path, dep: h.dep,
	}
	next.exec = New(Options{
		Store:     st,
		Config:    h.cfg,
		Providers: provider.NewRegistry(h.provider),
		Verifiers: verifier.NewRegistry(h.verifier),
		Manifests: h.manifests,
		Clock:     h.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// A different node id, because a restart is a new process and the
		// tests should not accidentally depend on it being the same one.
		NodeID: "node-test-2",
	})
	return next
}

func (h *harness) state(id string) store.State {
	h.t.Helper()
	d, err := h.store.Deployment(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d.State
}

func (h *harness) approve(dep *store.Deployment, user string) {
	h.t.Helper()
	if err := h.store.RecordApproval(context.Background(), &store.Approval{
		DeploymentID: dep.ID, User: user, At: h.clock.Now(), Source: "test",
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) appEnv(app, env string) *config.AppEnv {
	h.t.Helper()
	ae, err := h.cfg.AppEnv(app, env)
	if err != nil {
		h.t.Fatal(err)
	}
	return ae
}
