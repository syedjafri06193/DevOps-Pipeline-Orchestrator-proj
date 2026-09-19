package auth

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
)

const authConfig = `
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

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Decode(authConfig, "orch.yaml")
	if err != nil {
		t.Fatalf("test config: %v", err)
	}
	return cfg
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
}

// ------------------------------------------------------------------ tokens

// Section 12.1's first threat is a stolen CLI token, and the mitigation is
// "short TTL, scoped to app+env, revocable, audit every use". Each of these
// tests is one of those four.

func TestAnIssuedTokenIsShownOnceAndStoredHashed(t *testing.T) {
	s := NewStore(newClock().now)
	plain, tok, err := s.Issue("jordan", "laptop", time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(plain, "orch_") {
		// The prefix is what makes a leaked token greppable in a CI log and
		// recognisable to a secret scanner.
		t.Errorf("token %q has no recognisable prefix", plain)
	}
	if tok.Hash == plain {
		t.Fatal("the stored value is the token itself")
	}
	if strings.Contains(tok.Hash, plain[5:]) {
		t.Fatal("the stored hash contains the token")
	}
	// Nothing anywhere should be able to recover it.
	for _, stored := range s.List() {
		if stored.Hash == plain {
			t.Fatal("the plaintext is recoverable from the store")
		}
	}
}

func TestTwoTokensAreNeverTheSame(t *testing.T) {
	s := NewStore(newClock().now)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		plain, _, err := s.Issue("jordan", "", time.Hour, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[plain] {
			t.Fatal("a token was issued twice")
		}
		seen[plain] = true
	}
}

func TestLookupFindsAValidToken(t *testing.T) {
	s := NewStore(newClock().now)
	plain, issued, _ := s.Issue("jordan", "", time.Hour, nil, nil)

	got, err := s.Lookup(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != issued.ID {
		t.Fatalf("looked up %s, want %s", got.ID, issued.ID)
	}
}

func TestLookupRecordsTheLastUse(t *testing.T) {
	c := newClock()
	s := NewStore(c.now)
	plain, _, _ := s.Issue("jordan", "", time.Hour, nil, nil)

	c.advance(10 * time.Minute)
	tok, err := s.Lookup(plain)
	if err != nil {
		t.Fatal(err)
	}
	// "Audit every use" starts with knowing a token is still in use. A token
	// nobody has presented for months is one to revoke.
	if tok.LastUsedAt != c.now() {
		t.Errorf("last used = %s, want %s", tok.LastUsedAt, c.now())
	}
}

func TestAnExpiredTokenIsRefusedAndSaysWhen(t *testing.T) {
	c := newClock()
	s := NewStore(c.now)
	plain, _, _ := s.Issue("jordan", "", time.Minute, nil, nil)

	c.advance(2 * time.Minute)
	_, err := s.Lookup(plain)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v, want ErrExpired", err)
	}
	if !strings.Contains(err.Error(), "2026") {
		t.Errorf("the error does not say when it expired: %v", err)
	}
}

func TestARevokedTokenIsRefused(t *testing.T) {
	s := NewStore(newClock().now)
	plain, tok, _ := s.Issue("jordan", "", time.Hour, nil, nil)

	if err := s.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(plain); !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v, want ErrRevoked", err)
	}
}

func TestRevocationBeatsExpiryInTheMessage(t *testing.T) {
	c := newClock()
	s := NewStore(c.now)
	plain, tok, _ := s.Issue("jordan", "", time.Minute, nil, nil)
	_ = s.Revoke(tok.ID)
	c.advance(time.Hour)

	// A token that was revoked and has since expired was revoked. Reporting
	// expiry would tell someone to mint a new one when the answer is that
	// their access was taken away.
	if _, err := s.Lookup(plain); !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v, want ErrRevoked", err)
	}
}

func TestAnUnknownTokenIsRefused(t *testing.T) {
	s := NewStore(newClock().now)
	if _, err := s.Lookup("orch_not-a-real-token"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("got %v, want ErrUnknownToken", err)
	}
	if _, err := s.Lookup(""); !errors.Is(err, ErrNoToken) {
		t.Fatalf("got %v, want ErrNoToken", err)
	}
}

func TestATokenMustBelongToSomeone(t *testing.T) {
	s := NewStore(newClock().now)
	if _, _, err := s.Issue("", "", time.Hour, nil, nil); err == nil {
		t.Fatal("issued a token with no user; it could never be audited")
	}
}

func TestIssueDefaultsToAShortTTL(t *testing.T) {
	c := newClock()
	s := NewStore(c.now)
	_, tok, _ := s.Issue("jordan", "", 0, nil, nil)

	ttl := tok.ExpiresAt.Sub(tok.IssuedAt)
	if ttl != DefaultTTL {
		t.Fatalf("default TTL is %s, want %s", ttl, DefaultTTL)
	}
	if ttl > 24*time.Hour {
		t.Errorf("the default TTL is %s; section 12.2 asks for short-lived credentials", ttl)
	}
}

// ------------------------------------------------------------------- scope

func TestScopeNarrowsWhatATokenCanTouch(t *testing.T) {
	s := NewStore(newClock().now)
	_, tok, _ := s.Issue("sam", "ci", time.Hour, []string{"web"}, []string{"staging"})

	if !tok.CoversApp("web") || !tok.CoversEnvironment("staging") {
		t.Fatal("the token does not cover its own scope")
	}
	if tok.CoversApp("billing") {
		t.Error("the token covers an app outside its scope")
	}
	if tok.CoversEnvironment("prod") {
		t.Error("the token covers an environment outside its scope")
	}
}

func TestAnUnscopedTokenCoversEverything(t *testing.T) {
	s := NewStore(newClock().now)
	_, tok, _ := s.Issue("sam", "", time.Hour, nil, nil)
	if !tok.CoversApp("anything") || !tok.CoversEnvironment("anything") {
		t.Fatal("an unscoped token should not be narrower than its user")
	}
}

func TestAStarInScopeMeansEverything(t *testing.T) {
	s := NewStore(newClock().now)
	_, tok, _ := s.Issue("sam", "", time.Hour, []string{"*"}, []string{"staging", "prod"})
	if !tok.CoversApp("billing") {
		t.Error(`"*" should cover every app`)
	}
	if tok.CoversEnvironment("dev") {
		t.Error("an explicit environment list should not cover one outside it")
	}
}

// ------------------------------------------------------------------- authz

func TestScopeAndRBACMustBothAllowIt(t *testing.T) {
	cfg := testConfig(t)
	a := &Authorizer{Config: cfg}
	s := NewStore(newClock().now)

	// A release-manager may deploy anywhere, but this token may not.
	_, ci, _ := s.Issue("sam", "ci", time.Hour, nil, []string{"staging"})
	if err := a.Can(ci, "deploy:trigger", "web", "staging"); err != nil {
		t.Fatalf("in-scope deploy refused: %v", err)
	}
	if err := a.Can(ci, "deploy:trigger", "web", "prod"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden: the token is a capability, not a login", err)
	}
}

func TestRBACStillAppliesToAnUnscopedToken(t *testing.T) {
	cfg := testConfig(t)
	a := &Authorizer{Config: cfg}
	s := NewStore(newClock().now)

	_, jordan, _ := s.Issue("jordan", "", time.Hour, nil, nil) // developer
	if err := a.Can(jordan, "deploy:trigger", "web", "staging"); err != nil {
		t.Fatalf("a developer should be able to deploy to staging: %v", err)
	}
	if err := a.Can(jordan, "deploy:trigger", "web", "prod"); !errors.Is(err, ErrForbidden) {
		t.Fatal("a developer deployed to prod")
	}
	if err := a.Can(jordan, "deploy:approve", "web", "staging"); !errors.Is(err, ErrForbidden) {
		t.Fatal("a developer approved a deployment")
	}
}

func TestAUserWhoLeftIsRefusedEvenWithAValidToken(t *testing.T) {
	cfg := testConfig(t)
	a := &Authorizer{Config: cfg}
	s := NewStore(newClock().now)

	// The token is cryptographically fine. The identity has been removed from
	// the config, which is how offboarding works here.
	_, gone, _ := s.Issue("someone-who-left", "", time.Hour, nil, nil)
	err := a.Can(gone, "deploy:view", "web", "dev")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), "someone-who-left") {
		t.Errorf("the error does not name the user: %v", err)
	}
}

func TestNoTokenIsRefused(t *testing.T) {
	a := &Authorizer{Config: testConfig(t)}
	if err := a.Can(nil, "deploy:view", "web", "dev"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("got %v, want ErrNoToken", err)
	}
}

// -------------------------------------------------------------- the header

func TestBearerTokenReadsTheAuthorizationHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/status", nil)
	r.Header.Set("Authorization", "Bearer orch_abc123")
	if got := BearerToken(r); got != "orch_abc123" {
		t.Fatalf("got %q", got)
	}
}

func TestATokenInTheQueryStringIsIgnored(t *testing.T) {
	// Query strings end up in access logs, browser history and Referer
	// headers. Accepting one would put a live credential in all three.
	r := httptest.NewRequest("GET", "/v1/status?token=orch_abc123", nil)
	if got := BearerToken(r); got != "" {
		t.Fatalf("a query-string token was accepted: %q", got)
	}
}

func TestOtherAuthorizationSchemesAreIgnored(t *testing.T) {
	for _, header := range []string{"Basic abc", "bearer orch_x", "orch_x", ""} {
		r := httptest.NewRequest("GET", "/v1/status", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != "" {
			t.Errorf("Authorization: %q yielded %q", header, got)
		}
	}
}

func TestConcurrentLookups(t *testing.T) {
	s := NewStore(newClock().now)
	plain, _, _ := s.Issue("jordan", "", time.Hour, nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Lookup(plain); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
