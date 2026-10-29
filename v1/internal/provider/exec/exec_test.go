package exec

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/orch/internal/provider"
)

func target(cfg map[string]string) provider.Target {
	return provider.Target{App: "web", Environment: "prod", Provider: "exec", Config: cfg}
}

func TestValidate(t *testing.T) {
	p := New()
	cases := []struct {
		name    string
		cfg     map[string]string
		wantErr string
	}{
		{"ok", map[string]string{"deploy": "true", "current": "echo v1"}, ""},
		{"missing deploy", map[string]string{"current": "echo v1"}, "`deploy` command is required"},
		{"missing current", map[string]string{"deploy": "true"}, "`current` command is required"},
		{"current braces", map[string]string{"deploy": "true", "current": "echo ${VERSION}"}, "must not take a version"},
		{"current bare", map[string]string{"deploy": "true", "current": "echo $VERSION"}, "must not take a version"},
		{"current env", map[string]string{"deploy": "true", "current": "echo $ORCH_VERSION"}, "must not take a version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := p.Validate(target(c.cfg))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestCurrent(t *testing.T) {
	p := New()
	v, err := p.Current(context.Background(), target(map[string]string{"current": "echo '  v42 '"}))
	if err != nil || v.ID != "v42" || v.Unknown {
		t.Fatalf("got %+v, %v", v, err)
	}
	v, err = p.Current(context.Background(), target(map[string]string{"current": "true"}))
	if err != nil || !v.Unknown {
		t.Fatalf("empty output should be Unknown, got %+v, %v", v, err)
	}
}

func TestDeployPassesEnvironment(t *testing.T) {
	p := New()
	var out bytes.Buffer
	res, err := p.Deploy(context.Background(), provider.DeployRequest{
		Target: target(map[string]string{
			"deploy":  `echo "$ORCH_APP $ORCH_ENVIRONMENT $ORCH_VERSION $ORCH_FENCE $ORCH_IS_ROLLBACK $ORCH_CFG_REGION"`,
			"current": "echo x",
			"region":  "us-west-2",
		}),
		Version:    "v2",
		Fence:      7,
		IsRollback: true,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out.String()), "web prod v2 7 true us-west-2"; got != want {
		t.Fatalf("env: got %q, want %q", got, want)
	}
	if res.Handle != "exec:app:web/env:prod:v2" {
		t.Fatalf("handle: %q", res.Handle)
	}
}

func TestStderrIsCapturedAndExitCodeReported(t *testing.T) {
	p := New()
	var out bytes.Buffer
	_, err := p.Deploy(context.Background(), provider.DeployRequest{
		Target:  target(map[string]string{"deploy": "echo boom >&2; exit 3", "current": "echo x"}),
		Version: "v1",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Fatalf("stderr not captured: %q", out.String())
	}
}

func TestShiftAndAbortUnsupportedWithoutCommand(t *testing.T) {
	p := New()
	tg := target(map[string]string{"deploy": "true", "current": "echo x"})
	if err := p.Shift(context.Background(), tg, 50); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("shift: %v", err)
	}
	if err := p.Abort(context.Background(), tg, "h"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("abort: %v", err)
	}
}

// A script that leaves a background child holding stdout open used to make
// Run block until that child exited, so the timeout never fired.
func TestTimeoutKillsBackgroundChildren(t *testing.T) {
	p := &Provider{Timeout: 300 * time.Millisecond}
	var out bytes.Buffer
	start := time.Now()
	_, err := p.Deploy(context.Background(), provider.DeployRequest{
		Target:  target(map[string]string{"deploy": "sleep 30 & sleep 30", "current": "echo x"}),
		Version: "v1",
	}, &out)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s; background child kept Run blocked", elapsed)
	}
}
