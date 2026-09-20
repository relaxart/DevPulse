// Package collector synchronizes one explicitly configured GitHub organization
// into PostgreSQL. It never discovers or queries any other organization.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/github"
	"github.com/relaxart/dev-pulse/internal/models"
)

// GitHubAPI is the slice of the GraphQL client the collector needs. It exists so
// collector logic can be tested against recorded GraphQL responses.
type GitHubAPI interface {
	FetchOrganization(ctx context.Context, login string) (*github.Organization, error)
	FetchRepositories(ctx context.Context, login string, pageSize int, after string) (*github.Page[github.Repository], error)
	FetchCommits(ctx context.Context, owner, name string, since time.Time, until *time.Time, pageSize int, after string) (*github.Page[github.Commit], error)
	FetchPullRequests(ctx context.Context, owner, name string, pageSize int, after string) (*github.Page[github.PullRequest], error)
	FetchRepositoryReviews(ctx context.Context, owner, name string, pageSize, reviewPageSize int, after string) (*github.Page[github.PullRequestReviews], error)
	FetchPullRequestReviews(ctx context.Context, owner, name string, number, pageSize int, after string) (*github.ReviewPage, error)
	FetchActors(ctx context.Context, ids []string) ([]github.Actor, error)
	RateLimit() github.RateLimit
	CheckBudget() error
	ResetCost()
	Stats() (github.RateLimit, int, int)
}

// Store is the persistence surface used by the collector.
type Store interface {
	UpsertOrganization(ctx context.Context, githubID, login, name string) (*models.Organization, error)
	MarkOrganizationSynced(ctx context.Context, orgID int64, at time.Time) error
	UpsertRepository(ctx context.Context, r *models.Repository) (int64, error)
	ListRepositories(ctx context.Context, orgID int64, includeArchived bool) ([]models.Repository, error)
	UpsertContributor(ctx context.Context, c *models.Contributor) (int64, error)
	UpsertCommits(ctx context.Context, commits []models.Commit) (int, error)
	UpsertPullRequest(ctx context.Context, pr *models.PullRequest) (int64, error)
	PullRequestIDsByNumber(ctx context.Context, repoID int64) (map[int]int64, error)
	UpsertReviews(ctx context.Context, reviews []models.PullRequestReview) (int, error)
	ContributorsMissingDetails(ctx context.Context, limit int) ([]string, error)
	SetRepositoryWatermark(ctx context.Context, repoID int64, w database.Watermark, at time.Time) error
	RebuildDailyStats(ctx context.Context, orgID int64, repoIDs []int64, from, to time.Time) (int64, error)
	StartSyncRun(ctx context.Context, orgID int64, syncType string) (int64, error)
	FinishSyncRun(ctx context.Context, run *models.SyncRun) error
}

// Page sizes tuned to stay well inside GitHub's GraphQL node limits.
const (
	repoPageSize       = 50
	commitPageSize     = 100
	pullRequestPageSiz = 50
	reviewPRPageSize   = 25
	reviewPageSize     = 50
	actorBatchSize     = 90
	// overlap re-reads a small window before the watermark so records updated in
	// the seconds around the previous run are not missed.
	watermarkOverlap = 15 * time.Minute
)

// Collector runs the synchronization pipeline.
type Collector struct {
	cfg   *config.Config
	api   GitHubAPI
	store Store
	log   *slog.Logger
	now   func() time.Time

	mu           sync.Mutex
	running      bool
	lastError    string
	contributors map[string]int64
}

// New builds a Collector for the configured organization.
func New(cfg *config.Config, api GitHubAPI, store Store, log *slog.Logger) *Collector {
	return &Collector{
		cfg:          cfg,
		api:          api,
		store:        store,
		log:          log.With("organization", cfg.GitHubOrg),
		now:          time.Now,
		contributors: make(map[string]int64),
	}
}

// Result summarizes one synchronization run.
type Result struct {
	Status       string
	Repositories int
	Records      int
	Errors       []string
	Duration     time.Duration
	GraphQLCost  int
}

