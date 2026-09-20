# DevPulse Architecture

DevPulse collects engineering activity for **one explicitly configured GitHub
organization** and renders contributor analytics from PostgreSQL.

## Data flow

```
GITHUB_TOKEN (env, never persisted)
        |
        v
GitHub GraphQL API  <-- rate-limit aware client (cost/remaining/resetAt logged)
        |
        v
collector (SyncOrganization -> SyncRepositories -> SyncCommits
           -> SyncPullRequests -> SyncReviews -> SyncContributors
           -> RebuildAggregates)
        |
        v
PostgreSQL  (raw entities + contributor_daily_stats aggregate)
        |
        v
internal/http handlers -> internal/metrics queries (PostgreSQL only)
        |
        v
html/template + Bootstrap 5 (Material styling) + Chart.js
```

## Package responsibilities

| Package | Responsibility |
|---|---|
| `internal/config` | Env parsing + startup validation. Token is never printed. |
| `internal/github` | GraphQL transport, typed queries, pagination, rate-limit accounting. No DB. |
| `internal/collector` | Orchestration of sync steps, incremental windows, error isolation. |
| `internal/database` | pgx pool, embedded migrations, idempotent upserts, analytics queries. |
| `internal/metrics` | Date-range resolution, metric definitions, ranking/sorting rules. |
| `internal/http` | Router, handlers, template rendering. Reads DB only. |
| `internal/models` | Shared structs crossing package boundaries. |

## Key decisions

1. **Single configured org.** Every collector entry point takes the org login
   from config; there is no discovery of `viewer.organizations`. Every table
   carries `organization_id` and every dashboard query filters on it.
2. **Aggregate table.** `contributor_daily_stats` is the read model
   (`date x organization x contributor x repository`). The UI never scans raw
   commits except for `COUNT(DISTINCT pull_request_id)` (unique PRs reviewed),
   which cannot be summed from daily rows.
3. **Idempotency.** Every write is `INSERT ... ON CONFLICT (github key) DO UPDATE`,
   so reruns never duplicate rows.
4. **Incrementality.** Repositories store `commits_synced_at`, `prs_synced_at`,
   `reviews_synced_at`; subsequent runs use those as GraphQL `since` filters /
   early-stop bounds on `UPDATED_AT DESC` connections.
5. **No SPA.** Server-rendered `html/template` (contextual auto-escaping covers
   the "escape all GitHub-provided data" requirement) + Bootstrap 5 + Chart.js.
   Light/dark theming rides on Bootstrap 5.3 colour modes: the custom `--md-*`
   tokens are redefined under `:root[data-bs-theme="dark"]`, a tiny inline script
   in `<head>` resolves the stored choice before the first paint, and Chart.js
   charts are rebuilt from a small registry with a per-theme palette when the
   mode changes.
6. **Failure isolation.** A repository-level error is logged and recorded; the
   run continues and is marked `partial`. Sync runs in a goroutine and can never
   crash the HTTP server.
