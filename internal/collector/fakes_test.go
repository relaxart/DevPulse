package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/github"
	"github.com/relaxart/dev-pulse/internal/models"
)

// ---------------------------------------------------------------------------
// Fake GraphQL API
// ---------------------------------------------------------------------------

// orgFixture is a recorded GitHub organization the fake API can serve.
type orgFixture struct {
	org   github.Organization
	repos []repoFixture
}

type repoFixture struct {
	repo    github.Repository
	commits []github.Commit
	prs     []github.PullRequest
	// reviews keyed by pull request number.
	reviews map[int][]github.Review
}

// fakeAPI serves recorded GraphQL responses and records which organizations and
// repositories were asked for, so tests can assert on organization isolation.
type fakeAPI struct {
	mu sync.Mutex

	orgs map[string]*orgFixture

	// pageSize forces pagination regardless of the size the collector asks for.
	pageSize int

	// repoErrors injects a transport failure for a repository.
	repoErrors map[string]error

	// budgetAfter makes CheckBudget fail once this many calls have been made.
	budgetAfter int

	askedOrgs  []string
	askedRepos []string
	commitCall []commitCall
	calls      int
	rateLimit  github.RateLimit
}

type commitCall struct {
	repo  string
	since time.Time
}

func newFakeAPI(pageSize int, orgs ...*orgFixture) *fakeAPI {
	m := map[string]*orgFixture{}
	for _, o := range orgs {
		m[strings.ToLower(o.org.Login)] = o
	}
	return &fakeAPI{
		orgs:       m,
		pageSize:   pageSize,
		repoErrors: map[string]error{},
		rateLimit:  github.RateLimit{Limit: 5000, Remaining: 5000, Cost: 1},
	}
}

func (f *fakeAPI) count() {
	f.calls++
	f.rateLimit.Remaining -= 1
}

func (f *fakeAPI) FetchOrganization(ctx context.Context, login string) (*github.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	f.askedOrgs = append(f.askedOrgs, login)
	o, ok := f.orgs[strings.ToLower(login)]
	if !ok {
		return nil, fmt.Errorf("%w: organization %q", github.ErrNotFound, login)
	}
	org := o.org
	return &org, nil
}

func (f *fakeAPI) FetchRepositories(ctx context.Context, login string, pageSize int, after string) (*github.Page[github.Repository], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	f.askedOrgs = append(f.askedOrgs, login)
	o, ok := f.orgs[strings.ToLower(login)]
	if !ok {
		return nil, fmt.Errorf("%w: organization %q", github.ErrNotFound, login)
	}
	all := make([]github.Repository, 0, len(o.repos))
	for _, r := range o.repos {
		all = append(all, r.repo)
	}
	nodes, info := paginate(all, f.pageSize, after)
	return &github.Page[github.Repository]{Nodes: nodes, PageInfo: info}, nil
}

func (f *fakeAPI) repoFixture(owner, name string) (*repoFixture, error) {
	full := owner + "/" + name
	if err, ok := f.repoErrors[full]; ok {
		return nil, err
	}
	o, ok := f.orgs[strings.ToLower(owner)]
	if !ok {
		return nil, fmt.Errorf("%w: organization %q", github.ErrNotFound, owner)
	}
	for i := range o.repos {
		if o.repos[i].repo.Name == name {
			return &o.repos[i], nil
		}
	}
	return nil, fmt.Errorf("%w: repository %s", github.ErrNotFound, full)
}

func (f *fakeAPI) FetchCommits(ctx context.Context, owner, name string, since time.Time, until *time.Time, pageSize int, after string) (*github.Page[github.Commit], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	f.askedRepos = append(f.askedRepos, owner+"/"+name)
	f.commitCall = append(f.commitCall, commitCall{repo: owner + "/" + name, since: since})

	fixture, err := f.repoFixture(owner, name)
	if err != nil {
		return nil, err
	}
	var filtered []github.Commit
	for _, c := range fixture.commits {
		if !c.CommittedDate.Before(since) {
			filtered = append(filtered, c)
		}
	}
	nodes, info := paginate(filtered, f.pageSize, after)
	return &github.Page[github.Commit]{Nodes: nodes, PageInfo: info}, nil
}

func (f *fakeAPI) FetchPullRequests(ctx context.Context, owner, name string, pageSize int, after string) (*github.Page[github.PullRequest], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	fixture, err := f.repoFixture(owner, name)
	if err != nil {
		return nil, err
	}
	prs := append([]github.PullRequest(nil), fixture.prs...)
	// GitHub serves this connection ordered by UPDATED_AT descending.
	sort.Slice(prs, func(i, j int) bool { return prs[i].UpdatedAt.After(prs[j].UpdatedAt) })
	nodes, info := paginate(prs, f.pageSize, after)
	return &github.Page[github.PullRequest]{Nodes: nodes, PageInfo: info}, nil
}

