package database

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
	"github.com/relaxart/dev-pulse/migrations"
)

// These tests need a real PostgreSQL. They are skipped when TEST_DATABASE_URL
// is not set, so `go test ./...` still passes on a machine without a database.
func testDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the PostgreSQL integration tests")
	}
	ctx := context.Background()
	db, err := Connect(ctx, isolatedSchemaURL(t, url, "devpulse_test_database"), 5)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.WaitReady(ctx, 20*time.Second); err != nil {
		t.Fatalf("database not ready: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, migrations.FS, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncate(t, db)
	return db
}

// isolatedSchemaURL gives this test package its own PostgreSQL schema, so that
// `go test ./...` can run several packages against one TEST_DATABASE_URL without
// them truncating each other's tables.
func isolatedSchemaURL(t *testing.T, base, schema string) string {
	t.Helper()
	admin, err := Connect(context.Background(), base, 1)
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

func truncate(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), `
TRUNCATE contributor_daily_stats, pull_request_reviews, pull_requests, commits,
         repositories, contributors, sync_runs, organizations RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func day(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }

func mustOrg(t *testing.T, db *DB, githubID, login string) *models.Organization {
	t.Helper()
	org, err := db.UpsertOrganization(context.Background(), githubID, login, login)
	if err != nil {
		t.Fatalf("upsert organization: %v", err)
	}
	return org
}

func mustRepo(t *testing.T, db *DB, org *models.Organization, githubID, name string, archived bool) int64 {
	t.Helper()
	id, err := db.UpsertRepository(context.Background(), &models.Repository{
		OrganizationID: org.ID, GitHubID: githubID, Name: name,
		FullName: org.Login + "/" + name, IsArchived: archived,
		CreatedAt: day(1), UpdatedAt: day(1),
	})
	if err != nil {
		t.Fatalf("upsert repository: %v", err)
	}
	return id
}

func mustContributor(t *testing.T, db *DB, githubID, login string, isBot bool) int64 {
	t.Helper()
	id, err := db.UpsertContributor(context.Background(), &models.Contributor{
		GitHubID: githubID, Login: login, Name: login, IsBot: isBot,
	})
	if err != nil {
		t.Fatalf("upsert contributor: %v", err)
	}
	return id
}

func mustCommit(t *testing.T, db *DB, orgID, repoID int64, contributorID *int64, oid string, at time.Time, add, del, files int) {
	t.Helper()
	if _, err := db.UpsertCommits(context.Background(), []models.Commit{{
		OrganizationID: orgID, RepositoryID: repoID, ContributorID: contributorID,
		GitHubOID: oid, CommittedAt: at, Additions: add, Deletions: del, ChangedFiles: files,
	}}); err != nil {
		t.Fatalf("upsert commit: %v", err)
	}
}

func mustPR(t *testing.T, db *DB, orgID, repoID int64, authorID *int64, githubID string, number int, created time.Time, merged, closed *time.Time) int64 {
	t.Helper()
	state := models.PRStateOpen
	switch {
	case merged != nil:
		state = models.PRStateMerged
	case closed != nil:
		state = models.PRStateClosed
	}
	id, err := db.UpsertPullRequest(context.Background(), &models.PullRequest{
		OrganizationID: orgID, RepositoryID: repoID, AuthorID: authorID, GitHubID: githubID,
		Number: number, Title: "PR " + githubID, State: state,
		CreatedAt: created, UpdatedAt: created, MergedAt: merged, ClosedAt: closed,
		Additions: 10, Deletions: 5, ChangedFiles: 2,
	})
	if err != nil {
		t.Fatalf("upsert pull request: %v", err)
	}
	return id
}

func mustReview(t *testing.T, db *DB, orgID, repoID, prID int64, reviewerID *int64, githubID, state string, at time.Time, comments int) {
	t.Helper()
	if _, err := db.UpsertReviews(context.Background(), []models.PullRequestReview{{
		OrganizationID: orgID, RepositoryID: repoID, PullRequestID: prID, ReviewerID: reviewerID,
		GitHubID: githubID, State: state, SubmittedAt: at, CommentCount: comments,
	}}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
}

func rebuild(t *testing.T, db *DB, orgID int64, repoIDs []int64) {
	t.Helper()
	if _, err := db.RebuildDailyStats(context.Background(), orgID, repoIDs, day(1), day(30)); err != nil {
		t.Fatalf("rebuild aggregates: %v", err)
	}
}

func fullRange() Filter {
	return Filter{From: day(1), To: day(30)}
}

func countRows(t *testing.T, db *DB, table string) int64 {
	t.Helper()
	var n int64
	// table names here are test constants, never user input.
	if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db := testDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(context.Background(), migrations.FS, log); err != nil {
		t.Fatalf("re-running migrations must be a no-op: %v", err)
	}
	v, err := db.SchemaVersion(context.Background())
	if err != nil || v == "" {
		t.Fatalf("SchemaVersion = %q, %v", v, err)
	}
}

func TestUpsertsAreIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	write := func() {
		mustOrg(t, db, "O_1", "acme")
		mustRepo(t, db, org, "R_1", "api", false)
		mustContributor(t, db, "U_alice", "alice", false)
		mustCommit(t, db, org.ID, repoID, &alice, "c1", day(2), 10, 1, 2)
		prID := mustPR(t, db, org.ID, repoID, &alice, "PR_1", 1, day(2), nil, nil)
		mustReview(t, db, org.ID, repoID, prID, &alice, "RV_1", models.ReviewApproved, day(3), 4)
	}

	write()
	before := map[string]int64{}
	for _, table := range []string{"organizations", "repositories", "contributors", "commits", "pull_requests", "pull_request_reviews"} {
		before[table] = countRows(t, db, table)
	}

	// A full rerun of the same synchronization must not create a single new row.
	for i := 0; i < 3; i++ {
		write()
	}
	for table, want := range before {
		if got := countRows(t, db, table); got != want {
			t.Errorf("%s has %d rows after reruns, want %d (duplicate records were created)", table, got, want)
		}
	}

	// The same commit OID in a different repository is a different commit.
	repo2 := mustRepo(t, db, org, "R_2", "web", false)
	mustCommit(t, db, org.ID, repo2, &alice, "c1", day(2), 1, 1, 1)
	if got := countRows(t, db, "commits"); got != before["commits"]+1 {
		t.Errorf("commits = %d, want %d: the same OID in another repository must be stored separately", got, before["commits"]+1)
	}

	// Updated pull request state must overwrite, not duplicate.
	merged := day(5)
	mustPR(t, db, org.ID, repoID, &alice, "PR_1", 1, day(2), &merged, &merged)
	if got := countRows(t, db, "pull_requests"); got != before["pull_requests"] {
		t.Errorf("pull_requests = %d, want %d", got, before["pull_requests"])
	}
	var state string
	if err := db.Pool.QueryRow(ctx, "SELECT state FROM pull_requests WHERE github_id = 'PR_1'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != models.PRStateMerged {
		t.Errorf("state = %q, want MERGED", state)
	}
}

func TestAggregateRebuildIsIdempotentAndCorrect(t *testing.T) {
	db := testDB(t)
	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)
	bob := mustContributor(t, db, "U_bob", "bob", false)

	// Two commits on the same day, one on another day.
	mustCommit(t, db, org.ID, repoID, &alice, "c1", day(2), 10, 1, 2)
	mustCommit(t, db, org.ID, repoID, &alice, "c2", day(2), 5, 2, 1)
	mustCommit(t, db, org.ID, repoID, &alice, "c3", day(3), 1, 0, 1)
	// A commit with no GitHub account must not create an aggregate row.
	mustCommit(t, db, org.ID, repoID, nil, "c4", day(2), 100, 100, 9)

	merged := day(4)
	prID := mustPR(t, db, org.ID, repoID, &alice, "PR_1", 1, day(2), &merged, &merged)
	closedOnly := day(5)
	mustPR(t, db, org.ID, repoID, &alice, "PR_2", 2, day(3), nil, &closedOnly)

	mustReview(t, db, org.ID, repoID, prID, &bob, "RV_1", models.ReviewApproved, day(4), 3)
	mustReview(t, db, org.ID, repoID, prID, &bob, "RV_2", models.ReviewChangesRequested, day(4), 1)
	// Bob reviews the same pull request again on another day.
	mustReview(t, db, org.ID, repoID, prID, &bob, "RV_3", models.ReviewCommented, day(6), 0)

	rebuild(t, db, org.ID, []int64{repoID})
	rowsAfterFirst := countRows(t, db, "contributor_daily_stats")

	// Rebuilding must produce exactly the same table, never doubled counters.
	rebuild(t, db, org.ID, []int64{repoID})
	if got := countRows(t, db, "contributor_daily_stats"); got != rowsAfterFirst {
		t.Errorf("aggregate rows = %d after a second rebuild, want %d", got, rowsAfterFirst)
	}

	ctx := context.Background()
	var commits, additions, deletions, files, opened, merged2, closed int
	err := db.Pool.QueryRow(ctx, `
SELECT commit_count, additions, deletions, changed_files, prs_opened, prs_merged, prs_closed
  FROM contributor_daily_stats
 WHERE contributor_id = $1 AND date = $2`, alice, day(2).Format(metrics.DateLayout)).
		Scan(&commits, &additions, &deletions, &files, &opened, &merged2, &closed)
	if err != nil {
		t.Fatalf("read aggregate: %v", err)
	}
	if commits != 2 || additions != 15 || deletions != 3 || files != 3 {
		t.Errorf("code activity = %d commits, +%d/-%d, %d files; want 2, +15/-3, 3", commits, additions, deletions, files)
	}
	if opened != 1 {
		t.Errorf("prs_opened = %d, want 1", opened)
	}

	var reviews, approvals, changesRequested, comments int
	err = db.Pool.QueryRow(ctx, `
SELECT reviews_submitted, approvals, changes_requested, review_comments
  FROM contributor_daily_stats
 WHERE contributor_id = $1 AND date = $2`, bob, day(4).Format(metrics.DateLayout)).
		Scan(&reviews, &approvals, &changesRequested, &comments)
	if err != nil {
		t.Fatalf("read review aggregate: %v", err)
	}
	if reviews != 2 || approvals != 1 || changesRequested != 1 || comments != 4 {
		t.Errorf("review activity = %d/%d/%d/%d, want 2/1/1/4", reviews, approvals, changesRequested, comments)
	}

	// prs_closed counts only pull requests closed without a merge.
	var totalClosed, totalMerged int
	if err := db.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(prs_closed),0), COALESCE(SUM(prs_merged),0) FROM contributor_daily_stats WHERE contributor_id = $1`, alice).
		Scan(&totalClosed, &totalMerged); err != nil {
		t.Fatal(err)
	}
	if totalMerged != 1 {
		t.Errorf("prs_merged = %d, want 1", totalMerged)
	}
	if totalClosed != 1 {
		t.Errorf("prs_closed = %d, want 1 (the merged PR must not also count as closed)", totalClosed)
	}

	// A deleted source record must disappear from the aggregates on rebuild.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM commits WHERE github_oid = 'c3'`); err != nil {
		t.Fatal(err)
	}
	rebuild(t, db, org.ID, []int64{repoID})
	var day3Commits int
	err = db.Pool.QueryRow(ctx, `
SELECT COALESCE(SUM(commit_count), 0) FROM contributor_daily_stats
 WHERE contributor_id = $1 AND date = $2`, alice, day(3).Format(metrics.DateLayout)).Scan(&day3Commits)
	if err != nil {
		t.Fatal(err)
	}
	if day3Commits != 0 {
		t.Errorf("stale aggregate survived the rebuild: %d commits", day3Commits)
	}
}

