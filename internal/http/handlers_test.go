package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaxart/dev-pulse/internal/collector"
	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/github"
	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
	"github.com/relaxart/dev-pulse/migrations"
	"github.com/relaxart/dev-pulse/web"
)

// testServer wires the real handlers to a real PostgreSQL and a GitHub endpoint
// that fails the test if it is ever contacted.
type testServer struct {
	handler    http.Handler
	db         *database.DB
	githubHits *int
	org        *models.Organization
	repoID     int64
	alice      int64
	teamID     int64
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the handler integration tests")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, isolatedSchemaURL(t, url, "devpulse_test_http"), 5)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, migrations.FS, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
TRUNCATE contributor_daily_stats, pull_request_reviews, pull_requests, commits,
         repositories, contributors, sync_runs, organizations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	hits := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("the dashboard called GitHub while rendering %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(gh.Close)

	cfg := &config.Config{
		GitHubToken:    "github_pat_never_rendered",
		GitHubOrg:      "acme",
		DatabaseURL:    url,
		SyncInterval:   30 * time.Minute,
		HistoryMonths:  12,
		GitHubAPIURL:   gh.URL,
		MinRateLimit:   200,
		ExcludedUsers:  []string{"renovate[bot]"},
		ListenAddr:     ":0",
		RequestTimeout: 5 * time.Second,
	}
	client := github.New(github.Options{Endpoint: gh.URL, Token: cfg.GitHubToken, Logger: log})
	worker := collector.NewWorker(collector.New(cfg, client, db, log), cfg.SyncInterval, log)

	srv, err := New(cfg, db, worker, web.FS, "test", log)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := &testServer{handler: srv.Handler(), db: db, githubHits: &hits}
	ts.seed(t)
	return ts
}

// isolatedSchemaURL gives this test package its own PostgreSQL schema, so that
// `go test ./...` can run several packages against one TEST_DATABASE_URL without
// them truncating each other's tables.
func isolatedSchemaURL(t *testing.T, base, schema string) string {
	t.Helper()
	admin, err := database.Connect(context.Background(), base, 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Pool.Exec(context.Background(), "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=" + schema
}

// seed inserts a small, realistic dataset the way the collector would.
func (ts *testServer) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	day := func(d int) time.Time { return time.Now().UTC().AddDate(0, 0, -d) }

	org, err := ts.db.UpsertOrganization(ctx, "O_acme", "acme", "Acme Inc")
	if err != nil {
		t.Fatal(err)
	}
	ts.org = org

	repoID, err := ts.db.UpsertRepository(ctx, &models.Repository{
		OrganizationID: org.ID, GitHubID: "R_api", Name: "api", FullName: "acme/api",
		URL: "https://github.test/acme/api", CreatedAt: day(60), UpdatedAt: day(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.repoID = repoID
	if _, err := ts.db.UpsertRepository(ctx, &models.Repository{
		OrganizationID: org.ID, GitHubID: "R_legacy", Name: "legacy", FullName: "acme/legacy",
		IsArchived: true, CreatedAt: day(600), UpdatedAt: day(200),
	}); err != nil {
		t.Fatal(err)
	}

	alice, err := ts.db.UpsertContributor(ctx, &models.Contributor{GitHubID: "U_alice", Login: "alice", Name: "Alice Doe"})
	if err != nil {
		t.Fatal(err)
	}
	ts.alice = alice
	bot, err := ts.db.UpsertContributor(ctx, &models.Contributor{GitHubID: "U_bot", Login: "dependabot[bot]", IsBot: true})
	if err != nil {
		t.Fatal(err)
	}

	for i, d := range []int{2, 5, 40, 120, 300} {
		at := day(d)
		if _, err := ts.db.UpsertCommits(ctx, []models.Commit{{
			OrganizationID: org.ID, RepositoryID: repoID, ContributorID: &alice,
			GitHubOID: "c" + string(rune('a'+i)), CommittedAt: at, Additions: 10, Deletions: 2, ChangedFiles: 1,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.db.UpsertCommits(ctx, []models.Commit{{
		OrganizationID: org.ID, RepositoryID: repoID, ContributorID: &bot,
		GitHubOID: "cbot", CommittedAt: day(2), Additions: 999, Deletions: 999,
	}}); err != nil {
		t.Fatal(err)
	}

	prID, err := ts.db.UpsertPullRequest(ctx, &models.PullRequest{
		OrganizationID: org.ID, RepositoryID: repoID, AuthorID: &alice, GitHubID: "PR_1", Number: 1,
		Title: "Add pagination", URL: "https://github.test/acme/api/pull/1", State: models.PRStateOpen,
		CreatedAt: day(3), UpdatedAt: day(3), Additions: 20, Deletions: 4, ChangedFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.UpsertReviews(ctx, []models.PullRequestReview{{
		OrganizationID: org.ID, RepositoryID: repoID, PullRequestID: prID, ReviewerID: &alice,
		GitHubID: "RV_1", State: models.ReviewApproved, SubmittedAt: day(2), CommentCount: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	teamID, err := ts.db.UpsertTeam(ctx, &models.Team{
		OrganizationID: org.ID, GitHubID: "T_1", Slug: "platform", Name: "Platform",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.teamID = teamID
	if err := ts.db.ReplaceTeamMembers(ctx, org.ID, teamID, []int64{alice}); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.ReplaceTeamRepositories(ctx, org.ID, teamID, []int64{repoID}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.UpsertTeam(ctx, &models.Team{
		OrganizationID: org.ID, GitHubID: "T_2", Slug: "design", Name: "Design",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.db.RebuildDailyStats(ctx, org.ID, []int64{repoID},
		time.Now().AddDate(-2, 0, 0), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.MarkOrganizationSynced(ctx, org.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (ts *testServer) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)
	return w
}

func TestAllPagesRenderFromPostgresWithoutCallingGitHub(t *testing.T) {
	ts := newTestServer(t)

	paths := []string{
		"/",
		"/contributors",
		"/contributors/alice",
		"/repositories",
		"/repositories/api",
		"/repositories/legacy",
		"/status",
		"/health",
		"/ready",
	}
	periods := []string{"", "?period=30d", "?period=3m", "?period=6m", "?period=12m",
		"?period=custom&from=2026-01-01&to=2026-03-31"}

	for _, p := range paths {
		for _, q := range periods {
			w := ts.get(t, p+q)
			if w.Code != http.StatusOK {
				t.Errorf("GET %s%s = %d, want 200", p, q, w.Code)
			}
		}
	}

	// Every sort key must render too.
	for _, sortKey := range []string{
		"commits", "additions", "deletions", "changed_files",
		"prs_opened", "prs_merged", "prs_closed",
		"reviews_submitted", "approvals", "changes_requested", "review_comments",
		"unique_prs_reviewed", "active_days", "repositories",
	} {
		w := ts.get(t, "/contributors?sort="+sortKey+"&period=3m")
		if w.Code != http.StatusOK {
			t.Errorf("GET /contributors?sort=%s = %d", sortKey, w.Code)
		}
		if !strings.Contains(w.Body.String(), "sort="+sortKey) {
			t.Errorf("the page does not keep sort=%s in its links", sortKey)
		}
	}

	if *ts.githubHits != 0 {
		t.Fatalf("the dashboard made %d GitHub calls; it must read only from PostgreSQL", *ts.githubHits)
	}
}

func TestChangingThePeriodChangesTheNumbers(t *testing.T) {
	ts := newTestServer(t)

	short := ts.get(t, "/contributors?period=30d&sort=commits").Body.String()
	long := ts.get(t, "/contributors?period=12m&sort=commits").Body.String()
	if short == long {
		t.Error("the 30 day and 12 month rankings are identical; the date filter is not applied")
	}
	if *ts.githubHits != 0 {
		t.Fatal("changing the period must not trigger a GitHub call")
	}
}

func TestBotsAreAbsentFromTheContributorPage(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/contributors?period=3m").Body.String()
	if strings.Contains(body, "dependabot") {
		t.Error("a bot account appears in the contributor ranking")
	}
	if !strings.Contains(body, "alice") {
		t.Error("the human contributor is missing from the ranking")
	}
}

func TestArchivedRepositoryIsListedAndMarked(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/repositories?period=12m").Body.String()
	if !strings.Contains(body, "legacy") {
		t.Error("archived repositories must stay visible in the repository list")
	}
	if !strings.Contains(body, "archived") {
		t.Error("archived repositories must be marked as such")
	}

	detail := ts.get(t, "/repositories/legacy?period=12m")
	if detail.Code != http.StatusOK {
		t.Fatalf("archived repository detail page = %d", detail.Code)
	}
	if !strings.Contains(detail.Body.String(), "INCLUDE_ARCHIVED=false") {
		t.Error("the detail page should explain why an archived repository is excluded from rankings")
	}
}

func TestTokenNeverAppearsInAnyResponse(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/", "/contributors", "/contributors/alice", "/repositories",
		"/repositories/api", "/status", "/health", "/ready", "/does-not-exist"} {
		body := ts.get(t, p).Body.String()
		if strings.Contains(body, "github_pat_never_rendered") {
			t.Fatalf("the GitHub token appeared in the response for %s", p)
		}
	}
}

func TestUnknownContributorAndRepositoryReturn404(t *testing.T) {
	ts := newTestServer(t)
	if w := ts.get(t, "/contributors/nobody"); w.Code != http.StatusNotFound {
		t.Errorf("unknown contributor = %d, want 404", w.Code)
	}
	if w := ts.get(t, "/repositories/nothing"); w.Code != http.StatusNotFound {
		t.Errorf("unknown repository = %d, want 404", w.Code)
	}
	if w := ts.get(t, "/nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown page = %d, want 404", w.Code)
	}
}

func TestHealthEndpointReportsDatabaseHealth(t *testing.T) {
	ts := newTestServer(t)
	w := ts.get(t, "/health")
	if w.Code != http.StatusOK {
		t.Fatalf("/health = %d", w.Code)
	}
	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/health is not JSON: %v", err)
	}
	if resp.Status != "ok" || resp.Database != "ok" {
		t.Errorf("/health = %+v", resp)
	}
	if resp.Organization != "acme" {
		t.Errorf("organization = %q", resp.Organization)
	}

	ready := ts.get(t, "/ready")
	if ready.Code != http.StatusOK {
		t.Fatalf("/ready = %d", ready.Code)
	}
	if !strings.Contains(ready.Body.String(), "schema_version") {
		t.Error("/ready should report the applied schema version")
	}
}

func TestInvalidCustomRangeIsExplainedNotFatal(t *testing.T) {
	ts := newTestServer(t)
	w := ts.get(t, "/contributors?period=custom&from=2026-12-01&to=2026-01-01")
	if w.Code != http.StatusOK {
		t.Fatalf("an invalid range should still render, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Date filter ignored") {
		t.Error("the user should be told why their date filter was ignored")
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	ts := newTestServer(t)
	w := ts.get(t, "/")
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("a Content-Security-Policy header is missing")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is missing")
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/static/css/app.css", "/static/js/app.js"} {
		w := ts.get(t, p)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
}

func TestReportDownloadsAPDFForEveryPeriod(t *testing.T) {
	ts := newTestServer(t)

	periods := []string{"", "?period=7d", "?period=30d", "?period=3m", "?period=6m", "?period=12m",
		"?period=custom&from=2026-01-01&to=2026-03-31"}

	for _, q := range periods {
		w := ts.get(t, "/report.pdf"+q)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /report.pdf%s = %d", q, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Errorf("Content-Type = %q for %s", ct, q)
		}
		disposition := w.Header().Get("Content-Disposition")
		if !strings.HasPrefix(disposition, `attachment; filename="devpulse-acme-`) {
			t.Errorf("Content-Disposition = %q for %s", disposition, q)
		}
		body := w.Body.Bytes()
		if !bytes.HasPrefix(body, []byte("%PDF-")) {
			t.Errorf("%s did not return a PDF", q)
		}
		if len(body) < 2000 {
			t.Errorf("%s returned only %d bytes", q, len(body))
		}
		if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Errorf("Content-Length = %q but body is %d bytes", got, len(body))
		}
	}

	if *ts.githubHits != 0 {
		t.Fatal("generating a report must not call GitHub")
	}
}

func TestReportFilenameAndContentFollowThePeriod(t *testing.T) {
	ts := newTestServer(t)

	short := ts.get(t, "/report.pdf?period=7d")
	long := ts.get(t, "/report.pdf?period=12m")

	shortName := short.Header().Get("Content-Disposition")
	longName := long.Header().Get("Content-Disposition")
	if shortName == longName {
		t.Errorf("both periods produced the same filename: %q", shortName)
	}
	if bytes.Equal(short.Body.Bytes(), long.Body.Bytes()) {
		t.Error("the 7 day and 12 month reports are byte-identical; the period is not applied")
	}
}

func TestReportHonoursTheSortKey(t *testing.T) {
	ts := newTestServer(t)
	for _, sortKey := range []string{"commits", "reviews_submitted", "active_days", "not-a-metric"} {
		w := ts.get(t, "/report.pdf?period=12m&sort="+sortKey)
		if w.Code != http.StatusOK {
			t.Errorf("sort=%s returned %d", sortKey, w.Code)
		}
		if !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
			t.Errorf("sort=%s did not return a PDF", sortKey)
		}
	}
}

func TestReportNeverContainsTheToken(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/report.pdf?period=12m").Body.Bytes()
	if bytes.Contains(body, []byte("github_pat_never_rendered")) {
		t.Fatal("the GitHub token appeared inside the generated report")
	}
}

func TestReportMenuIsOfferedOnEveryPage(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/", "/contributors", "/repositories", "/status"} {
		body := ts.get(t, p+"?period=3m").Body.String()
		if !strings.Contains(body, "/report.pdf?period=3m") {
			t.Errorf("%s does not offer a report for the current filter", p)
		}
		if !strings.Contains(body, "/report.pdf?period=7d") {
			t.Errorf("%s does not offer the 7 day report preset", p)
		}
	}

	// The contributor ranking must carry its sort key into the download link.
	body := ts.get(t, "/contributors?period=3m&sort=approvals").Body.String()
	if !strings.Contains(body, "sort=approvals") || !strings.Contains(body, "/report.pdf?") {
		t.Error("the report link should preserve the selected sort key")
	}
}

// A deployment whose first synchronization has not produced data yet must still
// return a readable report rather than a 404: every other page renders in that
// state, and a fresh install is exactly when someone clicks around.
func TestReportBeforeTheFirstSyncIsAnEmptyDocumentNotAnError(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.db.Pool.Exec(context.Background(),
		`TRUNCATE contributor_daily_stats, pull_request_reviews, pull_requests, commits,
		          repositories, contributors, sync_runs, organizations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	w := ts.get(t, "/report.pdf?period=30d")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /report.pdf on an unsynchronized instance = %d, want 200", w.Code)
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Error("an unsynchronized instance did not return a PDF")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}
	if *ts.githubHits != 0 {
		t.Fatal("an empty report must not reach for GitHub to fill itself in")
	}
}

// countTag returns how many times an HTML tag opens inside the given fragment.
func countTag(fragment, tag string) int {
	return strings.Count(fragment, "<"+tag+" ") + strings.Count(fragment, "<"+tag+">")
}

func TestRepositoryTableHeadersAreSortLinks(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/repositories?period=12m").Body.String()

	for _, col := range []string{
		"name", "contributors", "commits", "additions",
		"prs_opened", "prs_merged", "reviews", "last_activity", "status",
	} {
		if !strings.Contains(body, "sort="+col) {
			t.Errorf("the repositories table offers no sort link for %q", col)
		}
	}
	if !strings.Contains(body, `aria-sort="descending"`) {
		t.Error("the active column should expose aria-sort for screen readers")
	}
	// Every header link must carry the current date filter.
	if strings.Count(body, "period=12m") < 9 {
		t.Error("header links drop the selected period")
	}
}

// The header list and the table body are written in two different places, so a
// column added to one and not the other would silently misalign every cell.
func TestRepositoryHeaderCountMatchesTheBodyColumns(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/repositories?period=12m").Body.String()

	head := body[strings.Index(body, "<thead>"):strings.Index(body, "</thead>")]
	headers := countTag(head, "th")

	rest := body[strings.Index(body, "<tbody>"):]
	firstRow := rest[:strings.Index(rest, "</tr>")]
	cells := countTag(firstRow, "td")

	if headers == 0 || cells == 0 {
		t.Fatalf("could not read the table: %d headers, %d cells", headers, cells)
	}
	if headers != cells {
		t.Errorf("the table has %d headers but %d cells per row", headers, cells)
	}
	if want := len(metrics.RepositoryColumns()); headers != want {
		t.Errorf("rendered %d headers, want %d sortable columns", headers, want)
	}
}

func TestRepositorySortChangesTheOrder(t *testing.T) {
	ts := newTestServer(t)

	// The seed has "api" (active, with commits) and "legacy" (archived, empty).
	byName := ts.get(t, "/repositories?period=12m&sort=name&dir=asc").Body.String()
	byNameDesc := ts.get(t, "/repositories?period=12m&sort=name&dir=desc").Body.String()

	apiFirst := strings.Index(byName, ">api<") < strings.Index(byName, ">legacy<")
	legacyFirst := strings.Index(byNameDesc, ">legacy<") < strings.Index(byNameDesc, ">api<")
	if !apiFirst {
		t.Error("ascending name sort did not put api before legacy")
	}
	if !legacyFirst {
		t.Error("descending name sort did not put legacy before api")
	}
}

// headerHrefs extracts the repositories table header links from a page.
func headerHrefs(t *testing.T, body string) []url.Values {
	t.Helper()
	var out []url.Values
	const marker = `href="/repositories?`
	for i := strings.Index(body, marker); i >= 0; {
		rest := body[i+len(marker):]
		raw := rest[:strings.Index(rest, `"`)]
		// The template escapes & as &amp; in attribute values.
		q, err := url.ParseQuery(strings.ReplaceAll(raw, "&amp;", "&"))
		if err != nil {
			t.Fatalf("header link %q is not a valid query: %v", raw, err)
		}
		out = append(out, q)
		next := strings.Index(rest, marker)
		if next < 0 {
			break
		}
		i = i + len(marker) + next
	}
	if len(out) == 0 {
		t.Fatal("no repositories header links were rendered")
	}
	return out
}

// hasLink reports whether any header link asks for exactly this column and
// direction.
func hasLink(links []url.Values, sortKey, dir string) bool {
	for _, q := range links {
		if q.Get("sort") == sortKey && q.Get("dir") == dir {
			return true
		}
	}
	return false
}

func TestRepositoryHeaderTogglesDirection(t *testing.T) {
	ts := newTestServer(t)

	// While sorted by commits descending, the commits header must offer ascending.
	links := headerHrefs(t, ts.get(t, "/repositories?period=12m&sort=commits&dir=desc").Body.String())
	if !hasLink(links, "commits", "asc") {
		t.Error("the active commits header does not toggle to ascending")
	}
	// An inactive column offers its own default rather than the active direction.
	if !hasLink(links, "name", "asc") {
		t.Error("the inactive name header should start ascending")
	}
	if !hasLink(links, "last_activity", "desc") {
		t.Error("the inactive last activity header should start descending")
	}

	body := ts.get(t, "/repositories?period=12m&sort=commits&dir=asc").Body.String()
	if !hasLink(headerHrefs(t, body), "commits", "desc") {
		t.Error("an ascending commits header does not toggle back to descending")
	}
	if !strings.Contains(body, `aria-sort="ascending"`) {
		t.Error("an ascending column should report aria-sort=ascending")
	}

	// Every header link keeps the selected period.
	for _, q := range headerHrefs(t, body) {
		if q.Get("period") != "12m" {
			t.Errorf("header link for %q dropped the period: %v", q.Get("sort"), q)
		}
	}
}

func TestRepositorySortSurvivesAPeriodChange(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/repositories?period=12m&sort=reviews&dir=asc").Body.String()

	// The period form posts every other parameter back as a hidden input.
	if !strings.Contains(body, `<input type="hidden" name="sort" value="reviews">`) {
		t.Error("the period picker drops the chosen sort column")
	}
	if !strings.Contains(body, `<input type="hidden" name="dir" value="asc">`) {
		t.Error("the period picker drops the chosen sort direction")
	}
}

func TestInvalidRepositorySortFallsBackWithoutFailing(t *testing.T) {
	ts := newTestServer(t)
	for _, q := range []string{
		"?sort=nonsense", "?sort=name;DROP+TABLE+repositories", "?sort=commits&dir=sideways", "?dir=asc",
	} {
		w := ts.get(t, "/repositories"+q)
		if w.Code != http.StatusOK {
			t.Errorf("GET /repositories%s = %d, want 200", q, w.Code)
		}
		if !strings.Contains(w.Body.String(), "sorted by") {
			t.Errorf("GET /repositories%s did not render the table", q)
		}
	}

	// An unknown column falls back to the documented default.
	body := ts.get(t, "/repositories?sort=nonsense").Body.String()
	if !strings.Contains(body, "sorted by <strong>Commits</strong>") {
		t.Error("an unknown column should fall back to the default Commits sort")
	}
}

func TestOnlyTheRepositoriesTableIsSortable(t *testing.T) {
	ts := newTestServer(t)
	// The dashboard's repository panel is a fixed top-N list, not a sortable
	// table; adding links there would suggest a sort that does not exist.
	body := ts.get(t, "/?period=12m").Body.String()
	if strings.Contains(body, `aria-sort=`) {
		t.Error("the dashboard should not advertise sortable headers")
	}
}

func TestTeamFilterIsOfferedOnEveryAnalyticsPage(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/", "/contributors", "/repositories"} {
		body := ts.get(t, path+"?period=12m").Body.String()
		if !strings.Contains(body, "All teams") {
			t.Errorf("%s does not offer the team filter", path)
		}
		for _, slug := range []string{"platform", "design"} {
			if !strings.Contains(body, "team="+slug) {
				t.Errorf("%s does not list the %q team", path, slug)
			}
		}
	}
}

func TestTeamFilterScopesEachPage(t *testing.T) {
	ts := newTestServer(t)

	// platform owns the api repository and alice; design owns neither.
	platform := ts.get(t, "/repositories?period=12m&team=platform").Body.String()
	if !strings.Contains(platform, ">api<") {
		t.Error("the platform team should list its api repository")
	}
	if strings.Contains(platform, ">legacy<") {
		t.Error("a repository outside the team is still listed")
	}

	design := ts.get(t, "/repositories?period=12m&team=design").Body.String()
	if strings.Contains(design, ">api<") {
		t.Error("the design team has no repositories but api is listed")
	}
	if !strings.Contains(design, "access to no imported repositories") {
		t.Error("an empty team should explain itself rather than look broken")
	}

	contributors := ts.get(t, "/contributors?period=12m&team=design").Body.String()
	if strings.Contains(contributors, "@alice") {
		t.Error("alice is not a member of design but appears in its ranking")
	}
	if !strings.Contains(contributors, "No members of this team were active") {
		t.Error("an empty team ranking should explain itself")
	}
	if !strings.Contains(ts.get(t, "/contributors?period=12m&team=platform").Body.String(), "@alice") {
		t.Error("alice is a member of platform but is missing from its ranking")
	}

	if *ts.githubHits != 0 {
		t.Fatal("filtering by team must not call GitHub")
	}
}

func TestTeamFilterSurvivesSortingAndPeriodChanges(t *testing.T) {
	ts := newTestServer(t)

	body := ts.get(t, "/contributors?period=3m&team=platform&sort=approvals").Body.String()
	if !strings.Contains(body, `<input type="hidden" name="team" value="platform">`) {
		t.Error("the period picker drops the team filter")
	}
	// Sort links keep the team.
	if !strings.Contains(body, "team=platform") || !strings.Contains(body, "sort=commits") {
		t.Error("the sort menu drops the team filter")
	}

	repos := ts.get(t, "/repositories?period=3m&team=platform&sort=reviews&dir=asc").Body.String()
	if !strings.Contains(repos, `<input type="hidden" name="team" value="platform">`) {
		t.Error("the repositories period picker drops the team filter")
	}
	for _, q := range headerHrefs(t, repos) {
		if q.Get("team") != "platform" {
			t.Errorf("the %q header link drops the team filter: %v", q.Get("sort"), q)
		}
	}
}

func TestUnknownTeamDegradesToTheFullView(t *testing.T) {
	ts := newTestServer(t)
	// A renamed or deleted team should not 404 a shared link.
	for _, path := range []string{"/contributors", "/repositories"} {
		w := ts.get(t, path+"?period=12m&team=does-not-exist")
		if w.Code != http.StatusOK {
			t.Errorf("%s with an unknown team = %d, want 200", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "All teams") {
			t.Errorf("%s with an unknown team did not fall back to the full view", path)
		}
	}
	if !strings.Contains(ts.get(t, "/repositories?period=12m&team=does-not-exist").Body.String(), ">legacy<") {
		t.Error("the unfiltered repository list should be shown for an unknown team")
	}
}

func TestReportStaysOrganizationWide(t *testing.T) {
	ts := newTestServer(t)
	// The report is documented as organization-wide, so its link must not carry
	// a team the report would then ignore.
	body := ts.get(t, "/contributors?period=3m&team=platform").Body.String()
	idx := strings.Index(body, "/report.pdf?")
	if idx < 0 {
		t.Fatal("no report link on the page")
	}
	link := body[idx : idx+strings.Index(body[idx:], `"`)]
	if strings.Contains(link, "team=") {
		t.Errorf("the report link carries a team filter it does not apply: %s", link)
	}

	w := ts.get(t, "/report.pdf?period=12m&team=platform")
	if w.Code != http.StatusOK || !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Errorf("/report.pdf with a team parameter = %d", w.Code)
	}
}

func TestStatusPageReportsTeams(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/status").Body.String()
	if !strings.Contains(body, "Teams") {
		t.Error("the status page does not report the team count")
	}
}

func TestOverviewTeamFilterScopesEverySection(t *testing.T) {
	ts := newTestServer(t)

	// The seed gives platform one member (alice) and the api repository;
	// design has neither.
	all := ts.get(t, "/?period=12m").Body.String()
	platform := ts.get(t, "/?period=12m&team=platform").Body.String()
	design := ts.get(t, "/?period=12m&team=design").Body.String()

	if !strings.Contains(platform, "team <strong>Platform</strong>") {
		t.Error("the overview does not state which team it is scoped to")
	}
	if !strings.Contains(platform, "Scoped to the") {
		t.Error("the overview does not explain what the team filter covers")
	}

	// Alice is the only active contributor and she is on platform, so the
	// platform view keeps her while design drops her.
	if !strings.Contains(platform, "@alice") {
		t.Error("a team member is missing from the scoped overview")
	}
	if strings.Contains(design, "@alice") {
		t.Error("a non-member appears in the design overview")
	}
	// The repositories panel follows team repository access.
	if !strings.Contains(platform, ">api<") {
		t.Error("the platform overview should list its api repository")
	}

	if all == platform {
		t.Error("selecting a team did not change the overview")
	}
	if *ts.githubHits != 0 {
		t.Fatal("filtering the overview by team must not call GitHub")
	}
}

func TestOverviewHeadlineNumbersRespectTheTeam(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	// A contributor outside every team, with activity in the period.
	outsider, err := ts.db.UpsertContributor(ctx, &models.Contributor{
		GitHubID: "U_out", Login: "outsider", Name: "Outsider",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.UpsertCommits(ctx, []models.Commit{{
		OrganizationID: ts.org.ID, RepositoryID: ts.repoID, ContributorID: &outsider,
		GitHubOID: "c-outsider", CommittedAt: time.Now().UTC().AddDate(0, 0, -3),
		Additions: 7, Deletions: 1, ChangedFiles: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.RebuildDailyStats(ctx, ts.org.ID, []int64{ts.repoID},
		time.Now().AddDate(-2, 0, 0), time.Now()); err != nil {
		t.Fatal(err)
	}

	f := database.Filter{
		OrganizationID: ts.org.ID,
		From:           time.Now().UTC().AddDate(-1, 0, 0),
		To:             time.Now().UTC(),
	}
	orgWide, err := ts.db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	f.TeamID = &ts.teamID
	scoped, err := ts.db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}

	if orgWide.ActiveContributors <= scoped.ActiveContributors {
		t.Errorf("the team overview counts %d contributors and the organization %d; the filter had no effect",
			scoped.ActiveContributors, orgWide.ActiveContributors)
	}
	if scoped.Commits >= orgWide.Commits {
		t.Errorf("the team overview counts %d commits, the organization %d", scoped.Commits, orgWide.Commits)
	}
	if scoped.Commits == 0 {
		t.Error("the team overview lost its own member's commits")
	}

	// Charts and the activity feed follow the same scope.
	points, err := ts.db.TimeSeries(ctx, f, metrics.GranularityMonth, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var charted int64
	for _, p := range points {
		charted += p.Commits
	}
	if charted != scoped.Commits {
		t.Errorf("the chart totals %d commits but the headline says %d", charted, scoped.Commits)
	}

	activity, err := ts.db.RecentActivity(ctx, f, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range activity {
		if row.Contributor.Login == "outsider" {
			t.Error("the activity feed shows a contributor outside the selected team")
		}
	}
}

func TestOverviewTeamFilterSurvivesNavigation(t *testing.T) {
	ts := newTestServer(t)
	body := ts.get(t, "/?period=3m&team=platform").Body.String()

	// The period form and the navigation links keep the selection.
	if !strings.Contains(body, `<input type="hidden" name="team" value="platform">`) {
		t.Error("the period picker drops the team filter on the overview")
	}
	for _, path := range []string{"/contributors", "/repositories"} {
		if !strings.Contains(body, path+"?period=3m&amp;team=platform") {
			t.Errorf("the %s link does not carry the team filter", path)
		}
	}
}

func TestUnknownTeamOnTheOverviewDegradesGracefully(t *testing.T) {
	ts := newTestServer(t)
	w := ts.get(t, "/?period=12m&team=does-not-exist")
	if w.Code != http.StatusOK {
		t.Fatalf("overview with an unknown team = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "All teams") {
		t.Error("the overview did not fall back to the unfiltered view")
	}
	if strings.Contains(w.Body.String(), "Scoped to the") {
		t.Error("the overview claims a team scope it did not apply")
	}
}