func (f *fakeAPI) FetchRepositoryReviews(ctx context.Context, owner, name string, pageSize, reviewPageSize int, after string) (*github.Page[github.PullRequestReviews], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	fixture, err := f.repoFixture(owner, name)
	if err != nil {
		return nil, err
	}
	prs := append([]github.PullRequest(nil), fixture.prs...)
	sort.Slice(prs, func(i, j int) bool { return prs[i].UpdatedAt.After(prs[j].UpdatedAt) })

	nodes := make([]github.PullRequestReviews, 0, len(prs))
	for _, pr := range prs {
		reviews := fixture.reviews[pr.Number]
		node := github.PullRequestReviews{ID: pr.ID, Number: pr.Number, UpdatedAt: pr.UpdatedAt}
		// Serve only the first page of reviews; the rest needs nested paging.
		if len(reviews) > f.pageSize {
			node.Reviews.Nodes = reviews[:f.pageSize]
			node.Reviews.PageInfo = github.PageInfo{HasNextPage: true, EndCursor: cursorAt(f.pageSize)}
		} else {
			node.Reviews.Nodes = reviews
		}
		nodes = append(nodes, node)
	}
	page, info := paginate(nodes, f.pageSize, after)
	return &github.Page[github.PullRequestReviews]{Nodes: page, PageInfo: info}, nil
}

func (f *fakeAPI) FetchPullRequestReviews(ctx context.Context, owner, name string, number, pageSize int, after string) (*github.ReviewPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	fixture, err := f.repoFixture(owner, name)
	if err != nil {
		return nil, err
	}
	nodes, info := paginate(fixture.reviews[number], f.pageSize, after)
	return &github.ReviewPage{PullRequestNumber: number, Reviews: nodes, PageInfo: info}, nil
}

func (f *fakeAPI) FetchActors(ctx context.Context, ids []string) ([]github.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count()
	return nil, nil
}

func (f *fakeAPI) RateLimit() github.RateLimit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rateLimit
}

func (f *fakeAPI) CheckBudget() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.budgetAfter > 0 && f.calls >= f.budgetAfter {
		return fmt.Errorf("%w: remaining=1 floor=200", github.ErrRateLimitLow)
	}
	return nil
}

func (f *fakeAPI) ResetCost() {}

func (f *fakeAPI) Stats() (github.RateLimit, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rateLimit, f.calls, f.calls
}

// paginate slices a fixture list the way a GraphQL cursor connection would.
func paginate[T any](all []T, size int, after string) ([]T, github.PageInfo) {
	if size <= 0 {
		size = len(all)
	}
	start := 0
	if after != "" {
		fmt.Sscanf(after, "cursor:%d", &start)
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + size
	if end > len(all) {
		end = len(all)
	}
	info := github.PageInfo{}
	if end < len(all) {
		info.HasNextPage = true
		info.EndCursor = cursorAt(end)
	}
	return all[start:end], info
}

func cursorAt(n int) string { return fmt.Sprintf("cursor:%d", n) }

// ---------------------------------------------------------------------------
// Fake store with real upsert semantics
// ---------------------------------------------------------------------------

type rebuildCall struct {
	orgID   int64
	repoIDs []int64
	from    time.Time
	to      time.Time
}

// fakeStore mirrors the uniqueness constraints of the real schema so the tests
// can prove that reruns do not create duplicates.
type fakeStore struct {
	mu sync.Mutex

	nextID int64

	orgsByGitHubID map[string]*models.Organization
	repos          map[string]*models.Repository // by GitHub node id
	reposByID      map[int64]*models.Repository
	contributors   map[string]*models.Contributor
	contributorIDs map[string]int64

	commits map[string]models.Commit            // repoID|oid
	prs     map[string]models.PullRequest       // GitHub node id
	prIDs   map[string]int64                    // repoID|number
	reviews map[string]models.PullRequestReview // GitHub node id

	rebuilds  []rebuildCall
	runs      []*models.SyncRun
	upsertOps int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		orgsByGitHubID: map[string]*models.Organization{},
		repos:          map[string]*models.Repository{},
		reposByID:      map[int64]*models.Repository{},
		contributors:   map[string]*models.Contributor{},
		contributorIDs: map[string]int64{},
		commits:        map[string]models.Commit{},
		prs:            map[string]models.PullRequest{},
		prIDs:          map[string]int64{},
		reviews:        map[string]models.PullRequestReview{},
	}
}

func (s *fakeStore) id() int64 {
	s.nextID++
	return s.nextID
}

func (s *fakeStore) UpsertOrganization(ctx context.Context, githubID, login, name string) (*models.Organization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if org, ok := s.orgsByGitHubID[githubID]; ok {
		org.Login, org.Name = login, name
		cp := *org
		return &cp, nil
	}
	org := &models.Organization{ID: s.id(), GitHubID: githubID, Login: login, Name: name}
	s.orgsByGitHubID[githubID] = org
	cp := *org
	return &cp, nil
}

func (s *fakeStore) MarkOrganizationSynced(ctx context.Context, orgID int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, org := range s.orgsByGitHubID {
		if org.ID == orgID {
			t := at
			org.LastSyncedAt = &t
		}
	}
	return nil
}

