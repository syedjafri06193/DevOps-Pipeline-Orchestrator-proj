// Package exec shells out to a script (design.md section 16's M1).
//
// The escape hatch that makes the tool useful before a first-class provider
// exists for your platform, and the one most likely to be reached for in a
// hurry. Three things it does that a naive `exec.Command` wrapper does not:
//
//   - It passes the fence token to the script, so a script that takes its own
//     lock can refuse a stale caller (section 6.2).
//   - It reads `current` from a separate command, because the whole
//     reconciliation design rests on being able to ask the environment what is
//     running rather than trusting the database.
//   - It treats a missing `current` command as a configuration error rather
//     than defaulting to "unknown", because a provider that can never say what
//     is deployed makes every crash unrecoverable.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/provider"
)

// Provider runs configured shell commands.
type Provider struct {
	// Timeout bounds a single command. Zero means 10 minutes.
	Timeout time.Duration
	// Shell is the interpreter. Defaults to "sh".
	Shell string
}

func New() *Provider { return &Provider{} }

func (p *Provider) Name() string { return "exec" }

func (p *Provider) Capabilities() provider.Capabilities {
	// A shell script has no traffic control unless the operator wires one up,
	// and claiming otherwise would let a canary configuration through
	// preflight and fail at step three.
	return provider.Capabilities{TrafficShifting: false, Abort: true}
}

func (p *Provider) Validate(t provider.Target) error {
	if t.Config["deploy"] == "" {
		return fmt.Errorf("exec provider for %s: `deploy` command is required", t)
	}
	if t.Config["current"] == "" {
		// Not optional. Without it, reconciliation cannot ask the environment
		// what is running, and every crash ends in UNKNOWN.
		return fmt.Errorf(
			"exec provider for %s: `current` command is required.\n"+
				"  It must print the currently deployed version to stdout.\n"+
				"  Without it, a crashed deploy cannot be reconciled and will always need a human", t)
	}
	for _, key := range []string{"deploy", "current", "shift", "abort"} {
		if cmd := t.Config[key]; strings.Contains(cmd, "${VERSION}") && key == "current" {
			return fmt.Errorf("exec provider for %s: the `current` command must not take a version; it reports what is deployed", t)
		}
	}
	return nil
}

func (p *Provider) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 10 * time.Minute
}

func (p *Provider) shell() string {
	if p.Shell != "" {
		return p.Shell
	}
	return "sh"
}

func (p *Provider) Current(ctx context.Context, t provider.Target) (provider.Version, error) {
	cmd := t.Config["current"]
	if cmd == "" {
		return provider.Version{}, fmt.Errorf("exec: no `current` command configured for %s", t)
	}
	var out bytes.Buffer
	if err := p.run(ctx, t, cmd, nil, &out); err != nil {
		return provider.Version{}, err
	}
	version := strings.TrimSpace(out.String())
	if version == "" {
		// An empty answer is not "nothing is deployed" -- it is a script that
		// did not answer. Saying Unknown is what stops the engine guessing.
		return provider.Version{Unknown: true}, nil
	}
	return provider.Version{ID: version}, nil
}

func (p *Provider) Deploy(ctx context.Context, req provider.DeployRequest, out io.Writer) (provider.DeployResult, error) {
	cmd := req.Target.Config["deploy"]
	if cmd == "" {
		return provider.DeployResult{}, fmt.Errorf("exec: no `deploy` command configured for %s", req.Target)
	}

	env := map[string]string{
		"ORCH_APP":             req.Target.App,
		"ORCH_ENVIRONMENT":     req.Target.Environment,
		"ORCH_VERSION":         req.Version,
		"ORCH_PREVIOUS":        req.Previous,
		"ORCH_STRATEGY":        req.Strategy,
		"ORCH_BATCH_SIZE":      req.BatchSize,
		"ORCH_IDEMPOTENCY_KEY": req.IdempotencyKey,
		// The fence token, so a script holding its own lock can refuse a
		// resumed zombie. A script that ignores it is no worse off than one
		// that was never given it.
		"ORCH_FENCE":       strconv.FormatInt(req.Fence, 10),
		"ORCH_IS_ROLLBACK": strconv.FormatBool(req.IsRollback),
	}

	if err := p.run(ctx, req.Target, cmd, env, out); err != nil {
		return provider.DeployResult{}, err
	}
	return provider.DeployResult{
		Handle: fmt.Sprintf("exec:%s:%s", req.Target.Resource(), req.Version),
	}, nil
}

func (p *Provider) Shift(ctx context.Context, t provider.Target, weight int) error {
	cmd := t.Config["shift"]
	if cmd == "" {
		return provider.ErrUnsupported
	}
	return p.run(ctx, t, cmd, map[string]string{
		"ORCH_WEIGHT": strconv.Itoa(weight),
	}, io.Discard)
}

func (p *Provider) Abort(ctx context.Context, t provider.Target, handle string) error {
	cmd := t.Config["abort"]
	if cmd == "" {
		return provider.ErrUnsupported
	}
	return p.run(ctx, t, cmd, map[string]string{"ORCH_HANDLE": handle}, io.Discard)
}

func (p *Provider) run(ctx context.Context, t provider.Target, script string,
	extra map[string]string, out io.Writer) error {

	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, p.shell(), "-c", script)
	cmd.Env = append(os.Environ(),
		"ORCH_APP="+t.App,
		"ORCH_ENVIRONMENT="+t.Environment,
	)
	for k, v := range t.Config {
		if k == "deploy" || k == "current" || k == "shift" || k == "abort" {
			continue
		}
		cmd.Env = append(cmd.Env, "ORCH_CFG_"+strings.ToUpper(k)+"="+v)
	}
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// stderr goes to the same writer as stdout: it is the half of a failing
	// script's output that explains the failure, and splitting them means the
	// deploy log has the "ok" lines and not the error.
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("exec: command timed out after %s: %s", p.timeout(), firstLine(script))
		}
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			return fmt.Errorf("exec: command exited %d: %s", exitErr.ExitCode(), firstLine(script))
		}
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " ..."
	}
	return strings.TrimSpace(s)
}
