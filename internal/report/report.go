// Package report builds downloadable PDF activity reports.
//
// Like every other read path in DevPulse, a report is assembled purely from
// PostgreSQL aggregates for the configured organization. Generating one never
// contacts GitHub.
package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
)

// Store is the slice of the database layer a report needs.
type Store interface {
	Overview(ctx context.Context, f database.Filter) (models.OverviewStats, error)
	TimeSeries(ctx context.Context, f database.Filter, g metrics.Granularity, contributorID, repositoryID *int64) ([]models.TimePoint, error)
	ContributorRanking(ctx context.Context, f database.Filter, sortKey string, limit, offset int) ([]models.ContributorStats, error)
	RepositoryRanking(ctx context.Context, f database.Filter) ([]models.RepositoryStats, error)
}

// Request describes the report to produce.
type Request struct {
	Organization string
	Filter       database.Filter
	Range        metrics.Range
	SortKey      string
	// MaxContributors and MaxRepositories bound the tables so a very large
	// organization cannot produce an unbounded document.
	MaxContributors int
	MaxRepositories int
	LastSync        *time.Time
}

// Default table bounds.
const (
	DefaultMaxContributors = 100
	DefaultMaxRepositories = 100
)

// Data is everything the renderer needs. It is a plain value so the PDF layout
// can be tested without a database.
type Data struct {
	Organization    string
	Range           metrics.Range
	Previous        metrics.Range
	Sort            metrics.Metric
	GeneratedAt     time.Time
	LastSync        *time.Time
	IncludeArchived bool
	ExcludedUsers   []string

	Overview     models.OverviewStats
	PreviousSum  models.OverviewStats
	Granularity  metrics.Granularity
	Series       []models.TimePoint
	Contributors []models.ContributorStats
	Repositories []models.RepositoryStats
	Metrics      []metrics.Metric

	// Truncated reports say so on the page rather than silently cutting off.
	ContributorsTruncated bool
	RepositoriesTruncated bool

	// NoData marks a report for an organization that has never been
	// synchronized, so the document can say so instead of looking like a period
	// with genuinely zero activity.
	NoData bool
}

// Empty builds a report for an organization that has not been synchronized yet.
// A fresh deployment should still produce a readable document rather than an
// error, exactly as the dashboard renders with a "no data yet" banner.
func Empty(organization string, rng metrics.Range) *Data {
	return &Data{
		Organization: organization,
		Range:        rng,
		Previous:     rng.PreviousRange(),
		Sort:         metrics.ResolveSort(""),
		GeneratedAt:  time.Now().UTC(),
		Granularity:  rng.Granularity(),
		Metrics:      metrics.RankingMetrics(),
		NoData:       true,
	}
}

// Collect assembles the report data from PostgreSQL.
func Collect(ctx context.Context, store Store, req Request) (*Data, error) {
	if req.MaxContributors <= 0 {
		req.MaxContributors = DefaultMaxContributors
	}
	if req.MaxRepositories <= 0 {
		req.MaxRepositories = DefaultMaxRepositories
	}

	previous := req.Range.PreviousRange()
	previousFilter := req.Filter
	previousFilter.From, previousFilter.To = previous.From, previous.To

	d := &Data{
		Organization:    req.Organization,
		Range:           req.Range,
		Previous:        previous,
		Sort:            metrics.ResolveSort(req.SortKey),
		GeneratedAt:     time.Now().UTC(),
		LastSync:        req.LastSync,
		IncludeArchived: req.Filter.IncludeArchived,
		ExcludedUsers:   req.Filter.ExcludedLogins,
		Granularity:     req.Range.Granularity(),
		Metrics:         metrics.RankingMetrics(),
	}

	var err error
	if d.Overview, err = store.Overview(ctx, req.Filter); err != nil {
		return nil, fmt.Errorf("report overview: %w", err)
	}
	if d.PreviousSum, err = store.Overview(ctx, previousFilter); err != nil {
		return nil, fmt.Errorf("report comparison period: %w", err)
	}
	if d.Series, err = store.TimeSeries(ctx, req.Filter, d.Granularity, nil, nil); err != nil {
		return nil, fmt.Errorf("report time series: %w", err)
	}

	// Fetch one extra row so the report can state that it was truncated.
	contributors, err := store.ContributorRanking(ctx, req.Filter, d.Sort.Key, req.MaxContributors+1, 0)
	if err != nil {
		return nil, fmt.Errorf("report contributor ranking: %w", err)
	}
	if len(contributors) > req.MaxContributors {
		d.ContributorsTruncated = true
		contributors = contributors[:req.MaxContributors]
	}
	d.Contributors = contributors

	repositories, err := store.RepositoryRanking(ctx, req.Filter)
	if err != nil {
		return nil, fmt.Errorf("report repository ranking: %w", err)
	}
	if len(repositories) > req.MaxRepositories {
		d.RepositoriesTruncated = true
		repositories = repositories[:req.MaxRepositories]
	}
	d.Repositories = repositories

	return d, nil
}

// Filename is the name the browser saves the download under.
func (d *Data) Filename() string {
	org := slug(d.Organization)
	if org == "" {
		org = "organization"
	}
	return fmt.Sprintf("devpulse-%s-%s-to-%s.pdf", org, d.Range.FromString(), d.Range.ToString())
}

// slug reduces an organization login to characters that are safe in a filename
// and in a Content-Disposition header.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-_")
}
