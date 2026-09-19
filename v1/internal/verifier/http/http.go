// Package http is the liveness/readiness verifier (design.md section 7.1).
//
// The document is careful to distinguish this from verification:
//
//	"An HTTP 200 from /health tells you a process is up. It tells you nothing
//	about whether the new version introduced a 3% error rate, doubled p99
//	latency, or broke a code path that only 5% of requests hit. Rolling back
//	on liveness alone misses most real regressions and fires on transient
//	blips."
//
// So this verifier is honest about what it measures. It returns 1 or 0 for
// reachability and the request latency as a second observation, and the
// config loader refuses a canary whose criteria are all HTTP probes, because
// an unversioned endpoint cannot tell the canary apart from the fleet.
package http

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/verifier"
)

// Verifier probes an HTTP endpoint.
type Verifier struct {
	Client *http.Client
}

func New() *Verifier {
	return &Verifier{Client: &http.Client{Timeout: 10 * time.Second}}
}

func (v *Verifier) Name() string { return "http" }

// Validate checks the criterion's settings at config load time, so a typo'd
// URL is a CI failure rather than a run of Inconclusive verdicts nobody can
// explain.
func (v *Verifier) Validate(settings map[string]string) error {
	url := settings["url"]
	if url == "" {
		return fmt.Errorf("http verifier: `url` is required")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("http verifier: url %q must start with http:// or https://", url)
	}
	if s := settings["expect_status"]; s != "" {
		code, err := strconv.Atoi(s)
		if err != nil || code < 100 || code > 599 {
			return fmt.Errorf("http verifier: expect_status %q is not an HTTP status code", s)
		}
	}
	if m := settings["measure"]; m != "" && m != "status" && m != "latency_ms" {
		return fmt.Errorf("http verifier: measure must be `status` or `latency_ms`, got %q", m)
	}
	return nil
}

// Sample probes the endpoint once.
//
// A transport error is Inconclusive, not unhealthy: we could not reach the
// endpoint, which is a different claim from "the endpoint said no". The
// distinction is section 7.3's, and getting it wrong here would mean a DNS
// blip rolls back a good deploy.
func (v *Verifier) Sample(ctx context.Context, t verifier.Target, window time.Duration) (verifier.Sample, error) {
	url := expand(t.Settings["url"], t)
	if url == "" {
		return verifier.Sample{}, verifier.Inconclusive("http: no url configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return verifier.Sample{}, verifier.Inconclusive("http: building request: %v", err)
	}
	req.Header.Set("User-Agent", "orch-verifier/1")

	start := time.Now()
	resp, err := v.Client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return verifier.Sample{}, verifier.Inconclusive("http: %v", err)
	}
	defer resp.Body.Close()

	want := 200
	if s := t.Settings["expect_status"]; s != "" {
		want, _ = strconv.Atoi(s)
	}

	if t.Settings["measure"] == "latency_ms" {
		return verifier.Sample{
			Value: float64(elapsed.Milliseconds()),
			At:    time.Now().UTC(),
			Note:  fmt.Sprintf("%s responded %d in %s", url, resp.StatusCode, elapsed.Round(time.Millisecond)),
		}, nil
	}

	// 1 for "as expected", 0 for anything else. A higher-is-better criterion
	// with max: 1 turns that into a check.
	value := 0.0
	if resp.StatusCode == want {
		value = 1
	}
	return verifier.Sample{
		Value: value,
		At:    time.Now().UTC(),
		Note:  fmt.Sprintf("%s responded %d (wanted %d) in %s", url, resp.StatusCode, want, elapsed.Round(time.Millisecond)),
	}, nil
}

// expand substitutes the deployment's identifiers into a URL or query.
//
// $VERSION is the one that matters: section 9 warns that a canary measured on
// an untagged metric is measuring the whole fleet. Making the substitution
// available here is what lets a criterion be version-specific.
func expand(s string, t verifier.Target) string {
	r := strings.NewReplacer(
		"$VERSION", t.Version,
		"${VERSION}", t.Version,
		"$PREVIOUS", t.Previous,
		"${PREVIOUS}", t.Previous,
		"$APP", t.App,
		"${APP}", t.App,
		"$ENV", t.Environment,
		"${ENV}", t.Environment,
	)
	return r.Replace(s)
}

// Expand is exported for other verifiers that take a templated query.
func Expand(s string, t verifier.Target) string { return expand(s, t) }
