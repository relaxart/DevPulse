// Package models holds the structs shared between the collector, the database
// layer and the HTTP handlers.
package models

import "time"

// Organization is the single configured GitHub organization.
type Organization struct {
	ID           int64
	GitHubID     string
	Login        string
	Name         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastSyncedAt *time.Time
}

// Repository belongs to exactly one organization.
type Repository struct {
	ID             int64
	OrganizationID int64
	GitHubID       string
	Name           string
	FullName       string
	URL            string
	Description    string
	PrimaryLang    string
	IsArchived     bool
	IsPrivate      bool
	DefaultBranch  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PushedAt       *time.Time

	CommitsSyncedAt *time.Time
	PRsSyncedAt     *time.Time
	ReviewsSyncedAt *time.Time
}

// Contributor is a GitHub account seen in the configured organization.
type Contributor struct {
	ID        int64
	GitHubID  string
	Login     string
	Name      string
	AvatarURL string
	URL       string
	IsBot     bool
}

// Commit is one commit on a repository's default branch.
type Commit struct {
	OrganizationID int64
	RepositoryID   int64
	ContributorID  *int64
	GitHubOID      string
	CommittedAt    time.Time
	Additions      int
	Deletions      int
	ChangedFiles   int
	AuthorName     string
}

// PullRequest state values as stored.
const (
	PRStateOpen   = "OPEN"
	PRStateClosed = "CLOSED"
	PRStateMerged = "MERGED"
)

// PullRequest is a pull request in a repository of the configured organization.
type PullRequest struct {
	ID             int64
	OrganizationID int64
	RepositoryID   int64
	AuthorID       *int64
	GitHubID       string
	Number         int
	Title          string
	URL            string
	State          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	MergedAt       *time.Time
	ClosedAt       *time.Time
	Additions      int
	Deletions      int
	ChangedFiles   int
}

// Review state values as reported by GitHub.
const (
	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
	ReviewDismissed        = "DISMISSED"
)

// PullRequestReview is one review submission on a pull request.
type PullRequestReview struct {
	OrganizationID int64
	RepositoryID   int64
	PullRequestID  int64
	ReviewerID     *int64
	GitHubID       string
	State          string
	SubmittedAt    time.Time
	CommentCount   int
}

// Sync run status values.
const (
	SyncStatusRunning = "running"
	SyncStatusSuccess = "success"
	SyncStatusPartial = "partial"
	SyncStatusFailed  = "failed"
)

// SyncRun records one synchronization attempt.
type SyncRun struct {
	ID               int64
	OrganizationID   int64
	SyncType         string
	Status           string
	StartedAt        time.Time
	FinishedAt       *time.Time
	DurationSeconds  float64
	RecordsProcessed int
	Repositories     int
	GraphQLCost      int
	RateLimitRemain  int
	RateLimitLimit   int
	RateLimitResetAt *time.Time
	ErrorCount       int
	ErrorMessage     string
}

// ContributorStats is one row of the contributor ranking.
type ContributorStats struct {
	Contributor
	Commits           int64
	Additions         int64
	Deletions         int64
	ChangedFiles      int64
	PRsOpened         int64
	PRsMerged         int64
	PRsClosed         int64
	ReviewsSubmitted  int64
	Approvals         int64
	ChangesRequested  int64
	ReviewComments    int64
	UniquePRsReviewed int64
	ActiveDays        int64
	Repositories      int64
}

// NetLines is additions minus deletions.
func (s ContributorStats) NetLines() int64 { return s.Additions - s.Deletions }

// DisplayName prefers the human name and falls back to the login.
func (c Contributor) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Login
}

// OverviewStats are the dashboard headline numbers for a period.
type OverviewStats struct {
	ActiveContributors int64
	Commits            int64
	Additions          int64
	Deletions          int64
	PRsOpened          int64
	PRsMerged          int64
	Reviews            int64
	ActiveRepositories int64
}

// RepositoryStats is one row of the repository list.
type RepositoryStats struct {
	Repository
	Contributors int64
	Commits      int64
	Additions    int64
	Deletions    int64
	PRsOpened    int64
	PRsMerged    int64
	Reviews      int64
	LastActivity *time.Time
}

// TimePoint is one bucket of a time series chart.
type TimePoint struct {
	Bucket           time.Time
	Label            string
	Commits          int64
	Additions        int64
	Deletions        int64
	PRsOpened        int64
	PRsMerged        int64
	ReviewsSubmitted int64
}

// ActivityRow is a recent-activity feed entry.
type ActivityRow struct {
	Date        time.Time
	Contributor Contributor
	Repository  string
	Commits     int64
	PRsOpened   int64
	PRsMerged   int64
	Reviews     int64
	Additions   int64
	Deletions   int64
}
