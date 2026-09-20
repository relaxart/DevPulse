-- Daily read model. The dashboard reads this table instead of scanning raw
-- commits / pull requests / reviews on every request.

CREATE TABLE contributor_daily_stats (
    date              DATE   NOT NULL,
    organization_id   BIGINT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    contributor_id    BIGINT NOT NULL REFERENCES contributors (id) ON DELETE CASCADE,
    repository_id     BIGINT NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,

    -- Code activity.
    commit_count      INTEGER NOT NULL DEFAULT 0,
    additions         INTEGER NOT NULL DEFAULT 0,
    deletions         INTEGER NOT NULL DEFAULT 0,
    changed_files     INTEGER NOT NULL DEFAULT 0,

    -- Pull request activity (attributed to the pull request author).
    prs_opened        INTEGER NOT NULL DEFAULT 0,
    prs_merged        INTEGER NOT NULL DEFAULT 0,
    prs_closed        INTEGER NOT NULL DEFAULT 0,

    -- Review activity (attributed to the reviewer).
    reviews_submitted INTEGER NOT NULL DEFAULT 0,
    approvals         INTEGER NOT NULL DEFAULT 0,
    changes_requested INTEGER NOT NULL DEFAULT 0,
    review_comments   INTEGER NOT NULL DEFAULT 0,

    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (date, organization_id, contributor_id, repository_id)
);

CREATE INDEX cds_org_date_idx ON contributor_daily_stats (organization_id, date);
CREATE INDEX cds_org_contributor_date_idx ON contributor_daily_stats (organization_id, contributor_id, date);
CREATE INDEX cds_org_repo_date_idx ON contributor_daily_stats (organization_id, repository_id, date);
