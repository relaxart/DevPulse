package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/github"
	"github.com/relaxart/dev-pulse/internal/models"
)

var syncNow = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(org string) *config.Config {
	return &config.Config{
		GitHubToken:   "github_pat_secret",
		GitHubOrg:     org,
		DatabaseURL:   "postgres://localhost/devpulse",
		SyncInterval:  30 * time.Minute,
		HistoryMonths: 12,
		MinRateLimit:  200,
		SyncTeams:     true,
	}
}

func actor(id, login string) *github.Actor {
	return &github.Actor{TypeName: "User", ID: id, Login: login, Name: strings.ToUpper(login[:1]) + login[1:]}
}

func ptr[T any](v T) *T { return &v }

// acmeFixture is a small but complete organization: two active repositories, one
// archived, commits with and without a linked GitHub account, pull requests and
// reviews (one of which needs nested review pagination).
func acmeFixture() *orgFixture {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 10, 0, 0, 0, time.UTC) }

	api := repoFixture{
		repo: github.Repository{
			ID: "R_api", Name: "api", NameWithOwner: "acme/api", URL: "https://github.com/acme/api",
			Owner: struct {
				Login string `json:"login"`
			}{Login: "acme"},
		},
		commits: []github.Commit{
			{OID: "c1", CommittedDate: day(1), Additions: 10, Deletions: 2, ChangedFilesIfAvailable: ptr(3),
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Alice", Email: "alice@acme.dev", User: actor("U_alice", "alice")}},
			{OID: "c2", CommittedDate: day(2), Additions: 5, Deletions: 1,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Bob", Email: "bob@acme.dev", User: actor("U_bob", "bob")}},
			{OID: "c3", CommittedDate: day(3), Additions: 1, Deletions: 0,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Former Colleague", Email: "gone@acme.dev", User: nil}},
			{OID: "c4", CommittedDate: day(4), Additions: 200, Deletions: 0,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "dependabot", Email: "bot@github.com",
					User: &github.Actor{TypeName: "Bot", ID: "U_dependabot", Login: "dependabot[bot]"}}},
		},
		prs: []github.PullRequest{
			{ID: "PR_1", Number: 1, Title: "Add endpoint", State: models.PRStateMerged,
				CreatedAt: day(1), UpdatedAt: day(5), MergedAt: ptr(day(5)), ClosedAt: ptr(day(5)),
				Additions: 40, Deletions: 3, ChangedFiles: 4, Author: actor("U_alice", "alice")},
			{ID: "PR_2", Number: 2, Title: "Fix flake", State: models.PRStateOpen,
				CreatedAt: day(6), UpdatedAt: day(6), Author: actor("U_bob", "bob")},
		},
		reviews: map[int][]github.Review{
			// Three reviews on PR 1 force nested review pagination at page size 2.
			1: {
				{ID: "RV_1", State: models.ReviewApproved, SubmittedAt: ptr(day(5)), Author: actor("U_bob", "bob")},
				{ID: "RV_2", State: models.ReviewCommented, SubmittedAt: ptr(day(5)), Author: actor("U_carol", "carol")},
				{ID: "RV_3", State: models.ReviewChangesRequested, SubmittedAt: ptr(day(5)), Author: actor("U_carol", "carol")},
			},
			2: {
				{ID: "RV_4", State: models.ReviewApproved, SubmittedAt: ptr(day(6)), Author: actor("U_alice", "alice")},
				// A pending review has no submittedAt and must not be counted.
				{ID: "RV_5", State: "PENDING", SubmittedAt: nil, Author: actor("U_alice", "alice")},
			},
		},
	}

	web := repoFixture{
		repo: github.Repository{ID: "R_web", Name: "web", NameWithOwner: "acme/web",
			Owner: struct {
				Login string `json:"login"`
			}{Login: "acme"}},
		commits: []github.Commit{
			{OID: "w1", CommittedDate: day(7), Additions: 3, Deletions: 3,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Carol", Email: "carol@acme.dev", User: actor("U_carol", "carol")}},
		},
		reviews: map[int][]github.Review{},
	}

	legacy := repoFixture{
		repo: github.Repository{ID: "R_legacy", Name: "legacy", NameWithOwner: "acme/legacy", IsArchived: true,
			Owner: struct {
				Login string `json:"login"`
			}{Login: "acme"}},
		commits: []github.Commit{
			{OID: "l1", CommittedDate: day(8), Additions: 1, Deletions: 1,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Alice", Email: "alice@acme.dev", User: actor("U_alice", "alice")}},
		},
		reviews: map[int][]github.Review{},
	}

	return &orgFixture{
		org:   github.Organization{ID: "O_acme", Login: "acme", Name: "Acme Inc"},
		repos: []repoFixture{api, web, legacy},
	}
}

// otherOrgFixture is an organization the token can see but that must never be
// queried, because it is not the one in GITHUB_ORG.
func otherOrgFixture() *orgFixture {
	return &orgFixture{
		org: github.Organization{ID: "O_other", Login: "open-source-org", Name: "Open Source Org"},
		repos: []repoFixture{{
			repo: github.Repository{ID: "R_other", Name: "sideproject", NameWithOwner: "open-source-org/sideproject",
				Owner: struct {
					Login string `json:"login"`
				}{Login: "open-source-org"}},
			commits: []github.Commit{{OID: "x1", CommittedDate: syncNow.AddDate(0, 0, -2), Additions: 999,
				Author: &struct {
					Name  string        `json:"name"`
					Email string        `json:"email"`
					User  *github.Actor `json:"user"`
				}{Name: "Mallory", User: actor("U_mallory", "mallory")}}},
			reviews: map[int][]github.Review{},
		}},
	}
}

func newTestCollector(t *testing.T, cfg *config.Config, api *fakeAPI, store *fakeStore) *Collector {
	t.Helper()
	c := New(cfg, api, store, testLogger())
	c.now = func() time.Time { return syncNow }
	return c
}

func TestSyncQueriesOnlyTheConfiguredOrganization(t *testing.T) {
	api := newFakeAPI(2, acmeFixture(), otherOrgFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if result.Status != models.SyncStatusSuccess {
		t.Fatalf("status = %s, errors = %v", result.Status, result.Errors)
	}

	for _, login := range api.askedOrgs {
		if !strings.EqualFold(login, "acme") {
			t.Errorf("organization %q was queried although GITHUB_ORG=acme", login)
		}
	}
	for _, repo := range api.askedRepos {
		if !strings.HasPrefix(repo, "acme/") {
			t.Errorf("repository %q was queried although it is outside the configured organization", repo)
		}
	}

	// Nothing from the other organization may have been stored.
	for id := range store.repos {
		if id == "R_other" {
			t.Error("a repository of open-source-org was imported")
		}
	}
	if _, ok := store.contributors["U_mallory"]; ok {
		t.Error("a contributor of open-source-org was imported")
	}
}

func TestEveryStoredRecordCarriesTheOrganization(t *testing.T) {
	api := newFakeAPI(2, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	org := store.orgsByGitHubID["O_acme"]
	if org == nil {
		t.Fatal("the organization was not stored")
	}

	for _, r := range store.repos {
		if r.OrganizationID != org.ID {
			t.Errorf("repository %s has organization_id %d, want %d", r.FullName, r.OrganizationID, org.ID)
		}
	}
	for key, commit := range store.commits {
		if commit.OrganizationID != org.ID {
			t.Errorf("commit %s has organization_id %d", key, commit.OrganizationID)
		}
	}
	for _, pr := range store.prs {
		if pr.OrganizationID != org.ID {
			t.Errorf("pull request #%d has organization_id %d", pr.Number, pr.OrganizationID)
		}
	}
	for id, rv := range store.reviews {
		if rv.OrganizationID != org.ID {
			t.Errorf("review %s has organization_id %d", id, rv.OrganizationID)
		}
	}
}

func TestPaginationCollectsEveryRecord(t *testing.T) {
	// A page size of 1 forces the collector through every cursor.
	api := newFakeAPI(1, acmeFixture())
	store := newFakeStore()
	cfg := testConfig("acme")
	cfg.IncludeArchived = true
	c := newTestCollector(t, cfg, api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	commits, prs, reviews, _, repos := store.counts()
	if repos != 3 {
		t.Errorf("stored %d repositories, want 3", repos)
	}
	if commits != 6 {
		t.Errorf("stored %d commits, want 6 (4 api + 1 web + 1 legacy)", commits)
	}
	if prs != 2 {
		t.Errorf("stored %d pull requests, want 2", prs)
	}
	// 4 submitted reviews; the pending one has no submittedAt and is skipped.
	if reviews != 4 {
		t.Errorf("stored %d reviews, want 4", reviews)
	}
}

func TestNestedReviewPaginationIsFollowed(t *testing.T) {
	api := newFakeAPI(2, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// PR 1 has three reviews while the fake serves two per page.
	for _, id := range []string{"RV_1", "RV_2", "RV_3"} {
		if _, ok := store.reviews[id]; !ok {
			t.Errorf("review %s was not collected: nested review pagination is incomplete", id)
		}
	}
}

func TestRerunningSyncCreatesNoDuplicates(t *testing.T) {
	api := newFakeAPI(2, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	commits1, prs1, reviews1, contributors1, repos1 := store.counts()

	// Reset the cache so the second run cannot be idempotent by accident.
	c.contributors = map[string]int64{}
	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	commits2, prs2, reviews2, contributors2, repos2 := store.counts()

	if commits1 != commits2 || prs1 != prs2 || reviews1 != reviews2 || contributors1 != contributors2 || repos1 != repos2 {
		t.Errorf("rerunning the sync changed row counts: commits %d→%d, prs %d→%d, reviews %d→%d, contributors %d→%d, repos %d→%d",
			commits1, commits2, prs1, prs2, reviews1, reviews2, contributors1, contributors2, repos1, repos2)
	}
}

func TestSecondSyncIsIncremental(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	cfg := testConfig("acme")
	c := newTestCollector(t, cfg, api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstRunCalls := len(api.commitCall)
	historyStart := cfg.HistoryStart(syncNow)
	for _, call := range api.commitCall {
		if !call.since.Equal(historyStart) {
			t.Errorf("initial sync of %s used since=%s, want the %d month history start %s",
				call.repo, call.since, cfg.HistoryMonths, historyStart)
		}
	}
	if store.lastRun().SyncType != "initial" {
		t.Errorf("first run recorded as %q, want initial", store.lastRun().SyncType)
	}

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.lastRun().SyncType != "incremental" {
		t.Errorf("second run recorded as %q, want incremental", store.lastRun().SyncType)
	}
	for _, call := range api.commitCall[firstRunCalls:] {
		if call.since.Equal(historyStart) {
			t.Errorf("incremental sync of %s re-read the whole history (since=%s)", call.repo, call.since)
		}
		if call.since.Before(syncNow.Add(-2 * watermarkOverlap)) {
			t.Errorf("incremental since=%s is further back than the watermark overlap allows", call.since)
		}
	}
}

func TestArchivedRepositoriesAreStoredButNotCollected(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	cfg := testConfig("acme") // INCLUDE_ARCHIVED=false
	c := newTestCollector(t, cfg, api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.repos["R_legacy"]; !ok {
		t.Fatal("the archived repository must still be stored")
	}
	if !store.repos["R_legacy"].IsArchived {
		t.Error("the archived flag was not persisted")
	}
	for _, repo := range api.askedRepos {
		if repo == "acme/legacy" {
			t.Error("activity of an archived repository was collected although INCLUDE_ARCHIVED=false")
		}
	}

	// With INCLUDE_ARCHIVED=true the same repository is collected.
	api2 := newFakeAPI(50, acmeFixture())
	store2 := newFakeStore()
	cfg2 := testConfig("acme")
	cfg2.IncludeArchived = true
	c2 := newTestCollector(t, cfg2, api2, store2)
	if _, err := c2.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, repo := range api2.askedRepos {
		if repo == "acme/legacy" {
			found = true
		}
	}
	if !found {
		t.Error("INCLUDE_ARCHIVED=true must collect archived repository activity")
	}
}

func TestBotAccountsAreFlagged(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	bot, ok := store.contributors["U_dependabot"]
	if !ok {
		t.Fatal("the bot author was not stored")
	}
	if !bot.IsBot {
		t.Error("a GitHub Bot account must be marked is_bot so rankings can exclude it")
	}
	if human := store.contributors["U_alice"]; human == nil || human.IsBot {
		t.Error("a human contributor must not be marked as a bot")
	}
}

func TestCommitsWithoutAGitHubAccountAreStoredUnattributed(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatalf("an unmapped commit author must not break synchronization: %v", err)
	}
	var found bool
	for _, commit := range store.commits {
		if commit.GitHubOID == "c3" {
			found = true
			if commit.ContributorID != nil {
				t.Error("a commit without a linked GitHub user must have no contributor")
			}
			if commit.AuthorName != "Former Colleague" {
				t.Errorf("the raw commit author name should be kept, got %q", commit.AuthorName)
			}
		}
	}
	if !found {
		t.Error("the unattributed commit was not stored")
	}
}

func TestOneFailingRepositoryDoesNotAbortTheRun(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	api.repoErrors["acme/api"] = errors.New("simulated GitHub failure")
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("a repository level failure must not fail the whole run: %v", err)
	}
	if result.Status != models.SyncStatusPartial {
		t.Errorf("status = %s, want partial", result.Status)
	}
	if len(result.Errors) == 0 {
		t.Error("the repository error must be reported, never silently discarded")
	}
	if !strings.Contains(strings.Join(result.Errors, " "), "acme/api") {
		t.Errorf("the error should name the failing repository, got %v", result.Errors)
	}

	// The healthy repository must still have been collected.
	var webCommits int
	for _, c := range store.commits {
		if c.GitHubOID == "w1" {
			webCommits++
		}
	}
	if webCommits != 1 {
		t.Error("acme/web was not synchronized after acme/api failed")
	}
	if store.lastRun().Status != models.SyncStatusPartial {
		t.Errorf("the run must be recorded as partial, got %q", store.lastRun().Status)
	}
}

func TestSyncStopsWhenTheRateLimitBudgetRunsOut(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	// Allow the organization and repository queries, then refuse.
	api.budgetAfter = 2
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("running out of budget mid-run is not a hard failure: %v", err)
	}
	if result.Status != models.SyncStatusPartial {
		t.Errorf("status = %s, want partial", result.Status)
	}
	joined := strings.Join(result.Errors, " ")
	if !strings.Contains(joined, "rate limit") {
		t.Errorf("the reason must be logged, got %v", result.Errors)
	}
	if result.Repositories != 0 {
		t.Errorf("no repository should have been processed, got %d", result.Repositories)
	}
}

func TestSyncRefusesToStartWithoutBudget(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	api.budgetAfter = 1
	api.calls = 5
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	_, err := c.Sync(context.Background())
	if !errors.Is(err, github.ErrRateLimitLow) {
		t.Fatalf("err = %v, want ErrRateLimitLow", err)
	}
	if len(store.runs) != 0 {
		t.Error("no sync run should be recorded when the run never started")
	}
}

func TestAggregatesAreRebuiltForTheTouchedWindowOnly(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rebuilds) == 0 {
		t.Fatal("daily aggregates were never rebuilt")
	}
	org := store.orgsByGitHubID["O_acme"]
	for _, rb := range store.rebuilds {
		if rb.orgID != org.ID {
			t.Errorf("aggregate rebuild used organization %d, want %d", rb.orgID, org.ID)
		}
		if len(rb.repoIDs) != 1 {
			t.Errorf("rebuild should be scoped to one repository, got %v", rb.repoIDs)
		}
		if rb.from.After(rb.to) {
			t.Errorf("rebuild window is inverted: %s..%s", rb.from, rb.to)
		}
	}

	// The api repository's activity spans 2026-09-01 to 2026-09-06.
	var apiRebuild *rebuildCall
	apiRepo := store.repos["R_api"]
	for i := range store.rebuilds {
		if store.rebuilds[i].repoIDs[0] == apiRepo.ID {
			apiRebuild = &store.rebuilds[i]
		}
	}
	if apiRebuild == nil {
		t.Fatal("no rebuild for acme/api")
	}
	if apiRebuild.from.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("rebuild window starts at %s, want 2026-09-01", apiRebuild.from.Format("2006-01-02"))
	}
	if apiRebuild.to.Format("2006-01-02") != "2026-09-06" {
		t.Errorf("rebuild window ends at %s, want 2026-09-06", apiRebuild.to.Format("2006-01-02"))
	}
}

func TestConcurrentSyncsAreRejected(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	c.mu.Lock()
	c.running = true
	c.mu.Unlock()

	if _, err := c.Sync(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
}

func TestUnknownOrganizationFailsClearly(t *testing.T) {
	api := newFakeAPI(50, acmeFixture())
	store := newFakeStore()
	c := newTestCollector(t, testConfig("does-not-exist"), api, store)

	_, err := c.Sync(context.Background())
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error should name the configured organization, got %v", err)
	}
}

func TestSplitFullName(t *testing.T) {
	owner, name := splitFullName("acme/api", "fallback", "repo")
	if owner != "acme" || name != "api" {
		t.Errorf("got %s/%s", owner, name)
	}
	owner, name = splitFullName("", "acme", "api")
	if owner != "acme" || name != "api" {
		t.Errorf("fallback failed: got %s/%s", owner, name)
	}
}

func teamMember(id, login string) github.Actor {
	return github.Actor{TypeName: "User", ID: id, Login: login, Name: login}
}

// withTeams adds two teams to the acme fixture: one small, one large enough to
// force nested pagination of both members and repositories.
func withTeams(f *orgFixture) *orgFixture {
	f.teams = []github.Team{
		{
			ID: "T_platform", Slug: "platform", Name: "Platform", Description: "Core services",
			Members: struct {
				PageInfo github.PageInfo `json:"pageInfo"`
				Nodes    []github.Actor  `json:"nodes"`
			}{Nodes: []github.Actor{
				teamMember("U_alice", "alice"),
				teamMember("U_bob", "bob"),
				teamMember("U_carol", "carol"),
			}},
			Repositories: struct {
				PageInfo github.PageInfo         `json:"pageInfo"`
				Nodes    []github.TeamRepository `json:"nodes"`
			}{Nodes: []github.TeamRepository{
				{ID: "R_api", Name: "api", NameWithOwner: "acme/api"},
				{ID: "R_web", Name: "web", NameWithOwner: "acme/web"},
				{ID: "R_unknown", Name: "elsewhere", NameWithOwner: "other/elsewhere"},
			}},
		},
		{
			ID: "T_design", Slug: "design", Name: "Design",
			Members: struct {
				PageInfo github.PageInfo `json:"pageInfo"`
				Nodes    []github.Actor  `json:"nodes"`
			}{Nodes: []github.Actor{teamMember("U_carol", "carol")}},
			Repositories: struct {
				PageInfo github.PageInfo         `json:"pageInfo"`
				Nodes    []github.TeamRepository `json:"nodes"`
			}{Nodes: []github.TeamRepository{{ID: "R_web", Name: "web", NameWithOwner: "acme/web"}}},
		},
	}
	return f
}

func TestSyncTeamsStoresMembershipAndRepositories(t *testing.T) {
	api := newFakeAPI(50, withTeams(acmeFixture()))
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != models.SyncStatusSuccess {
		t.Fatalf("status = %s, errors = %v", result.Status, result.Errors)
	}

	if len(store.teams) != 2 {
		t.Fatalf("stored %d teams, want 2", len(store.teams))
	}
	platform := store.teams["T_platform"]
	if platform == nil || platform.Slug != "platform" || platform.Name != "Platform" {
		t.Fatalf("platform team = %+v", platform)
	}
	org := store.orgsByGitHubID["O_acme"]
	if platform.OrganizationID != org.ID {
		t.Errorf("team organization_id = %d, want %d", platform.OrganizationID, org.ID)
	}

	if got := len(store.teamMembers[platform.ID]); got != 3 {
		t.Errorf("platform has %d members, want 3", got)
	}
	// A repository the team can reach but that is not part of this
	// organization's import must be skipped rather than stored dangling.
	if got := len(store.teamRepos[platform.ID]); got != 2 {
		t.Errorf("platform has %d repository links, want 2 (the foreign repo is skipped)", got)
	}
}

func TestSyncTeamsFollowsNestedPagination(t *testing.T) {
	// A page size of 1 forces the collector through the member and repository
	// cursors of every team.
	api := newFakeAPI(1, withTeams(acmeFixture()))
	store := newFakeStore()
	cfg := testConfig("acme")
	cfg.IncludeArchived = true
	c := newTestCollector(t, cfg, api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.teams) != 2 {
		t.Fatalf("stored %d teams, want 2", len(store.teams))
	}
	platform := store.teams["T_platform"]
	if got := len(store.teamMembers[platform.ID]); got != 3 {
		t.Errorf("nested member pagination collected %d of 3 members", got)
	}
	if got := len(store.teamRepos[platform.ID]); got != 2 {
		t.Errorf("nested repository pagination collected %d of 2 links", got)
	}
}

func TestTeamMembershipIsReplacedNotMerged(t *testing.T) {
	fixture := withTeams(acmeFixture())
	api := newFakeAPI(50, fixture)
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	platform := store.teams["T_platform"]
	if got := len(store.teamMembers[platform.ID]); got != 3 {
		t.Fatalf("first sync stored %d members", got)
	}

	// Bob and Carol leave the team on GitHub.
	fixture.teams[0].Members.Nodes = []github.Actor{teamMember("U_alice", "alice")}
	c.contributors = map[string]int64{}
	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(store.teamMembers[platform.ID]); got != 1 {
		t.Errorf("after members left the team still has %d members, want 1", got)
	}
}

func TestDeletedTeamsAreRemoved(t *testing.T) {
	fixture := withTeams(acmeFixture())
	api := newFakeAPI(50, fixture)
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.teams) != 2 {
		t.Fatalf("stored %d teams", len(store.teams))
	}

	fixture.teams = fixture.teams[:1]
	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.teams) != 1 {
		t.Errorf("a team deleted on GitHub is still stored: %d teams remain", len(store.teams))
	}
	if _, ok := store.teams["T_design"]; ok {
		t.Error("the design team should have been removed")
	}
}

func TestTeamMembersAreStoredEvenWithoutContributions(t *testing.T) {
	fixture := withTeams(acmeFixture())
	// Dave is on the team but has never committed anything.
	fixture.teams[0].Members.Nodes = append(fixture.teams[0].Members.Nodes, teamMember("U_dave", "dave"))
	api := newFakeAPI(50, fixture)
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.contributors["U_dave"]; !ok {
		t.Error("a team member with no contributions must still be stored, or the filter is incomplete")
	}
}

func TestTeamFailureDoesNotLoseContributionData(t *testing.T) {
	api := newFakeAPI(50, withTeams(acmeFixture()))
	// A token without read:org cannot list teams.
	api.teamsErr = errors.New("Resource not accessible by integration")
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("a team failure must not fail the whole run: %v", err)
	}
	if result.Status != models.SyncStatusPartial {
		t.Errorf("status = %s, want partial", result.Status)
	}
	if !strings.Contains(strings.Join(result.Errors, " "), "teams") {
		t.Errorf("the team error must be reported, got %v", result.Errors)
	}
	// Commits, pull requests and reviews were still collected.
	commits, prs, reviews, _, _ := store.counts()
	if commits == 0 || prs == 0 || reviews == 0 {
		t.Errorf("contribution data was lost: %d commits, %d PRs, %d reviews", commits, prs, reviews)
	}
}

func TestTeamsCanBeDisabled(t *testing.T) {
	api := newFakeAPI(50, withTeams(acmeFixture()))
	store := newFakeStore()
	cfg := testConfig("acme")
	cfg.SyncTeams = false
	c := newTestCollector(t, cfg, api, store)

	result, err := c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != models.SyncStatusSuccess {
		t.Errorf("status = %s, errors = %v", result.Status, result.Errors)
	}
	if len(store.teams) != 0 {
		t.Errorf("SYNC_TEAMS=false still imported %d teams", len(store.teams))
	}
}

func TestTeamSyncStaysInsideTheConfiguredOrganization(t *testing.T) {
	other := withTeams(otherOrgFixture())
	api := newFakeAPI(50, withTeams(acmeFixture()), other)
	store := newFakeStore()
	c := newTestCollector(t, testConfig("acme"), api, store)

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, login := range api.askedOrgs {
		if !strings.EqualFold(login, "acme") {
			t.Errorf("team sync queried organization %q", login)
		}
	}
	org := store.orgsByGitHubID["O_acme"]
	for _, team := range store.teams {
		if team.OrganizationID != org.ID {
			t.Errorf("team %s belongs to organization %d, want %d", team.Slug, team.OrganizationID, org.ID)
		}
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]int64{3, 1, 3, 2, 1})
	want := []int64{3, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("dedupe = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupe = %v, want %v", got, want)
		}
	}
}
