// Package api is the HTTP surface the CLI and dashboard talk to.
//
// Two things from the design document shape it:
//
//   - Every mutating call requires an `Idempotency-Key` (section 6.3), and a
//     repeat of the same key returns the original result rather than running
//     again. The CLI generates one per logical deploy and reuses it across
//     retries, so a flaky network does not deploy twice.
//   - Authn, then authz, then audit, in that order (section 4's diagram).
//     Every mutating request is audited whether it succeeded or not: a denied
//     deploy attempt is a more interesting audit row than a successful one.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/auth"
	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/store"
	"github.com/syedjafri06193/orch/internal/ulid"
)

// Server holds the API dependencies.
type Server struct {
	Store    *store.Store
	Config   *config.Config
	Executor *engine.Executor
	Tokens   *auth.Store
	Authz    *auth.Authorizer
	Log      *slog.Logger
	Now      func() time.Time
	// Run starts a deployment. Injected so the server does not have to own a
	// worker pool, and so tests can run deployments synchronously.
	Run func(depID string)
	// RequireAuth can be turned off for a single-user local install, where
	// demanding a token to reach a dashboard on localhost is friction with no
	// security benefit. It is on by default and the flag says so.
	RequireAuth bool
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Routes returns the handler.
//
// net/http's ServeMux with method patterns, rather than chi: since Go 1.22
// the standard library routes `POST /v1/deployments` and `GET /v1/x/{id}`
// directly, and a router dependency in a tool that holds prod credentials is
// supply-chain surface for no benefit (section 14.2).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", s.health)
	mux.Handle("GET /v1/deployments", s.authed("deploy:view", s.listDeployments))
	mux.Handle("POST /v1/deployments", s.authed("", s.createDeployment))
	mux.Handle("GET /v1/deployments/{id}", s.authed("deploy:view", s.getDeployment))
	mux.Handle("POST /v1/deployments/{id}/abort", s.authed("", s.abortDeployment))
	mux.Handle("POST /v1/deployments/{id}/approve", s.authed("", s.approveDeployment))
	mux.Handle("GET /v1/deployments/{id}/logs", s.authed("deploy:view", s.deploymentLogs))
	mux.Handle("GET /v1/status", s.authed("deploy:view", s.status))
	mux.Handle("GET /v1/audit", s.authed("deploy:view", s.auditLog))
	mux.Handle("POST /v1/audit/verify", s.authed("deploy:view", s.verifyAudit))
	mux.Handle("POST /v1/freezes", s.authed("", s.setFreeze))
	mux.Handle("GET /v1/freezes", s.authed("deploy:view", s.listFreezes))
	mux.Handle("GET /v1/config", s.authed("deploy:view", s.getConfig))

	return mux
}

// ------------------------------------------------------------ middleware

type ctxKey int

const tokenKey ctxKey = 1

// TokenFrom returns the authenticated token, if any.
func TokenFrom(ctx context.Context) *auth.Token {
	t, _ := ctx.Value(tokenKey).(*auth.Token)
	return t
}

// authed wraps a handler with authentication and a coarse permission check.
//
// The per-resource check happens inside each handler, because "may this user
// deploy" depends on which environment, and that is in the body.
func (s *Server) authed(permission string, h func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.RequireAuth {
			h(w, r)
			return
		}
		tok, err := s.Tokens.Lookup(auth.BearerToken(r))
		if err != nil {
			// The message distinguishes expired from unknown, because
			// "your token expired at 14:03" and "that token does not exist"
			// send someone to very different places.
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if permission != "" {
			id, ok := s.Config.Identities[tok.User]
			if !ok || !anyEnvironmentAllows(s.Config, id, permission) {
				writeError(w, http.StatusForbidden,
					fmt.Errorf("%w: %s does not have %s anywhere", auth.ErrForbidden, tok.User, permission))
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
	})
}

func anyEnvironmentAllows(cfg *config.Config, id *config.Identity, permission string) bool {
	for _, role := range id.Roles {
		for _, g := range cfg.Roles[role] {
			if g.Permission == permission {
				return true
			}
		}
	}
	return false
}

// --------------------------------------------------------------- handlers

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"time":       s.now().Format(time.RFC3339),
		"audit_head": s.Store.AuditHead(),
	})
}

