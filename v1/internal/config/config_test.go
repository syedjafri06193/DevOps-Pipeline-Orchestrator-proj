package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The acceptance criterion for M0 (section 16) is not "the parser works" but
// "a malformed config produces a message a stranger could act on". Most of
// this file is therefore about error text, not about successful parses.

func load(t *testing.T, src string) (*Config, error) {
	t.Helper()
	return Decode(src, "orch.yaml")
}

func mustLoad(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := load(t, src)
	if err != nil {
		t.Fatalf("expected the config to load, got: %v", err)
	}
	return cfg
}

// A minimal config every test can extend.
const base = `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
`

func TestTheShippedExampleLoads(t *testing.T) {
	// The example in examples/ is the documentation. If it stops loading,
	// every reader's first experience is a parse error.
	cfg, err := Load(filepath.Join("..", "..", "examples", "orch.yaml"))
	if err != nil {
		t.Fatalf("examples/orch.yaml does not load: %v", err)
	}
	if len(cfg.AppNames) != 1 || cfg.AppNames[0] != "web" {
		t.Fatalf("apps = %v", cfg.AppNames)
	}
	prod, err := cfg.AppEnv("web", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if prod.Strategy.Type != StrategyCanary {
		t.Errorf("prod strategy = %q", prod.Strategy.Type)
	}
	if got := len(prod.Strategy.Steps); got != 5 {
		t.Errorf("canary steps = %d, want 5", got)
	}
	if prod.PromoteFrom != "staging" {
		t.Errorf("promote_from = %q", prod.PromoteFrom)
	}
	if len(prod.Gates) != 2 {
		t.Errorf("gates = %d, want 2", len(prod.Gates))
	}
	if !prod.Rollback.CheckMigrationFloor {
		t.Error("check_migration_floor should be on")
	}
}

// ------------------------------------------------------- unknown keys

func TestUnknownKeyIsAnErrorNotADefault(t *testing.T) {
	// This is the headline rule of section 13, and the specific typo the
	// document names.
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
        verify:
          bake: 5m
          failure_threshhold: 3
          criteria:
            - name: x
              verifier: http
              expect_status: 200
`)
	if err == nil {
		t.Fatal("a typo'd key must be an error, not a silent default")
	}
	msg := err.Error()
	for _, want := range []string{
		"orch.yaml", // which file
		"10:11",     // where in it
		`unknown key "failure_threshhold"`,
		`did you mean "failure_threshold"?`, // what to do
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q:\n%s", want, msg)
		}
	}
}

func TestUnknownKeyNamesItsPath(t *testing.T) {
	_, err := load(t, base+`        strategy: { type: rolling, batch_sizes: 50% }
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "apps.web.environments.dev.strategy") {
		t.Errorf("error should name where it is:\n%v", err)
	}
}

func TestASuggestionIsOnlyOfferedWhenItIsClose(t *testing.T) {
	// Suggesting "strategy" for "kumquat" sends the reader to the wrong
	// place, which is worse than saying nothing.
	_, err := load(t, base+`        kumquat: 3
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("no suggestion should be offered for a distant key:\n%v", err)
	}
	if !strings.Contains(err.Error(), "valid keys here:") {
		t.Errorf("without a suggestion, list the valid keys:\n%v", err)
	}
}

func TestSeveralUnknownKeysAreAllReported(t *testing.T) {
	_, err := load(t, base+`        stratagy: rolling
        verfy: 3
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "2 other unknown keys") && !strings.Contains(err.Error(), "1 other unknown keys") {
		t.Errorf("fixing one typo at a time is a bad loop:\n%v", err)
	}
}

