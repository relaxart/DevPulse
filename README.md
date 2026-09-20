# DevPulse

[![CI](https://github.com/relaxart/dev-pulse/actions/workflows/ci.yml/badge.svg)](https://github.com/relaxart/dev-pulse/actions/workflows/ci.yml)

GitHub engineering analytics for **one** organization.

DevPulse collects commits, pull requests and reviews from the GitHub GraphQL
API, stores them in PostgreSQL, and renders contributor and repository
statistics in a server-rendered Bootstrap 5 dashboard.

> **DevPulse does not automatically analyze every GitHub organization your
> Personal Access Token can reach.** Only the organization configured in
> `GITHUB_ORG` is queried and analyzed. If your token has access to `org-a`,
> `org-b`, your personal account and a dozen open-source organizations, DevPulse
> still queries exactly the one you configured — it never enumerates
> `viewer.organizations` and never discovers organizations on its own.

---

## Table of contents

1. [What the project does](#1-what-the-project-does)
2. [Architecture](#2-architecture)
3. [Requirements](#3-requirements)
4. [GitHub PAT configuration](#4-github-pat-configuration)
5. [GitHub PAT permissions](#5-github-pat-permissions)
6. [Organization configuration](#6-organization-configuration)
7. [Docker installation](#7-docker-installation)
8. [Database setup](#8-database-setup)
9. [Initial synchronization](#9-initial-synchronization)
10. [Incremental synchronization](#10-incremental-synchronization)
11. [Date filters](#11-date-filters)
12. [Contributor ranking](#12-contributor-ranking)
13. [PDF reports](#13-pdf-reports)
14. [Bot exclusions](#14-bot-exclusions)
15. [Archived repository handling](#15-archived-repository-handling)
16. [GitHub GraphQL rate-limit handling](#16-github-graphql-rate-limit-handling)
17. [Security considerations](#17-security-considerations)
18. [Troubleshooting](#18-troubleshooting)
19. [Metric definitions](#19-metric-definitions)
20. [Development](#20-development)

---

## 1. What the project does

DevPulse answers questions like *"who reviewed the most pull requests last
quarter?"* or *"which repositories were active in the past 30 days?"* without
clicking through GitHub.

It provides:

- **Overview dashboard** — active contributors, commits, PRs opened, PRs merged,
  reviews, active repositories, plus time-series charts.
- **Contributor ranking** — every metric sortable independently.
- **Contributor detail pages** — summary metrics, charts, repositories, pull
  requests and reviews.
- **Repository list and detail pages** — contributors, commits, PRs, reviews,
  last activity, archived status.
- **Status page** — sync health, GraphQL rate limit, stored row counts, run history.
- **Global date filter** — 7 or 30 days, 3/6/12 months or a custom range, applied
  consistently across every page.
- **PDF reports** — download the whole picture for any period as a printable
  document.
- **Light and dark themes** — pick Light, Dark or System from the header. The
  choice is remembered per browser, System follows the operating system live, and
  the theme is applied before the first paint so navigation never flashes.

### What it deliberately does not do

DevPulse **does not** compute a single "performance score" and **does not** label
anyone productive or unproductive. Commits, pull requests and reviews measure
different kinds of contribution; they are presented and sorted separately so a
human can interpret them in context.

---

## 2. Architecture

```
GITHUB_TOKEN (environment variable, never persisted)
        │
        ▼
GitHub GraphQL API          rate-limit aware client:
        │                   cost / remaining / limit / resetAt logged per call
        ▼
Go collector / sync worker  SyncOrganization → SyncRepositories → SyncCommits
        │                   → SyncPullRequests → SyncReviews → SyncContributors
        │                   → rebuild affected daily aggregates
        ▼
PostgreSQL                  raw entities + contributor_daily_stats read model
        │
        ▼
Go HTTP application         html/template handlers; reads PostgreSQL only
        │
        ▼
Bootstrap 5 + Material UI   server-rendered pages, Chart.js charts
```

### Package layout

```
cmd/server/            entrypoint: config, migrations, worker, HTTP server
internal/config/       environment parsing and startup validation
internal/github/       GraphQL transport, queries, pagination, rate limiting
internal/collector/    sync orchestration, incremental windows, error isolation
internal/database/     pgx pool, embedded migrations, upserts, analytics queries
internal/metrics/      date ranges, metric definitions, sort allow-list
internal/http/         router, handlers, template rendering
internal/models/       shared structs
migrations/            SQL migrations (embedded into the binary)
web/templates/         server-rendered pages
web/static/            CSS and Chart.js wiring
```

`docs/ARCHITECTURE.md` records the design decisions in more detail.

### Database schema

| Table | Purpose |
|---|---|
| `organizations` | The configured organization. `last_synced_at` drives the status page. |
| `repositories` | One row per repository, with `commits_synced_at` / `prs_synced_at` / `reviews_synced_at` watermarks for incremental sync. |
| `contributors` | GitHub accounts, with `is_bot`. |
| `commits` | Default-branch commits; unique on `(repository_id, github_oid)`. |
| `pull_requests` | Unique on `github_id` and on `(repository_id, number)`. |
| `pull_request_reviews` | Review submissions; unique on `github_id`. |
| `contributor_daily_stats` | Daily read model keyed by `(date, organization_id, contributor_id, repository_id)`. |
| `sync_runs` | One row per synchronization attempt. |
| `schema_migrations` | Applied migration versions. |

Every GitHub-derived table carries `organization_id`, and every dashboard query
filters on it. v1 runs one configured organization per deployment, but the
schema already supports several.

Indexes exist for `organization_id`, `repository_id`, `contributor_id`, the
relevant timestamps and `pull_request_id`.

---

## 3. Requirements

- Docker and Docker Compose (the only requirement for running DevPulse).
- A GitHub Personal Access Token with read access to the organization.
- Go 1.25+ only if you want to build or test outside Docker.

---

## 4. GitHub PAT configuration

1. Create a token on GitHub (see permissions below).
2. Copy the example environment file and fill it in:

```bash
cp .env.example .env
$EDITOR .env
```

```dotenv
GITHUB_TOKEN=github_pat_xxxxxxxxxxxxxxxxxxxx
GITHUB_ORG=my-company
DATABASE_URL=postgres://devpulse:devpulse@postgres:5432/devpulse?sslmode=disable
SYNC_INTERVAL=30m
HISTORY_MONTHS=12
INCLUDE_ARCHIVED=false
EXCLUDED_USERS=dependabot[bot],renovate[bot]
```

`.env` is listed in `.gitignore`. **Never commit it.** The token is read from the
environment into memory and is never written to PostgreSQL, logs, HTML or API
responses.

### Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITHUB_TOKEN` | **yes** | — | Read-only Personal Access Token. |
| `GITHUB_ORG` | **yes** | — | The single organization login to analyze. A comma-separated list is rejected. |
| `DATABASE_URL` | **yes** | — | PostgreSQL connection string. |
| `SYNC_INTERVAL` | no | `30m` | Worker period. Minimum `1m`. |
| `HISTORY_MONTHS` | no | `12` | How far back the first sync reaches (1–120). |
| `INCLUDE_ARCHIVED` | no | `false` | Include archived repositories in analytics. |
| `EXCLUDED_USERS` | no | — | Comma-separated logins excluded from rankings. |
| `APP_PORT` | no | `8080` | Host port published by Docker Compose. |
| `LISTEN_ADDR` | no | `:8080` | Address the server binds inside the container. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn` or `error`. |
| `SYNC_ON_STARTUP` | no | `true` | Run one synchronization immediately at startup. |
| `MIN_RATE_LIMIT_REMAINING` | no | `200` | Stop syncing below this GraphQL point budget. |
| `MAX_REPOSITORIES` | no | `0` | Cap the repositories imported (`0` = no limit). |
| `GITHUB_API_URL` | no | `https://api.github.com/graphql` | Override for GitHub Enterprise Server. |

Configuration is validated at startup. Missing or malformed values produce a
clear error naming the variable — and never its value:

```
devpulse: missing required configuration:
  - GITHUB_TOKEN must be set (create a read-only Personal Access Token)
  - GITHUB_ORG must be set to exactly one organization login
```

---

## 5. GitHub PAT permissions

DevPulse only reads. It never requires, requests or performs a write.

### Fine-grained personal access token (recommended)

- **Resource owner:** the organization in `GITHUB_ORG`.
- **Repository access:** *All repositories* (or the subset you want analyzed).
- **Repository permissions** (all **Read-only**):
  - `Contents` — commit history, additions/deletions.
  - `Pull requests` — pull requests and their reviews.
  - `Metadata` — mandatory, granted automatically.
- **Organization permissions** (**Read-only**):
  - `Members` — resolve contributor logins and display names.

### Classic personal access token

- `read:org`
- `repo` — required only to read **private** repositories. For a
  public-only organization, `public_repo` is enough. GitHub's classic tokens
  offer no read-only variant of `repo`; the fine-grained token above is the
  least-privilege option and is what we recommend.
- `read:user`

If your organization enforces SAML SSO, authorize the token for the
organization after creating it, otherwise GitHub answers `401`/`NOT_FOUND`.

---

## 6. Organization configuration

`GITHUB_ORG` is the sole scope of the deployment.

```dotenv
GITHUB_ORG=my-company
```

- Every GraphQL query passes this login explicitly as a variable.
- The collector never calls `viewer.organizations`, `search` or any other
  discovery API.
- Repositories whose owner does not match `GITHUB_ORG` are skipped and logged,
  even if GitHub somehow returned them.
- A comma-separated value is rejected at startup — one organization per deployment.
- To analyze a second organization, run a second DevPulse deployment with its own
  database.

---

## 7. Docker installation

```bash
git clone <this repository>
cd dev-pulse

cp .env.example .env
$EDITOR .env          # set GITHUB_TOKEN and GITHUB_ORG

docker compose up -d
```

Then open <http://localhost:8080> (or `APP_PORT`).

Compose starts two services:

- `postgres` — PostgreSQL 17 with a named volume and a `pg_isready` health check.
- `app` — the DevPulse binary, started only once PostgreSQL reports healthy.

Useful commands:

```bash
docker compose logs -f app       # follow synchronization progress
docker compose ps                # service health
docker compose config            # validate the compose file
docker compose down              # stop (keeps data)
docker compose down -v           # stop and delete the database volume
```

The image is a multi-stage build: the Go toolchain stays in the build stage and
the runtime image is Alpine plus the static binary (~25 MB), running as a
non-root user.

---

## 8. Database setup

Nothing to do by hand. On startup the application:

1. Waits up to 60 s for PostgreSQL to accept connections.
2. Takes a PostgreSQL advisory lock, so several replicas can start at once.
3. Applies any migration in `migrations/*.sql` not yet in `schema_migrations`,
   each inside its own transaction.
4. Starts the HTTP server **only after** migrations succeed, so requests are
   never served against an incompatible schema.

Migrations are embedded in the binary; the `.sql` files do not ship separately.

Health endpoints:

| Endpoint | Meaning |
|---|---|
| `GET /health` | Liveness plus database reachability. `503` when PostgreSQL is down. |
| `GET /ready` | Readiness: database reachable **and** schema applied. Reports `schema_version`. |

```bash
curl -s localhost:8080/health
{"status":"ok","database":"ok","organization":"my-company","version":"1.0.0","uptime_seconds":142,"last_sync_at":"2026-09-20T10:30:00Z","sync_status":"success"}
```

Both Compose services declare health checks.

---

## 9. Initial synchronization

The first run starts automatically at startup (unless `SYNC_ON_STARTUP=false`)
and:

1. Loads the organization named by `GITHUB_ORG`.
2. Pages through its repositories (archived ones are stored too).
3. Loads `HISTORY_MONTHS` (default 12) of default-branch commits, pull requests
   and reviews per repository.
4. Persists everything in PostgreSQL with upserts.
5. Builds the `contributor_daily_stats` aggregates for the days it touched.

For a large organization the first run can take a long time and consume a lot of
GraphQL budget. Watch progress on `/status` or with `docker compose logs -f app`.
`MAX_REPOSITORIES=5` is handy for a first trial run.

You can also trigger a run from the **Sync now** button on `/status`.

---

## 10. Incremental synchronization

Later runs do **not** re-download the history.

Each repository stores three watermarks — `commits_synced_at`, `prs_synced_at`,
`reviews_synced_at`. On the next run:

- **Commits** are fetched with `history(since: <watermark − 15 min>)`.
- **Pull requests** are read `orderBy: {field: UPDATED_AT, direction: DESC}` and
  paging stops at the first pull request older than the watermark.
- **Reviews** use the same early-stop rule, following nested review pagination
  where a pull request has more reviews than fit on one page.

The 15-minute overlap re-reads a small window so nothing is missed at the boundary.

**Reruns are safe.** Every write is `INSERT … ON CONFLICT (<GitHub key>) DO UPDATE`:

| Table | Idempotency key |
|---|---|
| `organizations` | `github_id` |
| `repositories` | `github_id` |
| `contributors` | `github_id` |
| `commits` | `(repository_id, github_oid)` |
| `pull_requests` | `github_id` |
| `pull_request_reviews` | `github_id` |

Running the same synchronization ten times produces exactly the same rows.
Aggregates are recomputed by deleting and re-inserting the affected
`(repository, date)` window inside one transaction, so they cannot drift or
double-count.

A synchronization failure never affects the web application: the worker runs in
its own goroutine, recovers from panics, and the dashboard keeps serving whatever
is already in PostgreSQL.

---

## 11. Date filters

Every page takes the same filter and passes it along in links:

| Selection | URL |
|---|---|
| Last 7 days | `/contributors?period=7d` |
| Last 30 days | `/contributors?period=30d` |
| Last 3 months | `/contributors?period=3m` |
| Last 6 months | `/contributors?period=6m` |
| Last 12 months | `/contributors?period=12m` |
| Custom range | `/contributors?period=custom&from=2026-01-01&to=2026-03-31` |

Ranges are inclusive and expressed in **UTC calendar days**. An invalid custom
range does not error out: the page renders with the 30-day default and explains
what was wrong.

**Changing the period only queries PostgreSQL.** No GitHub call is made when
rendering statistics or changing filters — this is covered by an automated test
that fails if a handler contacts GitHub.

Chart granularity follows the range length: ≤45 days daily, ≤200 days weekly,
longer monthly.

---

## 12. Contributor ranking

`/contributors` ranks contributors by any single metric:

```
Top Contributors — Period: Last 3 months — Sort by: Reviews submitted

#  Name          Commits   +/-           PRs   Reviews   Active days
1  Erin Schmidt  111       +11.8k −4k    30    86        84
2  Carol Nwosu   112       +11.6k −3.8k  32    84        86
3  Bob Ferreira  110       +11.8k −3.8k  31    83        84
```

Sortable metrics:

- **Code contribution** — commits, additions, deletions, changed files
- **Pull requests** — PRs opened, PRs merged, PRs closed
- **Review activity** — reviews submitted, approvals, changes requested, review
  comments, unique PRs reviewed
- **Activity** — active days, repositories contributed to

Sorting is by `?sort=<key>`. The key is resolved against a fixed allow-list, so a
hand-edited URL can never reach the SQL `ORDER BY`.

There is **no combined score**. Metrics stay separate by design.

Clicking a contributor opens `/contributors/{login}` with summary metrics,
time-series charts, the repositories they contributed to, their pull requests and
their reviews — all for the selected period.

---

## 13. PDF reports

Every page carries a **Report** button in the header. It offers the period the
page is currently showing, plus each preset:

- Last 7 days
- Last 30 days
- Last 3 months
- Last 6 months
- Last 12 months

For a custom range, set it in the date selector and then choose **Current
filter** — the report follows whatever the page is showing.

The download is served by:

```
GET /report.pdf?period=3m
GET /report.pdf?period=custom&from=2026-01-01&to=2026-03-31
GET /report.pdf?period=12m&sort=reviews_submitted
```

`sort` accepts any [contributor ranking](#12-contributor-ranking) metric and
orders the contributor table in the document; it defaults to commits. Opening
the link from `/contributors` keeps whatever sort is on screen.

The document is landscape A4 and contains:

1. A header with the organization, the period (and its UTC bounds), when the
   report was generated, the sort key, the archived-repository policy, the
   excluded logins and the last successful synchronization.
2. The six overview numbers, each compared with the immediately preceding period
   of equal length.
3. A bar chart of commits, pull requests opened and reviews per bucket, at the
   same granularity the dashboard uses for that period.
4. The contributor table with all fourteen metrics, up to 100 rows.
5. The repository table, including archived and private repositories marked as
   such, up to 100 rows.
6. The metric glossary, with the same wording as the UI.

Tables that hit the row cap say so on the page rather than cutting off silently.

Reports are built from the same PostgreSQL aggregates as the dashboard, honour
`INCLUDE_ARCHIVED` and `EXCLUDED_USERS`, and are scoped to `GITHUB_ORG`.
**Generating a report never calls GitHub.**

The file is named `devpulse-<org>-<from>-to-<to>.pdf`.

Before the first successful synchronization the report is still a valid document;
it renders empty and says so, rather than returning an error.

### Limitation

The PDF uses the standard built-in fonts, which cover Latin-1. A display name in
another script (CJK, for example) or an emoji is replaced with `?` in the
document; the ASCII `@login` shown next to it is always intact. Embedding a
Unicode font would fix this at the cost of a larger binary.

---

## 14. Bot exclusions

Two mechanisms, both applied at query time:

1. **Automatic detection.** Accounts GitHub reports as `Bot` or `Mannequin`, and
   logins ending in `[bot]` (plus `github-actions`), are stored with `is_bot = true`
   and never appear in rankings, totals or charts.
2. **Manual exclusion.** `EXCLUDED_USERS=dependabot[bot],renovate[bot]` removes
   those logins (case-insensitive) from rankings and totals.

Because exclusion happens at query time, editing `EXCLUDED_USERS` and restarting
takes effect immediately — no re-synchronization needed. Excluded activity stays
in the database and can be restored by removing the login from the list.

---

## 15. Archived repository handling

Archived repositories are **always stored**, so nothing disappears from history.

With `INCLUDE_ARCHIVED=false` (the default):

- They are excluded from the dashboard overview, contributor rankings, repository
  rankings and charts.
- Their activity is not collected on subsequent runs (no point spending GraphQL
  budget on data that is filtered out).
- They still appear in `/repositories`, dimmed and badged **archived**.
- Their detail page still shows their own numbers, with a note explaining they
  are excluded from organization-wide rankings.

Set `INCLUDE_ARCHIVED=true` to fold them back into every statistic.

---

## 16. GitHub GraphQL rate-limit handling

GitHub's GraphQL API uses a point budget (5,000 points/hour for most accounts).

Every DevPulse query requests the `rateLimit` block, and every call logs:

```json
{"level":"DEBUG","msg":"github graphql call","operation":"commits",
 "duration_ms":210,"cost":1,"remaining":4874,"limit":5000,
 "reset_at":"2026-09-20T11:00:00Z"}
```

Each synchronization run logs its total cost, and `/status` shows the live budget
with a progress bar and reset time.

Protections:

- Before each run and before each repository, the remaining budget is checked
  against `MIN_RATE_LIMIT_REMAINING` (default 200). Below the floor the run stops
  cleanly, logs the reason, and is recorded as `partial`. It does **not** retry in
  a tight loop — it waits for the next scheduled tick.
- Transient `5xx`, `429` and secondary-rate-limit responses are retried with
  exponential backoff, at most four attempts.
- `NOT_FOUND` is never retried.
- Page sizes are tuned to stay inside GitHub's node limits, and incremental syncs
  avoid re-reading data that has not changed.

---

## 17. Security considerations

**The token.** `GITHUB_TOKEN` is read from the environment into memory. It is
never stored in PostgreSQL, never rendered in HTML, never included in API
responses or error pages, and never logged — the `Authorization` header is set on
the request and never echoed. Startup logs the configuration with the token
replaced by `[redacted]`. Automated tests assert that the token appears in no log
line and in no HTTP response.

**Least privilege.** DevPulse needs only read permissions and never writes to
GitHub. See [PAT permissions](#5-github-pat-permissions).

**SQL.** Every statement is parameterized. The only piece of SQL assembled at
runtime is the ranking `ORDER BY`, and it is chosen from a fixed table of known
column aliases; an unknown sort key falls back to the default.

**Output escaping.** Pages are rendered with `html/template`, which escapes
contextually — repository names, logins, pull request titles and descriptions
from GitHub are treated as untrusted data, never as safe HTML. Chart payloads are
JSON with `<`, `>` and `&` escaped so a crafted string cannot break out of the
`<script>` element.

**HTTP headers.** A Content-Security-Policy, `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY` and `Referrer-Policy: no-referrer` are set on every
response.

**Errors.** Database connection errors are stripped of embedded credentials
before they are logged. Internal errors render a generic page; details go to the
application log.

**Note on access control.** DevPulse has no authentication of its own. Anyone who
can reach the port sees the dashboard. Do not publish it to the internet without
putting an authenticating proxy in front of it.

---

## 18. Troubleshooting

**`GITHUB_TOKEN must be set` at startup**
Configuration validation failed. The message lists every missing or malformed
variable. Check that `.env` exists and that `docker compose` picked it up
(`docker compose config`).

**`github rejected the token (status 401)`**
The token is invalid, expired, or not authorized for the organization. If your
organization enforces SAML SSO, open the token settings and click *Authorize* for
that organization.

**`github resource not found ... organization "x"`**
`GITHUB_ORG` is misspelled, or the token cannot see the organization. The value
must be the organization **login** (as in `github.com/<login>`), not its display
name.

**The dashboard says "No data for … yet"**
The first synchronization has not finished. Check `/status` and
`docker compose logs -f app`. For a large organization the initial import takes a
while.

**Some commits have no contributor**
A commit whose author e-mail is not linked to a GitHub account (deleted user,
unconfigured local git e-mail) cannot be attributed. DevPulse stores it with the
raw author name but leaves `contributor_id` empty, so it counts in repository
totals but in nobody's ranking. Ask contributors to add their commit e-mail to
their GitHub account.

**Changed files are zero for some commits**
GitHub does not report `changedFilesIfAvailable` for every commit (very large
commits in particular). Unavailable values count as zero.

**A repository failed but others succeeded**
That is intended. Repository errors are logged with the repository name, the run
continues, and its status becomes `partial`. The errors are visible on `/status`.

**`synchronization postponed until the GitHub rate limit recovers`**
The point budget fell below `MIN_RATE_LIMIT_REMAINING`. Wait for `resetAt` (shown
on `/status`), lengthen `SYNC_INTERVAL`, or reduce `HISTORY_MONTHS`.

**Database not reachable**
`/health` returns `503` with `"database":"unavailable"`. Check
`docker compose ps` and the `postgres` logs. The app retries for 60 s at startup.

**Starting over**
`docker compose down -v` deletes the database volume; the next start
re-synchronizes from scratch.

---

## 19. Metric definitions

Also available in the UI under *"What each metric means"*.

| Metric | Definition |
|---|---|
| **Commits** | Commits on the default branch attributed to the contributor's GitHub account during the period. |
| **Additions** | Lines added, as reported by GitHub for those commits. |
| **Deletions** | Lines deleted, as reported by GitHub for those commits. |
| **Changed files** | Sum of files touched by those commits. Unavailable values count as zero. |
| **PRs opened** | Pull requests authored by the contributor and created during the period. |
| **PRs merged** | Pull requests authored by the contributor that were merged during the period. |
| **PRs closed** | Pull requests authored by the contributor that were closed **without** being merged. |
| **Reviews submitted** | Review submissions made during the period, including approvals, change requests and comment-only reviews. Pending (unsubmitted) reviews are not counted. |
| **Approvals** | Review submissions whose state was `APPROVED`. |
| **Changes requested** | Review submissions whose state was `CHANGES_REQUESTED`. |
| **Review comments** | Inline comments attached to those review submissions. |
| **Unique PRs reviewed** | Distinct pull requests reviewed at least once. Two reviews on one pull request count once. |
| **Active days** | Distinct UTC calendar days with any tracked activity (commit, pull request or review). |
| **Repositories** | Distinct repositories of the configured organization the contributor was active in. |

Attribution rules: commit metrics belong to the commit **author**; PR metrics to
the pull request **author**; review metrics to the **reviewer**. Days are UTC.

---

## 20. Development

The repository is plain Go with no code generation.

```bash
gofmt -l .
go vet ./...
go test ./...
```

`go test ./...` passes without a database: the PostgreSQL integration tests skip
themselves unless `TEST_DATABASE_URL` is set. To run everything:

```bash
docker run -d --name devpulse-test-pg \
  -e POSTGRES_USER=devpulse -e POSTGRES_PASSWORD=devpulse -e POSTGRES_DB=devpulse_test \
  -p 55432:5432 postgres:17-alpine

TEST_DATABASE_URL='postgres://devpulse:devpulse@127.0.0.1:55432/devpulse_test?sslmode=disable' \
  go test ./...
```

Test coverage includes date-range calculation, organization filtering and
isolation, contributor aggregation, ranking and sorting, bot exclusion, archived
repository exclusion, duplicate-synchronization prevention, upsert idempotency,
configuration validation, GraphQL pagination (including nested review pages),
rate-limit handling, template escaping of GitHub-provided strings, PDF report
generation for every period (including table widths that must fit the page), and
an end-to-end check that neither rendering a page nor generating a report ever
calls GitHub. Collector tests run
against recorded GraphQL responses — no network access required.

### Continuous integration

`.github/workflows/ci.yml` runs on every pull request against `main`, and can be
started by hand from the Actions tab. It needs no secrets, so pull requests from
forks work.

**Go tests** — `gofmt` check, `go mod tidy` check (fails if `go.mod`/`go.sum`
would change), `go build`, `go vet`, then `go test -race` against a PostgreSQL
service container, so the integration tests actually run instead of skipping.
The coverage profile is uploaded as a build artifact.

**Docker image and stack** — validates `docker compose config`, builds the image,
starts the full stack with a placeholder token and checks that `/health` and
`/ready` answer, that `/ready` reports an applied schema version, that every page
and a PDF report return `200`, and that the token never appears in the container
logs. The placeholder token makes synchronization fail on purpose: the web
application must keep serving regardless. Container logs are dumped if anything
fails.

Building the image:

```bash
docker build -t devpulse:local --build-arg VERSION=1.0.0 .
```

### Adding a migration

Drop a new `migrations/NNNN_name.sql` file in place. It is embedded at build time
and applied automatically, in lexical order, on the next start.
