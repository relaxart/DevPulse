package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
)

// Filter scopes every analytics query. OrganizationID is mandatory: there is no
// query in DevPulse that reads contribution data without an organization scope.
type Filter struct {
	OrganizationID  int64
	From            time.Time
	To              time.Time
	IncludeArchived bool
	// ExcludedLogins are lower-cased logins removed from rankings.
	ExcludedLogins []string
}

// args returns the four parameters shared by every analytics statement:
// $1 organization, $2 from, $3 to, $4 include-archived, $5 excluded logins.
func (f Filter) args() []any {
	excluded := f.ExcludedLogins
	if excluded == nil {
		excluded = []string{}
	}
	return append(f.baseArgs(), excluded)
}

// baseArgs returns the first four parameters, for statements that do not filter
// by contributor exclusions. PostgreSQL cannot infer the type of a parameter a
// statement never references, so unused placeholders must not be passed.
func (f Filter) baseArgs() []any {
	return []any{
		f.OrganizationID,
		f.From.UTC().Format(metrics.DateLayout),
		f.To.UTC().Format(metrics.DateLayout),
		f.IncludeArchived,
	}
}

// contributorFilterSQL is the shared WHERE clause applied to aggregate reads.
// Bots and explicitly excluded logins never appear in rankings.
const contributorFilterSQL = `
   WHERE s.organization_id = $1
     AND s.date BETWEEN $2 AND $3
     AND ($4 OR NOT r.is_archived)
     AND NOT c.is_bot
     AND lower(c.login) <> ALL($5)`

// Overview returns the dashboard headline numbers for the period.
func (db *DB) Overview(ctx context.Context, f Filter) (models.OverviewStats, error) {
	const q = `
SELECT COALESCE(COUNT(DISTINCT s.contributor_id), 0),
       COALESCE(SUM(s.commit_count), 0),
       COALESCE(SUM(s.additions), 0),
       COALESCE(SUM(s.deletions), 0),
       COALESCE(SUM(s.prs_opened), 0),
       COALESCE(SUM(s.prs_merged), 0),
       COALESCE(SUM(s.reviews_submitted), 0),
       COALESCE(COUNT(DISTINCT s.repository_id), 0)
  FROM contributor_daily_stats s
  JOIN repositories r ON r.id = s.repository_id
  JOIN contributors c ON c.id = s.contributor_id` + contributorFilterSQL

	var o models.OverviewStats
	err := db.Pool.QueryRow(ctx, q, f.args()...).Scan(
		&o.ActiveContributors, &o.Commits, &o.Additions, &o.Deletions,
		&o.PRsOpened, &o.PRsMerged, &o.Reviews, &o.ActiveRepositories)
	if err != nil {
		return o, fmt.Errorf("overview statistics: %w", redact(err))
	}
	return o, nil
}

