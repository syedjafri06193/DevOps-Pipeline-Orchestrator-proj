// Package auth handles API tokens and authorization (design.md section 12).
//
// Section 12.1's threat model starts with "stolen CLI token", and the
// mitigation is "short TTL, scoped to app+env, revocable, audit every use".
// All four are here.
//
// Tokens are stored as SHA-256 hashes, never in plaintext. The database of a
// tool that holds production credentials is itself a target, and a stolen
// token table that contains only hashes is a stolen token table that cannot
// be replayed.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
)

var (
	ErrNoToken      = errors.New("auth: no token presented")
	ErrUnknownToken = errors.New("auth: token is not recognised")
	ErrExpired      = errors.New("auth: token has expired")
	ErrRevoked      = errors.New("auth: token has been revoked")
	ErrForbidden    = errors.New("auth: not permitted")
)

// Token is an issued API credential.
//
// `Hash` rather than the token itself: a token is shown once, at issue, and
// is never recoverable afterwards.
type Token struct {
	ID   string
	Hash string
	User string
	// Scope narrows a token below its user's permissions. A CI token that can
	// only deploy one app to staging limits the blast radius of a leaked
	// pipeline variable, which is the most common way these leak.
	Apps         []string
	Environments []string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	Revoked      bool
	LastUsedAt   time.Time
	Description  string
}

func (t *Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
}

// CoversApp reports whether the token's scope allows an app.
func (t *Token) CoversApp(app string) bool { return covers(t.Apps, app) }

// CoversEnvironment reports whether the token's scope allows an environment.
func (t *Token) CoversEnvironment(env string) bool { return covers(t.Environments, env) }

func covers(list []string, want string) bool {
	if len(list) == 0 {
		return true // unscoped
	}
	for _, v := range list {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}

// Store holds tokens. In-memory here; the shape is what a table would be.
type Store struct {
	mu     sync.RWMutex
	tokens map[string]*Token // by hash
	now    func() time.Time
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Store{tokens: map[string]*Token{}, now: now}
}

// DefaultTTL is deliberately short. Section 12.2: "short-lived credentials
// wherever possible".
const DefaultTTL = 12 * time.Hour

// Issue mints a token and returns the plaintext exactly once.
func (s *Store) Issue(user, description string, ttl time.Duration, apps, envs []string) (plaintext string, t *Token, err error) {
	if user == "" {
		return "", nil, errors.New("auth: a token must belong to a user")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generating token: %w", err)
	}
	// The "orch_" prefix makes a leaked token greppable in a CI log and
	// recognisable to secret scanners.
	plaintext = "orch_" + base64.RawURLEncoding.EncodeToString(raw)

	now := s.now()
	t = &Token{
		ID:           hashToken(plaintext)[:12],
		Hash:         hashToken(plaintext),
		User:         user,
		Apps:         apps,
		Environments: envs,
		IssuedAt:     now,
		ExpiresAt:    now.Add(ttl),
		Description:  description,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[t.Hash] = t
	return plaintext, t, nil
}

// Add registers a pre-hashed token, for loading from persistent storage.
func (s *Store) Add(t *Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[t.Hash] = t
}

// Lookup resolves a plaintext token.
//
// The comparison is constant-time even though the lookup is by hash: hashing
// the presented token already removes the timing signal, and the extra check
// costs nothing and survives a future refactor that indexes differently.
func (s *Store) Lookup(plaintext string) (*Token, error) {
	if plaintext == "" {
		return nil, ErrNoToken
	}
	h := hashToken(plaintext)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) != 1 {
			continue
		}
		if t.Revoked {
			return nil, ErrRevoked
		}
		if t.Expired(s.now()) {
			return nil, fmt.Errorf("%w at %s", ErrExpired, t.ExpiresAt.Format(time.RFC3339))
		}
		t.LastUsedAt = s.now()
		return t, nil
	}
	return nil, ErrUnknownToken
}

// Revoke disables a token by id.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID == id {
			t.Revoked = true
			return nil
		}
	}
	return ErrUnknownToken
}

// List returns every token, without any plaintext.
func (s *Store) List() []*Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		cp := *t
		out = append(out, &cp)
	}
	return out
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// BearerToken extracts the credential from a request.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	// A header rather than a query parameter, always: query strings end up in
	// access logs, browser history and Referer headers.
	return ""
}

// Authorizer answers "may this identity do this?".
type Authorizer struct {
	Config *config.Config
}

// Can combines the token's scope with the user's RBAC grants.
//
// Both must allow it. A release-manager using a CI token scoped to staging
// cannot deploy to prod with it, which is the point of scoping: the token is
// a capability, not a login.
func (a *Authorizer) Can(t *Token, permission, app, env string) error {
	if t == nil {
		return ErrNoToken
	}
	if !t.CoversApp(app) {
		return fmt.Errorf("%w: this token is scoped to apps %s", ErrForbidden, strings.Join(t.Apps, ", "))
	}
	if !t.CoversEnvironment(env) {
		return fmt.Errorf("%w: this token is scoped to environments %s", ErrForbidden, strings.Join(t.Environments, ", "))
	}
	id, ok := a.Config.Identities[t.User]
	if !ok {
		return fmt.Errorf("%w: %q is not a configured user", ErrForbidden, t.User)
	}
	if !a.Config.Can(id, permission, env) {
		return fmt.Errorf("%w: %s does not have %s on %s", ErrForbidden, t.User, permission, env)
	}
	return nil
}
