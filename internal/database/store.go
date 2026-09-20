package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/relaxart/dev-pulse/internal/models"
)

// UpsertOrganization stores the configured organization and returns it.
// Re-running it never creates a second row for the same GitHub id.
func (db *DB) UpsertOrganization(ctx context.Context, githubID, login, name string) (*models.Organization, error) {
	const q = `
INSERT INTO organizations (github_id, login, name)
VALUES ($1, $2, $3)
ON CONFLICT (github_id) DO UPDATE
   SET login = EXCLUDED.login,
       name = EXCLUDED.name,
       updated_at = now()
RETURNING id, github_id, login, name, created_at, updated_at, last_synced_at`

	var org models.Organization
	err := db.Pool.QueryRow(ctx, q, githubID, login, name).Scan(
		&org.ID, &org.GitHubID, &org.Login, &org.Name,
		&org.CreatedAt, &org.UpdatedAt, &org.LastSyncedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert organization %s: %w", login, redact(err))
	}
	return &org, nil
}

// OrganizationByLogin loads the organization row, if it has been synced already.
func (db *DB) OrganizationByLogin(ctx context.Context, login string) (*models.Organization, error) {
	const q = `
SELECT id, github_id, login, name, created_at, updated_at, last_synced_at
  FROM organizations
 WHERE lower(login) = lower($1)`

	var org models.Organization
	err := db.Pool.QueryRow(ctx, q, login).Scan(
		&org.ID, &org.GitHubID, &org.Login, &org.Name,
		&org.CreatedAt, &org.UpdatedAt, &org.LastSyncedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load organization %s: %w", login, redact(err))
	}
	return &org, nil
}

// MarkOrganizationSynced records the last successful synchronization instant.
func (db *DB) MarkOrganizationSynced(ctx context.Context, orgID int64, at time.Time) error {
	_, err := db.Pool.Exec(ctx,
		`UPDATE organizations SET last_synced_at = $2, updated_at = now() WHERE id = $1`, orgID, at)
	if err != nil {
		return fmt.Errorf("mark organization synced: %w", redact(err))
	}
	return nil
}

// UpsertRepository stores a repository of the configured organization.
func (db *DB) UpsertRepository(ctx context.Context, r *models.Repository) (int64, error) {
	const q = `
INSERT INTO repositories (
    organization_id, github_id, name, full_name, url, description,
    primary_language, default_branch, is_archived, is_private,
    created_at, updated_at, pushed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (github_id) DO UPDATE
   SET organization_id = EXCLUDED.organization_id,
       name = EXCLUDED.name,
       full_name = EXCLUDED.full_name,
       url = EXCLUDED.url,
       description = EXCLUDED.description,
       primary_language = EXCLUDED.primary_language,
       default_branch = EXCLUDED.default_branch,
       is_archived = EXCLUDED.is_archived,
       is_private = EXCLUDED.is_private,
       updated_at = EXCLUDED.updated_at,
       pushed_at = EXCLUDED.pushed_at
RETURNING id`

	var id int64
	err := db.Pool.QueryRow(ctx, q,
		r.OrganizationID, r.GitHubID, r.Name, r.FullName, r.URL, r.Description,
		r.PrimaryLang, r.DefaultBranch, r.IsArchived, r.IsPrivate,
		r.CreatedAt, r.UpdatedAt, r.PushedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert repository %s: %w", r.FullName, redact(err))
	}
	return id, nil
}

