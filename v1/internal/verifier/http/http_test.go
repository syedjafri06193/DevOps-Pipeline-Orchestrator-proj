package http

import (
	"context"
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/verifier"
)

func TestValidate(t *testing.T) {
	v := New()
	cases := []struct {
		settings map[string]string
		wantErr  string
	}{
		{map[string]string{"url": "https://x/health"}, ""},
		{map[string]string{}, "`url` is required"},
		{map[string]string{"url": "x/health"}, "must start with"},
		{map[string]string{"url": "http://x", "expect_status": "abc"}, "not an HTTP status"},
		{map[string]string{"url": "http://x", "expect_status": "700"}, "not an HTTP status"},
		{map[string]string{"url": "http://x", "measure": "bogus"}, "measure must be"},
		{map[string]string{"url": "http://x", "measure": "latency_ms"}, ""},
	}
	for _, c := range cases {
		err := v.Validate(c.settings)
		if c.wantErr == "" && err != nil {
			t.Errorf("%v: unexpected %v", c.settings, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%v: got %v, want %q", c.settings, err, c.wantErr)
		}
	}
}

func TestSampleStatusAndVersionExpansion(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "bad") {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	v := New()
	tg := verifier.Target{App: "web", Version: "v9", Settings: map[string]string{"url": srv.URL + "/$APP/${VERSION}"}}
	s, err := v.Sample(context.Background(), tg, time.Second)
	if err != nil || s.Value != 1 {
		t.Fatalf("got %+v, %v", s, err)
	}
	if gotPath != "/web/v9" {
		t.Fatalf("expansion: %q", gotPath)
	}

	tg.Settings["url"] = srv.URL + "/bad"
	s, err = v.Sample(context.Background(), tg, time.Second)
	if err != nil || s.Value != 0 {
		t.Fatalf("unhealthy: got %+v, %v", s, err)
	}

	tg.Settings["expect_status"] = "503"
	s, _ = v.Sample(context.Background(), tg, time.Second)
	if s.Value != 1 {
		t.Fatalf("expect_status: got %+v", s)
	}
}

func TestTransportErrorIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(nethttp.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := New().Sample(context.Background(), verifier.Target{Settings: map[string]string{"url": url}}, time.Second)
	if !errors.Is(err, verifier.ErrInconclusive) {
		t.Fatalf("got %v, want ErrInconclusive", err)
	}
}

func TestLatencyMeasure(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		time.Sleep(20 * time.Millisecond)
	}))
	defer srv.Close()
	s, err := New().Sample(context.Background(), verifier.Target{Settings: map[string]string{
		"url": srv.URL, "measure": "latency_ms"}}, time.Second)
	if err != nil || s.Value < 15 {
		t.Fatalf("got %+v, %v", s, err)
	}
}
