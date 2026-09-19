package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Load reads and fully validates a config file.
//
// Everything is checked here, at load time, so that `orch config validate` in
// CI catches what would otherwise surface during a deploy. A config that
// loads is a config that will not produce a schema surprise at 3am.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Decode(string(data), path)
}

// Decode parses and validates config source.
func Decode(src, file string) (*Config, error) {
	root, err := Parse(src)
	if err != nil {
		if e, ok := err.(*Error); ok {
			e.File = file
			return nil, e
		}
		return nil, err
	}

	d := &decoder{file: file}
	m, err := d.mapping(root)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		SourceFile:   file,
		Apps:         map[string]*App{},
		Environments: map[string]*EnvPolicy{},
		Roles:        map[string][]Grant{},
		Identities:   map[string]*Identity{},
	}

	if cfg.Version, err = m.integer("version", 0); err != nil {
		return nil, err
	}
	if cfg.Version != 1 {
		return nil, m.d.errAtKey(m, "version",
			"config version must be 1, got %d", cfg.Version)
	}

	if err := decodeApps(d.at("apps"), m, cfg); err != nil {
		return nil, err
	}
	if err := decodeEnvironments(d.at("environments"), m, cfg); err != nil {
		return nil, err
	}
	if err := decodeNotifiers(d.at("notifiers"), m, cfg); err != nil {
		return nil, err
	}
	if err := decodeRoles(d.at("roles"), m, cfg); err != nil {
		return nil, err
	}
	if err := decodeIdentities(d.at("identities"), m, cfg); err != nil {
		return nil, err
	}
	if err := m.done(); err != nil {
		return nil, err
	}
	if err := crossValidate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// --------------------------------------------------------------------- apps

func decodeApps(d *decoder, parent *mapping, cfg *Config) error {
	n := parent.get("apps")
	if n == nil || n.IsNull() {
		return parent.d.errAtKey(parent, "apps", "no apps are defined")
	}
	apps, err := d.mapping(n)
	if err != nil {
		return err
	}
	for _, name := range apps.node.Keys {
		ad := d.at(name)
		am, err := ad.mapping(apps.get(name))
		if err != nil {
			return err
		}
		app := &App{Name: name, Environments: map[string]*AppEnv{}}

		if am.has("artifact") {
			artd := ad.at("artifact")
			artm, err := artd.mapping(am.get("artifact"))
			if err != nil {
				return err
			}
			if app.Artifact.Type, err = artm.oneOf("type", "none", "none", "ecr", "s3", "oci", "file"); err != nil {
				return err
			}
			if app.Artifact.Repository, err = artm.str("repository", ""); err != nil {
				return err
			}
			if app.Artifact.RequireSignature, err = artm.boolean("require_signature", false); err != nil {
				return err
			}
			if err := artm.done(); err != nil {
				return err
			}
		}

		envsNode, err := am.req("environments")
		if err != nil {
			return err
		}
		envs, err := ad.at("environments").mapping(envsNode)
		if err != nil {
			return err
		}
		for _, envName := range envs.node.Keys {
			ed := ad.at("environments").at(envName)
			ae, err := decodeAppEnv(ed, envs.get(envName), name, envName)
			if err != nil {
				return err
			}
			app.Environments[envName] = ae
			app.EnvNames = append(app.EnvNames, envName)
		}
		if err := envs.done(); err != nil {
			return err
		}
		if err := am.done(); err != nil {
			return err
		}

		cfg.Apps[name] = app
		cfg.AppNames = append(cfg.AppNames, name)
	}
	return apps.done()
}