// CreateDeploymentRequest is the body of POST /v1/deployments.
type CreateDeploymentRequest struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	Version     string `json:"version"`
	Strategy    string `json:"strategy,omitempty"`
	// Force bypasses the rollback circuit breaker. Requires deploy:override,
	// and section 12.5 says every use posts to Slack naming the person --
	// "not as punishment, as the thing that makes the override safe to have."
	Force  bool `json:"force,omitempty"`
	DryRun bool `json:"dry_run,omitempty"`
	// Rollback resolves the version server-side: the last one that deployed
	// successfully to this target before whatever is running now. Resolving it
	// here rather than in the CLI means the safety checks in section 8.3
	// cannot be skipped by a client that guesses a version itself.
	Rollback bool `json:"rollback,omitempty"`
	// Promote takes the version currently running in this environment's
	// `promote_from` source, so "the thing we tested" and "the thing we ship"
	// are the same artifact rather than two builds of the same commit.
	Promote bool `json:"promote,omitempty"`
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, errors.New(
			"Idempotency-Key header is required: without one, a retried request deploys twice"))
		return
	}

	var req CreateDeploymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decoding request: %w", err))
		return
	}

	// Same key, same result. Checked before anything else so a retry is
	// cheap and cannot race the original.
	if existing, err := s.Store.DeploymentByIdempotencyKey(r.Context(), key); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	cfgEnv, err := s.Config.AppEnv(req.App, req.Environment)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	if req.Rollback && req.Promote {
		writeError(w, http.StatusBadRequest,
			errors.New("a deployment is either a rollback or a promotion, not both"))
		return
	}

	// A rollback needs its own permission: someone who may deploy is not
	// automatically someone who may put yesterday's code back into production
	// while an incident is running.
	permission := "deploy:trigger"
	if req.Rollback {
		permission = "deploy:rollback"
	}

	tok := TokenFrom(r.Context())
	actor := "local"
	if tok != nil {
		actor = tok.User
		if err := s.Authz.Can(tok, permission, req.App, req.Environment); err != nil {
			// A denied attempt is audited. It is the row someone will look
			// for after an incident.
			s.audit(r.Context(), actor, "deploy.denied", cfgEnv.Resource(), map[string]string{
				"version": req.Version, "reason": err.Error(),
			})
			writeError(w, http.StatusForbidden, err)
			return
		}
		if req.Force {
			if err := s.Authz.Can(tok, "deploy:override", req.App, req.Environment); err != nil {
				writeError(w, http.StatusForbidden,
					fmt.Errorf("--force requires deploy:override: %w", err))
				return
			}
			s.audit(r.Context(), actor, "deploy.override", cfgEnv.Resource(), map[string]string{
				"version": req.Version,
				"note":    "circuit breaker bypassed",
			})
		}
	}

	// Resolving the version comes after authorization and before anything is
	// written, so a refusal costs nothing and an unsafe rollback is refused
	// rather than created and then abandoned halfway.
	var rollbackOf string
	switch {
	case req.Rollback:
		plan, current, err := s.planRollback(r.Context(), req, cfgEnv, actor)
		if err != nil {
			// A refusal, not a warning (section 8.3). 409 rather than 400: the
			// request is well formed, the state of the world is what says no.
			writeError(w, http.StatusConflict, err)
			return
		}
		req.Version, rollbackOf = plan.Version, plan.RollbackOf
		if req.Strategy == "" && current != nil {
			req.Strategy = current.Strategy
		}
	case req.Promote:
		version, err := s.promotionSource(r.Context(), req, cfgEnv)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		req.Version = version
	}

	if req.Version == "" {
		writeError(w, http.StatusBadRequest, errors.New("version is required"))
		return
	}

	strategy := req.Strategy
	if strategy == "" {
		strategy = string(cfgEnv.Strategy.Type)
	}

	if req.DryRun {
		// Section 17.3: "--dry-run prints the plan without executing. Should
		// be safe enough that people run it reflexively." So it creates
		// nothing at all -- not even a database row.
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run":      true,
			"app":          req.App,
			"environment":  req.Environment,
			"version":      req.Version,
			"is_rollback":  req.Rollback,
			"rollback_of":  rollbackOf,
			"strategy":     strategy,
			"provider":     cfgEnv.Provider,
			"gates":        describeGates(cfgEnv),
			"verify":       describeVerify(cfgEnv),
			"rollback":     cfgEnv.Rollback,
			"promote_from": cfgEnv.PromoteFrom,
		})
		return
	}

	dep := &store.Deployment{
		ID:             ulid.NewAt(s.now()).String(),
		App:            req.App,
		Environment:    req.Environment,
		Version:        req.Version,
		Strategy:       strategy,
		State:          store.StatePending,
		IdempotencyKey: key,
		TriggeredBy:    actor,
		TriggerSource:  store.TriggerAPI,
		Provider:       cfgEnv.Provider,
		IsRollback:     req.Rollback,
		RollbackOf:     rollbackOf,
		CreatedAt:      s.now(),
	}

	created, isNew, err := s.Store.CreateDeployment(r.Context(), dep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if isNew && s.Run != nil {
		s.Run(created.ID)
	}
	writeJSON(w, http.StatusAccepted, created)
}

