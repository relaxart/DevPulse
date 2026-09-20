-- Core GitHub entities. Every table carries organization_id so that data from
-- different organizations can never be mixed, and so multi-organization support
-- is a query change rather than a schema change.

CREATE TABLE organizations (
    id             BIGSERIAL PRIMARY KEY,
    github_id      TEXT        NOT NULL UNIQUE,
    login          TEXT        NOT NULL UNIQUE,
    name           TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_synced_at TIMESTAMPTZ
);

CREATE TABLE repositories (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    github_id         TEXT        NOT NULL UNIQUE,
    name              TEXT        NOT NULL,
    full_name         TEXT        NOT NULL,
    url               TEXT        NOT NULL DEFAULT '',
    description       TEXT        NOT NULL DEFAULT '',
    primary_language  TEXT        NOT NULL DEFAULT '',
    default_branch    TEXT        NOT NULL DEFAULT '',
    is_archived       BOOLEAN     NOT NULL DEFAULT FALSE,
    is_private        BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    pushed_at         TIMESTAMPTZ,
    -- Incremental synchronization watermarks.
    commits_synced_at TIMESTAMPTZ,
    prs_synced_at     TIMESTAMPTZ,
    reviews_synced_at TIMESTAMPTZ,
    UNIQUE (organization_id, name)
);

CREATE INDEX repositories_organization_idx ON repositories (organization_id);
CREATE INDEX repositories_org_archived_idx ON repositories (organization_id, is_archived);

CREATE TABLE contributors (
    id         BIGSERIAL PRIMARY KEY,
    github_id  TEXT        NOT NULL UNIQUE,
    login      TEXT        NOT NULL,
    name       TEXT        NOT NULL DEFAULT '',
    avatar_url TEXT        NOT NULL DEFAULT '',
    url        TEXT        NOT NULL DEFAULT '',
    is_bot     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX contributors_login_key ON contributors (lower(login));

CREATE TABLE commits (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    repository_id   BIGINT      NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- NULL when the commit e-mail maps to no GitHub account (deleted user,
    -- unlinked e-mail address). Such commits are stored but never ranked.
    contributor_id  BIGINT      REFERENCES contributors (id) ON DELETE SET NULL,
    github_oid      TEXT        NOT NULL,
    committed_at    TIMESTAMPTZ NOT NULL,
    additions       INTEGER     NOT NULL DEFAULT 0,
    deletions       INTEGER     NOT NULL DEFAULT 0,
    changed_files   INTEGER     NOT NULL DEFAULT 0,
    author_name     TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same commit OID can legitimately appear in two repositories (forks),
    -- so the idempotency key is per repository.
    UNIQUE (repository_id, github_oid)
);

CREATE INDEX commits_org_time_idx ON commits (organization_id, committed_at);
CREATE INDEX commits_repo_time_idx ON commits (repository_id, committed_at);
CREATE INDEX commits_contributor_time_idx ON commits (contributor_id, committed_at);

CREATE TABLE pull_requests (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    repository_id   BIGINT      NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    author_id       BIGINT      REFERENCES contributors (id) ON DELETE SET NULL,
    github_id       TEXT        NOT NULL UNIQUE,
    number          INTEGER     NOT NULL,
    title           TEXT        NOT NULL DEFAULT '',
    url             TEXT        NOT NULL DEFAULT '',
    state           TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    merged_at       TIMESTAMPTZ,
    closed_at       TIMESTAMPTZ,
    additions       INTEGER     NOT NULL DEFAULT 0,
    deletions       INTEGER     NOT NULL DEFAULT 0,
    changed_files   INTEGER     NOT NULL DEFAULT 0,
    UNIQUE (repository_id, number)
);

CREATE INDEX pull_requests_org_created_idx ON pull_requests (organization_id, created_at);
CREATE INDEX pull_requests_org_merged_idx ON pull_requests (organization_id, merged_at);
CREATE INDEX pull_requests_repo_created_idx ON pull_requests (repository_id, created_at);
CREATE INDEX pull_requests_author_created_idx ON pull_requests (author_id, created_at);
CREATE INDEX pull_requests_repo_updated_idx ON pull_requests (repository_id, updated_at DESC);

CREATE TABLE pull_request_reviews (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    repository_id   BIGINT      NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    pull_request_id BIGINT      NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    reviewer_id     BIGINT      REFERENCES contributors (id) ON DELETE SET NULL,
    github_id       TEXT        NOT NULL UNIQUE,
    state           TEXT        NOT NULL,
    submitted_at    TIMESTAMPTZ NOT NULL,
    comment_count   INTEGER     NOT NULL DEFAULT 0
);

CREATE INDEX pr_reviews_org_time_idx ON pull_request_reviews (organization_id, submitted_at);
CREATE INDEX pr_reviews_repo_time_idx ON pull_request_reviews (repository_id, submitted_at);
CREATE INDEX pr_reviews_reviewer_time_idx ON pull_request_reviews (reviewer_id, submitted_at);
CREATE INDEX pr_reviews_pull_request_idx ON pull_request_reviews (pull_request_id);