// Running reports whether a synchronization is currently in progress.
func (c *Collector) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// ErrAlreadyRunning is returned when a second run is requested concurrently.
var ErrAlreadyRunning = errors.New("a synchronization is already running")

// Sync runs the full pipeline once: organization, repositories, commits, pull
// requests, reviews, contributor enrichment and aggregate rebuild.
//
// A failure on one repository is recorded and the run continues with the next
// repository; the run is then reported as "partial".
func (c *Collector) Sync(ctx context.Context) (*Result, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	start := c.now().UTC()
	c.api.ResetCost()

	if err := c.api.CheckBudget(); err != nil {
		c.log.Warn("skipping synchronization: github rate limit budget too low", "error", err)
		return nil, err
	}

	// Step 1: the configured organization, and only that one.
	org, err := c.SyncOrganization(ctx)
	if err != nil {
		return nil, err
	}

	syncType := "incremental"
	if org.LastSyncedAt == nil {
		syncType = "initial"
	}

	runID, err := c.store.StartSyncRun(ctx, org.ID, syncType)
	if err != nil {
		return nil, err
	}

	result := &Result{Status: models.SyncStatusSuccess}
	res := c.run(ctx, org, syncType, start, result)

	rl, cost, calls := c.api.Stats()
	result.GraphQLCost = cost
	result.Duration = c.now().UTC().Sub(start)

	run := &models.SyncRun{
		ID:               runID,
		Status:           result.Status,
		DurationSeconds:  result.Duration.Seconds(),
		RecordsProcessed: result.Records,
		Repositories:     result.Repositories,
		GraphQLCost:      cost,
		RateLimitRemain:  rl.Remaining,
		RateLimitLimit:   rl.Limit,
		ErrorCount:       len(result.Errors),
		ErrorMessage:     strings.Join(result.Errors, "\n"),
	}
	if !rl.ResetAt.IsZero() {
		reset := rl.ResetAt
		run.RateLimitResetAt = &reset
	}
	if err := c.store.FinishSyncRun(ctx, run); err != nil {
		c.log.Error("could not record sync run outcome", "error", err)
	}

	if result.Status != models.SyncStatusFailed {
		if err := c.store.MarkOrganizationSynced(ctx, org.ID, start); err != nil {
			c.log.Error("could not record organization sync time", "error", err)
		}
	}

	c.mu.Lock()
	c.lastError = strings.Join(result.Errors, "\n")
	c.mu.Unlock()

	c.log.Info("synchronization finished",
		"sync_type", syncType,
		"status", result.Status,
		"repositories", result.Repositories,
		"records_processed", result.Records,
		"duration_seconds", result.Duration.Seconds(),
		"graphql_cost", cost,
		"graphql_calls", calls,
		"rate_limit_remaining", rl.Remaining,
		"rate_limit_limit", rl.Limit,
		"errors", len(result.Errors),
	)
	return result, res
}

// run executes the repository pipeline and never returns a repository-scoped error.
func (c *Collector) run(ctx context.Context, org *models.Organization, syncType string, start time.Time, result *Result) error {
	repos, err := c.SyncRepositories(ctx, org)
	if err != nil {
		result.Status = models.SyncStatusFailed
		result.Errors = append(result.Errors, err.Error())
		return err
	}
	result.Records += len(repos)

	for _, repo := range repos {
		select {
		case <-ctx.Done():
			result.Status = models.SyncStatusPartial
			result.Errors = append(result.Errors, "synchronization cancelled")
			return ctx.Err()
		default:
		}

		if err := c.api.CheckBudget(); err != nil {
			// Stop cleanly rather than hammering a depleted budget.
			result.Status = models.SyncStatusPartial
			result.Errors = append(result.Errors, err.Error())
			c.log.Warn("stopping synchronization early: rate limit budget exhausted",
				"error", err, "repositories_done", result.Repositories)
			return nil
		}

		// Archived repositories are always stored, but their activity is only
		// collected when INCLUDE_ARCHIVED is on, since it is excluded from
		// analytics otherwise.
		if repo.IsArchived && !c.cfg.IncludeArchived {
			c.log.Debug("skipping activity sync for archived repository", "repository", repo.FullName)
			continue
		}

		records, err := c.syncRepository(ctx, org, repo, start)
		result.Records += records
		result.Repositories++
		if err != nil {
			result.Status = models.SyncStatusPartial
			msg := fmt.Sprintf("%s: %v", repo.FullName, err)
			result.Errors = append(result.Errors, msg)
			c.log.Error("repository synchronization failed, continuing with the next repository",
				"repository", repo.FullName, "sync_type", syncType, "error", err)
		}
	}

	if err := c.SyncContributors(ctx); err != nil {
		result.Status = models.SyncStatusPartial
		result.Errors = append(result.Errors, fmt.Sprintf("contributor enrichment: %v", err))
		c.log.Error("contributor enrichment failed", "error", err)
	}
	return nil
}

