package github

import (
	"strings"
	"time"
)

// PageInfo mirrors the GraphQL cursor connection page info.
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// Actor is a GitHub account reference (User or Bot). TypeName lets us detect bots
// without guessing from the login alone.
type Actor struct {
	TypeName  string `json:"__typename"`
	ID        string `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
	URL       string `json:"url"`
}

// IsBot reports whether the actor is a GitHub App/bot account. GitHub reports
// `Bot`, `EnterpriseUserAccount` and `Mannequin` types; App installations also
// show up as users whose login ends in "[bot]".
func (a *Actor) IsBot() bool {
	if a == nil {
		return false
	}
	if a.TypeName == "Bot" || a.TypeName == "Mannequin" {
		return true
	}
	login := strings.ToLower(a.Login)
	return strings.HasSuffix(login, "[bot]") || login == "github-actions"
}

// Organization is the configured organization.
type Organization struct {
	ID         string    `json:"id"`
	DatabaseID int64     `json:"databaseId"`
	Login      string    `json:"login"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Repository is a repository owned by the configured organization.
type Repository struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	NameWithOwner string     `json:"nameWithOwner"`
	URL           string     `json:"url"`
	Description   string     `json:"description"`
	IsArchived    bool       `json:"isArchived"`
	IsPrivate     bool       `json:"isPrivate"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	PushedAt      *time.Time `json:"pushedAt"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	PrimaryLanguage *struct {
		Name string `json:"name"`
	} `json:"primaryLanguage"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
}

// Commit is a commit on the default branch.
type Commit struct {
	OID                     string    `json:"oid"`
	CommittedDate           time.Time `json:"committedDate"`
	Additions               int       `json:"additions"`
	Deletions               int       `json:"deletions"`
	ChangedFilesIfAvailable *int      `json:"changedFilesIfAvailable"`
	Author                  *struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		User  *Actor `json:"user"`
	} `json:"author"`
}

// ChangedFiles resolves the nullable changed-file count.
func (c Commit) ChangedFiles() int {
	if c.ChangedFilesIfAvailable == nil {
		return 0
	}
	return *c.ChangedFilesIfAvailable
}

// PullRequest is a pull request with its aggregate diff counters.
type PullRequest struct {
	ID           string     `json:"id"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	State        string     `json:"state"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	MergedAt     *time.Time `json:"mergedAt"`
	ClosedAt     *time.Time `json:"closedAt"`
	Additions    int        `json:"additions"`
	Deletions    int        `json:"deletions"`
	ChangedFiles int        `json:"changedFiles"`
	Author       *Actor     `json:"author"`
}

// Review is one review submission.
type Review struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submittedAt"`
	Author      *Actor     `json:"author"`
	Comments    struct {
		TotalCount int `json:"totalCount"`
	} `json:"comments"`
}

// ReviewPage carries the reviews of a single pull request plus its cursor.
type ReviewPage struct {
	PullRequestNumber int
	PullRequestID     string
	Reviews           []Review
	PageInfo          PageInfo
}

// Page is a generic paginated result.
type Page[T any] struct {
	Nodes    []T
	PageInfo PageInfo
}
