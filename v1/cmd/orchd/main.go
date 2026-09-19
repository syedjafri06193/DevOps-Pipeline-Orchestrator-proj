// Command orchd is the orchestrator server (design.md section 3, option B).
//
//	"One binary, one SQLite file, one systemd unit. Deployment is: copy one
//	file, run one systemd unit. That simplicity is a feature and should be in
//	the README's first paragraph."
//
// Everything is here: the HTTP API the CLI talks to, the embedded dashboard,
// the deployment executor, the outbox worker and the metrics endpoint. Section
// 3 argues for a central server over a stateless CLI because deployments need
// leases, approvals need somewhere to arrive, and a ten-minute bake needs
// something that survives the operator closing their laptop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/syedjafri06193/orch/internal/api"
	"github.com/syedjafri06193/orch/internal/auth"
	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/dashboard"
	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/metrics"
	"github.com/syedjafri06193/orch/internal/notify/slack"
	"github.com/syedjafri06193/orch/internal/provider"
	execprovider "github.com/syedjafri06193/orch/internal/provider/exec"
	fakeprovider "github.com/syedjafri06193/orch/internal/provider/fake"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/verifier"
	httpverifier "github.com/syedjafri06193/orch/internal/verifier/http"
)

// version is set by the linker: `-X main.version=$(git describe --tags)`.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "orchd: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "token":
		return tokenCmd(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `orchd -- the deployment orchestrator server

  orchd serve [flags]     run the API, dashboard, executor and outbox worker
  orchd token issue       mint an API token
  orchd token list        list issued tokens
  orchd token revoke ID   revoke a token
  orchd version           print the version

Run "orchd serve -h" for the flags.
`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "orch.yaml", "path to the configuration file")
		dbPath     = fs.String("db", "orch.journal", "path to the state file")
		listen     = fs.String("listen", "127.0.0.1:8080", "address to listen on")
		nodeID     = fs.String("node", hostname(), "this node's identity, recorded in leases and the audit log")
		manifests  = fs.String("manifests", "", "directory of release manifests (<app>/<version>.json)")
		dashURL    = fs.String("dashboard-url", "", "public URL of this dashboard, used in Slack messages")
		noAuth     = fs.Bool("no-auth", false, "disable API authentication (single-user local install only)")
		commit     = fs.String("config-commit", "", "git commit the config came from, shown on the config page")
		allowFake  = fs.Bool("allow-fake-provider", false, "register the in-memory fake provider (testing only)")
		verbose    = fs.Bool("v", false, "debug logging")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		// A config error is a refusal to start. Starting with a config that
		// half-parsed is how a gate silently stops being enforced.
		return err
	}
	cfg.SourceCommit = *commit
	log.Info("configuration loaded", "file", cfg.SourceFile, "apps", len(cfg.Apps))

	st, err := store.Open(*dbPath, store.RealClock)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// The audit chain is verified at startup rather than on demand. If it has
	// been tampered with, that is worth knowing before the first deploy of the
	// day, not during the incident review afterwards (section 12.4).
	if err := st.VerifyAudit(context.Background()); err != nil {
		return fmt.Errorf("the audit log does not verify, refusing to start: %w", err)
	}

	providers := []provider.Provider{execprovider.New()}
	if *allowFake {
		// Behind a flag, and the log line says so, because a fake provider
		// that reports success without deploying anything is the worst
		// possible thing to have running by accident.
		log.Warn("the fake provider is registered; deployments to it do nothing")
		providers = append(providers, fakeprovider.New())
	}

	met := metrics.New()

	// Left nil when no directory is configured, and that distinction is load
	// bearing. A DirManifests with an empty root answers "I could not read
	// that manifest" for every version, and section 8.3 treats an unreadable
	// manifest as a refusal -- so wiring one in unconditionally would block
	// every rollback in an installation that has not adopted manifests yet.
	// Nil means "no manifest source", which takes the documented path: warn
	// loudly, once, and proceed.
	var manifestSource engine.Manifests
	if *manifests != "" {
		manifestSource = &engine.DirManifests{Root: *manifests}
		log.Info("release manifests", "dir", *manifests)
	} else {
		log.Warn("no --manifests directory; rollback migration floors cannot be checked")
	}

	// Which notifiers exist decides which outbox entries are written, so this
	// is computed before the executor rather than after it.
	notifierNames := []string{"dashboard"}
	if sc := cfg.Notifiers.Slack; sc != nil && sc.BotToken != "" {
		notifierNames = append(notifierNames, "slack")
	}

	exec := engine.New(engine.Options{
		Store:     st,
		Config:    cfg,
		Providers: provider.NewRegistry(providers...),
		Verifiers: verifier.NewRegistry(httpverifier.New()),
		Manifests: manifestSource,
		Clock:     engine.RealClock,
		Logger:    log,
		NodeID:    *nodeID,
		Secrets:   secretsOf(cfg),
		Notifiers: notifierNames,
	})

	bus := dashboard.NewBus()
	notifiers := map[string]engine.Notifier{
		"stdout":    &engine.StdoutNotifier{Log: log},
		"dashboard": &dashboard.Publisher{Bus: bus},
	}

	var slackHandler *slack.Handler
	if sc := cfg.Notifiers.Slack; sc != nil && sc.BotToken != "" {
		transport := slack.NewHTTPTransport(sc.BotToken)
		notifiers["slack"] = &slack.Notifier{
			Store: st, Config: cfg, Transport: transport, Log: log,
			DashboardURL: *dashURL, MinUpdateInterval: 5 * time.Second,
		}
		slackHandler = &slack.Handler{
			Store: st, Config: cfg, Executor: exec, Transport: transport, Log: log,
		}
		log.Info("slack notifier configured", "mode", sc.Mode)
	}
	_ = slackHandler

	tokens := auth.NewStore(nil)
	if err := loadTokens(tokens, tokenPath(*dbPath)); err != nil {
		return err
	}

	// Startup reconciliation, before anything is served (section 17.2). A
	// server that starts accepting deploys while last night's crashed
	// deployment still holds a lease is a server that will refuse them for
	// reasons nobody can see.
	if res, err := exec.Reconcile(context.Background()); err != nil {
		log.Error("startup reconciliation failed", "error", err)
	} else if res.Examined > 0 {
		log.Warn("recovered orphaned deployments", "result", res.String())
		met.OrphansRecovered.Add(float64(res.Resumed), "resumed")
		met.OrphansRecovered.Add(float64(res.Unknown), "unknown")
	}

	runner := newRunner(exec, st, log, met)

	apiSrv := &api.Server{
		Store: st, Config: cfg, Executor: exec, Tokens: tokens,
		Authz: &auth.Authorizer{Config: cfg}, Log: log,
		RequireAuth: !*noAuth,
		Run:         runner.start,
	}
	if *noAuth {
		log.Warn("API authentication is disabled; anyone who can reach this port can deploy")
	}

	dash := &dashboard.Server{
		Store: st, Config: cfg, Bus: bus, Log: log,
		Version: version, NodeID: *nodeID,
	}
	dashHandler, err := dash.Routes()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", apiSrv.Routes())
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(met.Gather()))
	})
	mux.Handle("/", dashHandler)

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// No WriteTimeout: the dashboard's SSE endpoint is a long-lived
		// response by design, and a write timeout would sever it mid-deploy,
		// which is exactly the freeze the keepalive exists to prevent.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	outbox := &engine.OutboxWorker{
		Store: st, Notifiers: notifiers, Log: log,
		Clock: engine.RealClock, Interval: 2 * time.Second,
		OnPending: func(n int) { met.OutboxPending.Set(float64(n)) },
		OnFailure: func(n string) { met.NotifyFailures.Inc(n) },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = outbox.Run(ctx)
	}()

	go func() {
		log.Info("listening", "addr", *listen, "dashboard", "http://"+*listen+"/", "node", *nodeID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	// The order matters. Stop accepting new work, let in-flight deployments
	// reach a state they can be resumed from, then drain the outbox so people
	// are told what happened. A shutdown that drops the outbox is a shutdown
	// after which nobody knows the deploy finished.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	runner.wait(shutdownCtx)
	wg.Wait()

	log.Info("stopped")
	return nil
}

// runner owns the goroutines that execute deployments.
//
// A deployment that outlives the request that created it is the whole reason
// section 3 chose a server over a stateless CLI: a ten-minute bake must not
// depend on an HTTP connection staying open.
type runner struct {
	exec  *engine.Executor
	store *store.Store
	log   *slog.Logger
	met   *metrics.Metrics
	wg    sync.WaitGroup
}

func newRunner(e *engine.Executor, st *store.Store, log *slog.Logger, met *metrics.Metrics) *runner {
	return &runner{exec: e, store: st, log: log, met: met}
}

func (r *runner) start(depID string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// Deliberately not the request context: the client hanging up must not
		// abort a deployment that is already changing production.
		ctx := context.Background()
		if err := r.exec.Run(ctx, depID); err != nil {
			r.log.Warn("deployment ended with an error", "deployment", depID, "error", err)
		}
		r.record(ctx, depID)
	}()
}