// syncRepository collects commits, pull requests and reviews for one repository
// and rebuilds the affected daily aggregates.
func (c *Collector) syncRepository(ctx context.Context, org *models.Organization, repo models.Repository, runStart time.Time) (int, error) {
	started := c.now()
	window := newWindow()
	var records int
	var errs []string

	owner, name := splitFullName(repo.FullName, org.Login, repo.Name)

	n, err := c.SyncCommits(ctx, org, repo, owner, name, runStart, window)
	records += n
	if err != nil {
		errs = append(errs, fmt.Sprintf("commits: %v", err))
	}

	n, err = c.SyncPullRequests(ctx, org, repo, owner, name, runStart, window)
	records += n
	if err != nil {
		errs = append(errs, fmt.Sprintf("pull requests: %v", err))
	}

	n, err = c.SyncReviews(ctx, org, repo, owner, name, runStart, window)
	records += n
	if err != nil {
		errs = append(errs, fmt.Sprintf("reviews: %v", err))
	}

	if window.touched {
		if _, err := c.store.RebuildDailyStats(ctx, org.ID, []int64{repo.ID}, window.from, window.to); err != nil {
			errs = append(errs, fmt.Sprintf("aggregates: %v", err))
		}
	}

	c.log.Info("repository synchronized",
		"repository", repo.FullName,
		"records_processed", records,
		"duration_seconds", c.now().Sub(started).Seconds(),
		"rate_limit_remaining", c.api.RateLimit().Remaining,
		"errors", len(errs),
	)

	if len(errs) > 0 {
		return records, errors.New(strings.Join(errs, "; "))
	}
	return records, nil
}

// SyncOrganization loads and stores the single configured organization.
func (c *Collector) SyncOrganization(ctx context.Context) (*models.Organization, error) {
	ghOrg, err := c.api.FetchOrganization(ctx, c.cfg.GitHubOrg)
	if err != nil {
		return nil, fmt.Errorf("load organization %q: %w", c.cfg.GitHubOrg, err)
	}
	org, err := c.store.UpsertOrganization(ctx, ghOrg.ID, ghOrg.Login, ghOrg.Name)
	if err != nil {
		return nil, err
	}
	c.log.Debug("organization synchronized", "login", org.Login, "organization_id", org.ID)
	return org, nil
}