func (s *fakeStore) UpsertRepository(ctx context.Context, r *models.Repository) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertOps++
	if existing, ok := s.repos[r.GitHubID]; ok {
		id := existing.ID
		commits, prs, reviews := existing.CommitsSyncedAt, existing.PRsSyncedAt, existing.ReviewsSyncedAt
		*existing = *r
		existing.ID = id
		existing.CommitsSyncedAt, existing.PRsSyncedAt, existing.ReviewsSyncedAt = commits, prs, reviews
		return id, nil
	}
	cp := *r
	cp.ID = s.id()
	s.repos[r.GitHubID] = &cp
	s.reposByID[cp.ID] = &cp
	return cp.ID, nil
}

func (s *fakeStore) ListRepositories(ctx context.Context, orgID int64, includeArchived bool) ([]models.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []models.Repository
	for _, r := range s.repos {
		if r.OrganizationID != orgID {
			continue
		}
		if r.IsArchived && !includeArchived {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *fakeStore) UpsertContributor(ctx context.Context, c *models.Contributor) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertOps++
	if existing, ok := s.contributors[c.GitHubID]; ok {
		existing.Login = c.Login
		if c.Name != "" {
			existing.Name = c.Name
		}
		if c.AvatarURL != "" {
			existing.AvatarURL = c.AvatarURL
		}
		existing.IsBot = existing.IsBot || c.IsBot
		return existing.ID, nil
	}
	cp := *c
	cp.ID = s.id()
	s.contributors[c.GitHubID] = &cp
	s.contributorIDs[strings.ToLower(c.Login)] = cp.ID
	return cp.ID, nil
}

func (s *fakeStore) UpsertCommits(ctx context.Context, commits []models.Commit) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range commits {
		s.upsertOps++
		s.commits[fmt.Sprintf("%d|%s", c.RepositoryID, c.GitHubOID)] = c
	}
	return len(commits), nil
}

func (s *fakeStore) UpsertPullRequest(ctx context.Context, pr *models.PullRequest) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertOps++
	key := fmt.Sprintf("%d|%d", pr.RepositoryID, pr.Number)
	if id, ok := s.prIDs[key]; ok {
		cp := *pr
		cp.ID = id
		s.prs[pr.GitHubID] = cp
		return id, nil
	}
	cp := *pr
	cp.ID = s.id()
	s.prIDs[key] = cp.ID
	s.prs[pr.GitHubID] = cp
	return cp.ID, nil
}

func (s *fakeStore) PullRequestIDsByNumber(ctx context.Context, repoID int64) (map[int]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]int64{}
	for _, pr := range s.prs {
		if pr.RepositoryID == repoID {
			out[pr.Number] = pr.ID
		}
	}
	return out, nil
}

func (s *fakeStore) UpsertReviews(ctx context.Context, reviews []models.PullRequestReview) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range reviews {
		s.upsertOps++
		s.reviews[r.GitHubID] = r
	}
	return len(reviews), nil
}

func (s *fakeStore) ContributorsMissingDetails(ctx context.Context, limit int) ([]string, error) {
	return nil, nil
}

func (s *fakeStore) SetRepositoryWatermark(ctx context.Context, repoID int64, w database.Watermark, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.reposByID[repoID]
	if !ok {
		return fmt.Errorf("unknown repository %d", repoID)
	}
	t := at
	switch w {
	case database.WatermarkCommits:
		repo.CommitsSyncedAt = &t
	case database.WatermarkPRs:
		repo.PRsSyncedAt = &t
	case database.WatermarkReviews:
		repo.ReviewsSyncedAt = &t
	}
	return nil
}

func (s *fakeStore) RebuildDailyStats(ctx context.Context, orgID int64, repoIDs []int64, from, to time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rebuilds = append(s.rebuilds, rebuildCall{orgID: orgID, repoIDs: repoIDs, from: from, to: to})
	return int64(len(repoIDs)), nil
}

func (s *fakeStore) StartSyncRun(ctx context.Context, orgID int64, syncType string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := &models.SyncRun{ID: s.id(), OrganizationID: orgID, SyncType: syncType, Status: models.SyncStatusRunning}
	s.runs = append(s.runs, run)
	return run.ID, nil
}

func (s *fakeStore) FinishSyncRun(ctx context.Context, run *models.SyncRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ID == run.ID {
			r.Status = run.Status
			r.ErrorMessage = run.ErrorMessage
			r.RecordsProcessed = run.RecordsProcessed
			r.Repositories = run.Repositories
		}
	}
	return nil
}

// lastRun returns the most recently recorded synchronization run.
func (s *fakeStore) lastRun() *models.SyncRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) == 0 {
		return nil
	}
	return s.runs[len(s.runs)-1]
}

func (s *fakeStore) counts() (commits, prs, reviews, contributors, repos int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits), len(s.prs), len(s.reviews), len(s.contributors), len(s.repos)
}