func decodeAppEnv(d *decoder, n *Node, app, env string) (*AppEnv, error) {
	m, err := d.mapping(n)
	if err != nil {
		return nil, err
	}
	ae := &AppEnv{App: app, Name: env, ProviderCfg: map[string]string{}}

	if ae.Provider, err = m.reqStr("provider"); err != nil {
		return nil, err
	}
	if cfgNode := m.get("config"); cfgNode != nil && !cfgNode.IsNull() {
		pm, err := d.at("config").mapping(cfgNode)
		if err != nil {
			return nil, err
		}
		for _, k := range pm.node.Keys {
			v, err := pm.get(k).String()
			if err != nil {
				return nil, d.at("config").errf(pm.node.Values[k],
					"provider setting %q must be a simple value", k)
			}
			ae.ProviderCfg[k] = expandEnv(v)
		}
	}

	if ae.Strategy, err = decodeStrategy(d.at("strategy"), m.get("strategy")); err != nil {
		return nil, err
	}
	if ae.PromoteFrom, err = m.str("promote_from", ""); err != nil {
		return nil, err
	}
	if ae.Verify, err = decodeVerify(d.at("verify"), m.get("verify")); err != nil {
		return nil, err
	}
	if ae.Gates, err = decodeGates(d.at("gates"), m.get("gates")); err != nil {
		return nil, err
	}
	if ae.Rollback, err = decodeRollback(d.at("rollback"), m.get("rollback")); err != nil {
		return nil, err
	}
	if ae.Notify, err = decodeNotify(d.at("notify"), m.get("notify")); err != nil {
		return nil, err
	}
	if err := m.done(); err != nil {
		return nil, err
	}
	return ae, nil
}

// --------------------------------------------------------------- strategy

func decodeStrategy(d *decoder, n *Node) (Strategy, error) {
	s := Strategy{Type: StrategyRolling, BatchSize: "100%"}
	if n == nil || n.IsNull() {
		return s, nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return s, err
	}
	t, err := m.oneOf("type", "rolling", "recreate", "rolling", "blue-green", "canary")
	if err != nil {
		return s, err
	}
	s.Type = StrategyType(t)

	if s.BatchSize, err = m.str("batch_size", "100%"); err != nil {
		return s, err
	}
	if err := validateBatchSize(d, m, s.BatchSize); err != nil {
		return s, err
	}

	if stepsNode := m.get("steps"); stepsNode != nil && !stepsNode.IsNull() {
		if stepsNode.Kind != KindSequence {
			return s, d.errf(stepsNode, "strategy steps must be a list")
		}
		for i, item := range stepsNode.Items {
			sd := d.at(fmt.Sprintf("steps[%d]", i))
			sm, err := sd.mapping(item)
			if err != nil {
				return s, err
			}
			var step CanaryStep
			if wNode := sm.get("setWeight"); wNode != nil && !wNode.IsNull() {
				w, err := wNode.Int()
				if err != nil {
					return s, sd.errf(wNode, "setWeight must be a whole number of percent")
				}
				if w < 0 || w > 100 {
					return s, sd.errf(wNode, "setWeight must be between 0 and 100, got %d", w)
				}
				step.SetWeight = &w
			}
			if vNode := sm.get("verify"); vNode != nil && !vNode.IsNull() {
				vm, err := sd.at("verify").mapping(vNode)
				if err != nil {
					return s, err
				}
				bake, err := durationOf(sd, vm, "bake", 0)
				if err != nil {
					return s, err
				}
				step.Verify = &StepVerify{Bake: bake}
				if err := vm.done(); err != nil {
					return s, err
				}
			}
			if err := sm.done(); err != nil {
				return s, err
			}
			if step.SetWeight == nil && step.Verify == nil {
				return s, sd.errf(item, "a canary step must set a weight or verify")
			}
			if step.SetWeight != nil && step.Verify != nil {
				return s, sd.errf(item,
					"a canary step sets a weight or verifies, not both")
			}
			s.Steps = append(s.Steps, step)
		}
	}
	if err := m.done(); err != nil {
		return s, err
	}

	if s.Type == StrategyCanary {
		if len(s.Steps) == 0 {
			return s, d.errf(n, "a canary strategy needs steps")
		}
		last := s.Steps[len(s.Steps)-1]
		if last.SetWeight == nil || *last.SetWeight != 100 {
			// Without this, a canary that passes every step leaves the new
			// version serving a fraction of traffic forever, and the deploy
			// reports success.
			return s, d.errf(n,
				"the last canary step must be setWeight: 100, or the rollout never completes")
		}
	} else if len(s.Steps) > 0 {
		return s, d.errf(n, "steps are only valid for a canary strategy, not %q", s.Type)
	}
	return s, nil
}

var batchSizeRe = regexp.MustCompile(`^(\d+)(%?)$`)

