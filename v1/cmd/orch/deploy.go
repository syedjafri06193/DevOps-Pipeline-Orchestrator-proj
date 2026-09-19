package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/syedjafri06193/orch/internal/api"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
)

// commonFlags are the three every command takes.
type commonFlags struct {
	server string
	token  string
	output string
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.server, "server", "", "orchd's address (or $ORCH_SERVER)")
	fs.StringVar(&c.token, "token", "", "API token (or $ORCH_TOKEN / $ORCH_TOKEN_FILE)")
	fs.StringVar(&c.output, "output", "text", "output format: text or json")
	return c
}

func (c *commonFlags) client() (*client, error) {
	tok := apiToken(c.token)
	if tok == "" {
		return nil, errNoToken
	}
	return newClient(serverURL(c.server), tok), nil
}

func (c *commonFlags) json() bool { return c.output == "json" }

// ------------------------------------------------------------------ deploy

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	common := addCommon(fs)
	var (
		app      = fs.String("app", "", "application")
		env      = fs.String("env", "", "environment")
		ver      = fs.String("version", "", "artifact version")
		strategy = fs.String("strategy", "", "override the configured strategy")
		wait     = fs.Bool("wait", false, "block until the deployment reaches a terminal state")
		dryRun   = fs.Bool("dry-run", false, "print the plan without deploying anything")
		force    = fs.Bool("force", false, "bypass the rollback circuit breaker (requires deploy:override)")
		idem     = fs.String("idempotency-key", "", "reuse a key to retry safely (default: generated)")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *app == "" || *env == "" || *ver == "" {
		return errors.New("--app, --env and --version are all required")
	}

	return submit(common, api.CreateDeploymentRequest{
		App: *app, Environment: *env, Version: *ver,
		Strategy: *strategy, DryRun: *dryRun, Force: *force,
	}, *idem, *wait)
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	common := addCommon(fs)
	var (
		app    = fs.String("app", "", "application")
		env    = fs.String("env", "", "environment")
		to     = fs.String("to", "", "roll back to this version instead of the last good one")
		wait   = fs.Bool("wait", false, "block until the rollback finishes")
		dryRun = fs.Bool("dry-run", false, "print what would be rolled back to, and whether it is allowed")
		force  = fs.Bool("force", false, "override a refusal (requires deploy:override; audited by name)")
		idem   = fs.String("idempotency-key", "", "reuse a key to retry safely (default: generated)")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *app == "" || *env == "" {
		return errors.New("--app and --env are required")
	}

	return submit(common, api.CreateDeploymentRequest{
		App: *app, Environment: *env, Version: *to,
		Rollback: true, DryRun: *dryRun, Force: *force,
	}, *idem, *wait)
}

func cmdPromote(args []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	common := addCommon(fs)
	var (
		app    = fs.String("app", "", "application")
		env    = fs.String("env", "", "environment to promote into")
		wait   = fs.Bool("wait", false, "block until the promotion finishes")
		dryRun = fs.Bool("dry-run", false, "print the plan without deploying")
		idem   = fs.String("idempotency-key", "", "reuse a key to retry safely (default: generated)")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *app == "" || *env == "" {
		return errors.New("--app and --env are required")
	}

	return submit(common, api.CreateDeploymentRequest{
		App: *app, Environment: *env, Promote: true, DryRun: *dryRun,
	}, *idem, *wait)
}

// waitLimit bounds --wait. A pipeline that blocks forever on a stuck deploy
// holds a CI runner until someone notices, which is usually the next morning.
var waitLimit = 60 * time.Minute

var waitLimitDescription = "1h"

// submit is the shared path for deploy, rollback and promote.
func submit(common *commonFlags, req api.CreateDeploymentRequest, idem string, wait bool) error {
	c, err := common.client()
	if err != nil {
		return err
	}

	// One key per logical deployment, generated here and reused across
	// retries. Section 6.3: the retry is the whole point, and a key generated
	// per HTTP attempt would defeat it.
	if idem == "" {
		idem = ulid.New().String()
	}

	ctx, cancel := requestContext()
	defer cancel()

	if req.DryRun {
		var plan map[string]any
		if err := c.do(ctx, "POST", "/v1/deployments", req, &plan,
			map[string]string{"Idempotency-Key": idem}); err != nil {
			return explain(err)
		}
		if common.json() {
			return writeJSON(os.Stdout, plan)
		}
		printPlan(plan)
		return nil
	}

	var dep store.Deployment
	if err := c.do(ctx, "POST", "/v1/deployments", req, &dep,
		map[string]string{"Idempotency-Key": idem}); err != nil {
		return explain(err)
	}

	if common.json() && !wait {
		return writeJSON(os.Stdout, dep)
	}
	if !common.json() {
		verb := "deploying"
		if dep.IsRollback {
			verb = "rolling back to"
		}
		fmt.Printf("%s %s %s/%s %s\n", paint(bold, shortID(dep.ID)),
			verb, dep.App, dep.Environment, paint(bold, dep.Version))
		if !wait {
			fmt.Printf("  watch it: orch logs %s   (or add --wait)\n", dep.ID)
			return nil
		}
	}

	// Ctrl-C stops watching, not the deployment: the server owns it, and
	// killing the CLI mid-bake must not leave production half-deployed.
	waitCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	waitCtx, cancelWait := context.WithTimeout(waitCtx, waitLimit)
	defer cancelWait()

	final, err := waitFor(waitCtx, c, dep.ID, common)
	if err != nil {
		return err
	}
	if common.json() {
		return writeJSON(os.Stdout, final)
	}
	return exitFor(final)
}

// deploymentResponse is the shape of GET /v1/deployments/{id}.
//
// The deployment is nested, because the page that needs it also needs the
// steps, the samples and the approvals, and four round trips to render one
// screen is three too many.
type deploymentResponse struct {
	Deployment *store.Deployment `json:"deployment"`
	Steps      []*store.Step     `json:"steps"`
	Approvals  []*store.Approval `json:"approvals"`
}

// waitFor polls until the deployment is terminal.
//
// Polling rather than the dashboard's SSE stream: this is a CLI that may be
// running in a pipeline behind a proxy that mangles streaming responses, and a
// deploy that appears to hang because of a proxy is worse than one that prints
// a line every two seconds.
func waitFor(ctx context.Context, c *client, id string, common *commonFlags) (*store.Deployment, error) {
	var lastState store.State
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var res deploymentResponse
		err := c.do(reqCtx, "GET", "/v1/deployments/"+id, nil, &res, nil)
		cancel()
		if err != nil {
			return nil, explain(err)
		}
		dep := res.Deployment

		// A reply that carries no state is a protocol mismatch -- an older or
		// newer server, or a proxy returning something else entirely. Without
		// this check the loop below polls forever, printing nothing, which is
		// indistinguishable from a deployment that is simply taking a while.
		// Failing loudly here is the difference between a five-second fix and
		// a hung pipeline nobody can explain.
		if dep == nil || dep.State == "" {
			return nil, fmt.Errorf("the server's reply for deployment %s carried no state; "+
				"this orch may be too old or too new for this orchd (client %s)", id, version)
		}

		if dep.State != lastState {
			if !common.json() {
				fmt.Printf("  %-16s %s\n", renderState(dep.State), paint(dim, describeState(dep.State)))
			}
			lastState = dep.State
		}
		if dep.State.IsTerminal() {
			return dep, nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			// The deployment keeps going; only the watching stopped. Saying so
			// matters, because "my deploy timed out" and "my deploy was
			// cancelled" call for opposite next actions.
			return nil, fmt.Errorf("stopped waiting after %s, but %s is still %s.\n"+
				"  The deployment is still running. Watch it with: orch logs %s",
				waitLimitDescription, id, dep.State, id)
		}
	}
}

// exitFor turns a terminal state into an exit status.
//
// A rolled-back deployment exits non-zero. It is the single most important
// line in this file: a CI pipeline that treats "we deployed, it was bad, we
// put it back" as success will merrily promote the same artifact onward.
func exitFor(dep *store.Deployment) error {
	switch dep.State {
	case store.StateSucceeded:
		fmt.Printf("\n%s %s is live in %s/%s\n",
			paint(green, "✓"), dep.Version, dep.App, dep.Environment)
		return nil
	case store.StateRolledBack:
		return fmt.Errorf("%s failed verification and was rolled back\n  reason: %s",
			dep.Version, orUnknown(dep.Error))
	case store.StateRollbackFailed:
		return fmt.Errorf("%s\n  %s is in an UNKNOWN state and the rollback also failed.\n"+
			"  reason: %s\n  Someone needs to look at this now: orch logs %s",
			paint(red+bold, "ROLLBACK FAILED"), dep.Environment, orUnknown(dep.Error), dep.ID)
	case store.StateUnknown:
		return fmt.Errorf("the state of %s/%s could not be determined\n  reason: %s\n"+
			"  Check the provider by hand before deploying again.",
			dep.App, dep.Environment, orUnknown(dep.Error))
	case store.StateAborted:
		return fmt.Errorf("deployment aborted")
	default:
		return fmt.Errorf("deployment failed: %s", orUnknown(dep.Error))
	}
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not recorded"
	}
	return s
}

