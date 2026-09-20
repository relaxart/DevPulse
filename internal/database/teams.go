package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/relaxart/dev-pulse/internal/models"
)

// UpsertTeam stores a team of the configured organization.
func (db *DB) UpsertTeam(ctx context.Context, t *models.Team) (int64, error) {
	const q = `
INSERT INTO teams (organization_id, github_id, slug, name, description, url, privacy)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (github_id) DO UPDATE
   SET organization_id = EXCLUDED.organization_id,
       slug = EXCLUDED.slug,
       name = EXCLUDED.name,
       description = EXCLUDED.description,
       url = EXCLUDED.url,
       privacy = EXCLUDED.privacy,
       updated_at = now()
RETURNING id`

	var id int64
	err := db.Pool.QueryRow(ctx, q,
		t.OrganizationID, t.GitHubID, t.Slug, t.Name, t.Description, t.URL, t.Privacy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert team %s: %w", t.Slug, redact(err))
	}
	return id, nil
}

// ReplaceTeamMembers makes the stored membership match GitHub exactly.
//
// Membership is a set, not an append-only log: people leave teams, so a plain
// upsert would keep former members forever. The swap runs in one transaction so
// a concurrent dashboard query never sees a half-empty team.
func (db *DB) ReplaceTeamMembers(ctx context.Context, orgID, teamID int64, contributorIDs []int64) error {
	return db.replaceTeamLinks(ctx,
		`DELETE FROM team_members WHERE team_id = $1`,
		`INSERT INTO team_members (team_id, contributor_id, organization_id)
		 SELECT $1, unnest($2::bigint[]), $3
		 ON CONFLICT DO NOTHING`,
		orgID, teamID, contributorIDs, "members")
}

// ReplaceTeamRepositories makes the stored repository links match GitHub exactly.
func (db *DB) ReplaceTeamRepositories(ctx context.Context, orgID, teamID int64, repositoryIDs []int64) error {
	return db.replaceTeamLinks(ctx,
		`DELETE FROM team_repositories WHERE team_id = $1`,
		`INSERT INTO team_repositories (team_id, repository_id, organization_id)
		 SELECT $1, unnest($2::bigint[]), $3
		 ON CONFLICT DO NOTHING`,
		orgID, teamID, repositoryIDs, "repositories")
}

func (db *DB) replaceTeamLinks(ctx context.Context, deleteSQL, insertSQL string, orgID, teamID int64, ids []int64, what string) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin team %s update: %w", what, redact(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, deleteSQL, teamID); err != nil {
		return fmt.Errorf("clear team %s: %w", what, redact(err))
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, insertSQL, teamID, ids, orgID); err != nil {
			return fmt.Errorf("store team %s: %w", what, redact(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit team %s update: %w", what, redact(err))
	}
	return nil
}

// DeleteTeamsNotIn removes teams that no longer exist on GitHub. Passing an
// empty list deletes every team of the organization, which is what a genuinely
// team-less organization should end up with.
func (db *DB) DeleteTeamsNotIn(ctx context.Context, orgID int64, githubIDs []string) (int64, error) {
	if githubIDs == nil {
		githubIDs = []string{}
	}
	tag, err := db.Pool.Exec(ctx,
		`DELETE FROM teams WHERE organization_id = $1 AND NOT (github_id = ANY($2))`, orgID, githubIDs)
	if err != nil {
		return 0, fmt.Errorf("delete removed teams: %w", redact(err))
	}
	return tag.RowsAffected(), nil
}

const teamColumns = `
       t.id, t.organization_id, t.github_id, t.slug, t.name, t.description, t.url, t.privacy,
       (SELECT count(*) FROM team_members tm WHERE tm.team_id = t.id),
       (SELECT count(*) FROM team_repositories tr WHERE tr.team_id = t.id)`

func scanTeam(row pgx.Row) (*models.Team, error) {
	var t models.Team
	err := row.Scan(&t.ID, &t.OrganizationID, &t.GitHubID, &t.Slug, &t.Name,
		&t.Description, &t.URL, &t.Privacy, &t.MemberCount, &t.RepositoryCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, redact(err)
	}
	return &t, nil
}

// ListTeams returns the organization's teams with their member and repository
// counts, ordered for a filter menu.
func (db *DB) ListTeams(ctx context.Context, orgID int64) ([]models.Team, error) {
	q := `SELECT` + teamColumns + `
  FROM teams t
 WHERE t.organization_id = $1
 ORDER BY lower(COALESCE(NULLIF(t.name, ''), t.slug))`

	rows, err := db.Pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", redact(err))
	}
	defer rows.Close()

	var out []models.Team
	for rows.Next() {
		var t models.Team
		if err := rows.Scan(&t.ID, &t.OrganizationID, &t.GitHubID, &t.Slug, &t.Name,
			&t.Description, &t.URL, &t.Privacy, &t.MemberCount, &t.RepositoryCount); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TeamBySlug resolves a team slug inside one organization. An unknown slug
// yields a nil team rather than an error, so a stale link degrades to the
// unfiltered view.
func (db *DB) TeamBySlug(ctx context.Context, orgID int64, slug string) (*models.Team, error) {
	if slug == "" {
		return nil, nil
	}
	q := `SELECT` + teamColumns + `
  FROM teams t
 WHERE t.organization_id = $1 AND lower(t.slug) = lower($2)`
	team, err := scanTeam(db.Pool.QueryRow(ctx, q, orgID, slug))
	if err != nil {
		return nil, fmt.Errorf("load team %s: %w", slug, err)
	}
	return team, nil
}

// CountTeams reports how many teams are stored, for the status page.
func (db *DB) CountTeams(ctx context.Context, orgID int64) (int64, error) {
	var n int64
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM teams WHERE organization_id = $1`, orgID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count teams: %w", redact(err))
	}
	return n, nil
}

// RepositoryIDsByGitHubID maps GitHub node ids to internal repository ids, so
// team repository links can be resolved without one query per repository.
func (db *DB) RepositoryIDsByGitHubID(ctx context.Context, orgID int64) (map[string]int64, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT github_id, id FROM repositories WHERE organization_id = $1`, orgID)
	if err != nil {
		return nil, fmt.Errorf("load repository ids: %w", redact(err))
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var githubID string
		var id int64
		if err := rows.Scan(&githubID, &id); err != nil {
			return nil, fmt.Errorf("scan repository id: %w", err)
		}
		out[githubID] = id
	}
	return out, rows.Err()
}
