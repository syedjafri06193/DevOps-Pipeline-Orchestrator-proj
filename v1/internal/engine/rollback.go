package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/store"
)

// Manifest is `.orch/manifest.yaml`, built into the artifact at CI time
// (design.md section 8.3).
//
// This is the data structure that makes rollback a checked operation rather
// than a button, and section 8 calls it "probably the most defensible thing in
// the whole project. Most deployment tools, commercial ones included, treat
// rollback as unconditional."
type Manifest struct {
	Version string `json:"version"`
	// SchemaVersion is the database schema this artifact expects.
	SchemaVersion int `json:"schema_version"`
	// RollbackFloor is the oldest version this one can safely roll back to.
	// Below it, an irreversible migration has run.
	RollbackFloor string      `json:"rollback_floor"`
	Migrations    []Migration `json:"migrations"`
}

type Migration struct {
	ID string `json:"id"`
	// Reversible is the field that matters. A false here is what turns a
	// rollback from an inconvenience into an outage.
	Reversible  bool `json:"reversible"`
	ExpandPhase bool `json:"expand_phase"`
}

// Irreversible returns the migrations that cannot be undone.
func (m *Manifest) Irreversible() []Migration {
	var out []Migration
	for _, mig := range m.Migrations {
		if !mig.Reversible {
			out = append(out, mig)
		}
	}
	return out
}

// Manifests resolves an artifact's manifest.
//
// An interface because where manifests live is deployment-specific: an OCI
// annotation, an S3 object beside the artifact, a file in the image. What
// matters to the engine is only that it can ask.
type Manifests interface {
	Manifest(ctx context.Context, app, version string) (*Manifest, error)
}

// UnsafeRollbackError is the refusal (section 8.3).
//
// "When rollback is unsafe, the tool must refuse and say why. Not warn --
// refuse. The correct action in that situation is roll-forward with a fix,
// and a tool that makes the unsafe path easy will get someone paged at 3am."
//
// Every field here exists to be printed: what it would have done, why it
// will not, and what to do instead.
type UnsafeRollbackError struct {
	From   string
	To     string
	Floor  string
	Reason string
	Remedy string
}

func (e *UnsafeRollbackError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to roll back %s -> %s: %s", e.From, e.To, e.Reason)
	if e.Floor != "" {
		fmt.Fprintf(&b, "\n  rollback floor: %s", e.Floor)
	}
	if e.Remedy != "" {
		fmt.Fprintf(&b, "\n  what to do instead: %s", e.Remedy)
	}
	return b.String()
}

// BlockedError is the circuit breaker (section 7.4).
type BlockedError struct {
	Reason string
}

func (e *BlockedError) Error() string { return e.Reason }

// Plan is a deployment about to be run.
type Plan struct {
	Target     config.AppEnv
	Version    string
	Previous   string
	IsRollback bool
	RollbackOf string
	Reason     string
}

// PlanRollback builds the rollback plan, or refuses (section 8.3).
func (e *Executor) PlanRollback(ctx context.Context, dep *store.Deployment, cfg *config.AppEnv) (*Plan, error) {
	target, err := e.store.LastSuccessfulBefore(ctx, dep.App, dep.Environment, dep.ID)
	if err != nil {
		return nil, &UnsafeRollbackError{
			From:   dep.Version,
			Reason: "there is no previous successful deployment of this target to roll back to",
			Remedy: "roll forward with a fix, or deploy a known-good version explicitly",
		}
	}

	if cfg.Rollback.CheckMigrationFloor {
		if err := e.checkMigrationFloor(ctx, dep, target); err != nil {
			return nil, err
		}
	}

	return &Plan{
		Target:     *cfg,
		Version:    target.Version,
		Previous:   dep.Version,
		IsRollback: true,
		RollbackOf: dep.ID,
		Reason:     fmt.Sprintf("rolling back to %s, last deployed successfully at %s", target.Version, target.CreatedAt.Format(time.RFC3339)),
	}, nil
}

