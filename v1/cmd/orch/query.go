package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/syedjafri06193/orch/internal/api"
	"github.com/syedjafri06193/orch/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	common := addCommon(fs)
	app := fs.String("app", "", "only this application")
	env := fs.String("env", "", "only this environment")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	q := url.Values{}
	if *app != "" {
		q.Set("app", *app)
	}
	if *env != "" {
		q.Set("environment", *env)
	}

	ctx, cancel := requestContext()
	defer cancel()
	var body struct {
		Status []api.Status `json:"status"`
	}
	if err := c.do(ctx, "GET", "/v1/status?"+q.Encode(), nil, &body, nil); err != nil {
		return explain(err)
	}

	if common.json() {
		return writeJSON(os.Stdout, body)
	}
	if len(body.Status) == 0 {
		fmt.Println("nothing configured")
		return nil
	}

	now := time.Now().UTC()
	t := newTable("APP", "ENV", "VERSION", "STATE", "WHEN", "")
	for _, s := range body.Status {
		note := ""
		switch {
		case s.Frozen:
			note = paint(yellow, "frozen: "+s.FreezeReason)
		case s.Active:
			note = paint(blue, "in progress")
		case s.Error != "":
			note = paint(red, s.Error)
		}
		when := "-"
		if s.Since != nil {
			when = humanAgo(*s.Since, now)
		}
		t.add(s.App, s.Environment, orDash(s.Version), renderState(s.State), when, note)
	}
	t.write(os.Stdout)
	return nil
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	common := addCommon(fs)
	var (
		app   = fs.String("app", "", "only this application")
		env   = fs.String("env", "", "only this environment")
		limit = fs.Int("limit", 20, "how many deployments to show")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	q := url.Values{}
	if *app != "" {
		q.Set("app", *app)
	}
	if *env != "" {
		q.Set("environment", *env)
	}
	q.Set("limit", strconv.Itoa(*limit))

	ctx, cancel := requestContext()
	defer cancel()
	var body struct {
		Deployments []*store.Deployment `json:"deployments"`
	}
	if err := c.do(ctx, "GET", "/v1/deployments?"+q.Encode(), nil, &body, nil); err != nil {
		return explain(err)
	}

	if common.json() {
		return writeJSON(os.Stdout, body)
	}
	if len(body.Deployments) == 0 {
		fmt.Println("no deployments yet")
		return nil
	}

	now := time.Now().UTC()
	t := newTable("ID", "APP", "ENV", "VERSION", "STATE", "BY", "STARTED", "TOOK")
	for _, d := range body.Deployments {
		took := paint(dim, "running")
		if d.FinishedAt != nil {
			took = humanDuration(d.FinishedAt.Sub(d.CreatedAt))
		}
		version := d.Version
		if d.IsRollback {
			// Marked, because "why did v1.2.2 deploy after v1.2.3" is the
			// question this column exists to answer.
			version += paint(yellow, " (rollback)")
		}
		t.add(shortID(d.ID), d.App, d.Environment, version,
			renderState(d.State), d.TriggeredBy, humanAgo(d.CreatedAt, now), took)
	}
	t.write(os.Stdout)
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		return errors.New("usage: orch logs <deployment-id>")
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()
	out, err := c.text(ctx, "/v1/deployments/"+fs.Arg(0)+"/logs")
	if err != nil {
		return explain(err)
	}
	fmt.Print(out)
	return nil
}

func cmdAbort(args []string) error {
	fs := flag.NewFlagSet("abort", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		return errors.New("usage: orch abort <deployment-id>")
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()

	var dep store.Deployment
	if err := c.do(ctx, "POST", "/v1/deployments/"+fs.Arg(0)+"/abort", nil, &dep, nil); err != nil {
		return explain(err)
	}
	if common.json() {
		return writeJSON(os.Stdout, dep)
	}
	// Deliberately not claiming the environment is now in a known state. An
	// abort stops the orchestrator; whether the provider finished placing
	// instances first is a question only the provider can answer.
	fmt.Printf("%s aborted; %s is %s\n", shortID(dep.ID), dep.Environment, renderState(dep.State))
	fmt.Printf("  Check what is actually running: orch status --app %s --env %s\n", dep.App, dep.Environment)
	return nil
}

func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		return errors.New("usage: orch approve <deployment-id>")
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()

	var res map[string]any
	if err := c.do(ctx, "POST", "/v1/deployments/"+fs.Arg(0)+"/approve", nil, &res,
		map[string]string{"Idempotency-Key": "approve-" + fs.Arg(0)}); err != nil {
		return explain(err)
	}
	if common.json() {
		return writeJSON(os.Stdout, res)
	}
	fmt.Printf("%s approved by %v\n", paint(green, "✓"), res["by"])
	return nil
}

func cmdFreeze(args []string, lift bool) error {
	name := "freeze"
	if lift {
		name = "unfreeze"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	common := addCommon(fs)
	var (
		app    = fs.String("app", "", "application")
		env    = fs.String("env", "", "environment")
		reason = fs.String("reason", "", "why (required for a freeze)")
		until  = fs.String("until", "", "RFC3339 time to lift automatically")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *app == "" || *env == "" {
		return errors.New("--app and --env are required")
	}
	if !lift && *reason == "" {
		// The server refuses this too. Catching it here saves a round trip and
		// says the same thing: a freeze nobody can explain is a freeze someone
		// lifts at the worst moment.
		return errors.New("--reason is required: a freeze nobody can explain is a freeze someone lifts")
	}

	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()

	var res map[string]any
	err = c.do(ctx, "POST", "/v1/freezes", api.FreezeRequest{
		App: *app, Environment: *env, Reason: *reason, Until: *until, Lift: lift,
	}, &res, map[string]string{"Idempotency-Key": name + "-" + *app + "-" + *env})
	if err != nil {
		return explain(err)
	}
	if common.json() {
		return writeJSON(os.Stdout, res)
	}
	if lift {
		fmt.Printf("%s/%s unfrozen\n", *app, *env)
	} else {
		fmt.Printf("%s/%s frozen: %s\n", *app, *env, *reason)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