func TestADuplicateKeyIsRefused(t *testing.T) {
	// The last value silently winning is how the wrong thing reaches prod.
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
        provider: exec
`)
	if err == nil {
		t.Fatal("a duplicate key must be refused")
	}
	if !strings.Contains(err.Error(), "duplicate key") || !strings.Contains(err.Error(), "line 7") {
		t.Errorf("the error should name the first definition:\n%v", err)
	}
}

// ------------------------------------------------------------ the parser

func TestParserHandlesTheSchemasShapes(t *testing.T) {
	cfg := mustLoad(t, `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
        config: { cluster: dev, service: web }
        strategy:
          type: canary
          steps:
            - { setWeight: 5 }
            - { verify: { bake: 5m } }
            - { setWeight: 100 }
        verify:
          bake: 10m
          sample_interval: 30s
          min_samples: 3
          criteria:
            - name: error-rate
              verifier: prometheus
              query: |
                sum(rate(errors[2m]))
                / sum(rate(total[2m]))
              max: 0.01
              max_ratio: 1.5
              floor_value: 0.001
`)
	dev, err := cfg.AppEnv("web", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.ProviderCfg["cluster"] != "dev" || dev.ProviderCfg["service"] != "web" {
		t.Errorf("inline mapping: %v", dev.ProviderCfg)
	}
	if len(dev.Strategy.Steps) != 3 {
		t.Fatalf("steps = %d", len(dev.Strategy.Steps))
	}
	if dev.Strategy.Steps[1].Verify.Bake != 5*time.Minute {
		t.Errorf("nested inline mapping: %v", dev.Strategy.Steps[1].Verify)
	}
	q := dev.Verify.Criteria[0].Settings["query"]
	if !strings.Contains(q, "\n") || !strings.Contains(q, "sum(rate(errors[2m]))") {
		t.Errorf("block scalar did not survive: %q", q)
	}
}

func TestCommentsInsideQuotesSurvive(t *testing.T) {
	cfg := mustLoad(t, `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
        config:
          note: "a # inside quotes is not a comment"   # this one is
`)
	dev, _ := cfg.AppEnv("web", "dev")
	if got := dev.ProviderCfg["note"]; got != "a # inside quotes is not a comment" {
		t.Errorf("note = %q", got)
	}
}

func TestUnsupportedYamlFeaturesSayWhatTheyAre(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"anchor", "version: 1\ndefaults: &d\n  a: b\n", "anchors are not supported"},
		{"alias", "version: 1\napps: *d\n", "aliases are not supported"},
		{"merge", "version: 1\napps:\n  <<: other\n", "merge keys are not supported"},
		{"tab", "version: 1\napps:\n\tweb: x\n", "tab character in indentation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.src)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got:\n%v", tc.want, err)
			}
		})
	}
}

func TestBadIndentationIsExplained(t *testing.T) {
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      dev:
        provider: fake
         strategy: { type: rolling }
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "indent") {
		t.Errorf("indentation errors must say so:\n%v", err)
	}
}

// ----------------------------------------------------- semantic refusals

func TestMaxRatioWithoutFloorValueIsRefused(t *testing.T) {
	// Section 7.2 is emphatic: without a floor, 0.001% -> 0.004% trips a
	// ratio check and rolls back a good deploy. A warning would be ignored.
	_, err := load(t, base+`        verify:
          bake: 5m
          criteria:
            - name: error-rate
              verifier: prometheus
              query: up
              max_ratio: 2.0
`)
	if err == nil {
		t.Fatal("max_ratio without floor_value must be refused")
	}
	if !strings.Contains(err.Error(), "floor_value") || !strings.Contains(err.Error(), "0.001") {
		t.Errorf("the error should explain the failure it prevents:\n%v", err)
	}
}

func TestAMaxRatioBelowOneIsRefused(t *testing.T) {
	_, err := load(t, base+`        verify:
          bake: 5m
          criteria:
            - name: e
              verifier: prometheus
              query: up
              max_ratio: 0.9
              floor_value: 0.001
`)
	if err == nil || !strings.Contains(err.Error(), "never be any worse") {
		t.Fatalf("a ratio below 1 can never pass:\n%v", err)
	}
}

func TestACriterionWithNoThresholdIsRefused(t *testing.T) {
	_, err := load(t, base+`        verify:
          bake: 5m
          criteria:
            - name: e
              verifier: prometheus
`)
	if err == nil || !strings.Contains(err.Error(), "no threshold") {
		t.Fatalf("a criterion that can never fail is a mistake:\n%v", err)
	}
}

func TestABakeTooShortForMinSamplesIsRefused(t *testing.T) {
	// Otherwise every verification is Inconclusive, which in production
	// reads as "verification is broken" and nobody knows why.
	_, err := load(t, base+`        verify:
          bake: 1m
          sample_interval: 30s
          min_samples: 10
          criteria:
            - name: e
              verifier: http
              expect_status: 200
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Inconclusive") {
		t.Errorf("explain what would happen:\n%v", err)
	}
}