func describeState(st store.State) string {
	switch st {
	case store.StatePending:
		return "queued"
	case store.StatePreflight:
		return "checking the target, the artifact and the freeze windows"
	case store.StateAwaitApproval:
		return "waiting for someone to approve it"
	case store.StateDeploying:
		return "the provider is placing the new version"
	case store.StateVerifying:
		return "baking; watching the health criteria"
	case store.StatePromoting:
		return "shifting traffic"
	case store.StateRollingBack:
		return "putting the previous version back"
	default:
		return ""
	}
}

func printPlan(plan map[string]any) {
	fmt.Printf("%s\n\n", paint(bold, "this is a dry run; nothing was created"))
	order := []string{"app", "environment", "version", "strategy", "provider", "promote_from", "is_rollback"}
	for _, k := range order {
		if v, ok := plan[k]; ok && v != nil && v != "" && v != false {
			fmt.Printf("  %-14s %v\n", k, v)
		}
	}
	if gates, ok := plan["gates"].([]any); ok && len(gates) > 0 {
		fmt.Printf("\n  %s\n", paint(bold, "gates"))
		for _, g := range gates {
			m, ok := g.(map[string]any)
			if !ok {
				continue
			}
			fmt.Printf("    - %v", m["type"])
			if roles, ok := m["roles"]; ok && roles != nil {
				fmt.Printf(" by %v", roles)
			}
			fmt.Println()
		}
	}
	if v, ok := plan["verify"].(map[string]any); ok && len(v) > 0 {
		fmt.Printf("\n  %s\n", paint(bold, "verification"))
		for _, k := range []string{"bake", "sample_interval", "min_samples"} {
			if x, ok := v[k]; ok {
				fmt.Printf("    %-16s %v\n", k, x)
			}
		}
		if crit, ok := v["criteria"].([]any); ok {
			for _, c := range crit {
				fmt.Printf("    criterion        %v\n", c)
			}
		}
	}
}

// explain adds the next step to a server error.
//
// Section 17.4: an error that says only what failed leaves the person to guess
// what to do, which during an incident is the expensive kind of guessing.
func explain(err error) error {
	var e *apiError
	if !errors.As(err, &e) {
		return err
	}
	switch e.Status {
	case 401:
		return fmt.Errorf("%s\n  Mint a new token with: orchd token issue --user you", e.Message)
	case 403:
		return fmt.Errorf("%s\n  Check your roles on the config page, or ask someone who has the permission.", e.Message)
	case 409:
		// The refusals (an unsafe rollback, the circuit breaker, a freeze)
		// already carry their own remedy from the server, so this adds nothing
		// and would only bury it.
		return errors.New(e.Message)
	case 404:
		return fmt.Errorf("%s\n  Run `orch status` to see the configured apps and environments.", e.Message)
	default:
		return errors.New(e.Message)
	}
}