func validateBatchSize(d *decoder, m *mapping, v string) error {
	match := batchSizeRe.FindStringSubmatch(v)
	if match == nil {
		e := m.d.errAtKey(m, "batch_size", "batch_size %q is not a count or a percentage", v)
		e.Hint = `use a count like "2" or a percentage like "50%"`
		return e
	}
	n, _ := strconv.Atoi(match[1])
	if match[2] == "%" && (n < 1 || n > 100) {
		return m.d.errAtKey(m, "batch_size", "batch_size percentage must be 1-100, got %d", n)
	}
	if match[2] == "" && n < 1 {
		return m.d.errAtKey(m, "batch_size", "batch_size must be at least 1")
	}
	return nil
}

// ----------------------------------------------------------------- verify

func decodeVerify(d *decoder, n *Node) (VerifyPolicy, error) {
	// Defaults chosen to be safe rather than fast: three consecutive failures
	// before rollback, never a verdict on fewer than three samples.
	p := VerifyPolicy{
		SampleInterval:   30 * time.Second,
		MinSamples:       3,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		MaxInconclusive:  3,
	}
	if n == nil || n.IsNull() {
		return p, nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return p, err
	}
	if p.Bake, err = durationOf(d, m, "bake", 0); err != nil {
		return p, err
	}
	if p.SampleInterval, err = durationOf(d, m, "sample_interval", p.SampleInterval); err != nil {
		return p, err
	}
	if p.MinSamples, err = m.integer("min_samples", p.MinSamples); err != nil {
		return p, err
	}
	if p.FailureThreshold, err = m.integer("failure_threshold", p.FailureThreshold); err != nil {
		return p, err
	}
	if p.SuccessThreshold, err = m.integer("success_threshold", p.SuccessThreshold); err != nil {
		return p, err
	}
	if p.MaxInconclusive, err = m.integer("max_inconclusive", p.MaxInconclusive); err != nil {
		return p, err
	}

	if critNode := m.get("criteria"); critNode != nil && !critNode.IsNull() {
		if critNode.Kind != KindSequence {
			return p, d.errf(critNode, "criteria must be a list")
		}
		for i, item := range critNode.Items {
			c, err := decodeCriterion(d.at(fmt.Sprintf("criteria[%d]", i)), item)
			if err != nil {
				return p, err
			}
			p.Criteria = append(p.Criteria, c)
		}
	}
	if err := m.done(); err != nil {
		return p, err
	}

	if p.SampleInterval <= 0 {
		return p, d.errf(n, "sample_interval must be positive")
	}
	if p.FailureThreshold < 1 {
		return p, d.errf(n, "failure_threshold must be at least 1")
	}
	if p.MinSamples < 1 {
		return p, d.errf(n, "min_samples must be at least 1")
	}
	// A bake window that cannot fit min_samples can only ever be
	// inconclusive, which reads in production as "verification is broken".
	if p.Bake > 0 && p.SampleInterval > 0 {
		possible := int(p.Bake / p.SampleInterval)
		if possible < p.MinSamples {
			e := d.errf(n,
				"bake %s at sample_interval %s allows %d samples, but min_samples is %d",
				p.Bake, p.SampleInterval, possible, p.MinSamples)
			e.Hint = "every verification would be Inconclusive; lengthen bake, shorten sample_interval, or lower min_samples"
			return p, e
		}
	}
	return p, nil
}