// planRollback resolves what a rollback would deploy, and refuses if it would
// be unsafe.
//
// Section 8.3: "When rollback is unsafe, the tool must refuse and say why. Not
// warn -- refuse." The refusal carries the remedy, because the person reading
// it is mid-incident and "roll forward with a fix" is the only useful next
// sentence.
func (s *Server) planRollback(ctx context.Context, req CreateDeploymentRequest,
	cfgEnv *config.AppEnv, actor string) (*engine.Plan, *store.Deployment, error) {

	current, err := s.currentDeployment(ctx, req.App, req.Environment)
	if err != nil {
		return nil, nil, fmt.Errorf("nothing has been deployed to %s/%s, so there is nothing to roll back",
			req.App, req.Environment)
	}

	// The circuit breaker (section 7.4). Two rollbacks of the same version in
	// a day means rolling back is not fixing it, and a third is more likely to
	// make things worse than better.
	if !req.Force {
		max := cfgEnv.Rollback.MaxAttempts
		if max <= 0 {
			max = 2 // section 7.4's number
		}
		if err := s.Executor.CheckRollbackBudget(ctx, req.App, req.Environment, current.Version, max); err != nil {
			return nil, nil, err
		}
	}

	plan, err := s.Executor.PlanRollback(ctx, current, cfgEnv)
	if err != nil {
		if req.Force {
			// --force is audited by name (section 12.5): "not as punishment,
			// as the thing that makes the override safe to have."
			s.audit(ctx, actor, "rollback.forced", cfgEnv.Resource(), map[string]string{
				"from": current.Version, "refusal": err.Error(),
			})
			if plan == nil {
				return nil, nil, err // no target to force toward
			}
		} else {
			return nil, nil, err
		}
	}

	// An explicit --to overrides the resolved target, but only after the same
	// checks have run: naming a version is not a way around them.
	if req.Version != "" {
		plan.Version = req.Version
	}
	return plan, current, nil
}

// promotionSource is the version currently running in the source environment.
//
// Section 9: promotion ships the artifact that was tested, not a rebuild of
// the same commit. Two builds of one commit are not the same bytes, and the
// whole value of a staging soak is that the thing soaked is the thing shipped.
func (s *Server) promotionSource(ctx context.Context, req CreateDeploymentRequest,
	cfgEnv *config.AppEnv) (string, error) {

	if cfgEnv.PromoteFrom == "" {
		return "", fmt.Errorf("%s/%s has no promote_from, so there is no source to promote from",
			req.App, req.Environment)
	}
	src, err := s.Store.ListDeployments(ctx, store.ListFilter{
		App: req.App, Environment: cfgEnv.PromoteFrom,
		States: []store.State{store.StateSucceeded}, Limit: 1,
	})
	if err != nil {
		return "", err
	}
	if len(src) == 0 {
		return "", fmt.Errorf("nothing has deployed successfully to %s, so there is nothing to promote",
			cfgEnv.PromoteFrom)
	}
	return src[0].Version, nil
}