// contributorStatsSQL aggregates the daily read model per contributor. The only
// value that cannot be summed from daily rows is "unique PRs reviewed" (the same
// pull request may be reviewed on several days), so it is counted separately.
const contributorStatsSQL = `
WITH stats AS (
  SELECT s.contributor_id,
         COALESCE(SUM(s.commit_count), 0)      AS commits,
         COALESCE(SUM(s.additions), 0)         AS additions,
         COALESCE(SUM(s.deletions), 0)         AS deletions,
         COALESCE(SUM(s.changed_files), 0)     AS changed_files,
         COALESCE(SUM(s.prs_opened), 0)        AS prs_opened,
         COALESCE(SUM(s.prs_merged), 0)        AS prs_merged,
         COALESCE(SUM(s.prs_closed), 0)        AS prs_closed,
         COALESCE(SUM(s.reviews_submitted), 0) AS reviews_submitted,
         COALESCE(SUM(s.approvals), 0)         AS approvals,
         COALESCE(SUM(s.changes_requested), 0) AS changes_requested,
         COALESCE(SUM(s.review_comments), 0)   AS review_comments,
         COUNT(DISTINCT s.date)                AS active_days,
         COUNT(DISTINCT s.repository_id)       AS repositories
    FROM contributor_daily_stats s
    JOIN repositories r ON r.id = s.repository_id
    JOIN contributors c ON c.id = s.contributor_id` + contributorFilterSQL + `
     %s
   GROUP BY s.contributor_id
),
reviewed AS (
  SELECT pr.reviewer_id,
         COUNT(DISTINCT pr.pull_request_id) AS unique_prs_reviewed
    FROM pull_request_reviews pr
    JOIN repositories r ON r.id = pr.repository_id
   WHERE pr.organization_id = $1
     AND (pr.submitted_at AT TIME ZONE 'UTC')::date BETWEEN $2 AND $3
     AND ($4 OR NOT r.is_archived)
     AND pr.reviewer_id IS NOT NULL
   GROUP BY pr.reviewer_id
)
SELECT c.id, c.github_id, c.login, c.name, c.avatar_url, c.url, c.is_bot,
       stats.commits, stats.additions, stats.deletions, stats.changed_files,
       stats.prs_opened, stats.prs_merged, stats.prs_closed,
       stats.reviews_submitted, stats.approvals, stats.changes_requested, stats.review_comments,
       COALESCE(reviewed.unique_prs_reviewed, 0) AS unique_prs_reviewed,
       stats.active_days, stats.repositories
  FROM stats
  JOIN contributors c ON c.id = stats.contributor_id
  LEFT JOIN reviewed ON reviewed.reviewer_id = stats.contributor_id`

