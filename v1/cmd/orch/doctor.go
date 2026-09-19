package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
)

// ----------------------------------------------------------- config validate

// cmdConfig works without a server on purpose. Section 17.3 lists
// `config validate` among the commands, and the moment it is most wanted is in
// a pre-commit hook or a CI job, where there is no orchd to ask.
func cmdConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: orch config validate [FILE]")
	}
	switch args[0] {
	case "validate":
		return cmdConfigValidate(args[1:])
	case "show":
		return cmdConfigShow(args[1:])
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

func cmdConfigValidate(args []string) error {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	output := fs.String("output", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	path := "orch.yaml"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}

	cfg, err := config.Load(path)
	if err != nil {
		if *output == "json" {
			_ = writeJSON(os.Stdout, map[string]any{"valid": false, "error": err.Error()})
			return errSilent
		}
		// The error already carries file, line and column, and often a "did
		// you mean". Printing it as-is beats wrapping it in anything.
		return err
	}

	summary := map[string]any{
		"valid":        true,
		"file":         cfg.SourceFile,
		"apps":         len(cfg.Apps),
		"roles":        len(cfg.Roles),
		"identities":   len(cfg.Identities),
		"environments": environmentNames(cfg),
	}
	if *output == "json" {
		return writeJSON(os.Stdout, summary)
	}

	fmt.Printf("%s %s\n\n", paint(green, "✓"), cfg.SourceFile)
	for _, appName := range cfg.AppNames {
		app := cfg.Apps[appName]
		fmt.Printf("  %s\n", paint(bold, appName))
		for _, envName := range app.EnvNames {
			env := app.Environments[envName]
			bits := []string{string(env.Strategy.Type), "via " + env.Provider}
			if env.PromoteFrom != "" {
				bits = append(bits, "promoted from "+env.PromoteFrom)
			}
			if n := len(env.Gates); n > 0 {
				bits = append(bits, fmt.Sprintf("%d gate(s)", n))
			}
			if n := len(env.Verify.Criteria); n > 0 {
				bits = append(bits, fmt.Sprintf("%d criteria, %s bake", n, env.Verify.Bake))
			} else {
				// Worth saying out loud. A deploy with no criteria cannot fail
				// verification, which means automatic rollback will never fire.
				bits = append(bits, paint(yellow, "no verification"))
			}
			fmt.Printf("    %-10s %s\n", envName, strings.Join(bits, ", "))
		}
	}
	fmt.Printf("\n  %d role(s), %d identity(ies)\n", len(cfg.Roles), len(cfg.Identities))
	return nil
}

func cmdConfigShow(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()
	var body map[string]any
	if err := c.do(ctx, "GET", "/v1/config", nil, &body, nil); err != nil {
		return explain(err)
	}
	return writeJSON(os.Stdout, body)
}

func environmentNames(cfg *config.Config) []string {
	seen := map[string]bool{}
	for _, app := range cfg.Apps {
		for name := range app.Environments {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// errSilent means the message has already been printed.
var errSilent = errors.New("")

// -------------------------------------------------------------- audit verify

func cmdAudit(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: orch audit verify | orch audit log")
	}
	switch args[0] {
	case "verify":
		return cmdAuditVerify(args[1:])
	case "log":
		return cmdAuditLog(args[1:])
	default:
		return fmt.Errorf("unknown audit subcommand %q", args[0])
	}
}

func cmdAuditVerify(args []string) error {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()

	var res map[string]any
	if err := c.do(ctx, "POST", "/v1/audit/verify", nil, &res, nil); err != nil {
		return explain(err)
	}
	if common.json() {
		return writeJSON(os.Stdout, res)
	}
	if ok, _ := res["valid"].(bool); ok {
		fmt.Printf("%s the audit chain verifies\n  head %s\n", paint(green, "✓"), res["head"])
		return nil
	}
	// This is the one output in the tool that should ruin someone's afternoon.
	return fmt.Errorf("%s\n  %v\n\n  Either the state file was edited or it is corrupt. Do not deploy\n"+
		"  until you know which. The break-glass procedure is in docs/break-glass.md.",
		paint(red+bold, "THE AUDIT CHAIN DOES NOT VERIFY"), res["error"])
}

func cmdAuditLog(args []string) error {
	fs := flag.NewFlagSet("audit log", flag.ContinueOnError)
	common := addCommon(fs)
	limit := fs.Int("limit", 50, "how many entries")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	c, err := common.client()
	if err != nil {
		return err
	}
	ctx, cancel := requestContext()
	defer cancel()

	var body struct {
		Entries []struct {
			At       time.Time         `json:"at"`
			Actor    string            `json:"actor"`
			Action   string            `json:"action"`
			Resource string            `json:"resource"`
			Detail   map[string]string `json:"detail"`
		} `json:"entries"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/v1/audit?limit=%d", *limit), nil, &body, nil); err != nil {
		return explain(err)
	}
	if common.json() {
		return writeJSON(os.Stdout, body)
	}

	t := newTable("WHEN", "ACTOR", "ACTION", "RESOURCE", "DETAIL")
	for _, e := range body.Entries {
		action := e.Action
		if strings.HasSuffix(action, ".denied") || strings.Contains(action, "override") ||
			strings.Contains(action, "forced") {
			action = paint(yellow, action)
		}
		t.add(e.At.Format(time.RFC3339), e.Actor, action, e.Resource, flatten(e.Detail))
	}
	t.write(os.Stdout)
	return nil
}

func flatten(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

// ------------------------------------------------------------------- doctor

// cmdDoctor checks the things that are wrong when someone says "it does not
// work", in the order they are usually wrong.
//
// It keeps going after a failure rather than stopping at the first, because
// the useful output is the whole list: knowing that the server is unreachable
// *and* no token is set saves a second run.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	common := addCommon(fs)
	configPath := fs.String("config", "orch.yaml", "config file to check, if there is one here")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
		Fix    string `json:"fix,omitempty"`
	}
	var checks []check
	add := func(name string, ok bool, detail, fix string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail, Fix: fix})
	}

	add("client version", true, version, "")

	url := serverURL(common.server)
	token := apiToken(common.token)

	if token == "" {
		add("api token", false, "not set",
			"export ORCH_TOKEN=... (mint one with: orchd token issue --user you)")
	} else {
		add("api token", true, fmt.Sprintf("set (%d characters)", len(token)), "")
	}

	c := newClient(url, token)
	ctx, cancel := requestContext()
	var health map[string]any
	err := c.do(ctx, "GET", "/v1/health", nil, &health, nil)
	cancel()
	if err != nil {
		add("server", false, fmt.Sprintf("%s is not answering", url),
			"start it with: orchd serve, or set ORCH_SERVER")
	} else {
		add("server", true, fmt.Sprintf("%s is up", url), "")

		// Only worth trying if the server answered at all.
		if token != "" {
			ctx, cancel := requestContext()
			var body struct {
				Status []any `json:"status"`
			}
			err := c.do(ctx, "GET", "/v1/status", nil, &body, nil)
			cancel()
			if err != nil {
				add("authentication", false, err.Error(),
					"mint a new token with: orchd token issue --user you")
			} else {
				add("authentication", true, fmt.Sprintf("accepted; %d target(s) visible", len(body.Status)), "")
			}
		}
	}

	if _, err := os.Stat(*configPath); err == nil {
		if _, err := config.Load(*configPath); err != nil {
			add("config", false, err.Error(), "fix the file, or run: orch config validate "+*configPath)
		} else {
			add("config", true, *configPath+" parses", "")
		}
	}

	if common.json() {
		if err := writeJSON(os.Stdout, map[string]any{"checks": checks}); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			mark := paint(green, "✓")
			if !c.OK {
				mark = paint(red, "✗")
			}
			fmt.Printf("%s %-16s %s\n", mark, c.Name, c.Detail)
			if !c.OK && c.Fix != "" {
				fmt.Printf("  %s %s\n", paint(dim, "try:"), c.Fix)
			}
		}
	}

	for _, c := range checks {
		if !c.OK {
			// Non-zero, so `orch doctor` is usable as a readiness check in a
			// setup script.
			return errSilent
		}
	}
	return nil
}