// currentDeployment is what is running, or what ran last.
func (s *Server) currentDeployment(ctx context.Context, app, env string) (*store.Deployment, error) {
	if active, err := s.Store.ActiveDeployment(ctx, app, env); err == nil {
		return active, nil
	}
	recent, err := s.Store.ListDeployments(ctx, store.ListFilter{App: app, Environment: env, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(recent) == 0 {
		return nil, store.ErrNotFound
	}
	return recent[0], nil
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.Store.Deployment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	steps, _ := s.Store.Steps(r.Context(), dep.ID)
	samples, _ := s.Store.Samples(r.Context(), dep.ID)
	approvals, _ := s.Store.Approvals(r.Context(), dep.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"deployment": dep,
		"steps":      steps,
		"samples":    samples,
		"approvals":  approvals,
	})
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	deps, err := s.Store.ListDeployments(r.Context(), store.ListFilter{
		App:         r.URL.Query().Get("app"),
		Environment: r.URL.Query().Get("environment"),
		Limit:       limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": deps})
}

func (s *Server) abortDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.Store.Deployment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	actor := "local"
	if tok := TokenFrom(r.Context()); tok != nil {
		actor = tok.User
		if err := s.Authz.Can(tok, "deploy:abort", dep.App, dep.Environment); err != nil {
			// Falling back to deploy:rollback, because someone who may undo a
			// deploy may certainly stop one.
			if err2 := s.Authz.Can(tok, "deploy:rollback", dep.App, dep.Environment); err2 != nil {
				writeError(w, http.StatusForbidden, err)
				return
			}
		}
	}
	if err := s.Executor.Abort(r.Context(), dep.ID, actor); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	updated, _ := s.Store.Deployment(r.Context(), dep.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) approveDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.Store.Deployment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	cfgEnv, err := s.Config.AppEnv(dep.App, dep.Environment)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	actor := "local"
	if tok := TokenFrom(r.Context()); tok != nil {
		actor = tok.User
	}

	gate, ok := approvalGate(cfgEnv)
	if !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("%s has no approval gate", dep.Environment))
		return
	}
	// The same four checks as the Slack button and the gate itself. One
	// implementation: a CLI approval must not be easier to obtain than a
	// Slack one.
	if err := s.Executor.ApprovalIsValid(dep, cfgEnv, gate, actor, s.now()); err != nil {
		s.audit(r.Context(), actor, "approval.denied", dep.Resource(), map[string]string{
			"deployment": dep.ID, "reason": err.Error(),
		})
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := s.Store.RecordApproval(r.Context(), &store.Approval{
		DeploymentID: dep.ID, User: actor, At: s.now(), Source: "api",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": true, "by": actor})
}

func (s *Server) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	steps, err := s.Store.Steps(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var b strings.Builder
	for _, st := range steps {
		fmt.Fprintf(&b, "=== %s (%s) ===\n", st.Name, st.State)
		if st.Output != "" {
			b.WriteString(st.Output)
			if !strings.HasSuffix(st.Output, "\n") {
				b.WriteByte('\n')
			}
		}
		if st.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", st.Error)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io_WriteString(w, b.String())
}

// Status is one row of `orch status`.
type Status struct {
	App          string      `json:"app"`
	Environment  string      `json:"environment"`
	Version      string      `json:"version"`
	State        store.State `json:"state"`
	Since        *time.Time  `json:"since,omitempty"`
	Frozen       bool        `json:"frozen,omitempty"`
	FreezeReason string      `json:"freeze_reason,omitempty"`
	Active       bool        `json:"active,omitempty"`
	Error        string      `json:"error,omitempty"`
}

// status answers "what version is in prod right now, and is it healthy".
//
// Section 11.2: "The overview page answering that in under a second is the
// thing people will actually use it for. Optimize that one."
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	wantApp := r.URL.Query().Get("app")
	wantEnv := r.URL.Query().Get("environment")

	var out []Status
	for _, appName := range s.Config.AppNames {
		if wantApp != "" && appName != wantApp {
			continue
		}
		app := s.Config.Apps[appName]
		for _, envName := range app.EnvNames {
			if wantEnv != "" && envName != wantEnv {
				continue
			}
			st := Status{App: appName, Environment: envName}

			if f, frozen := s.Store.Freeze(r.Context(), "app:"+appName+"/env:"+envName); frozen {
				st.Frozen, st.FreezeReason = true, f.Reason
			}

			// The active deployment if there is one, otherwise the last
			// terminal one -- "what is running" and "what is happening" are
			// both what someone means by `orch status`.
			if active, err := s.Store.ActiveDeployment(r.Context(), appName, envName); err == nil {
				st.Version, st.State, st.Active = active.Version, active.State, true
				st.Since = &active.CreatedAt
			} else {
				recent, _ := s.Store.ListDeployments(r.Context(), store.ListFilter{
					App: appName, Environment: envName, Limit: 1,
				})
				if len(recent) > 0 {
					st.Version, st.State = recent[0].Version, recent[0].State
					st.Since, st.Error = recent[0].FinishedAt, recent[0].Error
				}
			}
			out = append(out, st)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": out})
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.Store.AuditEntries(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"head":    s.Store.AuditHead(),
	})
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.VerifyAudit(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "head": s.Store.AuditHead()})
}

// FreezeRequest is the body of POST /v1/freezes.
type FreezeRequest struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	Reason      string `json:"reason"`
	Until       string `json:"until,omitempty"`
	Lift        bool   `json:"lift,omitempty"`
}

func (s *Server) setFreeze(w http.ResponseWriter, r *http.Request) {
	var req FreezeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Reason == "" && !req.Lift {
		// A freeze nobody can explain is a freeze someone lifts.
		writeError(w, http.StatusBadRequest, errors.New("a freeze needs a reason"))
		return
	}
	actor := "local"
	if tok := TokenFrom(r.Context()); tok != nil {
		actor = tok.User
		if err := s.Authz.Can(tok, "deploy:freeze", req.App, req.Environment); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
	}

	f := &store.Freeze{
		Resource: "app:" + req.App + "/env:" + req.Environment,
		Reason:   req.Reason,
		By:       actor,
		At:       s.now(),
		Lifted:   req.Lift,
	}
	if req.Lift {
		f.LiftedBy = actor
	}
	if req.Until != "" {
		until, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("until must be an RFC3339 timestamp: %w", err))
			return
		}
		f.Until = &until
	}
	if err := s.Store.SetFreeze(r.Context(), f); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) listFreezes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"freezes": s.Store.Freezes(r.Context())})
}

