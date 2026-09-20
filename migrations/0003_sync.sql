-- Synchronization bookkeeping powering /status and incremental collection.

CREATE TABLE sync_runs (
    id                  BIGSERIAL PRIMARY KEY,
    organization_id     BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    sync_type           TEXT        NOT NULL,
    status              TEXT        NOT NULL,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    duration_seconds    DOUBLE PRECISION NOT NULL DEFAULT 0,
    records_processed   INTEGER     NOT NULL DEFAULT 0,
    repositories_synced INTEGER     NOT NULL DEFAULT 0,
    graphql_cost        INTEGER     NOT NULL DEFAULT 0,
    rate_limit_remaining INTEGER    NOT NULL DEFAULT 0,
    rate_limit_limit    INTEGER     NOT NULL DEFAULT 0,
    rate_limit_reset_at TIMESTAMPTZ,
    error_count         INTEGER     NOT NULL DEFAULT 0,
    error_message       TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX sync_runs_org_started_idx ON sync_runs (organization_id, started_at DESC);
CREATE INDEX sync_runs_org_status_idx ON sync_runs (organization_id, status, started_at DESC);
