package github

import (
	"context"
	"fmt"
	"time"
)

// rateLimitFragment is appended to every operation so cost/remaining/resetAt are
// always observable. GitHub charges nothing extra for this field.
const rateLimitFragment = `
  rateLimit {
    limit
    cost
    remaining
    resetAt
    nodeCount
  }`

// actorSelection selects the author of a pull request or a review.
//
// Those fields are typed as the `Actor` interface, which exposes only login,
// avatarUrl, url and resourcePath. `id` and `name` are NOT on the interface, so
// they must be requested through inline fragments on the concrete types that
// implement it -- otherwise GitHub rejects the entire query with
// "Field 'id' doesn't exist on type 'Actor'".
//
// A commit author is different: commit.author.user is already a concrete User,
// so that query can select those fields directly.
const actorSelection = `author {
          __typename
          login
          avatarUrl
          url
          ... on User { id name }
          ... on Bot { id }
          ... on Mannequin { id }
          ... on Organization { id name }
          ... on EnterpriseUserAccount { id name }
        }`

const orgQuery = `
query DevPulseOrganization($login: String!) {
  organization(login: $login) {
    id
    databaseId
    login
    name
    createdAt
  }` + rateLimitFragment + `
}`

// FetchOrganization loads exactly the organization named by login.
func (c *Client) FetchOrganization(ctx context.Context, login string) (*Organization, error) {
	var out struct {
		Organization *Organization `json:"organization"`
	}
	if err := c.do(ctx, "organization", orgQuery, map[string]any{"login": login}, &out); err != nil {
		return nil, err
	}
	if out.Organization == nil {
		return nil, fmt.Errorf("%w: organization %q", ErrNotFound, login)
	}
	return out.Organization, nil
}

const reposQuery = `
query DevPulseRepositories($login: String!, $first: Int!, $after: String) {
  organization(login: $login) {
    repositories(first: $first, after: $after, orderBy: {field: PUSHED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        name
        nameWithOwner
        url
        description
        isArchived
        isPrivate
        createdAt
        updatedAt
        pushedAt
        owner { login }
        primaryLanguage { name }
        defaultBranchRef { name }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchRepositories returns one page of repositories for the configured org.
func (c *Client) FetchRepositories(ctx context.Context, login string, pageSize int, after string) (*Page[Repository], error) {
	var out struct {
		Organization *struct {
			Repositories struct {
				PageInfo PageInfo     `json:"pageInfo"`
				Nodes    []Repository `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	}
	vars := map[string]any{"login": login, "first": pageSize, "after": cursor(after)}
	if err := c.do(ctx, "repositories", reposQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Organization == nil {
		return nil, fmt.Errorf("%w: organization %q", ErrNotFound, login)
	}
	return &Page[Repository]{
		Nodes:    out.Organization.Repositories.Nodes,
		PageInfo: out.Organization.Repositories.PageInfo,
	}, nil
}

// teamMemberFields selects a team member. Team members are concrete Users, so
// unlike a pull request author they can select id and name directly.
const teamMemberFields = `__typename id login name avatarUrl url`