// getConfig renders the loaded configuration (section 11.2's Config page).
//
// Secrets never appear: the config struct holds tokens only where the YAML
// referenced an environment variable, and those are stripped here rather than
// trusted not to be interesting.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	redacted := *s.Config
	if redacted.Notifiers.Slack != nil {
		cp := *redacted.Notifiers.Slack
		cp.BotToken = redactIfSet(cp.BotToken)
		cp.AppToken = redactIfSet(cp.AppToken)
		cp.SigningSecret = redactIfSet(cp.SigningSecret)
		redacted.Notifiers.Slack = &cp
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source_file": redacted.SourceFile,
		"apps":        redacted.AppNames,
		"config":      redacted,
	})
}

func redactIfSet(s string) string {
	if s == "" {
		return ""
	}
	return "[set]"
}

// --------------------------------------------------------------- helpers

func (s *Server) audit(ctx context.Context, actor, action, resource string, detail map[string]string) {
	if err := s.Store.AppendAudit(ctx, store.AuditEntry{
		At: s.now(), Actor: actor, Action: action, Resource: resource, Detail: detail,
	}); err != nil && s.Log != nil {
		s.Log.Error("appending audit entry", "error", err)
	}
}

func approvalGate(cfg *config.AppEnv) (config.Gate, bool) {
	for _, g := range cfg.Gates {
		if g.Type == config.GateApproval {
			return g, true
		}
	}
	return config.Gate{}, false
}

func describeGates(cfg *config.AppEnv) []map[string]any {
	var out []map[string]any
	for _, g := range cfg.Gates {
		m := map[string]any{"type": string(g.Type)}
		switch g.Type {
		case config.GateApproval:
			m["roles"] = g.Roles
			m["require_peer"] = g.RequirePeer
			m["ttl"] = g.TTL.String()
		case config.GateSchedule:
			var windows []string
			for _, w := range g.Deny {
				windows = append(windows, w.Raw)
			}
			m["deny"] = windows
			m["timezone"] = g.Timezone
		}
		out = append(out, m)
	}
	return out
}

func describeVerify(cfg *config.AppEnv) map[string]any {
	var names []string
	for _, c := range cfg.Verify.Criteria {
		names = append(names, c.Name)
	}
	return map[string]any{
		"bake":              cfg.Verify.Bake.String(),
		"sample_interval":   cfg.Verify.SampleInterval.String(),
		"min_samples":       cfg.Verify.MinSamples,
		"failure_threshold": cfg.Verify.FailureThreshold,
		"criteria":          names,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ErrorResponse is the shape of every error the API returns.
type ErrorResponse struct {
	Error string `json:"error"`
	// Hint carries the actionable half of a config.Error, which is the part
	// that makes a message worth reading.
	Hint string `json:"hint,omitempty"`
}

func writeError(w http.ResponseWriter, code int, err error) {
	resp := ErrorResponse{Error: err.Error()}
	var cfgErr *config.Error
	if errors.As(err, &cfgErr) && cfgErr.Hint != "" {
		resp.Error = strings.SplitN(err.Error(), "\n", 2)[0]
		resp.Hint = cfgErr.Hint
	}
	writeJSON(w, code, resp)
}

func io_WriteString(w http.ResponseWriter, s string) (int, error) {
	return w.Write([]byte(s))
}