// ListRepositories returns the repositories of one organization.
func (db *DB) ListRepositories(ctx context.Context, orgID int64, includeArchived bool) ([]models.Repository, error) {
	const q = `
SELECT id, organization_id, github_id, name, full_name, url, description,
       primary_language, default_branch, is_archived, is_private,
       created_at, updated_at, pushed_at,
       commits_synced_at, prs_synced_at, reviews_synced_at
  FROM repositories
 WHERE organization_id = $1
   AND ($2 OR NOT is_archived)
 ORDER BY pushed_at DESC NULLS LAST, name`

	rows, err := db.Pool.Query(ctx, q, orgID, includeArchived)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", redact(err))
	}
	defer rows.Close()

	var out []models.Repository
	for rows.Next() {
		var r models.Repository
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.GitHubID, &r.Name, &r.FullName,
			&r.URL, &r.Description, &r.PrimaryLang, &r.DefaultBranch, &r.IsArchived, &r.IsPrivate,
			&r.CreatedAt, &r.UpdatedAt, &r.PushedAt,
			&r.CommitsSyncedAt, &r.PRsSyncedAt, &r.ReviewsSyncedAt); err != nil {
			return nil, fmt.Errorf("scan repository: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RepositoryByName loads one repository of the organization by its short name.
func (db *DB) RepositoryByName(ctx context.Context, orgID int64, name string) (*models.Repository, error) {
	const q = `
SELECT id, organization_id, github_id, name, full_name, url, description,
       primary_language, default_branch, is_archived, is_private,
       created_at, updated_at, pushed_at,
       commits_synced_at, prs_synced_at, reviews_synced_at
  FROM repositories
 WHERE organization_id = $1 AND lower(name) = lower($2)`

	var r models.Repository
	err := db.Pool.QueryRow(ctx, q, orgID, name).Scan(
		&r.ID, &r.OrganizationID, &r.GitHubID, &r.Name, &r.FullName,
		&r.URL, &r.Description, &r.PrimaryLang, &r.DefaultBranch, &r.IsArchived, &r.IsPrivate,
		&r.CreatedAt, &r.UpdatedAt, &r.PushedAt,
		&r.CommitsSyncedAt, &r.PRsSyncedAt, &r.ReviewsSyncedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load repository: %w", redact(err))
	}
	return &r, nil
}

// Watermark names the incremental synchronization column to advance.
type Watermark string

// Supported watermark columns. The values are matched against a fixed allow-list
// before being interpolated, so no caller can inject SQL here.
const (
	WatermarkCommits Watermark = "commits_synced_at"
	WatermarkPRs     Watermark = "prs_synced_at"
	WatermarkReviews Watermark = "reviews_synced_at"
)

// SetRepositoryWatermark advances one incremental synchronization watermark.
func (db *DB) SetRepositoryWatermark(ctx context.Context, repoID int64, w Watermark, at time.Time) error {
	var q string
	switch w {
	case WatermarkCommits:
		q = `UPDATE repositories SET commits_synced_at = $2 WHERE id = $1`
	case WatermarkPRs:
		q = `UPDATE repositories SET prs_synced_at = $2 WHERE id = $1`
	case WatermarkReviews:
		q = `UPDATE repositories SET reviews_synced_at = $2 WHERE id = $1`
	default:
		return fmt.Errorf("unknown watermark %q", w)
	}
	if _, err := db.Pool.Exec(ctx, q, repoID, at); err != nil {
		return fmt.Errorf("set watermark %s: %w", w, redact(err))
	}
	return nil
}

// UpsertContributor stores a GitHub account and returns its internal id.
func (db *DB) UpsertContributor(ctx context.Context, c *models.Contributor) (int64, error) {
	const q = `
INSERT INTO contributors (github_id, login, name, avatar_url, url, is_bot)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (github_id) DO UPDATE
   SET login = EXCLUDED.login,
       name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE contributors.name END,
       avatar_url = CASE WHEN EXCLUDED.avatar_url <> '' THEN EXCLUDED.avatar_url ELSE contributors.avatar_url END,
       url = CASE WHEN EXCLUDED.url <> '' THEN EXCLUDED.url ELSE contributors.url END,
       is_bot = contributors.is_bot OR EXCLUDED.is_bot,
       updated_at = now()
RETURNING id`

	var id int64
	err := db.Pool.QueryRow(ctx, q, c.GitHubID, c.Login, c.Name, c.AvatarURL, c.URL, c.IsBot).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert contributor %s: %w", c.Login, redact(err))
	}
	return id, nil
}

const upsertCommitStmt = `
INSERT INTO commits (
    organization_id, repository_id, contributor_id, github_oid,
    committed_at, additions, deletions, changed_files, author_name)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (repository_id, github_oid) DO UPDATE
   SET contributor_id = COALESCE(EXCLUDED.contributor_id, commits.contributor_id),
       additions = EXCLUDED.additions,
       deletions = EXCLUDED.deletions,
       changed_files = EXCLUDED.changed_files,
       author_name = EXCLUDED.author_name`

// UpsertCommits stores a batch of commits idempotently.
func (db *DB) UpsertCommits(ctx context.Context, commits []models.Commit) (int, error) {
	if len(commits) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, c := range commits {
		batch.Queue(upsertCommitStmt,
			c.OrganizationID, c.RepositoryID, c.ContributorID, c.GitHubOID,
			c.CommittedAt, c.Additions, c.Deletions, c.ChangedFiles, c.AuthorName)
	}
	br := db.Pool.SendBatch(ctx, batch)
	defer br.Close()
	for i := range commits {
		if _, err := br.Exec(); err != nil {
			return i, fmt.Errorf("upsert commit %s: %w", commits[i].GitHubOID, redact(err))
		}
	}
	return len(commits), nil
}

// UpsertPullRequest stores a pull request and returns its internal id.
func (db *DB) UpsertPullRequest(ctx context.Context, pr *models.PullRequest) (int64, error) {
	const q = `
INSERT INTO pull_requests (
    organization_id, repository_id, author_id, github_id, number, title, url,
    state, created_at, updated_at, merged_at, closed_at,
    additions, deletions, changed_files)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (github_id) DO UPDATE
   SET author_id = COALESCE(EXCLUDED.author_id, pull_requests.author_id),
       title = EXCLUDED.title,
       url = EXCLUDED.url,
       state = EXCLUDED.state,
       updated_at = EXCLUDED.updated_at,
       merged_at = EXCLUDED.merged_at,
       closed_at = EXCLUDED.closed_at,
       additions = EXCLUDED.additions,
       deletions = EXCLUDED.deletions,
       changed_files = EXCLUDED.changed_files
RETURNING id`

	var id int64
	err := db.Pool.QueryRow(ctx, q,
		pr.OrganizationID, pr.RepositoryID, pr.AuthorID, pr.GitHubID, pr.Number, pr.Title, pr.URL,
		pr.State, pr.CreatedAt, pr.UpdatedAt, pr.MergedAt, pr.ClosedAt,
		pr.Additions, pr.Deletions, pr.ChangedFiles).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert pull request #%d: %w", pr.Number, redact(err))
	}
	return id, nil
}

