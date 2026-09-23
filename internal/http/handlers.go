// Package http renders the DevPulse dashboard. Handlers read exclusively from
// PostgreSQL: changing the date filter never triggers a GitHub API call.
package http

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
	"github.com/relaxart/dev-pulse/internal/report"
)

// AppName is shown in the header and the page titles.
const AppName = "DevPulse"

// layoutData is shared by every page.
type layoutData struct {
	AppName       string
	Title         string
	Nav           string
	Path          string
	Org           string
	OrgSynced     bool
	Range         metrics.Range
	RangeError    string
	PeriodOptions []metrics.PeriodOption
	Query         url.Values
	LastSync      *time.Time
	SyncStatus    string
	SyncRunning   bool
	NextSync      time.Time
	Now           time.Time
}

// pageContext resolves the organization scope and the date filter for a request.
type pageContext struct {
	Layout layoutData
	Org    *models.Organization
	Filter database.Filter
	// Team is the resolved ?team= selection, nil when unfiltered.
	Team *models.Team
	// Teams is the organization's team list, for the filter menu.
	Teams []models.Team
}

// resolveTeam loads the team list and the current selection. A team filter is
// only offered on the pages that support it, so callers opt in.
func (s *Server) resolveTeam(r *http.Request, pc *pageContext) error {
	if pc.Org == nil {
		return nil
	}
	teams, err := s.db.ListTeams(r.Context(), pc.Org.ID)
	if err != nil {
		return err
	}
	pc.Teams = teams

	slug := strings.TrimSpace(r.URL.Query().Get("team"))
	if slug == "" {
		return nil
	}
	// An unknown slug degrades to the unfiltered view rather than erroring: a
	// team can be renamed or deleted after a link was shared.
	team, err := s.db.TeamBySlug(r.Context(), pc.Org.ID, slug)
	if err != nil {
		return err
	}
	if team == nil {
		return nil
	}
	pc.Team = team
	pc.Filter.TeamID = &team.ID
	return nil
}

func (s *Server) context(w http.ResponseWriter, r *http.Request, nav string) (*pageContext, bool) {
	q := r.URL.Query()
	rng, rangeErr := metrics.ResolveRange(q.Get("period"), q.Get("from"), q.Get("to"), time.Now())

	pc := &pageContext{
		Layout: layoutData{
			AppName:       AppName,
			Nav:           nav,
			Path:          r.URL.Path,
			Org:           s.cfg.GitHubOrg,
			Range:         rng,
			PeriodOptions: metrics.PeriodOptions(),
			Query:         rng.Query(),
			NextSync:      s.worker.NextRun(),
			SyncRunning:   s.worker.Running(),
			Now:           time.Now().UTC(),
		},
	}
	if rangeErr != nil {
		pc.Layout.RangeError = rangeErr.Error()
	}

	// The organization row only exists once the first synchronization has run.
	org, err := s.db.OrganizationByLogin(r.Context(), s.cfg.GitHubOrg)
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	if org == nil {
		pc.Layout.SyncStatus = "waiting for first synchronization"
		return pc, true
	}

	pc.Org = org
	pc.Layout.OrgSynced = true
	pc.Layout.LastSync = org.LastSyncedAt
	pc.Filter = database.Filter{
		OrganizationID:  org.ID,
		From:            rng.From,
		To:              rng.To,
		IncludeArchived: s.cfg.IncludeArchived,
		ExcludedLogins:  s.cfg.ExcludedUsers,
	}

	if run, err := s.db.LastSyncRun(r.Context(), org.ID); err == nil && run != nil {
		pc.Layout.SyncStatus = run.Status
		if run.Status == models.SyncStatusRunning {
			pc.Layout.SyncRunning = true
		}
	}
	return pc, true
}

type chartSeries struct {
	Labels    []string `json:"labels"`
	Commits   []int64  `json:"commits"`
	Additions []int64  `json:"additions"`
	Deletions []int64  `json:"deletions"`
	PRsOpened []int64  `json:"prsOpened"`
	PRsMerged []int64  `json:"prsMerged"`
	Reviews   []int64  `json:"reviews"`
}

