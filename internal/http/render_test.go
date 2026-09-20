package http

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
	"github.com/relaxart/dev-pulse/web"
)

func testRenderer(t *testing.T) *renderer {
	t.Helper()
	r, err := newRenderer(web.FS)
	if err != nil {
		t.Fatalf("templates do not parse: %v", err)
	}
	return r
}

func TestEveryPageTemplateParses(t *testing.T) {
	r := testRenderer(t)
	for _, name := range pageNames {
		if _, ok := r.pages[name]; !ok {
			t.Errorf("template %s was not parsed", name)
		}
	}
}

func baseLayout() layoutData {
	rng, _ := metrics.ResolveRange(metrics.Period30d, "", "", time.Now())
	return layoutData{
		AppName:       AppName,
		Title:         "Overview",
		Nav:           "overview",
		Path:          "/",
		Org:           "acme",
		OrgSynced:     true,
		Range:         rng,
		PeriodOptions: metrics.PeriodOptions(),
		Query:         rng.Query(),
		Now:           time.Now(),
	}
}

// evil is a string a malicious repository name, login or pull request title
// could contain. It must never reach the browser unescaped.
const evil = `<script>alert('xss')</script>`

func TestGitHubProvidedStringsAreEscaped(t *testing.T) {
	r := testRenderer(t)
	now := time.Now()

	view := dashboardView{layoutData: baseLayout(), Granularity: "day"}
	view.Overview = models.OverviewStats{ActiveContributors: 2, Commits: 10}
	view.TopCommits = []models.ContributorStats{{
		Contributor: models.Contributor{ID: 1, Login: evil, Name: evil, AvatarURL: "https://example.test/a.png"},
		Commits:     5,
	}}
	view.Repositories = []models.RepositoryStats{{
		Repository: models.Repository{ID: 1, Name: evil, Description: evil},
		Commits:    3,
	}}
	view.Activity = []models.ActivityRow{{
		Date:        now,
		Contributor: models.Contributor{ID: 1, Login: evil, Name: evil},
		Repository:  evil,
		Commits:     1,
	}}
	view.Series = chartSeries{Labels: []string{evil}, Commits: []int64{1}}

	w := httptest.NewRecorder()
	if err := r.render(w, 200, "dashboard.html", view); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>alert(") {
		t.Fatal("a GitHub provided string was rendered as live HTML")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("expected the payload to appear HTML-escaped somewhere in the page")
	}
	// The chart payload is injected into a <script> block; it must not be able
	// to close that block.
	if strings.Contains(body, "[\"<script>") {
		t.Fatal("chart JSON escaped its script element")
	}
	if !strings.Contains(body, "\\u003cscript\\u003e") {
		t.Error("chart JSON should carry the escaped payload")
	}
}

func TestPullRequestTitlesAreEscapedOnDetailPages(t *testing.T) {
	r := testRenderer(t)
	view := contributorView{layoutData: baseLayout(), Granularity: "day", Metrics: metrics.RankingMetrics()}
	view.Contributor = models.ContributorStats{
		Contributor: models.Contributor{ID: 1, Login: "alice", Name: evil, URL: "https://github.test/alice"},
	}
	view.PullRequests = []database.PullRequestRow{{
		PullRequest:    models.PullRequest{Number: 1, Title: evil, URL: "https://github.test/pr/1", State: "OPEN", CreatedAt: time.Now()},
		RepositoryName: evil,
	}}
	view.Reviews = []database.ReviewRow{{
		State: "APPROVED", SubmittedAt: time.Now(), RepositoryName: evil,
		PullRequestNumber: 1, PullRequestTitle: evil, PullRequestURL: "https://github.test/pr/1",
	}}

	w := httptest.NewRecorder()
	if err := r.render(w, 200, "contributor.html", view); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(w.Body.String(), "<script>alert(") {
		t.Fatal("a pull request title was rendered as live HTML")
	}
}