const teamsQuery = `
query DevPulseTeams($login: String!, $first: Int!, $after: String, $members: Int!, $repos: Int!) {
  organization(login: $login) {
    teams(first: $first, after: $after, orderBy: {field: NAME, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        slug
        name
        description
        url
        privacy
        members(first: $members) {
          pageInfo { hasNextPage endCursor }
          nodes { ` + teamMemberFields + ` }
        }
        repositories(first: $repos) {
          pageInfo { hasNextPage endCursor }
          nodes { id name nameWithOwner }
        }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchTeams returns one page of the configured organization's teams, each with
// its first page of members and repositories.
func (c *Client) FetchTeams(ctx context.Context, login string, pageSize, memberPageSize, repoPageSize int, after string) (*Page[Team], error) {
	var out struct {
		Organization *struct {
			Teams struct {
				PageInfo PageInfo `json:"pageInfo"`
				Nodes    []Team   `json:"nodes"`
			} `json:"teams"`
		} `json:"organization"`
	}
	vars := map[string]any{
		"login":   login,
		"first":   pageSize,
		"after":   cursor(after),
		"members": memberPageSize,
		"repos":   repoPageSize,
	}
	if err := c.do(ctx, "teams", teamsQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Organization == nil {
		return nil, fmt.Errorf("%w: organization %q", ErrNotFound, login)
	}
	t := out.Organization.Teams
	return &Page[Team]{Nodes: t.Nodes, PageInfo: t.PageInfo}, nil
}

const teamMembersQuery = `
query DevPulseTeamMembers($login: String!, $slug: String!, $first: Int!, $after: String) {
  organization(login: $login) {
    team(slug: $slug) {
      members(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { ` + teamMemberFields + ` }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchTeamMembers pages the members of a single team.
func (c *Client) FetchTeamMembers(ctx context.Context, login, slug string, pageSize int, after string) (*Page[Actor], error) {
	var out struct {
		Organization *struct {
			Team *struct {
				Members struct {
					PageInfo PageInfo `json:"pageInfo"`
					Nodes    []Actor  `json:"nodes"`
				} `json:"members"`
			} `json:"team"`
		} `json:"organization"`
	}
	vars := map[string]any{"login": login, "slug": slug, "first": pageSize, "after": cursor(after)}
	if err := c.do(ctx, "teamMembers", teamMembersQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Organization == nil || out.Organization.Team == nil {
		return nil, fmt.Errorf("%w: team %s/%s", ErrNotFound, login, slug)
	}
	m := out.Organization.Team.Members
	return &Page[Actor]{Nodes: m.Nodes, PageInfo: m.PageInfo}, nil
}

const teamRepositoriesQuery = `
query DevPulseTeamRepositories($login: String!, $slug: String!, $first: Int!, $after: String) {
  organization(login: $login) {
    team(slug: $slug) {
      repositories(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { id name nameWithOwner }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchTeamRepositories pages the repositories a single team has access to.
func (c *Client) FetchTeamRepositories(ctx context.Context, login, slug string, pageSize int, after string) (*Page[TeamRepository], error) {
	var out struct {
		Organization *struct {
			Team *struct {
				Repositories struct {
					PageInfo PageInfo         `json:"pageInfo"`
					Nodes    []TeamRepository `json:"nodes"`
				} `json:"repositories"`
			} `json:"team"`
		} `json:"organization"`
	}
	vars := map[string]any{"login": login, "slug": slug, "first": pageSize, "after": cursor(after)}
	if err := c.do(ctx, "teamRepositories", teamRepositoriesQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Organization == nil || out.Organization.Team == nil {
		return nil, fmt.Errorf("%w: team %s/%s", ErrNotFound, login, slug)
	}
	r := out.Organization.Team.Repositories
	return &Page[TeamRepository]{Nodes: r.Nodes, PageInfo: r.PageInfo}, nil
}

const commitsQuery = `
query DevPulseCommits($owner: String!, $name: String!, $since: GitTimestamp!, $until: GitTimestamp, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef {
      target {
        ... on Commit {
          history(since: $since, until: $until, first: $first, after: $after) {
            pageInfo { hasNextPage endCursor }
            nodes {
              oid
              committedDate
              additions
              deletions
              changedFilesIfAvailable
              author {
                name
                email
                user { __typename id login name avatarUrl url }
              }
            }
          }
        }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchCommits returns one page of default-branch commits in [since, until).
func (c *Client) FetchCommits(ctx context.Context, owner, name string, since time.Time, until *time.Time, pageSize int, after string) (*Page[Commit], error) {
	var out struct {
		Repository *struct {
			DefaultBranchRef *struct {
				Target *struct {
					History struct {
						PageInfo PageInfo `json:"pageInfo"`
						Nodes    []Commit `json:"nodes"`
					} `json:"history"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
		} `json:"repository"`
	}
	vars := map[string]any{
		"owner": owner,
		"name":  name,
		"since": since.UTC().Format(time.RFC3339),
		"until": nil,
		"first": pageSize,
		"after": cursor(after),
	}
	if until != nil {
		vars["until"] = until.UTC().Format(time.RFC3339)
	}
	if err := c.do(ctx, "commits", commitsQuery, vars, &out); err != nil {
		return nil, err
	}
	// An empty repository has no default branch; that is not an error.
	if out.Repository == nil || out.Repository.DefaultBranchRef == nil || out.Repository.DefaultBranchRef.Target == nil {
		return &Page[Commit]{}, nil
	}
	h := out.Repository.DefaultBranchRef.Target.History
	return &Page[Commit]{Nodes: h.Nodes, PageInfo: h.PageInfo}, nil
}

const pullRequestsQuery = `
query DevPulsePullRequests($owner: String!, $name: String!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}, states: [OPEN, CLOSED, MERGED]) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        number
        title
        url
        state
        createdAt
        updatedAt
        mergedAt
        closedAt
        additions
        deletions
        changedFiles
        ` + actorSelection + `
      }
    }
  }` + rateLimitFragment + `
}`

// FetchPullRequests returns one page of pull requests ordered by UPDATED_AT desc,
// which lets incremental syncs stop as soon as they reach already-seen updates.
func (c *Client) FetchPullRequests(ctx context.Context, owner, name string, pageSize int, after string) (*Page[PullRequest], error) {
	var out struct {
		Repository *struct {
			PullRequests struct {
				PageInfo PageInfo      `json:"pageInfo"`
				Nodes    []PullRequest `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "name": name, "first": pageSize, "after": cursor(after)}
	if err := c.do(ctx, "pullRequests", pullRequestsQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Repository == nil {
		return nil, fmt.Errorf("%w: repository %s/%s", ErrNotFound, owner, name)
	}
	pr := out.Repository.PullRequests
	return &Page[PullRequest]{Nodes: pr.Nodes, PageInfo: pr.PageInfo}, nil
}

const repoReviewsQuery = `
query DevPulseRepositoryReviews($owner: String!, $name: String!, $first: Int!, $after: String, $reviews: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}, states: [OPEN, CLOSED, MERGED]) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        number
        updatedAt
        reviews(first: $reviews) {
          pageInfo { hasNextPage endCursor }
          nodes {
            id
            state
            submittedAt
            ` + actorSelection + `
            comments { totalCount }
          }
        }
      }
    }
  }` + rateLimitFragment + `
}`

// PullRequestReviews is a PR node carrying its first page of reviews.
type PullRequestReviews struct {
	ID        string    `json:"id"`
	Number    int       `json:"number"`
	UpdatedAt time.Time `json:"updatedAt"`
	Reviews   struct {
		PageInfo PageInfo `json:"pageInfo"`
		Nodes    []Review `json:"nodes"`
	} `json:"reviews"`
}

// FetchRepositoryReviews returns a page of pull requests each with their first
// page of reviews. Pull requests whose reviews report hasNextPage are completed
// with FetchPullRequestReviews.
func (c *Client) FetchRepositoryReviews(ctx context.Context, owner, name string, pageSize, reviewPageSize int, after string) (*Page[PullRequestReviews], error) {
	var out struct {
		Repository *struct {
			PullRequests struct {
				PageInfo PageInfo             `json:"pageInfo"`
				Nodes    []PullRequestReviews `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	vars := map[string]any{
		"owner":   owner,
		"name":    name,
		"first":   pageSize,
		"after":   cursor(after),
		"reviews": reviewPageSize,
	}
	if err := c.do(ctx, "repositoryReviews", repoReviewsQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Repository == nil {
		return nil, fmt.Errorf("%w: repository %s/%s", ErrNotFound, owner, name)
	}
	pr := out.Repository.PullRequests
	return &Page[PullRequestReviews]{Nodes: pr.Nodes, PageInfo: pr.PageInfo}, nil
}

const prReviewsQuery = `
query DevPulsePullRequestReviews($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id
      number
      reviews(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          state
          submittedAt
          ` + actorSelection + `
          comments { totalCount }
        }
      }
    }
  }` + rateLimitFragment + `
}`

// FetchPullRequestReviews pages the reviews of a single pull request.
func (c *Client) FetchPullRequestReviews(ctx context.Context, owner, name string, number, pageSize int, after string) (*ReviewPage, error) {
	var out struct {
		Repository *struct {
			PullRequest *struct {
				ID      string `json:"id"`
				Number  int    `json:"number"`
				Reviews struct {
					PageInfo PageInfo `json:"pageInfo"`
					Nodes    []Review `json:"nodes"`
				} `json:"reviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "name": name, "number": number, "first": pageSize, "after": cursor(after)}
	if err := c.do(ctx, "pullRequestReviews", prReviewsQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Repository == nil || out.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%w: pull request %s/%s#%d", ErrNotFound, owner, name, number)
	}
	p := out.Repository.PullRequest
	return &ReviewPage{
		PullRequestNumber: p.Number,
		PullRequestID:     p.ID,
		Reviews:           p.Reviews.Nodes,
		PageInfo:          p.Reviews.PageInfo,
	}, nil
}

const usersQuery = `
query DevPulseUsers($ids: [ID!]!) {
  nodes(ids: $ids) {
    __typename
    ... on User { id login name avatarUrl url }
    ... on Bot { id login avatarUrl url }
  }` + rateLimitFragment + `
}`

// FetchActors enriches contributor records (name, avatar) by node id.
func (c *Client) FetchActors(ctx context.Context, ids []string) ([]Actor, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out struct {
		Nodes []*Actor `json:"nodes"`
	}
	if err := c.do(ctx, "actors", usersQuery, map[string]any{"ids": ids}, &out); err != nil {
		return nil, err
	}
	actors := make([]Actor, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		if n != nil && n.ID != "" {
			actors = append(actors, *n)
		}
	}
	return actors, nil
}

// cursor converts an empty string into a GraphQL null so `after` is omitted on
// the first page.
func cursor(after string) any {
	if after == "" {
		return nil
	}
	return after
}
