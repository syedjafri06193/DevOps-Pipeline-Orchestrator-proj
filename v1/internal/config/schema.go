package config

import (
	"fmt"
	"strings"
	"time"
)

// Config is orch.yaml (design.md section 13).
type Config struct {
	Version      int
	Apps         map[string]*App
	AppNames     []string // file order, for stable output
	Environments map[string]*EnvPolicy
	Notifiers    Notifiers
	Roles        map[string][]Grant
	Identities   map[string]*Identity

	// SourceFile and SourceCommit are shown on the dashboard's config page
	// (section 11.2: "Rendered current config with the source file and commit
	// that produced it").
	SourceFile   string
	SourceCommit string
}

type App struct {
	Name         string
	Artifact     Artifact
	Environments map[string]*AppEnv
	EnvNames     []string
}

type Artifact struct {
	Type             string
	Repository       string
	RequireSignature bool
}

type AppEnv struct {
	App         string
	Name        string
	Provider    string
	ProviderCfg map[string]string
	Strategy    Strategy
	// PromoteFrom enforces build-once-deploy-many (section 13): a version can
	// only reach this environment if the identical artifact succeeded in the
	// named one. "Rebuilding per environment means prod runs a binary that was
	// never tested, and it's a shockingly common mistake."
	PromoteFrom string
	Verify      VerifyPolicy
	Gates       []Gate
	Rollback    RollbackPolicy
	Notify      NotifyPolicy
}

// Resource is the lease key and the audit resource name for this target.
func (e *AppEnv) Resource() string { return "app:" + e.App + "/env:" + e.Name }

// ------------------------------------------------------------- strategies

type StrategyType string

const (
	StrategyRecreate  StrategyType = "recreate"
	StrategyRolling   StrategyType = "rolling"
	StrategyBlueGreen StrategyType = "blue-green"
	StrategyCanary    StrategyType = "canary"
)

type Strategy struct {
	Type      StrategyType
	BatchSize string // rolling: "50%" or "2"
	Steps     []CanaryStep
}

// CanaryStep is one entry of section 9's step list: either a traffic weight
// or a verification pause.
type CanaryStep struct {
	SetWeight *int
	Verify    *StepVerify
}

type StepVerify struct {
	Bake time.Duration
}

// ------------------------------------------------------------ verification

type Direction string

const (
	LowerIsBetter  Direction = "lower-is-better"
	HigherIsBetter Direction = "higher-is-better"
)

// VerifyPolicy is section 7.3's decision policy.
type VerifyPolicy struct {
	Bake             time.Duration
	SampleInterval   time.Duration
	MinSamples       int
	FailureThreshold int
	SuccessThreshold int
	MaxInconclusive  int
	Criteria         []Criterion
}

// Criterion is section 7.2's baseline-relative check.
type Criterion struct {
	Name      string
	Verifier  string
	Direction Direction

	// Max is an absolute ceiling, applied regardless of baseline.
	Max *float64
	// MaxRatio is the relative tolerance: 1.20 means the canary may be 20%
	// worse than baseline.
	MaxRatio *float64
	// FloorValue ignores the criterion below this absolute value.
	//
	// "That FloorValue field looks like a detail and is not. Without it, a
	// service whose error rate moves from 0.001% to 0.004% trips a 2x ratio
	// check and rolls back a perfectly good deploy."
	FloorValue float64

	// Verifier-specific settings, kept as strings so a new verifier needs no
	// schema change.
	Settings map[string]string
}

// ------------------------------------------------------------------- gates

type GateType string

const (
	GateApproval GateType = "approval"
	GateSchedule GateType = "schedule"
	GateFreeze   GateType = "freeze_window"
)

type Gate struct {
	Type GateType

	// approval
	RequirePeer bool
	Roles       []string
	TTL         time.Duration

	// schedule
	Deny     []DenyWindow
	Timezone string

	// freeze_window
	Source string
}

// DenyWindow is a parsed "Fri 16:00-23:59" or "Sat".
type DenyWindow struct {
	Day   time.Weekday
	Start int // minutes from midnight
	End   int
	Raw   string
}