// record turns the finished deployment into the section 19.1 metrics.
//
// Read back from the store rather than tracked in memory, so a deployment
// recovered by reconciliation after a restart is counted the same way as one
// that ran start to finish here.
func (r *runner) record(ctx context.Context, depID string) {
	dep, err := r.store.Deployment(ctx, depID)
	if err != nil {
		return
	}
	r.met.DeploymentsTotal.Inc(dep.App, dep.Environment, strings.ToLower(string(dep.State)))
	if dep.FinishedAt != nil {
		r.met.DeploymentDuration.ObserveDuration(
			dep.FinishedAt.Sub(dep.CreatedAt), dep.App, dep.Environment, dep.Strategy)
	}
	switch {
	case dep.State == store.StateRolledBack:
		r.met.RollbacksTotal.Inc(dep.App, dep.Environment, "succeeded")
	case dep.State == store.StateRollbackFailed:
		r.met.RollbacksTotal.Inc(dep.App, dep.Environment, "failed")
	case dep.IsRollback:
		// A rollback someone ran by hand is a change failure too. Counting
		// only the automatic ones understates the change failure rate by
		// exactly the deploys a human caught first, which are the ones a team
		// most wants to see on the graph.
		outcome := "succeeded"
		if dep.State != store.StateSucceeded {
			outcome = "failed"
		}
		r.met.RollbacksTotal.Inc(dep.App, dep.Environment, outcome)
	}
}