func buildSeries(points []models.TimePoint) chartSeries {
	s := chartSeries{
		Labels:    make([]string, 0, len(points)),
		Commits:   make([]int64, 0, len(points)),
		Additions: make([]int64, 0, len(points)),
		Deletions: make([]int64, 0, len(points)),
		PRsOpened: make([]int64, 0, len(points)),
		PRsMerged: make([]int64, 0, len(points)),
		Reviews:   make([]int64, 0, len(points)),
	}
	for _, p := range points {
		s.Labels = append(s.Labels, p.Label)
		s.Commits = append(s.Commits, p.Commits)
		s.Additions = append(s.Additions, p.Additions)
		s.Deletions = append(s.Deletions, p.Deletions)
		s.PRsOpened = append(s.PRsOpened, p.PRsOpened)
		s.PRsMerged = append(s.PRsMerged, p.PRsMerged)
		s.Reviews = append(s.Reviews, p.ReviewsSubmitted)
	}
	return s
}

type dashboardView struct {
	layoutData
	Overview     models.OverviewStats
	Previous     models.OverviewStats
	Series       chartSeries
	Granularity  string
	TopCommits   []models.ContributorStats
	TopAuthors   []models.ContributorStats
	TopReviewers []models.ContributorStats
	Repositories []models.RepositoryStats
	Activity     []models.ActivityRow
	Teams        []models.Team
	Team         *models.Team
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "overview")
	if !ok {
		return
	}
	view := dashboardView{layoutData: pc.Layout}
	view.Title = "Overview"
	view.Granularity = string(pc.Layout.Range.Granularity())

	if err := s.resolveTeam(r, pc); err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Teams, view.Team = pc.Teams, pc.Team
	if pc.Team != nil {
		view.Query = cloneValues(pc.Layout.Range.Query())
		view.Query.Set("team", pc.Team.Slug)
	}

	if pc.Org == nil {
		s.render(w, r, http.StatusOK, "dashboard.html", view)
		return
	}

	ctx := r.Context()
	var err error
	if view.Overview, err = s.db.Overview(ctx, pc.Filter); err != nil {
		s.serverError(w, r, err)
		return
	}

	prevRange := pc.Layout.Range.PreviousRange()
	prevFilter := pc.Filter
	prevFilter.From, prevFilter.To = prevRange.From, prevRange.To
	if view.Previous, err = s.db.Overview(ctx, prevFilter); err != nil {
		s.serverError(w, r, err)
		return
	}

	points, err := s.db.TimeSeries(ctx, pc.Filter, pc.Layout.Range.Granularity(), nil, nil)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Series = buildSeries(points)

	if view.TopCommits, err = s.db.ContributorRanking(ctx, pc.Filter, "commits", 5, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.TopAuthors, err = s.db.ContributorRanking(ctx, pc.Filter, "prs_opened", 5, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.TopReviewers, err = s.db.ContributorRanking(ctx, pc.Filter, "reviews_submitted", 5, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	repos, err := s.db.RepositoryRanking(ctx, pc.Filter, metrics.DefaultRepositorySort, metrics.SortDesc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(repos) > 6 {
		repos = repos[:6]
	}
	view.Repositories = repos

	if view.Activity, err = s.db.RecentActivity(ctx, pc.Filter, 20); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "dashboard.html", view)
}

type contributorsView struct {
	layoutData
	Rows        []models.ContributorStats
	Sort        metrics.Metric
	MetricGroup []struct {
		Name    string
		Metrics []metrics.Metric
	}
	AllMetrics      []metrics.Metric
	Page            int
	PageSize        int
	Offset          int
	PrevPage        int
	NextPage        int
	HasNext         bool
	IncludeArchived bool
	Teams           []models.Team
	Team            *models.Team
}

func (s *Server) handleContributors(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "contributors")
	if !ok {
		return
	}
	sortKey := r.URL.Query().Get("sort")
	view := contributorsView{
		layoutData:      pc.Layout,
		Sort:            metrics.ResolveSort(sortKey),
		MetricGroup:     metrics.MetricGroups(),
		AllMetrics:      metrics.RankingMetrics(),
		PageSize:        50,
		Page:            pageNumber(r.URL.Query().Get("page")),
		IncludeArchived: s.cfg.IncludeArchived,
	}
	view.Offset = (view.Page - 1) * view.PageSize
	view.PrevPage = view.Page - 1
	view.NextPage = view.Page + 1
	view.Title = "Contributors"

	if err := s.resolveTeam(r, pc); err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Teams, view.Team = pc.Teams, pc.Team

	// Keep the sort and team selections in every link on the page.
	view.Query = cloneValues(pc.Layout.Range.Query())
	view.Query.Set("sort", view.Sort.Key)
	if pc.Team != nil {
		view.Query.Set("team", pc.Team.Slug)
	}

	if pc.Org == nil {
		s.render(w, r, http.StatusOK, "contributors.html", view)
		return
	}

	rows, err := s.db.ContributorRanking(r.Context(), pc.Filter, view.Sort.Key, view.PageSize+1, view.Offset)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(rows) > view.PageSize {
		view.HasNext = true
		rows = rows[:view.PageSize]
	}
	view.Rows = rows

	s.render(w, r, http.StatusOK, "contributors.html", view)
}

type contributorView struct {
	layoutData
	Contributor  models.ContributorStats
	Series       chartSeries
	Granularity  string
	Repositories []models.RepositoryStats
	PullRequests []database.PullRequestRow
	Reviews      []database.ReviewRow
	Metrics      []metrics.Metric
}

func (s *Server) handleContributor(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "contributors")
	if !ok {
		return
	}
	login := r.PathValue("login")
	if pc.Org == nil {
		s.notFound(w, r, pc.Layout, "No data has been synchronized yet.")
		return
	}

	ctx := r.Context()
	contributor, err := s.db.ContributorByLogin(ctx, pc.Org.ID, login)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if contributor == nil {
		s.notFound(w, r, pc.Layout, "No contributor with that login has activity in "+s.cfg.GitHubOrg+".")
		return
	}

	view := contributorView{layoutData: pc.Layout, Metrics: metrics.RankingMetrics()}
	view.Title = contributor.DisplayName()
	view.Granularity = string(pc.Layout.Range.Granularity())

	if view.Contributor, err = s.db.ContributorTotals(ctx, pc.Filter, contributor.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	points, err := s.db.TimeSeries(ctx, pc.Filter, pc.Layout.Range.Granularity(), &contributor.ID, nil)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Series = buildSeries(points)

	if view.Repositories, err = s.db.ContributorRepositories(ctx, pc.Filter, contributor.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.PullRequests, err = s.db.ContributorPullRequests(ctx, pc.Filter, contributor.ID, 50); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.Reviews, err = s.db.ContributorReviews(ctx, pc.Filter, contributor.ID, 50); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "contributor.html", view)
}

// sortHeader is one clickable column header of the repositories table.
type sortHeader struct {
	Label string
	Title string
	// Numeric headers are right-aligned, matching their cells.
	Numeric bool
	// Active marks the column the table is currently ordered by.
	Active bool
	// Ascending is meaningful only when Active is set.
	Ascending bool
	Href      template.URL
	// AriaSort is the value of the th aria-sort attribute.
	AriaSort string
}

type repositoriesView struct {
	layoutData
	Rows            []models.RepositoryStats
	Headers         []sortHeader
	SortKey         string
	SortDir         string
	SortLabel       string
	IncludeArchived bool
	Teams           []models.Team
	Team            *models.Team
}

func (s *Server) handleRepositories(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "repositories")
	if !ok {
		return
	}
	q := r.URL.Query()
	col, dir := metrics.ResolveRepositorySort(q.Get("sort"), q.Get("dir"))

	view := repositoriesView{
		layoutData:      pc.Layout,
		IncludeArchived: s.cfg.IncludeArchived,
		SortKey:         col.Key,
		SortDir:         dir,
		SortLabel:       col.Label,
	}
	view.Title = "Repositories"

	if err := s.resolveTeam(r, pc); err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Teams, view.Team = pc.Teams, pc.Team

	// Keep the chosen column and team in the period picker and the navigation.
	base := cloneValues(pc.Layout.Range.Query())
	if pc.Team != nil {
		base.Set("team", pc.Team.Slug)
	}
	view.Headers = repositoryHeaders(base, col.Key, dir)
	view.Query = cloneValues(base)
	view.Query.Set("sort", col.Key)
	view.Query.Set("dir", dir)

	if pc.Org == nil {
		s.render(w, r, http.StatusOK, "repositories.html", view)
		return
	}

	// The repository page always lists archived repositories so they remain
	// visible and clearly marked, even when excluded from analytics.
	filter := pc.Filter
	filter.IncludeArchived = true
	rows, err := s.db.RepositoryRanking(r.Context(), filter, col.Key, dir)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Rows = rows
	s.render(w, r, http.StatusOK, "repositories.html", view)
}