func scanContributorStats(rows pgx.Rows) ([]models.ContributorStats, error) {
	var out []models.ContributorStats
	for rows.Next() {
		var s models.ContributorStats
		if err := rows.Scan(
			&s.ID, &s.GitHubID, &s.Login, &s.Name, &s.AvatarURL, &s.URL, &s.IsBot,
			&s.Commits, &s.Additions, &s.Deletions, &s.ChangedFiles,
			&s.PRsOpened, &s.PRsMerged, &s.PRsClosed,
			&s.ReviewsSubmitted, &s.Approvals, &s.ChangesRequested, &s.ReviewComments,
			&s.UniquePRsReviewed, &s.ActiveDays, &s.Repositories); err != nil {
			return nil, fmt.Errorf("scan contributor stats: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ContributorRanking returns contributors ordered by one metric.
//
// sortKey is resolved through the metrics allow-list, so the ORDER BY clause can
// only ever contain a known column alias.
func (db *DB) ContributorRanking(ctx context.Context, f Filter, sortKey string, limit, offset int) ([]models.ContributorStats, error) {
	metric := metrics.ResolveSort(sortKey)
	q := fmt.Sprintf(contributorStatsSQL, "") +
		fmt.Sprintf("\n ORDER BY %s DESC, commits DESC, lower(c.login) ASC\n LIMIT $6 OFFSET $7", metric.SortExpr)

	args := append(f.args(), limit, offset)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("contributor ranking: %w", redact(err))
	}
	defer rows.Close()
	return scanContributorStats(rows)
}

// ContributorTotals returns the period statistics for a single contributor.
// Bot/exclusion filters are intentionally not applied here: an operator opening
// a direct link to an excluded account should still see its data.
func (db *DB) ContributorTotals(ctx context.Context, f Filter, contributorID int64) (models.ContributorStats, error) {
	q := `
WITH stats AS (
  SELECT s.contributor_id,
         COALESCE(SUM(s.commit_count), 0)      AS commits,
         COALESCE(SUM(s.additions), 0)         AS additions,
         COALESCE(SUM(s.deletions), 0)         AS deletions,
         COALESCE(SUM(s.changed_files), 0)     AS changed_files,
         COALESCE(SUM(s.prs_opened), 0)        AS prs_opened,
         COALESCE(SUM(s.prs_merged), 0)        AS prs_merged,
         COALESCE(SUM(s.prs_closed), 0)        AS prs_closed,
         COALESCE(SUM(s.reviews_submitted), 0) AS reviews_submitted,
         COALESCE(SUM(s.approvals), 0)         AS approvals,
         COALESCE(SUM(s.changes_requested), 0) AS changes_requested,
         COALESCE(SUM(s.review_comments), 0)   AS review_comments,
         COUNT(DISTINCT s.date)                AS active_days,
         COUNT(DISTINCT s.repository_id)       AS repositories
    FROM contributor_daily_stats s
    JOIN repositories r ON r.id = s.repository_id
   WHERE s.organization_id = $1
     AND s.date BETWEEN $2 AND $3
     AND ($4 OR NOT r.is_archived)
     AND s.contributor_id = $5
   GROUP BY s.contributor_id
),
reviewed AS (
  SELECT pr.reviewer_id, COUNT(DISTINCT pr.pull_request_id) AS unique_prs_reviewed
    FROM pull_request_reviews pr
    JOIN repositories r ON r.id = pr.repository_id
   WHERE pr.organization_id = $1
     AND (pr.submitted_at AT TIME ZONE 'UTC')::date BETWEEN $2 AND $3
     AND ($4 OR NOT r.is_archived)
     AND pr.reviewer_id = $5
   GROUP BY pr.reviewer_id
)
SELECT c.id, c.github_id, c.login, c.name, c.avatar_url, c.url, c.is_bot,
       COALESCE(stats.commits, 0), COALESCE(stats.additions, 0), COALESCE(stats.deletions, 0),
       COALESCE(stats.changed_files, 0), COALESCE(stats.prs_opened, 0), COALESCE(stats.prs_merged, 0),
       COALESCE(stats.prs_closed, 0), COALESCE(stats.reviews_submitted, 0), COALESCE(stats.approvals, 0),
       COALESCE(stats.changes_requested, 0), COALESCE(stats.review_comments, 0),
       COALESCE(reviewed.unique_prs_reviewed, 0),
       COALESCE(stats.active_days, 0), COALESCE(stats.repositories, 0)
  FROM contributors c
  LEFT JOIN stats ON stats.contributor_id = c.id
  LEFT JOIN reviewed ON reviewed.reviewer_id = c.id
 WHERE c.id = $5`

	args := append(f.baseArgs(), contributorID)
	var s models.ContributorStats
	err := db.Pool.QueryRow(ctx, q, args...).Scan(
		&s.ID, &s.GitHubID, &s.Login, &s.Name, &s.AvatarURL, &s.URL, &s.IsBot,
		&s.Commits, &s.Additions, &s.Deletions, &s.ChangedFiles,
		&s.PRsOpened, &s.PRsMerged, &s.PRsClosed,
		&s.ReviewsSubmitted, &s.Approvals, &s.ChangesRequested, &s.ReviewComments,
		&s.UniquePRsReviewed, &s.ActiveDays, &s.Repositories)
	if err != nil {
		return s, fmt.Errorf("contributor totals: %w", redact(err))
	}
	return s, nil
}

// ContributorByLogin resolves a login to a contributor that has activity in the
// configured organization.
func (db *DB) ContributorByLogin(ctx context.Context, orgID int64, login string) (*models.Contributor, error) {
	const q = `
SELECT DISTINCT c.id, c.github_id, c.login, c.name, c.avatar_url, c.url, c.is_bot
  FROM contributors c
 WHERE lower(c.login) = lower($2)
   AND (EXISTS (SELECT 1 FROM contributor_daily_stats s WHERE s.contributor_id = c.id AND s.organization_id = $1)
     OR EXISTS (SELECT 1 FROM commits cm WHERE cm.contributor_id = c.id AND cm.organization_id = $1)
     OR EXISTS (SELECT 1 FROM pull_requests p WHERE p.author_id = c.id AND p.organization_id = $1)
     OR EXISTS (SELECT 1 FROM pull_request_reviews rv WHERE rv.reviewer_id = c.id AND rv.organization_id = $1))`

	var c models.Contributor
	err := db.Pool.QueryRow(ctx, q, orgID, login).Scan(
		&c.ID, &c.GitHubID, &c.Login, &c.Name, &c.AvatarURL, &c.URL, &c.IsBot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load contributor %s: %w", login, redact(err))
	}
	return &c, nil
}

// TimeSeries returns chart buckets for the whole organization, one contributor
// or one repository, depending on which optional id is supplied.
func (db *DB) TimeSeries(ctx context.Context, f Filter, g metrics.Granularity, contributorID, repositoryID *int64) ([]models.TimePoint, error) {
	q := `
SELECT date_trunc($6, s.date::timestamp)::date AS bucket,
       COALESCE(SUM(s.commit_count), 0),
       COALESCE(SUM(s.additions), 0),
       COALESCE(SUM(s.deletions), 0),
       COALESCE(SUM(s.prs_opened), 0),
       COALESCE(SUM(s.prs_merged), 0),
       COALESCE(SUM(s.reviews_submitted), 0)
  FROM contributor_daily_stats s
  JOIN repositories r ON r.id = s.repository_id
  JOIN contributors c ON c.id = s.contributor_id` + contributorFilterSQL + `
     AND ($7::bigint IS NULL OR s.contributor_id = $7)
     AND ($8::bigint IS NULL OR s.repository_id = $8)
 GROUP BY bucket
 ORDER BY bucket`

	args := append(f.args(), string(g), contributorID, repositoryID)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("time series: %w", redact(err))
	}
	defer rows.Close()

	var out []models.TimePoint
	for rows.Next() {
		var p models.TimePoint
		if err := rows.Scan(&p.Bucket, &p.Commits, &p.Additions, &p.Deletions,
			&p.PRsOpened, &p.PRsMerged, &p.ReviewsSubmitted); err != nil {
			return nil, fmt.Errorf("scan time point: %w", err)
		}
		p.Label = p.Bucket.Format(metrics.DateLayout)
		out = append(out, p)
	}
	return out, rows.Err()
}

// repositoryStatsSQL aggregates per repository. Repositories with no activity in
// the period are kept (LEFT JOIN) so the repository list stays complete.
const repositoryStatsSQL = `
WITH stats AS (
  SELECT s.repository_id,
         COUNT(DISTINCT s.contributor_id)      AS contributors,
         COALESCE(SUM(s.commit_count), 0)      AS commits,
         COALESCE(SUM(s.additions), 0)         AS additions,
         COALESCE(SUM(s.deletions), 0)         AS deletions,
         COALESCE(SUM(s.prs_opened), 0)        AS prs_opened,
         COALESCE(SUM(s.prs_merged), 0)        AS prs_merged,
         COALESCE(SUM(s.reviews_submitted), 0) AS reviews_submitted,
         MAX(s.date)                           AS last_activity
    FROM contributor_daily_stats s
    JOIN repositories r ON r.id = s.repository_id
    JOIN contributors c ON c.id = s.contributor_id` + contributorFilterSQL + `
   GROUP BY s.repository_id
)
SELECT r.id, r.organization_id, r.github_id, r.name, r.full_name, r.url, r.description,
       r.primary_language, r.is_archived, r.is_private, r.pushed_at,
       COALESCE(stats.contributors, 0), COALESCE(stats.commits, 0),
       COALESCE(stats.additions, 0), COALESCE(stats.deletions, 0),
       COALESCE(stats.prs_opened, 0), COALESCE(stats.prs_merged, 0),
       COALESCE(stats.reviews_submitted, 0),
       stats.last_activity
  FROM repositories r
  LEFT JOIN stats ON stats.repository_id = r.id
 WHERE r.organization_id = $1
   AND ($4 OR NOT r.is_archived)`

// RepositoryRanking returns repository rows for the repositories page.
func (db *DB) RepositoryRanking(ctx context.Context, f Filter) ([]models.RepositoryStats, error) {
	q := repositoryStatsSQL + `
 ORDER BY COALESCE(stats.commits, 0) DESC, COALESCE(stats.prs_opened, 0) DESC, lower(r.name) ASC`

	rows, err := db.Pool.Query(ctx, q, f.args()...)
	if err != nil {
		return nil, fmt.Errorf("repository ranking: %w", redact(err))
	}
	defer rows.Close()

	var out []models.RepositoryStats
	for rows.Next() {
		var r models.RepositoryStats
		var lastActivity *time.Time
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.GitHubID, &r.Name, &r.FullName, &r.URL,
			&r.Description, &r.PrimaryLang, &r.IsArchived, &r.IsPrivate, &r.PushedAt,
			&r.Contributors, &r.Commits, &r.Additions, &r.Deletions,
			&r.PRsOpened, &r.PRsMerged, &r.Reviews, &lastActivity); err != nil {
			return nil, fmt.Errorf("scan repository stats: %w", err)
		}
		r.LastActivity = lastActivity
		out = append(out, r)
	}
	return out, rows.Err()
}

// RepositoryTotals returns period statistics for one repository.
func (db *DB) RepositoryTotals(ctx context.Context, f Filter, repositoryID int64) (models.RepositoryStats, error) {
	q := repositoryStatsSQL + ` AND r.id = $6`
	args := append(f.args(), repositoryID)

	var r models.RepositoryStats
	var lastActivity *time.Time
	err := db.Pool.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.OrganizationID, &r.GitHubID, &r.Name, &r.FullName, &r.URL,
		&r.Description, &r.PrimaryLang, &r.IsArchived, &r.IsPrivate, &r.PushedAt,
		&r.Contributors, &r.Commits, &r.Additions, &r.Deletions,
		&r.PRsOpened, &r.PRsMerged, &r.Reviews, &lastActivity)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("repository totals: %w", redact(err))
	}
	r.LastActivity = lastActivity
	return r, nil
}