// seedTwoOrganizations creates two organizations with identical-looking data.
func seedTwoOrganizations(t *testing.T, db *DB) (a, b *models.Organization) {
	t.Helper()
	a = mustOrg(t, db, "O_a", "org-a")
	b = mustOrg(t, db, "O_b", "org-b")

	repoA := mustRepo(t, db, a, "R_a", "api", false)
	repoB := mustRepo(t, db, b, "R_b", "api", false)

	alice := mustContributor(t, db, "U_alice", "alice", false)
	mallory := mustContributor(t, db, "U_mallory", "mallory", false)

	mustCommit(t, db, a.ID, repoA, &alice, "a1", day(2), 10, 1, 1)
	mustCommit(t, db, a.ID, repoA, &alice, "a2", day(3), 10, 1, 1)
	prA := mustPR(t, db, a.ID, repoA, &alice, "PR_a", 1, day(2), nil, nil)
	mustReview(t, db, a.ID, repoA, prA, &alice, "RV_a", models.ReviewApproved, day(3), 1)

	// org-b gets far more activity; none of it may leak into org-a's numbers.
	for i := 0; i < 20; i++ {
		mustCommit(t, db, b.ID, repoB, &mallory, fmt.Sprintf("b%d", i), day(2), 100, 50, 5)
	}
	prB := mustPR(t, db, b.ID, repoB, &mallory, "PR_b", 1, day(2), nil, nil)
	mustReview(t, db, b.ID, repoB, prB, &mallory, "RV_b", models.ReviewApproved, day(3), 9)

	rebuild(t, db, a.ID, []int64{repoA})
	rebuild(t, db, b.ID, []int64{repoB})
	return a, b
}

func TestOrganizationIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	a, b := seedTwoOrganizations(t, db)

	f := fullRange()
	f.OrganizationID = a.ID

	overview, err := db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 2 {
		t.Errorf("org-a commits = %d, want 2 (org-b data leaked in)", overview.Commits)
	}
	if overview.ActiveContributors != 1 {
		t.Errorf("org-a contributors = %d, want 1", overview.ActiveContributors)
	}

	ranking, err := db.ContributorRanking(ctx, f, "commits", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range ranking {
		if row.Login == "mallory" {
			t.Error("a contributor active only in org-b appears in org-a's ranking")
		}
	}

	repos, err := db.RepositoryRanking(ctx, f, metrics.DefaultRepositorySort, metrics.SortDesc)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Errorf("org-a has %d repositories in the ranking, want 1", len(repos))
	}

	counts, err := db.OrganizationCounts(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Commits != 2 || counts.Repositories != 1 || counts.Reviews != 1 {
		t.Errorf("org-a counts = %+v", counts)
	}

	// A contributor active only in org-b must not resolve inside org-a.
	c, err := db.ContributorByLogin(ctx, a.ID, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Error("ContributorByLogin must be scoped to the organization")
	}
	if c, err = db.ContributorByLogin(ctx, b.ID, "mallory"); err != nil || c == nil {
		t.Errorf("mallory should resolve inside org-b: %v %v", c, err)
	}

	// The same repository name exists in both organizations.
	repo, err := db.RepositoryByName(ctx, a.ID, "api")
	if err != nil || repo == nil {
		t.Fatalf("RepositoryByName: %v %v", repo, err)
	}
	if repo.OrganizationID != a.ID {
		t.Error("RepositoryByName returned a repository of the wrong organization")
	}
}