// checkMigrationFloor is the refusal logic.
func (e *Executor) checkMigrationFloor(ctx context.Context, dep *store.Deployment, target *store.Deployment) error {
	if e.manifests == nil {
		// No manifest source configured. Refusing every rollback would make
		// the tool unusable for anyone who has not adopted manifests; running
		// every rollback blind would make the check a lie. Say which it is,
		// once, loudly, and proceed -- the operator configured it this way.
		e.log.Warn("no manifest source configured; the rollback migration floor cannot be checked",
			"app", dep.App, "environment", dep.Environment)
		return nil
	}

	cur, err := e.manifests.Manifest(ctx, dep.App, dep.Version)
	if err != nil {
		// A missing manifest is not permission to proceed. The whole point of
		// the check is that an unsafe rollback looks exactly like a safe one.
		return &UnsafeRollbackError{
			From: dep.Version, To: target.Version,
			Reason: fmt.Sprintf("the manifest for %s could not be read (%v), so its rollback floor is unknown", dep.Version, err),
			Remedy: "roll forward, or confirm by hand that no irreversible migration ran and use --force (requires deploy:override)",
		}
	}

	if cur.RollbackFloor != "" && versionLess(target.Version, cur.RollbackFloor) {
		return &UnsafeRollbackError{
			From: dep.Version, To: target.Version, Floor: cur.RollbackFloor,
			Reason: "the target predates the current version's rollback floor; an irreversible migration ran between them",
			Remedy: "roll forward with a fix, or run the documented manual recovery procedure for this migration",
		}
	}

	targetManifest, err := e.manifests.Manifest(ctx, target.App, target.Version)
	if err == nil && targetManifest.SchemaVersion != cur.SchemaVersion {
		if irreversible := cur.Irreversible(); len(irreversible) > 0 {
			ids := make([]string, len(irreversible))
			for i, m := range irreversible {
				ids[i] = m.ID
			}
			return &UnsafeRollbackError{
				From: dep.Version, To: target.Version, Floor: cur.RollbackFloor,
				Reason: fmt.Sprintf("schema moved from %d to %d and %s irreversible (section 8.1: the old code will run against the new schema)",
					targetManifest.SchemaVersion, cur.SchemaVersion, plural(ids)),
				Remedy: "roll forward with a fix",
			}
		}
	}
	return nil
}

func plural(ids []string) string {
	if len(ids) == 1 {
		return "migration " + ids[0] + " is"
	}
	return "migrations " + strings.Join(ids, ", ") + " are"
}

// versionLess compares two artifact versions.
//
// Artifact versions in this system look like "2026.09.14-a3f9c21": a
// sortable date prefix and a commit sha. Lexicographic comparison is correct
// for that shape and for semver-without-prereleases up to two digits per
// component, and it is wrong for "v9" vs "v10".
//
// So the comparison is done component-wise with numeric awareness, and where
// the two versions are not comparable at all it returns false -- which makes
// the caller treat the rollback as *below* the floor only when it can prove
// it, and never blocks on an unparseable pair. The document's `semverLess`
// leaves this unspecified and it is the kind of ambiguity that decides
// whether a 3am rollback is allowed.
func versionLess(a, b string) bool {
	if a == b {
		return false
	}
	as, bs := splitVersion(a), splitVersion(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aIsNum := as[i].num, as[i].isNum
		bn, bIsNum := bs[i].num, bs[i].isNum
		switch {
		case aIsNum && bIsNum:
			if an != bn {
				return an < bn
			}
		case as[i].text != bs[i].text:
			return as[i].text < bs[i].text
		}
	}
	return len(as) < len(bs)
}

type versionPart struct {
	text  string
	num   int64
	isNum bool
}

var versionSep = func(r rune) bool {
	return r == '.' || r == '-' || r == '+' || r == '_'
}

func splitVersion(v string) []versionPart {
	v = strings.TrimPrefix(v, "v")
	fields := strings.FieldsFunc(v, versionSep)
	out := make([]versionPart, 0, len(fields))
	for _, f := range fields {
		p := versionPart{text: f}
		if n, err := parseInt(f); err == nil {
			p.num, p.isNum = n, true
		}
		out = append(out, p)
	}
	return out
}

func parseInt(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// CheckRollbackBudget is section 7.4's circuit breaker.
//
//	"If a version has been deployed and rolled back twice, stop trying.
//	Automatic retry of a known-bad deploy wastes error budget and confuses
//	everyone watching Slack."
func (e *Executor) CheckRollbackBudget(ctx context.Context, app, env, version string, max int) error {
	n, err := e.store.CountRollbacks(ctx, app, env, version, 24*time.Hour)
	if err != nil {
		return err
	}
	if n >= max {
		return &BlockedError{
			Reason: fmt.Sprintf(
				"version %s has been rolled back %d times in the last 24h; deploy blocked. "+
					"Override with --force (requires deploy:override)",
				version, n),
		}
	}
	return nil
}

// ------------------------------------------------------- manifest sources
//
// The directory-backed source lives in manifests.go.

// MapManifests is an in-memory source, for tests.
type MapManifests struct {
	Manifests map[string]*Manifest
	Err       map[string]error
}

func NewMapManifests() *MapManifests {
	return &MapManifests{Manifests: map[string]*Manifest{}, Err: map[string]error{}}
}

func (m *MapManifests) Add(app string, man *Manifest) {
	m.Manifests[app+"/"+man.Version] = man
}

func (m *MapManifests) Manifest(ctx context.Context, app, version string) (*Manifest, error) {
	key := app + "/" + version
	if err, ok := m.Err[key]; ok {
		return nil, err
	}
	man, ok := m.Manifests[key]
	if !ok {
		return nil, fmt.Errorf("no manifest for %s", key)
	}
	return man, nil
}

// SortedVersions is a helper for the CLI's rollback target listing.
func SortedVersions(vs []string) []string {
	out := append([]string(nil), vs...)
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}