func decodeCriterion(d *decoder, n *Node) (Criterion, error) {
	var c Criterion
	m, err := d.mapping(n)
	if err != nil {
		return c, err
	}
	if c.Name, err = m.reqStr("name"); err != nil {
		return c, err
	}
	if c.Verifier, err = m.reqStr("verifier"); err != nil {
		return c, err
	}
	dir, err := m.oneOf("direction", string(LowerIsBetter), string(LowerIsBetter), string(HigherIsBetter))
	if err != nil {
		return c, err
	}
	c.Direction = Direction(dir)
	if c.Max, err = m.float("max"); err != nil {
		return c, err
	}
	if c.MaxRatio, err = m.float("max_ratio"); err != nil {
		return c, err
	}
	if c.FloorValue, err = m.floatDef("floor_value", 0); err != nil {
		return c, err
	}

	// Anything left is verifier configuration (a URL, a PromQL query, an
	// expected status). Collected rather than rejected so that adding a
	// verifier needs no schema change -- but the verifier itself validates
	// its own keys at load time, so a typo is still caught.
	c.Settings = map[string]string{}
	for _, k := range m.node.Keys {
		if m.seen[k] {
			continue
		}
		v, err := m.node.Values[k].String()
		if err != nil {
			return c, d.errf(m.node.Values[k], "criterion setting %q must be a simple value", k)
		}
		c.Settings[k] = expandEnv(v)
		m.seen[k] = true
	}

	if c.Max == nil && c.MaxRatio == nil && len(c.Settings) == 0 {
		e := d.errf(n, "criterion %q has no threshold", c.Name)
		e.Hint = "set max (absolute), max_ratio (relative to baseline), or a verifier-specific check such as expect_status"
		return c, e
	}
	if c.MaxRatio != nil && *c.MaxRatio <= 1.0 {
		e := d.errf(n, "max_ratio %v means the canary may never be any worse than baseline", *c.MaxRatio)
		e.Hint = "max_ratio is a tolerance above 1.0; 1.2 allows the canary to be 20% worse"
		return c, e
	}
	if c.MaxRatio != nil && c.FloorValue == 0 && c.Direction == LowerIsBetter {
		// Section 7.2 is emphatic about this one, so it is a refusal rather
		// than a warning.
		e := d.errf(n, "criterion %q sets max_ratio without floor_value", c.Name)
		e.Hint = "without a floor, a move from 0.001%% to 0.004%% trips a ratio check and rolls back a good deploy (design.md section 7.2). Set floor_value to the smallest value worth reacting to."
		return c, e
	}
	return c, nil
}

// ------------------------------------------------------------------ gates

func decodeGates(d *decoder, n *Node) ([]Gate, error) {
	if n == nil || n.IsNull() {
		return nil, nil
	}
	if n.Kind != KindSequence {
		return nil, d.errf(n, "gates must be a list")
	}
	var out []Gate
	for i, item := range n.Items {
		gd := d.at(fmt.Sprintf("[%d]", i))
		m, err := gd.mapping(item)
		if err != nil {
			return nil, err
		}
		t, err := m.oneOf("type", "", string(GateApproval), string(GateSchedule), string(GateFreeze))
		if err != nil {
			return nil, err
		}
		g := Gate{Type: GateType(t)}

		switch g.Type {
		case GateApproval:
			if g.RequirePeer, err = m.boolean("require_peer", false); err != nil {
				return nil, err
			}
			if g.Roles, err = m.strings("roles"); err != nil {
				return nil, err
			}
			if g.TTL, err = durationOf(gd, m, "ttl", time.Hour); err != nil {
				return nil, err
			}
			if len(g.Roles) == 0 {
				e := gd.errf(item, "an approval gate must name the roles that may approve")
				e.Hint = "without roles, any linked user could approve; section 10.3"
				return nil, e
			}
			if g.TTL <= 0 {
				e := gd.errf(item, "approval ttl must be positive")
				e.Hint = "approvals expire: a button posted at 2pm should not be actionable at 11pm"
				return nil, e
			}
		case GateSchedule:
			raw, err := m.strings("deny")
			if err != nil {
				return nil, err
			}
			if g.Timezone, err = m.str("timezone", "UTC"); err != nil {
				return nil, err
			}
			if _, err := time.LoadLocation(g.Timezone); err != nil {
				e := gd.errf(item, "unknown timezone %q", g.Timezone)
				e.Hint = "use an IANA name such as America/Los_Angeles"
				return nil, e
			}
			for _, r := range raw {
				w, err := ParseDenyWindow(r)
				if err != nil {
					e := gd.errf(item, "%v", err)
					e.Hint = `windows look like "Fri 16:00-23:59" or "Sat"`
					return nil, e
				}
				g.Deny = append(g.Deny, w...)
			}
			if len(g.Deny) == 0 {
				return nil, gd.errf(item, "a schedule gate with no deny windows blocks nothing")
			}
		case GateFreeze:
			if g.Source, err = m.str("source", "manual"); err != nil {
				return nil, err
			}
		}
		if err := m.done(); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// ParseDenyWindow turns "Fri 16:00-23:59" or "Sat" into windows.
//
// A bare day name is the whole day, which is what someone writing "Sat"
// means and what a parser that required a time range would reject.
func ParseDenyWindow(s string) ([]DenyWindow, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty deny window")
	}
	day, ok := weekdays[strings.ToLower(fields[0])]
	if !ok {
		return nil, fmt.Errorf("unknown day %q in deny window %q", fields[0], s)
	}
	if len(fields) == 1 {
		return []DenyWindow{{Day: day, Start: 0, End: 24*60 - 1, Raw: s}}, nil
	}
	span := strings.SplitN(fields[1], "-", 2)
	if len(span) != 2 {
		return nil, fmt.Errorf("deny window %q needs a start and end time", s)
	}
	start, err := parseClock(span[0])
	if err != nil {
		return nil, fmt.Errorf("deny window %q: %w", s, err)
	}
	end, err := parseClock(span[1])
	if err != nil {
		return nil, fmt.Errorf("deny window %q: %w", s, err)
	}
	if end < start {
		// Wrapping past midnight becomes two windows rather than silently
		// matching nothing, which is what a naive start<=m<=end does.
		return []DenyWindow{
			{Day: day, Start: start, End: 24*60 - 1, Raw: s},
			{Day: (day + 1) % 7, Start: 0, End: end, Raw: s},
		}, nil
	}
	return []DenyWindow{{Day: day, Start: start, End: end, Raw: s}}, nil
}

func parseClock(s string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("hour %q out of range", parts[0])
	}
	mnt, err := strconv.Atoi(parts[1])
	if err != nil || mnt < 0 || mnt > 59 {
		return 0, fmt.Errorf("minute %q out of range", parts[1])
	}
	return h*60 + mnt, nil
}