// SyncRepositories stores every repository owned by the configured organization.
func (c *Collector) SyncRepositories(ctx context.Context, org *models.Organization) ([]models.Repository, error) {
	var cursor string
	seen := 0
	for {
		page, err := c.api.FetchRepositories(ctx, c.cfg.GitHubOrg, repoPageSize, cursor)
		if err != nil {
			return nil, fmt.Errorf("list repositories: %w", err)
		}
		for _, r := range page.Nodes {
			// Defensive: the query is already scoped to the configured org, but
			// never persist a repository owned by anybody else.
			if r.Owner.Login != "" && !strings.EqualFold(r.Owner.Login, c.cfg.GitHubOrg) {
				c.log.Warn("ignoring repository outside the configured organization",
					"repository", r.NameWithOwner, "owner", r.Owner.Login)
				continue
			}
			repo := &models.Repository{
				OrganizationID: org.ID,
				GitHubID:       r.ID,
				Name:           r.Name,
				FullName:       r.NameWithOwner,
				URL:            r.URL,
				Description:    r.Description,
				IsArchived:     r.IsArchived,
				IsPrivate:      r.IsPrivate,
				CreatedAt:      r.CreatedAt,
				UpdatedAt:      r.UpdatedAt,
				PushedAt:       r.PushedAt,
			}
			if r.PrimaryLanguage != nil {
				repo.PrimaryLang = r.PrimaryLanguage.Name
			}
			if r.DefaultBranchRef != nil {
				repo.DefaultBranch = r.DefaultBranchRef.Name
			}
			if _, err := c.store.UpsertRepository(ctx, repo); err != nil {
				return nil, err
			}
			seen++
			if c.cfg.MaxRepositories > 0 && seen >= c.cfg.MaxRepositories {
				c.log.Warn("reached MAX_REPOSITORIES limit", "limit", c.cfg.MaxRepositories)
				page.PageInfo.HasNextPage = false
				break
			}
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			break
		}
		cursor = page.PageInfo.EndCursor
	}

	// Reload from the database so every repository carries its internal id and
	// incremental watermarks. Archived repositories are always included here so
	// the repository list stays complete.
	repos, err := c.store.ListRepositories(ctx, org.ID, true)
	if err != nil {
		return nil, err
	}
	c.log.Info("repositories synchronized", "records_processed", seen, "stored", len(repos))
	return repos, nil
}

// SyncCommits pages the default-branch history of one repository.
func (c *Collector) SyncCommits(ctx context.Context, org *models.Organization, repo models.Repository, owner, name string, runStart time.Time, w *window) (int, error) {
	since := c.sinceFor(repo.CommitsSyncedAt, runStart)

	var cursor string
	total := 0
	for {
		page, err := c.api.FetchCommits(ctx, owner, name, since, nil, commitPageSize, cursor)
		if err != nil {
			return total, err
		}
		batch := make([]models.Commit, 0, len(page.Nodes))
		for _, node := range page.Nodes {
			commit := models.Commit{
				OrganizationID: org.ID,
				RepositoryID:   repo.ID,
				GitHubOID:      node.OID,
				CommittedAt:    node.CommittedDate,
				Additions:      node.Additions,
				Deletions:      node.Deletions,
				ChangedFiles:   node.ChangedFiles(),
			}
			if node.Author != nil {
				commit.AuthorName = node.Author.Name
				// A commit whose e-mail is not linked to a GitHub account has no
				// user; it is still stored, just not attributed to anybody.
				id, err := c.contributorID(ctx, node.Author.User)
				if err != nil {
					return total, err
				}
				commit.ContributorID = id
			}
			batch = append(batch, commit)
			w.add(node.CommittedDate)
		}
		n, err := c.store.UpsertCommits(ctx, batch)
		total += n
		if err != nil {
			return total, err
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			break
		}
		cursor = page.PageInfo.EndCursor
	}

	if err := c.store.SetRepositoryWatermark(ctx, repo.ID, database.WatermarkCommits, runStart); err != nil {
		return total, err
	}
	return total, nil
}

