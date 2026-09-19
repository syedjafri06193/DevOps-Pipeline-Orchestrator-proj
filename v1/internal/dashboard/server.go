package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/config"
	"github.com/syedjafri06193/orch/internal/store"
)

// The dashboard is compiled into the binary (design.md section 3).
//
//	"The dashboard is compiled into the binary with embed.FS. Deployment is:
//	copy one file, run one systemd unit. That simplicity is a feature and
//	should be in the README's first paragraph."
//
//go:embed templates/*.html static/*
var assets embed.FS

// Server renders the pages from section 11.2.
type Server struct {
	Store   *store.Store
	Config  *config.Config
	Bus     *Bus
	Log     *slog.Logger
	Version string
	NodeID  string
	Now     func() time.Time

	tmpl map[string]*template.Template
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Routes builds the handler, parsing templates once at startup.
//
// Parsing at startup rather than per request means a broken template is a
// failure to start rather than a 500 during an incident, which is when
// someone is most likely to open the dashboard.
func (s *Server) Routes() (http.Handler, error) {
	if err := s.parse(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /deployments", s.history)
	mux.HandleFunc("GET /deployments/{id}", s.deployment)
	mux.HandleFunc("GET /audit", s.audit)
	mux.HandleFunc("GET /config", s.configPage)
	mux.HandleFunc("GET /events/{topic}", func(w http.ResponseWriter, r *http.Request) {
		s.Bus.Stream(w, r, r.PathValue("topic"))
	})
	return mux, nil
}

func (s *Server) parse() error {
	funcs := template.FuncMap{
		"stateClass": stateClass,
		"stepClass":  stepClass,
		"since":      func(t time.Time) string { return humanSince(t, s.now()) },
		"createdAt":  func(d *store.Deployment) time.Time { return d.CreatedAt },
		"duration": func(d *store.Deployment) string {
			if d.FinishedAt == nil {
				return "running"
			}
			return humanDuration(d.FinishedAt.Sub(d.CreatedAt))
		},
		"pages": func(st store.State) bool { return st.Pages() },
		"shortHash": func(h string) string {
			if len(h) > 12 {
				return h[:12]
			}
			return h
		},
		"detail": renderDetail,
	}

	s.tmpl = map[string]*template.Template{}
	for _, page := range []string{"overview", "deployment", "history", "audit", "config"} {
		t, err := template.New("layout").Funcs(funcs).ParseFS(assets,
			"templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return fmt.Errorf("dashboard: parsing %s: %w", page, err)
		}
		s.tmpl[page] = t
	}
	return nil
}

type pageData struct {
	Page      string
	Title     string
	Version   string
	NodeID    string
	AuditHead string
}

func (s *Server) base(page, title string) pageData {
	return pageData{
		Page: page, Title: title, Version: s.Version,
		NodeID: s.NodeID, AuditHead: s.Store.AuditHead(),
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil && s.Log != nil {
		// The response is already partly written by this point, so there is
		// nothing useful to send the browser. Log it and move on.
		s.Log.Error("rendering dashboard page", "page", page, "error", err)
	}
}

// ------------------------------------------------------------- overview

type overviewData struct {
	pageData
	Status  []envStatus
	Recent  []*store.Deployment
	Freezes []*store.Freeze
	Paging  []pagingItem
	Dora    doraStats
}

type envStatus struct {
	App          string
	Environment  string
	Version      string
	State        store.State
	Since        *time.Time
	Frozen       bool
	FreezeReason string
}

type pagingItem struct {
	ID          string
	App         string
	Environment string
	State       store.State
	Error       string
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := overviewData{pageData: s.base("overview", "Overview")}

	for _, appName := range s.Config.AppNames {
		app := s.Config.Apps[appName]
		for _, envName := range app.EnvNames {
			st := envStatus{App: appName, Environment: envName}
			if f, frozen := s.Store.Freeze(ctx, "app:"+appName+"/env:"+envName); frozen {
				st.Frozen, st.FreezeReason = true, f.Reason
			}
			if active, err := s.Store.ActiveDeployment(ctx, appName, envName); err == nil {
				st.Version, st.State, st.Since = active.Version, active.State, &active.CreatedAt
			} else {
				recent, _ := s.Store.ListDeployments(ctx, store.ListFilter{
					App: appName, Environment: envName, Limit: 1})
				if len(recent) > 0 {
					st.Version, st.State = recent[0].Version, recent[0].State
					st.Since = recent[0].FinishedAt
				}
			}
			data.Status = append(data.Status, st)
		}
	}

	data.Recent, _ = s.Store.ListDeployments(ctx, store.ListFilter{Limit: 15})
	data.Freezes = s.Store.Freezes(ctx)

	// Anything in a state that pages goes at the top, because that is the
	// only thing on this page that needs acting on right now.
	all, _ := s.Store.ListDeployments(ctx, store.ListFilter{Limit: 200})
	for _, d := range all {
		if d.State.Pages() {
			data.Paging = append(data.Paging, pagingItem{
				ID: d.ID, App: d.App, Environment: d.Environment,
				State: d.State, Error: d.Error,
			})
		}
	}

	data.Dora = s.dora(ctx, 30*24*time.Hour)
	s.render(w, "overview", data)
}

// doraStats are section 19.1's "essentially for free" metrics.
type doraStats struct {
	Window            string
	Frequency         string
	ChangeFailureRate string
	MedianDuration    string
	MTTR              string
}

func (s *Server) dora(ctx context.Context, window time.Duration) doraStats {
	out := doraStats{Window: humanDuration(window), Frequency: "—",
		ChangeFailureRate: "—", MedianDuration: "—", MTTR: "—"}

	deps, err := s.Store.ListDeployments(ctx, store.ListFilter{Limit: 1000})
	if err != nil {
		return out
	}
	cutoff := s.now().Add(-window)

	var total, failures int
	var durations []time.Duration
	var recoveries []time.Duration
	for _, d := range deps {
		if d.CreatedAt.Before(cutoff) || d.State.Active() {
			continue
		}
		total++
		if d.FinishedAt != nil {
			durations = append(durations, d.FinishedAt.Sub(d.CreatedAt))
		}
		switch d.State {
		case store.StateRolledBack, store.StateRollbackFailed, store.StateFailed, store.StateUnknown:
			failures++
			if d.FinishedAt != nil {
				// Time from the deploy starting to it being resolved, which
				// is the closest honest proxy for MTTR from this data.
				recoveries = append(recoveries, d.FinishedAt.Sub(d.CreatedAt))
			}
		}
	}
	if total == 0 {
		return out
	}

	days := window.Hours() / 24
	out.Frequency = fmt.Sprintf("%.1f/day", float64(total)/days)
	out.ChangeFailureRate = fmt.Sprintf("%.0f%% (%d of %d)",
		100*float64(failures)/float64(total), failures, total)
	if len(durations) > 0 {
		out.MedianDuration = humanDuration(median(durations))
	}
	if len(recoveries) > 0 {
		out.MTTR = humanDuration(median(recoveries))
	}
	return out
}

func median(ds []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// ------------------------------------------------------------ deployment

type deploymentData struct {
	pageData
	Deployment *store.Deployment
	Steps      []*store.Step
	Samples    []*store.HealthSample
	Approvals  []*store.Approval
	Log        string
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dep, err := s.Store.Deployment(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, "deployment not found", http.StatusNotFound)
		return
	}
	data := deploymentData{
		pageData:   s.base("deployment", dep.App+" → "+dep.Environment),
		Deployment: dep,
	}
	data.Steps, _ = s.Store.Steps(ctx, dep.ID)
	data.Samples, _ = s.Store.Samples(ctx, dep.ID)
	data.Approvals, _ = s.Store.Approvals(ctx, dep.ID)

	var b strings.Builder
	for _, st := range data.Steps {
		if st.Output != "" {
			b.WriteString(st.Output)
		}
	}
	data.Log = b.String()
	if data.Log == "" {
		data.Log = "(no output yet)"
	}
	s.render(w, "deployment", data)
}

// --------------------------------------------------------------- history

type historyData struct {
	pageData
	Recent []*store.Deployment
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	data := historyData{pageData: s.base("history", "History")}
	data.Recent, _ = s.Store.ListDeployments(r.Context(), store.ListFilter{
		App:         r.URL.Query().Get("app"),
		Environment: r.URL.Query().Get("environment"),
		Limit:       200,
	})
	s.render(w, "history", data)
}

// ----------------------------------------------------------------- audit

type auditData struct {
	pageData
	Entries     []*store.AuditEntry
	Valid       bool
	VerifyError string
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	data := auditData{pageData: s.base("audit", "Audit log")}
	data.Entries, _ = s.Store.AuditEntries(r.Context(), 300)
	if err := s.Store.VerifyAudit(r.Context()); err != nil {
		data.VerifyError = err.Error()
	} else {
		data.Valid = true
	}
	s.render(w, "audit", data)
}

// ---------------------------------------------------------------- config

type configData struct {
	pageData
	SourceFile   string
	SourceCommit string
	Rendered     string
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	data := configData{
		pageData:     s.base("config", "Config"),
		SourceFile:   s.Config.SourceFile,
		SourceCommit: s.Config.SourceCommit,
	}
	// Rendered from the loaded structure rather than by echoing the file, so
	// what is shown is what the process is actually running -- which is the
	// question someone opens this page to answer.
	redacted := *s.Config
	if redacted.Notifiers.Slack != nil {
		cp := *redacted.Notifiers.Slack
		cp.BotToken, cp.AppToken, cp.SigningSecret = mask(cp.BotToken), mask(cp.AppToken), mask(cp.SigningSecret)
		redacted.Notifiers.Slack = &cp
	}
	out, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		data.Rendered = "could not render configuration: " + err.Error()
	} else {
		data.Rendered = string(out)
	}
	s.render(w, "config", data)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "[set]"
}

// --------------------------------------------------------------- helpers

func stateClass(s store.State) string {
	switch s {
	case store.StateSucceeded:
		return "ok"
	case store.StateFailed, store.StateRollbackFailed, store.StateUnknown:
		return "bad"
	case store.StateRolledBack, store.StateAborted, store.StateAwaitApproval:
		return "warn"
	case store.StatePending:
		return "idle"
	default:
		return "run"
	}
}

func stepClass(s string) string {
	switch s {
	case "ok":
		return "ok"
	case "failed":
		return "bad"
	default:
		return "run"
	}
}

func humanSince(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	return humanDuration(d) + " ago"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func renderDetail(d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, " ")
}