// repositoryHeaders builds one header link per sortable column. Clicking the
// active column flips its direction; clicking any other starts at that column's
// natural direction.
func repositoryHeaders(base url.Values, activeKey, activeDir string) []sortHeader {
	cols := metrics.RepositoryColumns()
	out := make([]sortHeader, 0, len(cols))
	for _, c := range cols {
		active := c.Key == activeKey
		next := c.ToggleDirection(activeKey, activeDir)

		v := cloneValues(base)
		v.Set("sort", c.Key)
		v.Set("dir", next)

		aria := "none"
		if active {
			aria = "descending"
			if activeDir == metrics.SortAsc {
				aria = "ascending"
			}
		}
		out = append(out, sortHeader{
			Label:     c.Label,
			Title:     c.Title,
			Numeric:   c.Numeric,
			Active:    active,
			Ascending: active && activeDir == metrics.SortAsc,
			Href:      template.URL("/repositories?" + v.Encode()),
			AriaSort:  aria,
		})
	}
	return out
}

type repositoryView struct {
	layoutData
	Repository   models.RepositoryStats
	Series       chartSeries
	Granularity  string
	Contributors []models.ContributorStats
	PullRequests []database.PullRequestRow
	Reviews      []database.ReviewRow
	Excluded     bool
}

func (s *Server) handleRepository(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "repositories")
	if !ok {
		return
	}
	if pc.Org == nil {
		s.notFound(w, r, pc.Layout, "No data has been synchronized yet.")
		return
	}

	ctx := r.Context()
	repo, err := s.db.RepositoryByName(ctx, pc.Org.ID, r.PathValue("name"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if repo == nil {
		s.notFound(w, r, pc.Layout, "No repository with that name exists in "+s.cfg.GitHubOrg+".")
		return
	}

	// A detail page always shows the repository's own numbers, including when it
	// is archived and therefore excluded from organization-wide rankings.
	filter := pc.Filter
	filter.IncludeArchived = true

	view := repositoryView{layoutData: pc.Layout, Granularity: string(pc.Layout.Range.Granularity())}
	view.Title = repo.Name
	view.Excluded = repo.IsArchived && !s.cfg.IncludeArchived

	if view.Repository, err = s.db.RepositoryTotals(ctx, filter, repo.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	points, err := s.db.TimeSeries(ctx, filter, pc.Layout.Range.Granularity(), nil, &repo.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Series = buildSeries(points)

	if view.Contributors, err = s.db.RepositoryContributors(ctx, filter, repo.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.PullRequests, err = s.db.RepositoryPullRequests(ctx, filter, repo.ID, 50); err != nil {
		s.serverError(w, r, err)
		return
	}
	if view.Reviews, err = s.db.RepositoryReviews(ctx, filter, repo.ID, 50); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "repository.html", view)
}

type statusView struct {
	layoutData
	Counts          database.Counts
	TeamCount       int64
	SyncTeams       bool
	LastRun         *models.SyncRun
	LastSuccess     *models.SyncRun
	Runs            []models.SyncRun
	RateLimit       rateLimitView
	SchemaVersion   string
	Uptime          string
	SyncInterval    string
	HistoryMonths   int
	IncludeArchived bool
	ExcludedUsers   []string
	WorkerError     string
	DatabaseOK      bool
	Triggered       bool
	Metrics         []metrics.Metric
}

type rateLimitView struct {
	Limit     int
	Remaining int
	Cost      int
	ResetAt   time.Time
	Known     bool
	Percent   int
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "status")
	if !ok {
		return
	}
	rl := s.worker.RateLimit()
	view := statusView{
		layoutData:      pc.Layout,
		SyncInterval:    s.cfg.SyncInterval.String(),
		SyncTeams:       s.cfg.SyncTeams,
		HistoryMonths:   s.cfg.HistoryMonths,
		IncludeArchived: s.cfg.IncludeArchived,
		ExcludedUsers:   s.cfg.ExcludedUsers,
		WorkerError:     s.worker.LastError(),
		Uptime:          formatDuration(time.Since(s.startedAt)),
		Triggered:       r.URL.Query().Get("triggered") == "1",
		Metrics:         metrics.RankingMetrics(),
		RateLimit: rateLimitView{
			Limit:     rl.Limit,
			Remaining: rl.Remaining,
			Cost:      rl.Cost,
			ResetAt:   rl.ResetAt,
			Known:     rl.Limit > 0,
		},
	}
	view.Title = "Status"
	if rl.Limit > 0 {
		view.RateLimit.Percent = rl.Remaining * 100 / rl.Limit
	}
	view.DatabaseOK = s.db.Ping(r.Context()) == nil
	if v, err := s.db.SchemaVersion(r.Context()); err == nil {
		view.SchemaVersion = v
	}

	if pc.Org != nil {
		ctx := r.Context()
		var err error
		if view.Counts, err = s.db.OrganizationCounts(ctx, pc.Org.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if view.LastRun, err = s.db.LastSyncRun(ctx, pc.Org.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if view.LastSuccess, err = s.db.LastSuccessfulSyncRun(ctx, pc.Org.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if view.Runs, err = s.db.RecentSyncRuns(ctx, pc.Org.ID, 15); err != nil {
			s.serverError(w, r, err)
			return
		}
		if view.TeamCount, err = s.db.CountTeams(ctx, pc.Org.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	s.render(w, r, http.StatusOK, "status.html", view)
}

// handleSyncNow queues an out-of-band synchronization from the status page.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	queued := s.worker.Trigger()
	s.log.Info("manual synchronization requested", "queued", queued)
	target := "/status"
	if queued {
		target += "?triggered=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleReport streams a PDF activity report for the selected period.
//
// Like every other page, it is built purely from PostgreSQL aggregates; asking
// for a report never triggers a GitHub call.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "")
	if !ok {
		return
	}
	// Before the first successful synchronization there is no organization row
	// and nothing to query. Produce an empty but valid report that says so,
	// rather than a 404: every other page renders in that state too.
	var data *report.Data
	if pc.Org == nil {
		data = report.Empty(s.cfg.GitHubOrg, pc.Layout.Range)
	} else {
		var err error
		data, err = report.Collect(r.Context(), s.db, report.Request{
			Organization: s.cfg.GitHubOrg,
			Filter:       pc.Filter,
			Range:        pc.Layout.Range,
			SortKey:      r.URL.Query().Get("sort"),
			LastSync:     pc.Org.LastSyncedAt,
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	pdf, err := report.Render(data)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(pdf)))
	// The filename is built from a slug of the organization and the ISO dates,
	// so it cannot inject header characters.
	w.Header().Set("Content-Disposition", `attachment; filename="`+data.Filename()+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(pdf); err != nil {
		s.log.Warn("report download interrupted", "error", err)
		return
	}
	s.log.Info("report generated",
		"organization", s.cfg.GitHubOrg,
		"period", data.Range.Key,
		"from", data.Range.FromString(),
		"to", data.Range.ToString(),
		"contributors", len(data.Contributors),
		"repositories", len(data.Repositories),
		"bytes", len(pdf),
	)
}

type healthResponse struct {
	Status       string `json:"status"`
	Database     string `json:"database"`
	Organization string `json:"organization"`
	Version      string `json:"version"`
	UptimeSecs   int64  `json:"uptime_seconds"`
	LastSyncAt   string `json:"last_sync_at,omitempty"`
	SyncStatus   string `json:"sync_status,omitempty"`
}

// handleHealth reports liveness plus basic database health.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:       "ok",
		Database:     "ok",
		Organization: s.cfg.GitHubOrg,
		Version:      s.version,
		UptimeSecs:   int64(time.Since(s.startedAt).Seconds()),
	}
	code := http.StatusOK
	if err := s.db.Ping(r.Context()); err != nil {
		resp.Status = "degraded"
		resp.Database = "unavailable"
		code = http.StatusServiceUnavailable
		s.log.Warn("health check: database unavailable", "error", err)
	} else if org, err := s.db.OrganizationByLogin(r.Context(), s.cfg.GitHubOrg); err == nil && org != nil {
		if org.LastSyncedAt != nil {
			resp.LastSyncAt = org.LastSyncedAt.UTC().Format(time.RFC3339)
		}
		if run, err := s.db.LastSyncRun(r.Context(), org.ID); err == nil && run != nil {
			resp.SyncStatus = run.Status
		}
	}
	writeJSON(w, code, resp)
}

// handleReady reports whether the schema is applied and the app can serve data.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ready", "organization": s.cfg.GitHubOrg}
	code := http.StatusOK
	if err := s.db.Ping(r.Context()); err != nil {
		resp["status"] = "not ready"
		resp["reason"] = "database unavailable"
		code = http.StatusServiceUnavailable
	} else if v, err := s.db.SchemaVersion(r.Context()); err != nil {
		resp["status"] = "not ready"
		resp["reason"] = "schema migrations have not been applied"
		code = http.StatusServiceUnavailable
	} else {
		resp["schema_version"] = v
	}
	writeJSON(w, code, resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func pageNumber(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return n
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}