// ContributorRepositories lists the repositories one contributor was active in.
func (db *DB) ContributorRepositories(ctx context.Context, f Filter, contributorID int64) ([]models.RepositoryStats, error) {
	const q = `
SELECT r.id, r.name, r.full_name, r.url, r.is_archived,
       COALESCE(SUM(s.commit_count), 0),
       COALESCE(SUM(s.additions), 0),
       COALESCE(SUM(s.deletions), 0),
       COALESCE(SUM(s.prs_opened), 0),
       COALESCE(SUM(s.prs_merged), 0),
       COALESCE(SUM(s.reviews_submitted), 0),
       MAX(s.date)
  FROM contributor_daily_stats s
  JOIN repositories r ON r.id = s.repository_id
 WHERE s.organization_id = $1
   AND s.date BETWEEN $2 AND $3
   AND ($4 OR NOT r.is_archived)
   AND s.contributor_id = $5
 GROUP BY r.id
 ORDER BY 6 DESC, lower(r.name)`

	args := append(f.baseArgs(), contributorID)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("contributor repositories: %w", redact(err))
	}
	defer rows.Close()

	var out []models.RepositoryStats
	for rows.Next() {
		var r models.RepositoryStats
		var last *time.Time
		if err := rows.Scan(&r.ID, &r.Name, &r.FullName, &r.URL, &r.IsArchived,
			&r.Commits, &r.Additions, &r.Deletions, &r.PRsOpened, &r.PRsMerged, &r.Reviews, &last); err != nil {
			return nil, fmt.Errorf("scan contributor repository: %w", err)
		}
		r.LastActivity = last
		out = append(out, r)
	}
	return out, rows.Err()
}