func TestArchivedRepositoriesAreExcludedFromAnalytics(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	active := mustRepo(t, db, org, "R_1", "api", false)
	archived := mustRepo(t, db, org, "R_2", "legacy", true)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	mustCommit(t, db, org.ID, active, &alice, "c1", day(2), 1, 0, 1)
	mustCommit(t, db, org.ID, archived, &alice, "c2", day(2), 50, 0, 5)
	rebuild(t, db, org.ID, []int64{active, archived})

	f := fullRange()
	f.OrganizationID = org.ID

	overview, err := db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 1 {
		t.Errorf("commits = %d, want 1: archived repositories must be excluded by default", overview.Commits)
	}
	if overview.ActiveRepositories != 1 {
		t.Errorf("active repositories = %d, want 1", overview.ActiveRepositories)
	}

	ranking, err := db.ContributorRanking(ctx, f, "commits", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking) != 1 || ranking[0].Commits != 1 {
		t.Errorf("ranking = %+v, want a single row with 1 commit", ranking)
	}

	// The archived data is still in the database and appears when included.
	f.IncludeArchived = true
	overview, err = db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 2 {
		t.Errorf("commits with INCLUDE_ARCHIVED=true = %d, want 2", overview.Commits)
	}

	// The repository list keeps archived repositories visible and flagged.
	f.IncludeArchived = true
	repos, err := db.RepositoryRanking(ctx, f, metrics.DefaultRepositorySort, metrics.SortDesc)
	if err != nil {
		t.Fatal(err)
	}
	var sawArchived bool
	for _, r := range repos {
		if r.Name == "legacy" {
			sawArchived = r.IsArchived
		}
	}
	if !sawArchived {
		t.Error("the archived repository must still be listed and flagged")
	}
}

