-- GitHub organization teams ("groups") and their membership.
--
-- Teams belong to exactly one organization, like every other imported entity,
-- and the join tables carry organization_id too so a team filter can never
-- reach across organizations.

CREATE TABLE teams (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    github_id       TEXT        NOT NULL UNIQUE,
    slug            TEXT        NOT NULL,
    name            TEXT        NOT NULL DEFAULT '',
    description     TEXT        NOT NULL DEFAULT '',
    url             TEXT        NOT NULL DEFAULT '',
    privacy         TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, slug)
);

CREATE INDEX teams_organization_idx ON teams (organization_id);

CREATE TABLE team_members (
    team_id         BIGINT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    contributor_id  BIGINT NOT NULL REFERENCES contributors (id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, contributor_id)
);

CREATE INDEX team_members_contributor_idx ON team_members (contributor_id);
CREATE INDEX team_members_organization_idx ON team_members (organization_id);

CREATE TABLE team_repositories (
    team_id         BIGINT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    repository_id   BIGINT NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, repository_id)
);

CREATE INDEX team_repositories_repository_idx ON team_repositories (repository_id);
CREATE INDEX team_repositories_organization_idx ON team_repositories (organization_id);