func (w DenyWindow) Contains(t time.Time) bool {
	if t.Weekday() != w.Day {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= w.Start && m <= w.End
}

type RollbackPolicy struct {
	Automatic bool
	// CheckMigrationFloor refuses a rollback that would cross an irreversible
	// migration (section 8.3). Defaults to true: the safe default has to be
	// the one you get by saying nothing.
	CheckMigrationFloor bool
	MaxAttempts         int
}

type NotifyPolicy struct {
	Slack *SlackNotify
}

type SlackNotify struct {
	Channel          string
	On               []string
	MentionOnFailure []string
}

type Notifiers struct {
	Slack *SlackConfig
}

type SlackConfig struct {
	Mode          string // socket | http
	BotToken      string
	AppToken      string
	SigningSecret string
}

type EnvPolicy struct {
	Name                string
	RequirePeerApproval bool
	Protected           bool
}

// ------------------------------------------------------------------- RBAC

// Grant is one permission over a set of environments (section 12.5).
type Grant struct {
	Permission string
	On         []string // environment names, or "*"
}

func (g Grant) Covers(env string) bool {
	for _, e := range g.On {
		if e == "*" || e == env {
			return true
		}
	}
	return false
}

// Identity maps an external identity (a Slack user id) to an internal user.
//
// Section 10.3: "A Slack 'Approve' button carries a Slack user ID. It does
// not carry your system's authorization." This table is the mapping, and the
// absence of an entry is a refusal, never a default.
type Identity struct {
	User     string
	SlackID  string
	Email    string
	Roles    []string
	Disabled bool
}

// ---------------------------------------------------------------- lookups

func (c *Config) App(name string) (*App, bool) {
	a, ok := c.Apps[name]
	return a, ok
}

func (c *Config) AppEnv(app, env string) (*AppEnv, error) {
	a, ok := c.Apps[app]
	if !ok {
		e := &Error{File: c.SourceFile, Msg: fmt.Sprintf("unknown app %q", app)}
		if best, ok := closest(app, c.AppNames); ok {
			e.Hint = fmt.Sprintf("did you mean %q?", best)
		} else if len(c.AppNames) > 0 {
			e.Hint = "known apps: " + strings.Join(c.AppNames, ", ")
		}
		return nil, e
	}
	ae, ok := a.Environments[env]
	if !ok {
		e := &Error{File: c.SourceFile, Msg: fmt.Sprintf("app %q has no environment %q", app, env)}
		if best, ok := closest(env, a.EnvNames); ok {
			e.Hint = fmt.Sprintf("did you mean %q?", best)
		} else {
			e.Hint = "known environments: " + strings.Join(a.EnvNames, ", ")
		}
		return nil, e
	}
	return ae, nil
}

// EnvPolicyFor returns the global policy for an environment, defaulting to
// the unprotected zero value rather than failing: an environment that is not
// listed is simply not protected.
func (c *Config) EnvPolicyFor(name string) EnvPolicy {
	if p, ok := c.Environments[name]; ok {
		return *p
	}
	return EnvPolicy{Name: name}
}

// IdentityBySlackID is the section 10.3 lookup. Returns false for an unknown
// or disabled user; both are refusals.
func (c *Config) IdentityBySlackID(slackID string) (*Identity, bool) {
	for _, id := range c.Identities {
		if id.SlackID == slackID && !id.Disabled {
			return id, true
		}
	}
	return nil, false
}

// Can answers the RBAC question (section 12.5).
//
// Permissions are separate on purpose: "can trigger a deploy" is not "can
// approve a prod deploy" is not "can bypass a gate".
func (c *Config) Can(user *Identity, permission, env string) bool {
	if user == nil || user.Disabled {
		return false
	}
	for _, role := range user.Roles {
		for _, g := range c.Roles[role] {
			if g.Permission == permission && g.Covers(env) {
				return true
			}
		}
	}
	return false
}

// Permissions the system recognises. Listed so that a typo in a role
// definition is caught at load time rather than silently granting nothing --
// a role that grants nothing looks identical to a correctly-denied user.
var KnownPermissions = []string{
	"deploy:trigger",
	"deploy:approve",
	"deploy:rollback",
	"deploy:view",
	"deploy:override",
	"deploy:freeze",
	"deploy:abort",
}