// RepositoryContributors lists the contributors active in one repository.
func (db *DB) RepositoryContributors(ctx context.Context, f Filter, repositoryID int64) ([]models.ContributorStats, error) {
	q := fmt.Sprintf(contributorStatsSQL, "AND s.repository_id = $6") +
		"\n ORDER BY commits DESC, reviews_submitted DESC, lower(c.login) ASC\n LIMIT 200"

	args := append(f.args(), repositoryID)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repository contributors: %w", redact(err))
	}
	defer rows.Close()
	return scanContributorStats(rows)
}

// PullRequestRow is a pull request as rendered in detail tables.
type PullRequestRow struct {
	models.PullRequest
	RepositoryName string
	AuthorLogin    string
}

const pullRequestRowSQL = `
SELECT p.id, p.number, p.title, p.url, p.state, p.created_at, p.merged_at, p.closed_at,
       p.additions, p.deletions, p.changed_files, r.name, COALESCE(c.login, '')
  FROM pull_requests p
  JOIN repositories r ON r.id = p.repository_id
  LEFT JOIN contributors c ON c.id = p.author_id
 WHERE p.organization_id = $1
   AND ($4 OR NOT r.is_archived)
   AND (p.created_at AT TIME ZONE 'UTC')::date BETWEEN $2 AND $3`