func TestCriteriaWithoutABakeWindowAreRefused(t *testing.T) {
	_, err := load(t, base+`        verify:
          criteria:
            - name: e
              verifier: http
              expect_status: 200
`)
	if err == nil || !strings.Contains(err.Error(), "never evaluated") {
		t.Fatalf("criteria that never run are worse than none:\n%v", err)
	}
}

func TestACanaryMustEndAtFullWeight(t *testing.T) {
	// Otherwise a fully-passing canary leaves the new version on a fraction
	// of traffic forever and reports success.
	_, err := load(t, base+`        strategy:
          type: canary
          steps:
            - { setWeight: 5 }
            - { verify: { bake: 5m } }
            - { setWeight: 50 }
`)
	if err == nil || !strings.Contains(err.Error(), "setWeight: 100") {
		t.Fatalf("expected a refusal:\n%v", err)
	}
}

func TestACanaryStepDoesOneThing(t *testing.T) {
	_, err := load(t, base+`        strategy:
          type: canary
          steps:
            - { setWeight: 5, verify: { bake: 1m } }
            - { setWeight: 100 }
`)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected a refusal:\n%v", err)
	}
}

func TestStepsOnANonCanaryStrategyAreRefused(t *testing.T) {
	_, err := load(t, base+`        strategy:
          type: rolling
          steps:
            - { setWeight: 100 }
`)
	if err == nil || !strings.Contains(err.Error(), "only valid for a canary") {
		t.Fatalf("expected a refusal:\n%v", err)
	}
}

func TestACanaryMeasuredOnlyByHttpIsRefused(t *testing.T) {
	// Section 9: "If your metrics aren't tagged by version, your canary error
	// rate is actually the whole fleet's error rate diluted by 90% of healthy
	// traffic, and you will never detect anything."
	_, err := load(t, base+`        strategy:
          type: canary
          steps:
            - { setWeight: 10 }
            - { verify: { bake: 5m } }
            - { setWeight: 100 }
        verify:
          bake: 5m
          sample_interval: 30s
          min_samples: 3
          criteria:
            - name: up
              verifier: http
              url: https://example.com/healthz
              expect_status: 200
`)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "measuring nothing") {
		t.Errorf("explain why:\n%v", err)
	}
}

func TestAnInvalidStrategyListsTheAlternatives(t *testing.T) {
	_, err := load(t, base+`        strategy: { type: bluegreen }
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `did you mean "blue-green"`) {
		t.Errorf("want a suggestion:\n%v", err)
	}
}

func TestBatchSizeMustBeACountOrPercentage(t *testing.T) {
	_, err := load(t, base+`        strategy: { type: rolling, batch_size: half }
`)
	if err == nil || !strings.Contains(err.Error(), "50%") {
		t.Fatalf("show the shape:\n%v", err)
	}
}

// --------------------------------------------------------------- gates

func TestAnApprovalGateMustNameRoles(t *testing.T) {
	_, err := load(t, base+`        gates:
          - type: approval
            require_peer: true
`)
	if err == nil || !strings.Contains(err.Error(), "roles") {
		t.Fatalf("an approval gate with no roles authorises everyone:\n%v", err)
	}
}

func TestAGateRequiringARoleNobodyHoldsIsRefused(t *testing.T) {
	// The deploy would sit in AWAIT_APPROVAL until someone aborted it.
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
        gates:
          - type: approval
            roles: [release-manager]
            ttl: 1h
roles:
  release-manager:
    - deploy:approve   on: ["*"]
identities:
  jordan:
    slack_id: U1
    roles: [developer]
`)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// The unknown role on the identity is caught first, which is also correct.
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("want a role error:\n%v", err)
	}
}