func TestJavascriptURLsInGitHubDataAreNeutralised(t *testing.T) {
	r := testRenderer(t)
	view := contributorView{layoutData: baseLayout(), Granularity: "day", Metrics: metrics.RankingMetrics()}
	view.Contributor = models.ContributorStats{
		Contributor: models.Contributor{ID: 1, Login: "alice", Name: "Alice", URL: "javascript:alert(1)"},
	}
	w := httptest.NewRecorder()
	if err := r.render(w, 200, "contributor.html", view); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(w.Body.String(), `href="javascript:alert(1)"`) {
		t.Fatal("html/template must have filtered the javascript: URL")
	}
}

func TestEveryPageRendersWithEmptyData(t *testing.T) {
	r := testRenderer(t)
	l := baseLayout()
	l.OrgSynced = false

	cases := []struct {
		page string
		data any
	}{
		{"dashboard.html", dashboardView{layoutData: l}},
		{"contributors.html", contributorsView{layoutData: l, Sort: metrics.ResolveSort("commits"),
			MetricGroup: metrics.MetricGroups(), AllMetrics: metrics.RankingMetrics(), Page: 1, PageSize: 50}},
		{"contributor.html", contributorView{layoutData: l, Metrics: metrics.RankingMetrics()}},
		{"repositories.html", repositoriesView{layoutData: l}},
		{"repository.html", repositoryView{layoutData: l}},
		{"status.html", statusView{layoutData: l, Metrics: metrics.RankingMetrics()}},
		{"error.html", errorView{layoutData: l, Code: 404, Heading: "Not found", Message: "nope"}},
	}
	for _, c := range cases {
		t.Run(c.page, func(t *testing.T) {
			w := httptest.NewRecorder()
			if err := r.render(w, 200, c.page, c.data); err != nil {
				t.Fatalf("render %s: %v", c.page, err)
			}
			if w.Body.Len() == 0 {
				t.Errorf("%s produced an empty page", c.page)
			}
		})
	}
}

func TestSortLinkRejectsUnknownKeys(t *testing.T) {
	q := url.Values{"period": []string{"3m"}}
	if got := string(sortLink("/contributors", q, "reviews_submitted")); !strings.Contains(got, "sort=reviews_submitted") {
		t.Errorf("sortLink dropped a valid key: %s", got)
	}
	got := string(sortLink("/contributors", q, "commits; DROP TABLE commits"))
	if strings.Contains(got, "DROP") {
		t.Errorf("sortLink must not carry an unknown sort key: %s", got)
	}
	if !strings.Contains(got, "period=3m") {
		t.Errorf("sortLink must keep the date filter: %s", got)
	}
}

func TestFormatters(t *testing.T) {
	if got := formatNumber(int64(1234567)); got != "1,234,567" {
		t.Errorf("formatNumber = %q", got)
	}
	if got := formatNumber(7); got != "7" {
		t.Errorf("formatNumber(int) = %q", got)
	}
	if got := formatCompact(12400); got != "12.4k" {
		t.Errorf("formatCompact = %q", got)
	}
	if got := formatSigned(12400); got != "+12.4k" {
		t.Errorf("formatSigned = %q", got)
	}
	if got := formatSigned(-1500); got != "-1.5k" {
		t.Errorf("formatSigned = %q", got)
	}
	if got := humanizeAgo(nil); got != "never" {
		t.Errorf("humanizeAgo(nil) = %q", got)
	}
	if got := percentChange(5, 0); got != "n/a" {
		t.Errorf("percentChange with no baseline = %q", got)
	}
}

func TestExtraParamsKeepsOnlyForeignKeys(t *testing.T) {
	q := url.Values{"period": []string{"custom"}, "from": []string{"2026-01-01"}, "to": []string{"2026-02-01"}, "sort": []string{"approvals"}}
	got := extraParams(q)
	if len(got) != 1 || got[0].Key != "sort" || got[0].Value != "approvals" {
		t.Errorf("extraParams = %+v, want only the sort key", got)
	}
}

func TestToJSONNeverClosesTheScriptElement(t *testing.T) {
	out := string(toJSON(map[string]string{"repo": "</script><img src=x onerror=alert(1)>"}))
	if strings.Contains(out, "</script>") {
		t.Fatalf("toJSON leaked a closing script tag: %s", out)
	}
	if !strings.Contains(out, "\\u003c") {
		t.Errorf("toJSON should escape angle brackets: %s", out)
	}
}
