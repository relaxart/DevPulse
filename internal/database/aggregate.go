package database

import (
	"context"
	"fmt"
	"time"
)

// rebuildStatsSQL recomputes contributor_daily_stats from the raw tables for a
// set of repositories and a date window. Days are UTC calendar days.
//
// Commits, pull requests and reviews contribute to different counters of the
// same (date, contributor, repository) row, which is why the source is a UNION
// ALL of five shapes rather than a chain of joins: joining them would multiply
// rows and inflate every counter.
const rebuildStatsSQL = `
WITH src AS (
    SELECT (c.committed_at AT TIME ZONE 'UTC')::date AS day,
           c.contributor_id,
           c.repository_id,
           1 AS commit_count, c.additions, c.deletions, c.changed_files,
           0 AS prs_opened, 0 AS prs_merged, 0 AS prs_closed,
           0 AS reviews_submitted, 0 AS approvals, 0 AS changes_requested, 0 AS review_comments
      FROM commits c
     WHERE c.organization_id = $1
       AND c.repository_id = ANY($2)
       AND c.contributor_id IS NOT NULL
       AND (c.committed_at AT TIME ZONE 'UTC')::date BETWEEN $3 AND $4

    UNION ALL

    SELECT (p.created_at AT TIME ZONE 'UTC')::date, p.author_id, p.repository_id,
           0, 0, 0, 0,
           1, 0, 0,
           0, 0, 0, 0
      FROM pull_requests p
     WHERE p.organization_id = $1
       AND p.repository_id = ANY($2)
       AND p.author_id IS NOT NULL
       AND (p.created_at AT TIME ZONE 'UTC')::date BETWEEN $3 AND $4

    UNION ALL

    SELECT (p.merged_at AT TIME ZONE 'UTC')::date, p.author_id, p.repository_id,
           0, 0, 0, 0,
           0, 1, 0,
           0, 0, 0, 0
      FROM pull_requests p
     WHERE p.organization_id = $1
       AND p.repository_id = ANY($2)
       AND p.author_id IS NOT NULL
       AND p.merged_at IS NOT NULL
       AND (p.merged_at AT TIME ZONE 'UTC')::date BETWEEN $3 AND $4

    UNION ALL

    -- "PRs closed" counts pull requests closed WITHOUT being merged; merged ones
    -- are already counted by prs_merged.
    SELECT (p.closed_at AT TIME ZONE 'UTC')::date, p.author_id, p.repository_id,
           0, 0, 0, 0,
           0, 0, 1,
           0, 0, 0, 0
      FROM pull_requests p
     WHERE p.organization_id = $1
       AND p.repository_id = ANY($2)
       AND p.author_id IS NOT NULL
       AND p.closed_at IS NOT NULL
       AND p.merged_at IS NULL
       AND (p.closed_at AT TIME ZONE 'UTC')::date BETWEEN $3 AND $4

    UNION ALL

    SELECT (r.submitted_at AT TIME ZONE 'UTC')::date, r.reviewer_id, r.repository_id,
           0, 0, 0, 0,
           0, 0, 0,
           1,
           CASE WHEN r.state = 'APPROVED' THEN 1 ELSE 0 END,
           CASE WHEN r.state = 'CHANGES_REQUESTED' THEN 1 ELSE 0 END,
           r.comment_count
      FROM pull_request_reviews r
     WHERE r.organization_id = $1
       AND r.repository_id = ANY($2)
       AND r.reviewer_id IS NOT NULL
       AND (r.submitted_at AT TIME ZONE 'UTC')::date BETWEEN $3 AND $4
)
INSERT INTO contributor_daily_stats (
    date, organization_id, contributor_id, repository_id,
    commit_count, additions, deletions, changed_files,
    prs_opened, prs_merged, prs_closed,
    reviews_submitted, approvals, changes_requested, review_comments, updated_at)
SELECT day, $1, contributor_id, repository_id,
       SUM(commit_count), SUM(additions), SUM(deletions), SUM(changed_files),
       SUM(prs_opened), SUM(prs_merged), SUM(prs_closed),
       SUM(reviews_submitted), SUM(approvals), SUM(changes_requested), SUM(review_comments),
       now()
  FROM src
 GROUP BY day, contributor_id, repository_id
ON CONFLICT (date, organization_id, contributor_id, repository_id) DO UPDATE
   SET commit_count = EXCLUDED.commit_count,
       additions = EXCLUDED.additions,
       deletions = EXCLUDED.deletions,
       changed_files = EXCLUDED.changed_files,
       prs_opened = EXCLUDED.prs_opened,
       prs_merged = EXCLUDED.prs_merged,
       prs_closed = EXCLUDED.prs_closed,
       reviews_submitted = EXCLUDED.reviews_submitted,
       approvals = EXCLUDED.approvals,
       changes_requested = EXCLUDED.changes_requested,
       review_comments = EXCLUDED.review_comments,
       updated_at = now()`

// RebuildDailyStats recomputes the aggregate rows for the given repositories and
// date window. It is idempotent: stale rows inside the window are deleted first,
// so a rerun produces exactly the same table contents.
func (db *DB) RebuildDailyStats(ctx context.Context, orgID int64, repoIDs []int64, from, to time.Time) (int64, error) {
	if len(repoIDs) == 0 {
		return 0, nil
	}
	fromDate := from.UTC().Format("2006-01-02")
	toDate := to.UTC().Format("2006-01-02")

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin aggregate rebuild: %w", redact(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
DELETE FROM contributor_daily_stats
 WHERE organization_id = $1
   AND repository_id = ANY($2)
   AND date BETWEEN $3 AND $4`, orgID, repoIDs, fromDate, toDate); err != nil {
		return 0, fmt.Errorf("clear stale aggregates: %w", redact(err))
	}

	tag, err := tx.Exec(ctx, rebuildStatsSQL, orgID, repoIDs, fromDate, toDate)
	if err != nil {
		return 0, fmt.Errorf("rebuild aggregates: %w", redact(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit aggregate rebuild: %w", redact(err))
	}
	return tag.RowsAffected(), nil
}