func (r *runner) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// The deployments that are still running have persisted their state,
		// so the next process's startup reconciliation picks them up. That is
		// what the journal-before-side-effect ordering buys.
		r.log.Warn("deployments still running at shutdown; they will be reconciled on restart")
	}
}

func secretsOf(cfg *config.Config) []string {
	var out []string
	if sc := cfg.Notifiers.Slack; sc != nil {
		for _, s := range []string{sc.BotToken, sc.AppToken, sc.SigningSecret} {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node-1"
	}
	return h
}

func tokenPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "tokens.json")
}

// ------------------------------------------------------------------ tokens

func tokenCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: orchd token issue|list|revoke")
	}
	switch args[0] {
	case "issue":
		return tokenIssue(args[1:])
	case "list":
		return tokenList(args[1:])
	case "revoke":
		return tokenRevoke(args[1:])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func tokenIssue(args []string) error {
	fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
	var (
		dbPath = fs.String("db", "orch.journal", "path to the state file")
		user   = fs.String("user", "", "the user this token acts as (must exist in the config)")
		desc   = fs.String("description", "", "what this token is for")
		ttl    = fs.Duration("ttl", auth.DefaultTTL, "how long the token is valid")
		apps   = fs.String("apps", "", "comma-separated apps this token may touch (default: all)")
		envs   = fs.String("environments", "", "comma-separated environments this token may touch (default: all)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return errors.New("--user is required: a token that belongs to nobody cannot be audited")
	}

	path := tokenPath(*dbPath)
	tokens := auth.NewStore(nil)
	if err := loadTokens(tokens, path); err != nil {
		return err
	}
	plain, tok, err := tokens.Issue(*user, *desc, *ttl, splitList(*apps), splitList(*envs))
	if err != nil {
		return err
	}
	if err := saveTokens(tokens, path); err != nil {
		return err
	}

	// Printed once, on stdout, and never recoverable. Everything persisted is
	// a SHA-256 hash, so a stolen token file cannot be replayed.
	fmt.Printf("%s\n", plain)
	fmt.Fprintf(os.Stderr, "\nid         %s\nuser       %s\nexpires    %s\nscope      apps=%s environments=%s\n\n"+
		"This is the only time the token is shown. Store it somewhere your CI can read and nobody else can.\n",
		tok.ID, tok.User, tok.ExpiresAt.Format(time.RFC3339),
		orAll(tok.Apps), orAll(tok.Environments))
	return nil
}

func tokenList(args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	dbPath := fs.String("db", "orch.journal", "path to the state file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tokens := auth.NewStore(nil)
	if err := loadTokens(tokens, tokenPath(*dbPath)); err != nil {
		return err
	}
	list := tokens.List()
	if len(list) == 0 {
		fmt.Println("no tokens issued")
		return nil
	}
	fmt.Printf("%-14s %-16s %-22s %-8s %s\n", "ID", "USER", "EXPIRES", "STATE", "DESCRIPTION")
	for _, t := range list {
		state := "active"
		switch {
		case t.Revoked:
			state = "revoked"
		case t.Expired(time.Now().UTC()):
			state = "expired"
		}
		fmt.Printf("%-14s %-16s %-22s %-8s %s\n",
			t.ID, t.User, t.ExpiresAt.Format(time.RFC3339), state, t.Description)
	}
	return nil
}

func tokenRevoke(args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	dbPath := fs.String("db", "orch.journal", "path to the state file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: orchd token revoke <id>")
	}
	path := tokenPath(*dbPath)
	tokens := auth.NewStore(nil)
	if err := loadTokens(tokens, path); err != nil {
		return err
	}
	if err := tokens.Revoke(fs.Arg(0)); err != nil {
		return err
	}
	if err := saveTokens(tokens, path); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", fs.Arg(0))
	return nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orAll(list []string) string {
	if len(list) == 0 {
		return "*"
	}
	return strings.Join(list, "|")
}