func TestAGateWithNoHolderOfItsRoleIsRefused(t *testing.T) {
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
        gates:
          - type: approval
            roles: [sre]
            ttl: 1h
roles:
  sre:
    - deploy:approve   on: ["*"]
  developer:
    - deploy:view      on: ["*"]
identities:
  jordan:
    slack_id: U1
    roles: [developer]
`)
	if err == nil || !strings.Contains(err.Error(), "no identity holds") {
		t.Fatalf("expected a refusal naming the empty role:\n%v", err)
	}
}

func TestApprovalTtlMustBePositive(t *testing.T) {
	_, err := load(t, base+`        gates:
          - type: approval
            roles: [sre]
            ttl: 0s
`)
	if err == nil || !strings.Contains(err.Error(), "expire") {
		t.Fatalf("expected a refusal explaining approval expiry:\n%v", err)
	}
}

func TestDenyWindows(t *testing.T) {
	cfg := mustLoad(t, base+`        gates:
          - type: schedule
            deny: ["Fri 16:00-23:59", "Sat"]
            timezone: America/Los_Angeles
`)
	dev, _ := cfg.AppEnv("web", "dev")
	g := dev.Gates[0]
	if len(g.Deny) != 2 {
		t.Fatalf("windows = %d", len(g.Deny))
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	fridayEvening := time.Date(2026, 9, 18, 17, 0, 0, 0, loc)
	fridayMorning := time.Date(2026, 9, 18, 9, 0, 0, 0, loc)
	if !g.Deny[0].Contains(fridayEvening) {
		t.Error("Friday 17:00 should be denied")
	}
	if g.Deny[0].Contains(fridayMorning) {
		t.Error("Friday 09:00 should be allowed")
	}
	saturday := time.Date(2026, 9, 19, 3, 0, 0, 0, loc)
	if !g.Deny[1].Contains(saturday) {
		t.Error("a bare day name should cover the whole day")
	}
}

func TestAWindowCrossingMidnightBecomesTwo(t *testing.T) {
	// A naive start<=m<=end silently matches nothing for "Fri 22:00-02:00".
	ws, err := ParseDenyWindow("Fri 22:00-02:00")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("windows = %d, want 2", len(ws))
	}
	if ws[0].Day != time.Friday || ws[1].Day != time.Saturday {
		t.Errorf("days = %v, %v", ws[0].Day, ws[1].Day)
	}
	loc := time.UTC
	if !ws[1].Contains(time.Date(2026, 9, 19, 1, 0, 0, 0, loc)) {
		t.Error("Saturday 01:00 should be covered")
	}
}

func TestAnUnknownTimezoneIsRefused(t *testing.T) {
	_, err := load(t, base+`        gates:
          - type: schedule
            deny: ["Sat"]
            timezone: Pacific/Nowhere
`)
	if err == nil || !strings.Contains(err.Error(), "IANA") {
		t.Fatalf("expected a refusal with a hint:\n%v", err)
	}
}

// ---------------------------------------------------------------- RBAC

func TestRolesAndPermissions(t *testing.T) {
	cfg := mustLoad(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
roles:
  developer:
    - deploy:trigger    on: [dev, staging]
    - deploy:view       on: ["*"]
  sre:
    - deploy:override   on: ["*"]
identities:
  jordan:
    slack_id: U01JORDAN
    roles: [developer]
  alex:
    slack_id: U03ALEX
    roles: [sre]
`)
	jordan := cfg.Identities["jordan"]
	if !cfg.Can(jordan, "deploy:trigger", "staging") {
		t.Error("jordan should be able to deploy to staging")
	}
	if cfg.Can(jordan, "deploy:trigger", "prod") {
		t.Error("jordan must not be able to deploy to prod")
	}
	if cfg.Can(jordan, "deploy:override", "dev") {
		t.Error("permissions are separate, not a ladder")
	}
	if !cfg.Can(jordan, "deploy:view", "prod") {
		t.Error(`"*" should cover prod`)
	}
}

