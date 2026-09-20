package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaxart/dev-pulse/internal/collector"
	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/github"
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