// SyncPullRequests pages pull requests newest-updated first and stops as soon as
// it reaches updates older than the previous run.
func (c *Collector) SyncPullRequests(ctx context.Context, org *models.Organization, repo models.Repository, owner, name string, runStart time.Time, w *window) (int, error) {
	stopBefore := c.sinceFor(repo.PRsSyncedAt, runStart)
	historyStart := c.cfg.HistoryStart(runStart)

	var cursor string
	total := 0
	for {
		page, err := c.api.FetchPullRequests(ctx, owner, name, pullRequestPageSiz, cursor)
		if err != nil {
			return total, err
		}
		done := false
		for _, node := range page.Nodes {
			if node.UpdatedAt.Before(stopBefore) {
				// Ordered by UPDATED_AT desc: everything after this is older.
				done = true
				break
			}
			authorID, err := c.contributorID(ctx, node.Author)
			if err != nil {
				return total, err
			}
			pr := &models.PullRequest{
				OrganizationID: org.ID,
				RepositoryID:   repo.ID,
				AuthorID:       authorID,
				GitHubID:       node.ID,
				Number:         node.Number,
				Title:          node.Title,
				URL:            node.URL,
				State:          node.State,
				CreatedAt:      node.CreatedAt,
				UpdatedAt:      node.UpdatedAt,
				MergedAt:       node.MergedAt,
				ClosedAt:       node.ClosedAt,
				Additions:      node.Additions,
				Deletions:      node.Deletions,
				ChangedFiles:   node.ChangedFiles,
			}
			if _, err := c.store.UpsertPullRequest(ctx, pr); err != nil {
				return total, err
			}
			total++
			w.add(node.CreatedAt)
			if node.MergedAt != nil {
				w.add(*node.MergedAt)
			}
			if node.ClosedAt != nil {
				w.add(*node.ClosedAt)
			}
		}
		if done || !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			break
		}
		// On an initial sync there is no watermark, so stop at the history horizon.
		if len(page.Nodes) > 0 {
			last := page.Nodes[len(page.Nodes)-1]
			if last.UpdatedAt.Before(historyStart) && last.CreatedAt.Before(historyStart) {
				break
			}
		}
		cursor = page.PageInfo.EndCursor
	}

	if err := c.store.SetRepositoryWatermark(ctx, repo.ID, database.WatermarkPRs, runStart); err != nil {
		return total, err
	}
	return total, nil
}

// SyncReviews collects review submissions for the pull requests touched since
// the previous run, following nested review pagination where needed.
func (c *Collector) SyncReviews(ctx context.Context, org *models.Organization, repo models.Repository, owner, name string, runStart time.Time, w *window) (int, error) {
	stopBefore := c.sinceFor(repo.ReviewsSyncedAt, runStart)
	historyStart := c.cfg.HistoryStart(runStart)

	prIDs, err := c.store.PullRequestIDsByNumber(ctx, repo.ID)
	if err != nil {
		return 0, err
	}

	var cursor string
	total := 0
	for {
		page, err := c.api.FetchRepositoryReviews(ctx, owner, name, reviewPRPageSize, reviewPageSize, cursor)
		if err != nil {
			return total, err
		}
		done := false
		for _, node := range page.Nodes {
			if node.UpdatedAt.Before(stopBefore) {
				done = true
				break
			}
			prID, ok := prIDs[node.Number]
			if !ok {
				// The pull request sync stopped before this one (or failed);
				// its reviews are picked up on the next run.
				continue
			}
			n, err := c.storeReviews(ctx, org, repo, prID, node.Reviews.Nodes, w)
			total += n
			if err != nil {
				return total, err
			}

			// Nested connection: keep paging this pull request's reviews.
			reviewCursor := node.Reviews.PageInfo.EndCursor
			for node.Reviews.PageInfo.HasNextPage && reviewCursor != "" {
				rp, err := c.api.FetchPullRequestReviews(ctx, owner, name, node.Number, reviewPageSize, reviewCursor)
				if err != nil {
					return total, err
				}
				n, err := c.storeReviews(ctx, org, repo, prID, rp.Reviews, w)
				total += n
				if err != nil {
					return total, err
				}
				if !rp.PageInfo.HasNextPage || rp.PageInfo.EndCursor == "" {
					break
				}
				reviewCursor = rp.PageInfo.EndCursor
			}
		}
		if done || !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			break
		}
		if len(page.Nodes) > 0 && page.Nodes[len(page.Nodes)-1].UpdatedAt.Before(historyStart) {
			break
		}
		cursor = page.PageInfo.EndCursor
	}

	if err := c.store.SetRepositoryWatermark(ctx, repo.ID, database.WatermarkReviews, runStart); err != nil {
		return total, err
	}
	return total, nil
}