// --------------------------------------------------------------- rollback

func decodeRollback(d *decoder, n *Node) (RollbackPolicy, error) {
	// CheckMigrationFloor defaults to true. Section 8.3 makes refusing an
	// unsafe rollback the whole point, so the default has to be the safe one.
	p := RollbackPolicy{Automatic: true, CheckMigrationFloor: true, MaxAttempts: 2}
	if n == nil || n.IsNull() {
		return p, nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return p, err
	}
	if p.Automatic, err = m.boolean("automatic", p.Automatic); err != nil {
		return p, err
	}
	if p.CheckMigrationFloor, err = m.boolean("check_migration_floor", p.CheckMigrationFloor); err != nil {
		return p, err
	}
	if p.MaxAttempts, err = m.integer("max_attempts", p.MaxAttempts); err != nil {
		return p, err
	}
	if err := m.done(); err != nil {
		return p, err
	}
	if p.MaxAttempts < 1 {
		return p, d.errf(n, "rollback max_attempts must be at least 1")
	}
	return p, nil
}

// --------------------------------------------------------------- notifiers

func decodeNotify(d *decoder, n *Node) (NotifyPolicy, error) {
	var p NotifyPolicy
	if n == nil || n.IsNull() {
		return p, nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return p, err
	}
	if sn := m.get("slack"); sn != nil && !sn.IsNull() {
		sd := d.at("slack")
		sm, err := sd.mapping(sn)
		if err != nil {
			return p, err
		}
		s := &SlackNotify{}
		if s.Channel, err = sm.reqStr("channel"); err != nil {
			return p, err
		}
		if s.On, err = sm.strings("on"); err != nil {
			return p, err
		}
		if s.MentionOnFailure, err = sm.strings("mention_on_failure"); err != nil {
			return p, err
		}
		for _, ev := range s.On {
			if !validNotifyEvent(ev) {
				e := sd.errf(sn, "unknown notification event %q", ev)
				if best, ok := closest(ev, NotifyEvents); ok {
					e.Hint = fmt.Sprintf("did you mean %q?", best)
				} else {
					e.Hint = "valid events: " + strings.Join(NotifyEvents, ", ")
				}
				return p, e
			}
		}
		if err := sm.done(); err != nil {
			return p, err
		}
		p.Slack = s
	}
	return p, m.done()
}