func TestAMisspelledPermissionIsRefused(t *testing.T) {
	// It would grant nothing, which looks exactly like a correctly-denied
	// user. Nobody finds out until an incident.
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
roles:
  developer:
    - deploy:trigers    on: [dev]
`)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), `did you mean "deploy:trigger"`) {
		t.Errorf("want a suggestion:\n%v", err)
	}
}

func TestADisabledIdentityCanDoNothing(t *testing.T) {
	cfg := mustLoad(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
roles:
  sre:
    - deploy:approve   on: ["*"]
identities:
  alex:
    slack_id: U03ALEX
    roles: [sre]
    disabled: true
`)
	if cfg.Can(cfg.Identities["alex"], "deploy:approve", "prod") {
		t.Error("a disabled identity must hold no permissions")
	}
	if _, ok := cfg.IdentityBySlackID("U03ALEX"); ok {
		t.Error("a disabled identity must not resolve from Slack")
	}
}

func TestAnUnknownSlackIdDoesNotResolve(t *testing.T) {
	cfg := mustLoad(t, base)
	if _, ok := cfg.IdentityBySlackID("U-whoever"); ok {
		t.Error("an unmapped Slack account must not resolve to a user")
	}
}

func TestOneSlackAccountMapsToOneUser(t *testing.T) {
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      prod:
        provider: fake
roles:
  sre:
    - deploy:approve   on: ["*"]
identities:
  alex:
    slack_id: U1
    roles: [sre]
  alexandra:
    slack_id: U1
    roles: [sre]
`)
	if err == nil || !strings.Contains(err.Error(), "wrong person") {
		t.Fatalf("approvals would be attributed to the wrong person:\n%v", err)
	}
}

// -------------------------------------------------------------- promotion

func TestPromoteFromMustNameARealEnvironment(t *testing.T) {
	_, err := load(t, `
version: 1
apps:
  web:
    environments:
      staging:
        provider: fake
      prod:
        provider: fake
        promote_from: stagng
`)
	if err == nil || !strings.Contains(err.Error(), `did you mean "staging"`) {
		t.Fatalf("want a suggestion:\n%v", err)
	}
}

// ----------------------------------------------------------------- slack

func TestHttpSlackModeRequiresASigningSecret(t *testing.T) {
	_, err := load(t, base+`
notifiers:
  slack:
    mode: http
    bot_token: xoxb-test
`)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "anyone on the internet") {
		t.Errorf("say what the risk is:\n%v", err)
	}
}

func TestAnUnknownNotifyEventIsRefused(t *testing.T) {
	_, err := load(t, base+`        notify:
          slack:
            channel: "#deploys"
            on: [started, suceeded]
`)
	if err == nil || !strings.Contains(err.Error(), `did you mean "succeeded"`) {
		t.Fatalf("want a suggestion:\n%v", err)
	}
}

func TestEnvSubstitution(t *testing.T) {
	t.Setenv("TEST_ORCH_TOKEN", "xoxb-secret")
	cfg := mustLoad(t, base+`
notifiers:
  slack:
    mode: socket
    bot_token: ${TEST_ORCH_TOKEN}
    app_token: xapp-test
`)
	if cfg.Notifiers.Slack.BotToken != "xoxb-secret" {
		t.Errorf("bot_token = %q", cfg.Notifiers.Slack.BotToken)
	}
}

// --------------------------------------------------------------- lookups

func TestAnUnknownAppSuggests(t *testing.T) {
	cfg := mustLoad(t, base)
	_, err := cfg.AppEnv("wbe", "dev")
	if err == nil || !strings.Contains(err.Error(), `did you mean "web"`) {
		t.Fatalf("want a suggestion:\n%v", err)
	}
}

func TestAnUnknownEnvironmentListsTheRealOnes(t *testing.T) {
	cfg := mustLoad(t, base)
	_, err := cfg.AppEnv("web", "production")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "known environments: dev") {
		t.Errorf("list what exists:\n%v", err)
	}
}

func TestDefaultsAreTheSafeOnes(t *testing.T) {
	cfg := mustLoad(t, base)
	dev, _ := cfg.AppEnv("web", "dev")
	if !dev.Rollback.CheckMigrationFloor {
		t.Error("the migration floor check must be on by default")
	}
	if dev.Verify.FailureThreshold < 2 {
		t.Error("a single bad sample must not be enough to roll back by default")
	}
	if dev.Verify.MinSamples < 1 {
		t.Error("min_samples must default to at least 1")
	}
}