func (c *Collector) storeReviews(ctx context.Context, org *models.Organization, repo models.Repository, prID int64, reviews []github.Review, w *window) (int, error) {
	if len(reviews) == 0 {
		return 0, nil
	}
	batch := make([]models.PullRequestReview, 0, len(reviews))
	for _, r := range reviews {
		if r.SubmittedAt == nil {
			// Pending reviews are not submissions and are not counted.
			continue
		}
		reviewerID, err := c.contributorID(ctx, r.Author)
		if err != nil {
			return 0, err
		}
		batch = append(batch, models.PullRequestReview{
			OrganizationID: org.ID,
			RepositoryID:   repo.ID,
			PullRequestID:  prID,
			ReviewerID:     reviewerID,
			GitHubID:       r.ID,
			State:          r.State,
			SubmittedAt:    *r.SubmittedAt,
			CommentCount:   r.Comments.TotalCount,
		})
		w.add(*r.SubmittedAt)
	}
	return c.store.UpsertReviews(ctx, batch)
}

// SyncContributors fills in display names and avatars for accounts first seen
// through a connection that did not carry them.
func (c *Collector) SyncContributors(ctx context.Context) error {
	ids, err := c.store.ContributorsMissingDetails(ctx, 500)
	if err != nil {
		return err
	}
	for start := 0; start < len(ids); start += actorBatchSize {
		end := min(start+actorBatchSize, len(ids))
		actors, err := c.api.FetchActors(ctx, ids[start:end])
		if err != nil {
			return err
		}
		for i := range actors {
			a := actors[i]
			if _, err := c.store.UpsertContributor(ctx, &models.Contributor{
				GitHubID:  a.ID,
				Login:     a.Login,
				Name:      a.Name,
				AvatarURL: a.AvatarURL,
				URL:       a.URL,
				IsBot:     a.IsBot(),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// contributorID resolves a GitHub actor to an internal contributor id, caching
// the mapping for the lifetime of the collector. A nil actor (unlinked commit
// e-mail, deleted account) yields a nil id rather than an error.
func (c *Collector) contributorID(ctx context.Context, actor *github.Actor) (*int64, error) {
	if actor == nil || actor.ID == "" || actor.Login == "" {
		return nil, nil
	}
	c.mu.Lock()
	id, ok := c.contributors[actor.ID]
	c.mu.Unlock()
	if ok {
		return &id, nil
	}
	id, err := c.store.UpsertContributor(ctx, &models.Contributor{
		GitHubID:  actor.ID,
		Login:     actor.Login,
		Name:      actor.Name,
		AvatarURL: actor.AvatarURL,
		URL:       actor.URL,
		IsBot:     actor.IsBot(),
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.contributors[actor.ID] = id
	c.mu.Unlock()
	return &id, nil
}

// sinceFor computes the lower bound of an incremental fetch: the previous
// watermark minus a small overlap, or the configured history horizon on the
// first run.
func (c *Collector) sinceFor(watermark *time.Time, runStart time.Time) time.Time {
	historyStart := c.cfg.HistoryStart(runStart)
	if watermark == nil {
		return historyStart
	}
	since := watermark.Add(-watermarkOverlap)
	if since.Before(historyStart) {
		return historyStart
	}
	return since
}

// window tracks the span of dates a repository sync touched, so only those days
// need their aggregates rebuilt.
type window struct {
	touched bool
	from    time.Time
	to      time.Time
}

func newWindow() *window { return &window{} }

func (w *window) add(t time.Time) {
	if t.IsZero() {
		return
	}
	t = t.UTC()
	if !w.touched {
		w.touched, w.from, w.to = true, t, t
		return
	}
	if t.Before(w.from) {
		w.from = t
	}
	if t.After(w.to) {
		w.to = t
	}
}

// splitFullName splits "owner/name", falling back to the configured org.
func splitFullName(fullName, fallbackOwner, fallbackName string) (owner, name string) {
	if idx := strings.IndexByte(fullName, '/'); idx > 0 && idx < len(fullName)-1 {
		return fullName[:idx], fullName[idx+1:]
	}
	return fallbackOwner, fallbackName
}