// PullRequestIDsByNumber maps pull request numbers of one repository to ids so
// reviews can be attached without one round trip per review.
func (db *DB) PullRequestIDsByNumber(ctx context.Context, repoID int64) (map[int]int64, error) {
	rows, err := db.Pool.Query(ctx, `SELECT number, id FROM pull_requests WHERE repository_id = $1`, repoID)
	if err != nil {
		return nil, fmt.Errorf("load pull request ids: %w", redact(err))
	}
	defer rows.Close()

	out := map[int]int64{}
	for rows.Next() {
		var number int
		var id int64
		if err := rows.Scan(&number, &id); err != nil {
			return nil, fmt.Errorf("scan pull request id: %w", err)
		}
		out[number] = id
	}
	return out, rows.Err()
}

const upsertReviewStmt = `
INSERT INTO pull_request_reviews (
    organization_id, repository_id, pull_request_id, reviewer_id,
    github_id, state, submitted_at, comment_count)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (github_id) DO UPDATE
   SET reviewer_id = COALESCE(EXCLUDED.reviewer_id, pull_request_reviews.reviewer_id),
       state = EXCLUDED.state,
       submitted_at = EXCLUDED.submitted_at,
       comment_count = EXCLUDED.comment_count`

// UpsertReviews stores a batch of reviews idempotently.
func (db *DB) UpsertReviews(ctx context.Context, reviews []models.PullRequestReview) (int, error) {
	if len(reviews) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, r := range reviews {
		batch.Queue(upsertReviewStmt,
			r.OrganizationID, r.RepositoryID, r.PullRequestID, r.ReviewerID,
			r.GitHubID, r.State, r.SubmittedAt, r.CommentCount)
	}
	br := db.Pool.SendBatch(ctx, batch)
	defer br.Close()
	for i := range reviews {
		if _, err := br.Exec(); err != nil {
			return i, fmt.Errorf("upsert review %s: %w", reviews[i].GitHubID, redact(err))
		}
	}
	return len(reviews), nil
}