// NotifyEvents are the events a notifier may subscribe to.
//
// These are deliberately NOT the state names. "started" reads better in a
// config file than "preflight", and a config vocabulary that tracks internal
// state names would break every customer's file the day a state is renamed.
// engine.EventType maps one to the other, and this list is what the config
// loader validates against -- so a typo'd event name is a load error rather
// than a notification that silently never fires.
var NotifyEvents = []string{
	"started", "awaiting_approval", "approved", "deploying", "verifying",
	"promoting", "succeeded", "rolling_back", "rolled_back", "rollback_failed",
	"failed", "aborted", "unknown", "blocked",
}

func validNotifyEvent(s string) bool {
	for _, e := range NotifyEvents {
		if e == s {
			return true
		}
	}
	return false
}

func decodeNotifiers(d *decoder, parent *mapping, cfg *Config) error {
	n := parent.get("notifiers")
	if n == nil || n.IsNull() {
		return nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return err
	}
	if sn := m.get("slack"); sn != nil && !sn.IsNull() {
		sd := d.at("slack")
		sm, err := sd.mapping(sn)
		if err != nil {
			return err
		}
		s := &SlackConfig{}
		if s.Mode, err = sm.oneOf("mode", "socket", "socket", "http"); err != nil {
			return err
		}
		if s.BotToken, err = sm.str("bot_token", ""); err != nil {
			return err
		}
		if s.AppToken, err = sm.str("app_token", ""); err != nil {
			return err
		}
		if s.SigningSecret, err = sm.str("signing_secret", ""); err != nil {
			return err
		}
		s.BotToken = expandEnv(s.BotToken)
		s.AppToken = expandEnv(s.AppToken)
		s.SigningSecret = expandEnv(s.SigningSecret)

		if s.Mode == "http" && s.SigningSecret == "" {
			e := sd.errf(sn, "slack mode http requires signing_secret")
			e.Hint = "an unverified Slack endpoint lets anyone on the internet deploy to production (design.md section 10.2). Socket Mode needs no endpoint at all."
			return e
		}
		if s.Mode == "socket" && s.AppToken == "" {
			return sd.errf(sn, "slack socket mode requires app_token (xapp-...)")
		}
		if err := sm.done(); err != nil {
			return err
		}
		cfg.Notifiers.Slack = s
	}
	return m.done()
}

// ------------------------------------------------------ environments, RBAC

func decodeEnvironments(d *decoder, parent *mapping, cfg *Config) error {
	n := parent.get("environments")
	if n == nil || n.IsNull() {
		return nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return err
	}
	for _, name := range m.node.Keys {
		ed := d.at(name)
		em, err := ed.mapping(m.get(name))
		if err != nil {
			return err
		}
		p := &EnvPolicy{Name: name}
		if p.RequirePeerApproval, err = em.boolean("require_peer_approval", false); err != nil {
			return err
		}
		if p.Protected, err = em.boolean("protected", false); err != nil {
			return err
		}
		if err := em.done(); err != nil {
			return err
		}
		cfg.Environments[name] = p
	}
	return m.done()
}

var grantRe = regexp.MustCompile(`^([a-z]+:[a-z]+)\s+on:\s*(.+)$`)

func decodeRoles(d *decoder, parent *mapping, cfg *Config) error {
	n := parent.get("roles")
	if n == nil || n.IsNull() {
		return nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return err
	}
	for _, role := range m.node.Keys {
		rd := d.at(role)
		list := m.get(role)
		if list.Kind != KindSequence {
			return rd.errf(list, "role %q must be a list of grants", role)
		}
		var grants []Grant
		for _, item := range list.Items {
			g, err := decodeGrant(rd, item)
			if err != nil {
				return err
			}
			grants = append(grants, g)
		}
		cfg.Roles[role] = grants
	}
	return m.done()
}