func scanPullRequestRows(rows pgx.Rows) ([]PullRequestRow, error) {
	var out []PullRequestRow
	for rows.Next() {
		var p PullRequestRow
		if err := rows.Scan(&p.ID, &p.Number, &p.Title, &p.URL, &p.State, &p.CreatedAt,
			&p.MergedAt, &p.ClosedAt, &p.Additions, &p.Deletions, &p.ChangedFiles,
			&p.RepositoryName, &p.AuthorLogin); err != nil {
			return nil, fmt.Errorf("scan pull request row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ContributorPullRequests lists pull requests authored by one contributor.
func (db *DB) ContributorPullRequests(ctx context.Context, f Filter, contributorID int64, limit int) ([]PullRequestRow, error) {
	q := pullRequestRowSQL + ` AND p.author_id = $5 ORDER BY p.created_at DESC LIMIT $6`
	args := append(f.baseArgs(), contributorID, limit)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("contributor pull requests: %w", redact(err))
	}
	defer rows.Close()
	return scanPullRequestRows(rows)
}

// RepositoryPullRequests lists pull requests of one repository.
func (db *DB) RepositoryPullRequests(ctx context.Context, f Filter, repositoryID int64, limit int) ([]PullRequestRow, error) {
	q := pullRequestRowSQL + ` AND p.repository_id = $5 ORDER BY p.created_at DESC LIMIT $6`
	args := append(f.baseArgs(), repositoryID, limit)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repository pull requests: %w", redact(err))
	}
	defer rows.Close()
	return scanPullRequestRows(rows)
}

// ReviewRow is a review as rendered in detail tables.
type ReviewRow struct {
	State             string
	SubmittedAt       time.Time
	CommentCount      int
	RepositoryName    string
	PullRequestNumber int
	PullRequestTitle  string
	PullRequestURL    string
	ReviewerLogin     string
}

const reviewRowSQL = `
SELECT rv.state, rv.submitted_at, rv.comment_count, r.name, p.number, p.title, p.url, COALESCE(c.login, '')
  FROM pull_request_reviews rv
  JOIN repositories r ON r.id = rv.repository_id
  JOIN pull_requests p ON p.id = rv.pull_request_id
  LEFT JOIN contributors c ON c.id = rv.reviewer_id
 WHERE rv.organization_id = $1
   AND ($4 OR NOT r.is_archived)
   AND (rv.submitted_at AT TIME ZONE 'UTC')::date BETWEEN $2 AND $3`

func scanReviewRows(rows pgx.Rows) ([]ReviewRow, error) {
	var out []ReviewRow
	for rows.Next() {
		var r ReviewRow
		if err := rows.Scan(&r.State, &r.SubmittedAt, &r.CommentCount, &r.RepositoryName,
			&r.PullRequestNumber, &r.PullRequestTitle, &r.PullRequestURL, &r.ReviewerLogin); err != nil {
			return nil, fmt.Errorf("scan review row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContributorReviews lists review submissions by one contributor.
func (db *DB) ContributorReviews(ctx context.Context, f Filter, contributorID int64, limit int) ([]ReviewRow, error) {
	q := reviewRowSQL + ` AND rv.reviewer_id = $5 ORDER BY rv.submitted_at DESC LIMIT $6`
	args := append(f.baseArgs(), contributorID, limit)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("contributor reviews: %w", redact(err))
	}
	defer rows.Close()
	return scanReviewRows(rows)
}

// RepositoryReviews lists review submissions in one repository.
func (db *DB) RepositoryReviews(ctx context.Context, f Filter, repositoryID int64, limit int) ([]ReviewRow, error) {
	q := reviewRowSQL + ` AND rv.repository_id = $5 ORDER BY rv.submitted_at DESC LIMIT $6`
	args := append(f.baseArgs(), repositoryID, limit)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repository reviews: %w", redact(err))
	}
	defer rows.Close()
	return scanReviewRows(rows)
}

// RecentActivity feeds the dashboard activity list.
func (db *DB) RecentActivity(ctx context.Context, f Filter, limit int) ([]models.ActivityRow, error) {
	q := `
SELECT s.date, c.id, c.login, c.name, c.avatar_url, r.name,
       s.commit_count, s.prs_opened, s.prs_merged, s.reviews_submitted, s.additions, s.deletions
  FROM contributor_daily_stats s
  JOIN repositories r ON r.id = s.repository_id
  JOIN contributors c ON c.id = s.contributor_id` + contributorFilterSQL + `
 ORDER BY s.date DESC, (s.commit_count + s.prs_opened + s.reviews_submitted) DESC
 LIMIT $6`

	args := append(f.args(), limit)
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent activity: %w", redact(err))
	}
	defer rows.Close()

	var out []models.ActivityRow
	for rows.Next() {
		var a models.ActivityRow
		if err := rows.Scan(&a.Date, &a.Contributor.ID, &a.Contributor.Login, &a.Contributor.Name,
			&a.Contributor.AvatarURL, &a.Repository, &a.Commits, &a.PRsOpened, &a.PRsMerged,
			&a.Reviews, &a.Additions, &a.Deletions); err != nil {
			return nil, fmt.Errorf("scan activity row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Counts holds the entity totals shown on the status page.
type Counts struct {
	Repositories         int64
	ArchivedRepositories int64
	Contributors         int64
	Commits              int64
	PullRequests         int64
	Reviews              int64
	AggregateRows        int64
}

// OrganizationCounts returns row counts scoped to one organization.
func (db *DB) OrganizationCounts(ctx context.Context, orgID int64) (Counts, error) {
	const q = `
SELECT (SELECT COUNT(*) FROM repositories WHERE organization_id = $1),
       (SELECT COUNT(*) FROM repositories WHERE organization_id = $1 AND is_archived),
       (SELECT COUNT(DISTINCT contributor_id) FROM contributor_daily_stats WHERE organization_id = $1),
       (SELECT COUNT(*) FROM commits WHERE organization_id = $1),
       (SELECT COUNT(*) FROM pull_requests WHERE organization_id = $1),
       (SELECT COUNT(*) FROM pull_request_reviews WHERE organization_id = $1),
       (SELECT COUNT(*) FROM contributor_daily_stats WHERE organization_id = $1)`

	var c Counts
	err := db.Pool.QueryRow(ctx, q, orgID).Scan(&c.Repositories, &c.ArchivedRepositories,
		&c.Contributors, &c.Commits, &c.PullRequests, &c.Reviews, &c.AggregateRows)
	if err != nil {
		return c, fmt.Errorf("organization counts: %w", redact(err))
	}
	return c, nil
}
