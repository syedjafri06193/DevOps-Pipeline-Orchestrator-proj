// Package slack implements the Slack integration (design.md section 10).
//
// Section 16 calls M8 "the security milestone", and this file is why:
//
//	"Unverified Slack webhook handlers are a straightforward path to 'anyone
//	on the internet can deploy to your production.'"
//
// Socket Mode is the default because it needs no public endpoint at all
// (section 10.1), and the dialling of that WebSocket is the one piece of this
// package that needs the network. Everything that decides anything --
// signature verification, identity mapping, authorization, message building,
// rate-limit coalescing -- is here and is tested, because that is where the
// security properties live.
package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ReplayWindow is how old a signed request may be.
//
// Slack's own guidance. Without it, a captured request can be replayed
// forever, and a captured "approve" is a production deploy.
const ReplayWindow = 5 * time.Minute

var (
	ErrNoSignature  = errors.New("slack: request is not signed")
	ErrBadTimestamp = errors.New("slack: request timestamp is not a number")
	ErrStaleRequest = errors.New("slack: request is outside the replay window")
	ErrBadSignature = errors.New("slack: signature does not match")
	ErrNoSigningKey = errors.New("slack: no signing secret is configured")
)

// VerifySignature checks an inbound Slack HTTP request (section 10.2).
//
// Three details, each of which is a real vulnerability if missed:
//
//   - `body` must be the RAW bytes, read before any JSON decoding. The
//     signature covers exact bytes, and a decode-then-re-encode round trip
//     will not match. The caller reads the body once and passes it here.
//   - The replay window is checked, not just the signature. A valid signature
//     stays valid forever.
//   - The comparison is constant-time. A naive `==` short-circuits on the
//     first differing byte and is a timing oracle that lets an attacker
//     recover the expected signature byte by byte.
func VerifySignature(signingSecret, signature, timestamp string, body []byte, now time.Time) error {
	if signingSecret == "" {
		return ErrNoSigningKey
	}
	if signature == "" || timestamp == "" {
		return ErrNoSignature
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBadTimestamp, timestamp)
	}

	// Replay protection. Absolute difference, so a clock skewed the other way
	// is caught too -- a request from the future is as suspicious as one from
	// last week (section 19.4).
	age := now.Unix() - ts
	if age < 0 {
		age = -age
	}
	if time.Duration(age)*time.Second > ReplayWindow {
		return fmt.Errorf("%w: %ds old", ErrStaleRequest, age)
	}

	base := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	// Constant-time comparison.
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return ErrBadSignature
	}
	return nil
}

// Sign produces the signature Slack would send. Used by the tests, and by
// anyone building a request replayer for debugging.
func Sign(signingSecret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte("v0:" + timestamp + ":" + string(body)))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}