func TestBotsAndExcludedUsersAreLeftOutOfRankings(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)

	alice := mustContributor(t, db, "U_alice", "alice", false)
	bot := mustContributor(t, db, "U_bot", "dependabot[bot]", true)
	renovate := mustContributor(t, db, "U_renovate", "renovate[bot]", false) // not flagged by GitHub
	ci := mustContributor(t, db, "U_ci", "acme-ci", false)

	mustCommit(t, db, org.ID, repoID, &alice, "c1", day(2), 1, 0, 1)
	mustCommit(t, db, org.ID, repoID, &bot, "c2", day(2), 900, 900, 90)
	mustCommit(t, db, org.ID, repoID, &renovate, "c3", day(2), 800, 800, 80)
	mustCommit(t, db, org.ID, repoID, &ci, "c4", day(2), 700, 700, 70)
	rebuild(t, db, org.ID, []int64{repoID})

	f := fullRange()
	f.OrganizationID = org.ID
	f.ExcludedLogins = []string{"renovate[bot]", "acme-ci"}

	ranking, err := db.ContributorRanking(ctx, f, "commits", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking) != 1 {
		t.Fatalf("ranking has %d rows, want only alice: %+v", len(ranking), ranking)
	}
	if ranking[0].Login != "alice" {
		t.Errorf("ranking[0] = %s, want alice", ranking[0].Login)
	}

	overview, err := db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 1 {
		t.Errorf("overview commits = %d, want 1: bot and excluded activity must not be counted", overview.Commits)
	}

	// Exclusion is a query-time decision, so clearing the list restores the rows
	// without re-synchronizing anything.
	f.ExcludedLogins = nil
	ranking, err = db.ContributorRanking(ctx, f, "commits", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking) != 3 {
		t.Errorf("ranking has %d rows, want 3 (the GitHub bot stays excluded)", len(ranking))
	}
	for _, row := range ranking {
		if row.Login == "dependabot[bot]" {
			t.Error("an account GitHub reports as a bot must always stay out of rankings")
		}
	}
}

func TestContributorRankingSortsByEveryMetric(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	repo1 := mustRepo(t, db, org, "R_1", "api", false)
	repo2 := mustRepo(t, db, org, "R_2", "web", false)

	alice := mustContributor(t, db, "U_alice", "alice", false)
	bob := mustContributor(t, db, "U_bob", "bob", false)

	// Alice: many commits, few reviews. Bob: few commits, many reviews.
	for i := 0; i < 5; i++ {
		mustCommit(t, db, org.ID, repo1, &alice, fmt.Sprintf("a%d", i), day(2+i), 100, 10, 4)
	}
	mustCommit(t, db, org.ID, repo2, &alice, "a-web", day(2), 1, 0, 1)
	mustCommit(t, db, org.ID, repo1, &bob, "b1", day(2), 5, 1, 1)

	prA := mustPR(t, db, org.ID, repo1, &alice, "PR_1", 1, day(2), nil, nil)
	prB := mustPR(t, db, org.ID, repo1, &bob, "PR_2", 2, day(2), nil, nil)
	merged := day(4)
	mustPR(t, db, org.ID, repo1, &bob, "PR_3", 3, day(3), &merged, &merged)

	// Bob reviews Alice's PR twice on two days: two submissions, one unique PR.
	mustReview(t, db, org.ID, repo1, prA, &bob, "RV_1", models.ReviewApproved, day(3), 5)
	mustReview(t, db, org.ID, repo1, prA, &bob, "RV_2", models.ReviewCommented, day(4), 2)
	mustReview(t, db, org.ID, repo1, prB, &bob, "RV_3", models.ReviewChangesRequested, day(4), 1)
	mustReview(t, db, org.ID, repo1, prB, &bob, "RV_4", models.ReviewApproved, day(5), 0)
	mustReview(t, db, org.ID, repo1, prB, &alice, "RV_5", models.ReviewApproved, day(5), 0)

	rebuild(t, db, org.ID, []int64{repo1, repo2})

	f := fullRange()
	f.OrganizationID = org.ID

	wantTop := map[string]string{
		"commits":             "alice",
		"additions":           "alice",
		"deletions":           "alice",
		"changed_files":       "alice",
		"prs_opened":          "bob",
		"prs_merged":          "bob",
		"reviews_submitted":   "bob",
		"approvals":           "bob",
		"changes_requested":   "bob",
		"review_comments":     "bob",
		"unique_prs_reviewed": "bob",
		"active_days":         "alice",
		"repositories":        "alice",
	}
	for _, metric := range metrics.RankingMetrics() {
		rows, err := db.ContributorRanking(ctx, f, metric.Key, 10, 0)
		if err != nil {
			t.Fatalf("sort by %s: %v", metric.Key, err)
		}
		if len(rows) != 2 {
			t.Fatalf("sort by %s returned %d rows", metric.Key, len(rows))
		}
		if want, ok := wantTop[metric.Key]; ok && rows[0].Login != want {
			t.Errorf("sorted by %s the top contributor is %s, want %s", metric.Key, rows[0].Login, want)
		}
	}

	// prs_closed has no data; sorting must still work and stay deterministic.
	rows, err := db.ContributorRanking(ctx, f, "prs_closed", 10, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("sort by prs_closed: %v (%d rows)", err, len(rows))
	}

	// "Unique PRs reviewed" must not double count a pull request reviewed twice.
	byLogin := map[string]models.ContributorStats{}
	rows, err = db.ContributorRanking(ctx, f, "reviews_submitted", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		byLogin[r.Login] = r
	}
	if got := byLogin["bob"].ReviewsSubmitted; got != 4 {
		t.Errorf("bob reviews_submitted = %d, want 4", got)
	}
	if got := byLogin["bob"].UniquePRsReviewed; got != 2 {
		t.Errorf("bob unique_prs_reviewed = %d, want 2 (two submissions on one PR count once)", got)
	}
	// Alice committed on days 2-6; the pull request and review she opened fall on
	// days already covered, so she has five distinct active days.
	if got := byLogin["alice"].ActiveDays; got != 5 {
		t.Errorf("alice active_days = %d, want 5", got)
	}
	if got := byLogin["alice"].Repositories; got != 2 {
		t.Errorf("alice repositories = %d, want 2", got)
	}

	// An unknown sort key must fall back rather than break the query.
	if _, err := db.ContributorRanking(ctx, f, "commits; DROP TABLE commits", 10, 0); err != nil {
		t.Fatalf("an unknown sort key must be ignored, got %v", err)
	}
	if countRows(t, db, "commits") == 0 {
		t.Fatal("the commits table disappeared: the sort key reached SQL")
	}
}

func TestDateFilteringChangesOnlyTheWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	mustCommit(t, db, org.ID, repoID, &alice, "old", day(2), 1, 0, 1)
	mustCommit(t, db, org.ID, repoID, &alice, "new", day(20), 1, 0, 1)
	rebuild(t, db, org.ID, []int64{repoID})

	f := Filter{OrganizationID: org.ID, From: day(15), To: day(25)}
	overview, err := db.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 1 {
		t.Errorf("commits in the narrow window = %d, want 1", overview.Commits)
	}

	f.From, f.To = day(1), day(30)
	if overview, err = db.Overview(ctx, f); err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 2 {
		t.Errorf("commits in the wide window = %d, want 2", overview.Commits)
	}

	// Boundaries are inclusive.
	f.From, f.To = day(20), day(20)
	if overview, err = db.Overview(ctx, f); err != nil {
		t.Fatal(err)
	}
	if overview.Commits != 1 {
		t.Errorf("a single-day window must include that day, got %d commits", overview.Commits)
	}
}

func TestTimeSeriesGranularity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	for _, d := range []int{1, 2, 8, 9, 20} {
		mustCommit(t, db, org.ID, repoID, &alice, fmt.Sprintf("c%d", d), day(d), 1, 0, 1)
	}
	rebuild(t, db, org.ID, []int64{repoID})

	f := fullRange()
	f.OrganizationID = org.ID

	daily, err := db.TimeSeries(ctx, f, metrics.GranularityDay, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 5 {
		t.Errorf("daily buckets = %d, want 5", len(daily))
	}

	weekly, err := db.TimeSeries(ctx, f, metrics.GranularityWeek, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(weekly) >= len(daily) {
		t.Errorf("weekly buckets (%d) should be fewer than daily (%d)", len(weekly), len(daily))
	}

	monthly, err := db.TimeSeries(ctx, f, metrics.GranularityMonth, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(monthly) != 1 {
		t.Errorf("monthly buckets = %d, want 1", len(monthly))
	}
	var total int64
	for _, p := range daily {
		total += p.Commits
	}
	if total != monthly[0].Commits {
		t.Errorf("granularity changed the totals: %d vs %d", total, monthly[0].Commits)
	}

	// Scoping to one contributor and one repository must also work.
	if _, err := db.TimeSeries(ctx, f, metrics.GranularityDay, &alice, &repoID); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryAndContributorDetailQueries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	repoID := mustRepo(t, db, org, "R_1", "api", false)
	empty := mustRepo(t, db, org, "R_2", "untouched", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	mustCommit(t, db, org.ID, repoID, &alice, "c1", day(2), 10, 2, 3)
	prID := mustPR(t, db, org.ID, repoID, &alice, "PR_1", 1, day(2), nil, nil)
	mustReview(t, db, org.ID, repoID, prID, &alice, "RV_1", models.ReviewApproved, day(3), 2)
	rebuild(t, db, org.ID, []int64{repoID, empty})

	f := fullRange()
	f.OrganizationID = org.ID

	totals, err := db.ContributorTotals(ctx, f, alice)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Commits != 1 || totals.PRsOpened != 1 || totals.ReviewsSubmitted != 1 || totals.UniquePRsReviewed != 1 {
		t.Errorf("contributor totals = %+v", totals)
	}

	repos, err := db.ContributorRepositories(ctx, f, alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "api" {
		t.Errorf("contributor repositories = %+v", repos)
	}

	prs, err := db.ContributorPullRequests(ctx, f, alice, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].AuthorLogin != "alice" || prs[0].RepositoryName != "api" {
		t.Errorf("contributor pull requests = %+v", prs)
	}

	reviews, err := db.ContributorReviews(ctx, f, alice, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].PullRequestNumber != 1 {
		t.Errorf("contributor reviews = %+v", reviews)
	}

	repoTotals, err := db.RepositoryTotals(ctx, f, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if repoTotals.Commits != 1 || repoTotals.Contributors != 1 || repoTotals.Reviews != 1 {
		t.Errorf("repository totals = %+v", repoTotals)
	}

	repoContributors, err := db.RepositoryContributors(ctx, f, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repoContributors) != 1 {
		t.Errorf("repository contributors = %+v", repoContributors)
	}

	// A repository with no activity in the window must still be listed.
	ranking, err := db.RepositoryRanking(ctx, f, metrics.DefaultRepositorySort, metrics.SortDesc)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking) != 2 {
		t.Errorf("repository ranking = %d rows, want 2 including the inactive repository", len(ranking))
	}

	activity, err := db.RecentActivity(ctx, f, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity) == 0 {
		t.Error("recent activity should not be empty")
	}
}

func TestSyncRunBookkeeping(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")

	id, err := db.StartSyncRun(ctx, org.ID, "initial")
	if err != nil {
		t.Fatal(err)
	}
	last, err := db.LastSyncRun(ctx, org.ID)
	if err != nil || last == nil || last.Status != models.SyncStatusRunning {
		t.Fatalf("LastSyncRun = %+v, %v", last, err)
	}
	if ok, err := db.LastSuccessfulSyncRun(ctx, org.ID); err != nil || ok != nil {
		t.Fatalf("there is no successful run yet: %+v %v", ok, err)
	}

	reset := day(3)
	if err := db.FinishSyncRun(ctx, &models.SyncRun{
		ID: id, Status: models.SyncStatusPartial, DurationSeconds: 37, RecordsProcessed: 120,
		Repositories: 4, GraphQLCost: 88, RateLimitRemain: 4321, RateLimitLimit: 5000,
		RateLimitResetAt: &reset, ErrorCount: 1, ErrorMessage: "acme/api: boom",
	}); err != nil {
		t.Fatal(err)
	}

	success, err := db.LastSuccessfulSyncRun(ctx, org.ID)
	if err != nil || success == nil {
		t.Fatalf("a partial run counts as a successful-enough run: %+v %v", success, err)
	}
	if success.RecordsProcessed != 120 || success.GraphQLCost != 88 || success.RateLimitRemain != 4321 {
		t.Errorf("run metadata = %+v", success)
	}

	runs, err := db.RecentSyncRuns(ctx, org.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentSyncRuns = %d rows, %v", len(runs), err)
	}

	// An interrupted run is closed out at the next startup.
	if _, err := db.StartSyncRun(ctx, org.ID, "incremental"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkStaleRunsFailed(ctx); err != nil {
		t.Fatal(err)
	}
	last, err = db.LastSyncRun(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last.Status != models.SyncStatusFailed {
		t.Errorf("an interrupted run must be marked failed, got %q", last.Status)
	}
}

func TestConnectionErrorsHideCredentials(t *testing.T) {
	_, err := Connect(context.Background(), "postgres://user:sup3rsecret@nonexistent.invalid:5432/db?sslmode=disable&pool_max_conns=abc", 1)
	if err == nil {
		t.Skip("the malformed URL was accepted; nothing to assert")
	}
	if got := err.Error(); containsSecret(got) {
		t.Fatalf("connection error leaked the password: %s", got)
	}
}

func containsSecret(s string) bool {
	return len(s) > 0 && (contains(s, "sup3rsecret"))
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestRepositoryRankingSortsByEveryColumn(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")

	// zulu: most commits, no reviews, active, newest activity.
	zulu := mustRepo(t, db, org, "R_z", "zulu", false)
	// alpha: fewest commits, most reviews, oldest activity.
	alpha := mustRepo(t, db, org, "R_a", "alpha", false)
	// mike is archived and has no activity at all.
	mike := mustRepo(t, db, org, "R_m", "mike", true)

	alice := mustContributor(t, db, "U_alice", "alice", false)
	bob := mustContributor(t, db, "U_bob", "bob", false)

	for i := 0; i < 6; i++ {
		mustCommit(t, db, org.ID, zulu, &alice, fmt.Sprintf("z%d", i), day(10+i), 100, 10, 5)
	}
	mustCommit(t, db, org.ID, alpha, &alice, "a1", day(2), 5, 1, 1)
	mustCommit(t, db, org.ID, alpha, &bob, "a2", day(2), 5, 1, 1)

	prZ := mustPR(t, db, org.ID, zulu, &alice, "PR_z", 1, day(11), nil, nil)
	prA := mustPR(t, db, org.ID, alpha, &alice, "PR_a1", 1, day(2), nil, nil)
	merged := day(3)
	mustPR(t, db, org.ID, alpha, &bob, "PR_a2", 2, day(2), &merged, &merged)
	_ = prZ

	mustReview(t, db, org.ID, alpha, prA, &bob, "RV_1", models.ReviewApproved, day(3), 1)
	mustReview(t, db, org.ID, alpha, prA, &alice, "RV_2", models.ReviewCommented, day(3), 1)

	rebuild(t, db, org.ID, []int64{zulu, alpha, mike})

	f := fullRange()
	f.OrganizationID = org.ID
	f.IncludeArchived = true

	cases := []struct {
		key, dir string
		wantTop  string
	}{
		{"name", metrics.SortAsc, "alpha"},
		{"name", metrics.SortDesc, "zulu"},
		{"commits", metrics.SortDesc, "zulu"},
		{"contributors", metrics.SortDesc, "alpha"},
		{"additions", metrics.SortDesc, "zulu"},
		{"prs_opened", metrics.SortDesc, "alpha"},
		{"prs_merged", metrics.SortDesc, "alpha"},
		{"reviews", metrics.SortDesc, "alpha"},
		{"last_activity", metrics.SortDesc, "zulu"},
		{"last_activity", metrics.SortAsc, "alpha"},
		{"status", metrics.SortDesc, "mike"},
	}
	for _, c := range cases {
		t.Run(c.key+"-"+c.dir, func(t *testing.T) {
			rows, err := db.RepositoryRanking(ctx, f, c.key, c.dir)
			if err != nil {
				t.Fatalf("sort by %s %s: %v", c.key, c.dir, err)
			}
			if len(rows) != 3 {
				t.Fatalf("got %d rows, want 3", len(rows))
			}
			if rows[0].Name != c.wantTop {
				names := make([]string, len(rows))
				for i, r := range rows {
					names[i] = r.Name
				}
				t.Errorf("sorted by %s %s the order is %v, want %q first", c.key, c.dir, names, c.wantTop)
			}
		})
	}
}

func TestRepositoriesWithoutActivitySortLastInBothDirections(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	active := mustRepo(t, db, org, "R_a", "active", false)
	idle := mustRepo(t, db, org, "R_i", "idle", false)
	alice := mustContributor(t, db, "U_alice", "alice", false)

	mustCommit(t, db, org.ID, active, &alice, "c1", day(2), 10, 1, 1)
	rebuild(t, db, org.ID, []int64{active, idle})

	f := fullRange()
	f.OrganizationID = org.ID

	// A repository with no activity in the period has a NULL last_activity. It
	// must never jump to the top of an ascending sort.
	for _, dir := range []string{metrics.SortAsc, metrics.SortDesc} {
		rows, err := db.RepositoryRanking(ctx, f, "last_activity", dir)
		if err != nil {
			t.Fatal(err)
		}
		if rows[len(rows)-1].Name != "idle" {
			t.Errorf("sorting last_activity %s put %q last, want idle", dir, rows[len(rows)-1].Name)
		}
	}
}

func TestRepositoryRankingIgnoresAnInjectedSortKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	org := mustOrg(t, db, "O_1", "acme")
	mustRepo(t, db, org, "R_1", "api", false)

	f := fullRange()
	f.OrganizationID = org.ID

	rows, err := db.RepositoryRanking(ctx, f, "name; DROP TABLE repositories", "ASC; DROP TABLE repositories")
	if err != nil {
		t.Fatalf("an unknown sort must be ignored, not fail: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1", len(rows))
	}
	if countRows(t, db, "repositories") != 1 {
		t.Fatal("the repositories table disappeared: the sort key reached SQL")
	}
}