// ContributorsMissingDetails lists GitHub node ids whose display data is still
// incomplete, so SyncContributors can enrich them in one batched query.
func (db *DB) ContributorsMissingDetails(ctx context.Context, limit int) ([]string, error) {
	const q = `
SELECT github_id
  FROM contributors
 WHERE name = '' OR avatar_url = ''
 ORDER BY updated_at
 LIMIT $1`

	rows, err := db.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list contributors missing details: %w", redact(err))
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// StartSyncRun records the beginning of a synchronization attempt.
func (db *DB) StartSyncRun(ctx context.Context, orgID int64, syncType string) (int64, error) {
	const q = `
INSERT INTO sync_runs (organization_id, sync_type, status, started_at)
VALUES ($1, $2, $3, now())
RETURNING id`
	var id int64
	if err := db.Pool.QueryRow(ctx, q, orgID, syncType, models.SyncStatusRunning).Scan(&id); err != nil {
		return 0, fmt.Errorf("start sync run: %w", redact(err))
	}
	return id, nil
}

// FinishSyncRun records the outcome of a synchronization attempt.
func (db *DB) FinishSyncRun(ctx context.Context, run *models.SyncRun) error {
	const q = `
UPDATE sync_runs
   SET status = $2,
       finished_at = now(),
       duration_seconds = $3,
       records_processed = $4,
       repositories_synced = $5,
       graphql_cost = $6,
       rate_limit_remaining = $7,
       rate_limit_limit = $8,
       rate_limit_reset_at = $9,
       error_count = $10,
       error_message = $11
 WHERE id = $1`

	_, err := db.Pool.Exec(ctx, q, run.ID, run.Status, run.DurationSeconds,
		run.RecordsProcessed, run.Repositories, run.GraphQLCost,
		run.RateLimitRemain, run.RateLimitLimit, run.RateLimitResetAt,
		run.ErrorCount, run.ErrorMessage)
	if err != nil {
		return fmt.Errorf("finish sync run: %w", redact(err))
	}
	return nil
}

const syncRunColumns = `
       id, organization_id, sync_type, status, started_at, finished_at,
       duration_seconds, records_processed, repositories_synced, graphql_cost,
       rate_limit_remaining, rate_limit_limit, rate_limit_reset_at,
       error_count, error_message`

func scanSyncRun(row pgx.Row) (*models.SyncRun, error) {
	var r models.SyncRun
	err := row.Scan(&r.ID, &r.OrganizationID, &r.SyncType, &r.Status, &r.StartedAt, &r.FinishedAt,
		&r.DurationSeconds, &r.RecordsProcessed, &r.Repositories, &r.GraphQLCost,
		&r.RateLimitRemain, &r.RateLimitLimit, &r.RateLimitResetAt,
		&r.ErrorCount, &r.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, redact(err)
	}
	return &r, nil
}

// LastSyncRun returns the most recent run of any status.
func (db *DB) LastSyncRun(ctx context.Context, orgID int64) (*models.SyncRun, error) {
	q := `SELECT` + syncRunColumns + ` FROM sync_runs WHERE organization_id = $1 ORDER BY started_at DESC LIMIT 1`
	return scanSyncRun(db.Pool.QueryRow(ctx, q, orgID))
}

// LastSuccessfulSyncRun returns the most recent fully or partially successful run.
func (db *DB) LastSuccessfulSyncRun(ctx context.Context, orgID int64) (*models.SyncRun, error) {
	q := `SELECT` + syncRunColumns + `
  FROM sync_runs
 WHERE organization_id = $1 AND status IN ('success', 'partial')
 ORDER BY started_at DESC LIMIT 1`
	return scanSyncRun(db.Pool.QueryRow(ctx, q, orgID))
}

// RecentSyncRuns returns the run history for the status page.
func (db *DB) RecentSyncRuns(ctx context.Context, orgID int64, limit int) ([]models.SyncRun, error) {
	q := `SELECT` + syncRunColumns + ` FROM sync_runs WHERE organization_id = $1 ORDER BY started_at DESC LIMIT $2`
	rows, err := db.Pool.Query(ctx, q, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync runs: %w", redact(err))
	}
	defer rows.Close()

	var out []models.SyncRun
	for rows.Next() {
		var r models.SyncRun
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.SyncType, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.DurationSeconds, &r.RecordsProcessed, &r.Repositories, &r.GraphQLCost,
			&r.RateLimitRemain, &r.RateLimitLimit, &r.RateLimitResetAt,
			&r.ErrorCount, &r.ErrorMessage); err != nil {
			return nil, fmt.Errorf("scan sync run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkStaleRunsFailed closes runs left in `running` by a crash or restart.
func (db *DB) MarkStaleRunsFailed(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `
UPDATE sync_runs
   SET status = $1,
       finished_at = now(),
       error_message = 'interrupted: the application restarted while this run was in progress'
 WHERE status = $2`, models.SyncStatusFailed, models.SyncStatusRunning)
	if err != nil {
		return fmt.Errorf("mark stale sync runs failed: %w", redact(err))
	}
	return nil
}