// decodeGrant accepts both shapes section 12.5 uses:
//
//   - deploy:trigger    on: [dev, staging]        (a scalar with an "on:" tail)
//   - { permission: deploy:trigger, on: [dev] }   (an explicit mapping)
func decodeGrant(d *decoder, n *Node) (Grant, error) {
	var g Grant
	switch n.Kind {
	case KindScalar:
		match := grantRe.FindStringSubmatch(strings.TrimSpace(n.Value))
		if match == nil {
			e := d.errf(n, "malformed grant %q", n.Value)
			e.Hint = `write: deploy:trigger    on: [dev, staging]`
			return g, e
		}
		g.Permission = match[1]
		g.On = splitList(match[2])
	case KindMapping:
		m, err := d.mapping(n)
		if err != nil {
			return g, err
		}
		// The documented syntax -- `deploy:trigger    on: [dev, staging]` --
		// parses as a one-key mapping whose key is "deploy:trigger    on",
		// because the colon inside the permission is not a separator. Accept
		// that shape rather than making the config file uglier than the
		// document it implements.
		if len(m.node.Keys) == 1 {
			if fields := strings.Fields(m.node.Keys[0]); len(fields) == 2 && fields[1] == "on" {
				g.Permission = fields[0]
				if g.On, err = m.strings(m.node.Keys[0]); err != nil {
					return g, err
				}
				break
			}
		}
		if g.Permission, err = m.reqStr("permission"); err != nil {
			return g, err
		}
		if g.On, err = m.strings("on"); err != nil {
			return g, err
		}
		if err := m.done(); err != nil {
			return g, err
		}
	default:
		return g, d.errf(n, "a grant must be a string or a mapping")
	}

	if !knownPermission(g.Permission) {
		e := d.errf(n, "unknown permission %q", g.Permission)
		if best, ok := closest(g.Permission, KnownPermissions); ok {
			e.Hint = fmt.Sprintf("did you mean %q?", best)
		} else {
			e.Hint = "known permissions: " + strings.Join(KnownPermissions, ", ")
		}
		// A misspelled permission grants nothing, which looks exactly like a
		// correctly-denied user. Catching it here is the only way anyone finds
		// out before an incident.
		return g, e
	}
	if len(g.On) == 0 {
		return g, d.errf(n, "grant %q names no environments", g.Permission)
	}
	return g, nil
}

func knownPermission(p string) bool {
	for _, k := range KnownPermissions {
		if k == p {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := unquote(strings.TrimSpace(p)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func decodeIdentities(d *decoder, parent *mapping, cfg *Config) error {
	n := parent.get("identities")
	if n == nil || n.IsNull() {
		return nil
	}
	m, err := d.mapping(n)
	if err != nil {
		return err
	}
	for _, user := range m.node.Keys {
		id := &Identity{User: user}
		ud := d.at(user)
		um, err := ud.mapping(m.get(user))
		if err != nil {
			return err
		}
		if id.SlackID, err = um.str("slack_id", ""); err != nil {
			return err
		}
		if id.Email, err = um.str("email", ""); err != nil {
			return err
		}
		if id.Roles, err = um.strings("roles"); err != nil {
			return err
		}
		if id.Disabled, err = um.boolean("disabled", false); err != nil {
			return err
		}
		if err := um.done(); err != nil {
			return err
		}
		for _, r := range id.Roles {
			if _, ok := cfg.Roles[r]; !ok {
				e := ud.errf(m.get(user), "user %q has unknown role %q", user, r)
				known := make([]string, 0, len(cfg.Roles))
				for k := range cfg.Roles {
					known = append(known, k)
				}
				sort.Strings(known)
				if best, ok := closest(r, known); ok {
					e.Hint = fmt.Sprintf("did you mean %q?", best)
				} else {
					e.Hint = "defined roles: " + strings.Join(known, ", ")
				}
				return e
			}
		}
		cfg.Identities[user] = id
	}
	return m.done()
}

// -------------------------------------------------------- cross-validation

// crossValidate checks the things that only make sense once the whole file is
// loaded: promotion chains, referenced environments, duplicate Slack ids.
func crossValidate(cfg *Config) error {
	for _, appName := range cfg.AppNames {
		app := cfg.Apps[appName]
		for _, envName := range app.EnvNames {
			env := app.Environments[envName]

			if env.PromoteFrom != "" {
				if _, ok := app.Environments[env.PromoteFrom]; !ok {
					e := &Error{File: cfg.SourceFile,
						Msg:  fmt.Sprintf("app %q environment %q promotes from %q, which is not an environment of this app", appName, envName, env.PromoteFrom),
						Path: "apps." + appName + ".environments." + envName}
					if best, ok := closest(env.PromoteFrom, app.EnvNames); ok {
						e.Hint = fmt.Sprintf("did you mean %q?", best)
					}
					return e
				}
			}

			// An approval gate naming a role nobody holds can never be
			// satisfied, and the deploy hangs in AWAIT_APPROVAL until it is
			// aborted by hand.
			for _, g := range env.Gates {
				if g.Type != GateApproval {
					continue
				}
				for _, role := range g.Roles {
					if _, ok := cfg.Roles[role]; !ok {
						return &Error{File: cfg.SourceFile,
							Msg:  fmt.Sprintf("approval gate for %s/%s names undefined role %q", appName, envName, role),
							Path: "apps." + appName + ".environments." + envName + ".gates",
							Hint: "a gate requiring a role nobody holds can never be satisfied"}
					}
					if !roleHeld(cfg, role) {
						return &Error{File: cfg.SourceFile,
							Msg:  fmt.Sprintf("approval gate for %s/%s requires role %q, which no identity holds", appName, envName, role),
							Path: "apps." + appName + ".environments." + envName + ".gates",
							Hint: "add the role to at least one entry under identities, or the deploy will wait forever"}
					}
				}
			}

			// Canary without per-version metrics is section 9's warning made
			// checkable: a canary whose criteria are all HTTP probes is
			// measuring the load balancer, not the canary.
			if env.Strategy.Type == StrategyCanary && len(env.Verify.Criteria) > 0 {
				allHTTP := true
				for _, c := range env.Verify.Criteria {
					if c.Verifier != "http" {
						allHTTP = false
					}
				}
				if allHTTP {
					return &Error{File: cfg.SourceFile,
						Msg:  fmt.Sprintf("%s/%s uses a canary strategy with only http criteria", appName, envName),
						Path: "apps." + appName + ".environments." + envName + ".verify",
						Hint: "an http probe cannot tell the canary apart from the rest of the fleet, so the canary weight is measuring nothing. Add a metrics verifier with a per-version query, or use blue-green."}
				}
			}

			if len(env.Verify.Criteria) > 0 && env.Verify.Bake <= 0 {
				return &Error{File: cfg.SourceFile,
					Msg:  fmt.Sprintf("%s/%s defines verification criteria but no bake window", appName, envName),
					Path: "apps." + appName + ".environments." + envName + ".verify",
					Hint: "set verify.bake, or the criteria are never evaluated"}
			}
		}
	}

	seenSlack := map[string]string{}
	for _, name := range sortedKeys(cfg.Identities) {
		id := cfg.Identities[name]
		if id.SlackID == "" {
			continue
		}
		if prev, dup := seenSlack[id.SlackID]; dup {
			return &Error{File: cfg.SourceFile,
				Msg:  fmt.Sprintf("slack_id %q is mapped to both %q and %q", id.SlackID, prev, name),
				Path: "identities",
				Hint: "one Slack account must map to exactly one internal user, or approvals are attributed to the wrong person"}
		}
		seenSlack[id.SlackID] = name
	}
	return nil
}

func roleHeld(cfg *Config, role string) bool {
	for _, id := range cfg.Identities {
		if id.Disabled {
			continue
		}
		for _, r := range id.Roles {
			if r == role {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --------------------------------------------------------------- utilities

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${VAR} from the environment.
//
// Section 12.2: secrets never live in the config file. This is how a token
// reaches the process without being written down next to it.
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

func durationOf(d *decoder, m *mapping, key string, def time.Duration) (time.Duration, error) {
	n := m.get(key)
	if n == nil || n.IsNull() {
		return def, nil
	}
	s, err := n.String()
	if err != nil {
		return 0, m.wrap(key, err)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		e := d.errf(n, "%q is not a duration", s)
		e.Hint = `durations look like "30s", "5m", "1h30m"`
		return 0, e
	}
	if v < 0 {
		return 0, d.errf(n, "duration %q must not be negative", s)
	}
	return v, nil
}
