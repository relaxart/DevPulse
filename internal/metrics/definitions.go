package metrics

// Metric documents one observable contribution metric.
//
// DevPulse deliberately has no single "performance score": these metrics
// describe different kinds of contribution and are always presented and sorted
// independently.
type Metric struct {
	Key         string
	Label       string
	ShortLabel  string
	Group       string
	Description string
	// SortExpr is the SQL expression used when ranking by this metric. It comes
	// from this fixed table only and is never built from user input.
	SortExpr string
}

// Metric groups shown in the UI.
const (
	GroupCode     = "Code contribution"
	GroupPRs      = "Pull requests"
	GroupReviews  = "Review activity"
	GroupActivity = "Activity"
)

// rankingMetrics is the complete allow-list of sortable contributor metrics.
var rankingMetrics = []Metric{
	{Key: "commits", Label: "Commits", ShortLabel: "Commits", Group: GroupCode,
		Description: "Commits on the default branch attributed to the contributor's GitHub account during the selected period.",
		SortExpr:    "commits"},
	{Key: "additions", Label: "Additions", ShortLabel: "Additions", Group: GroupCode,
		Description: "Lines added, as reported by GitHub for those commits.",
		SortExpr:    "additions"},
	{Key: "deletions", Label: "Deletions", ShortLabel: "Deletions", Group: GroupCode,
		Description: "Lines deleted, as reported by GitHub for those commits.",
		SortExpr:    "deletions"},
	{Key: "changed_files", Label: "Changed files", ShortLabel: "Files", Group: GroupCode,
		Description: "Sum of files touched by those commits. GitHub does not report this for every commit; unavailable values count as zero.",
		SortExpr:    "changed_files"},

	{Key: "prs_opened", Label: "PRs opened", ShortLabel: "PRs opened", Group: GroupPRs,
		Description: "Pull requests authored by the contributor and created during the selected period.",
		SortExpr:    "prs_opened"},
	{Key: "prs_merged", Label: "PRs merged", ShortLabel: "PRs merged", Group: GroupPRs,
		Description: "Pull requests authored by the contributor that were merged during the selected period.",
		SortExpr:    "prs_merged"},
	{Key: "prs_closed", Label: "PRs closed", ShortLabel: "PRs closed", Group: GroupPRs,
		Description: "Pull requests authored by the contributor that were closed without being merged during the selected period.",
		SortExpr:    "prs_closed"},

	{Key: "reviews_submitted", Label: "Reviews submitted", ShortLabel: "Reviews", Group: GroupReviews,
		Description: "Review submissions made by the contributor during the selected period, including approvals, change requests and comment-only reviews.",
		SortExpr:    "reviews_submitted"},
	{Key: "approvals", Label: "Approvals", ShortLabel: "Approvals", Group: GroupReviews,
		Description: "Review submissions whose state was APPROVED.",
		SortExpr:    "approvals"},
	{Key: "changes_requested", Label: "Changes requested", ShortLabel: "Changes req.", Group: GroupReviews,
		Description: "Review submissions whose state was CHANGES_REQUESTED.",
		SortExpr:    "changes_requested"},
	{Key: "review_comments", Label: "Review comments", ShortLabel: "Review comments", Group: GroupReviews,
		Description: "Inline comments attached to those review submissions.",
		SortExpr:    "review_comments"},
	{Key: "unique_prs_reviewed", Label: "Unique PRs reviewed", ShortLabel: "PRs reviewed", Group: GroupReviews,
		Description: "Distinct pull requests the contributor reviewed at least once during the period. Reviewing the same pull request twice counts once.",
		SortExpr:    "unique_prs_reviewed"},

	{Key: "active_days", Label: "Active days", ShortLabel: "Active days", Group: GroupActivity,
		Description: "Distinct UTC calendar days with any tracked activity (commit, pull request or review).",
		SortExpr:    "active_days"},
	{Key: "repositories", Label: "Repositories", ShortLabel: "Repos", Group: GroupActivity,
		Description: "Distinct repositories of the configured organization the contributor was active in.",
		SortExpr:    "repositories"},
}

// DefaultSort is applied when no (or an unknown) sort key is requested.
const DefaultSort = "commits"

var metricsByKey = func() map[string]Metric {
	m := make(map[string]Metric, len(rankingMetrics))
	for _, metric := range rankingMetrics {
		m[metric.Key] = metric
	}
	return m
}()

// RankingMetrics returns every sortable metric in display order.
func RankingMetrics() []Metric {
	out := make([]Metric, len(rankingMetrics))
	copy(out, rankingMetrics)
	return out
}

// MetricGroups returns the sortable metrics grouped for the sort menu.
func MetricGroups() []struct {
	Name    string
	Metrics []Metric
} {
	order := []string{GroupCode, GroupPRs, GroupReviews, GroupActivity}
	out := make([]struct {
		Name    string
		Metrics []Metric
	}, 0, len(order))
	for _, g := range order {
		var group []Metric
		for _, m := range rankingMetrics {
			if m.Group == g {
				group = append(group, m)
			}
		}
		out = append(out, struct {
			Name    string
			Metrics []Metric
		}{Name: g, Metrics: group})
	}
	return out
}

// ResolveSort maps a requested sort key onto the allow-list. Unknown keys fall
// back to the default, so a hand-edited URL can never reach the SQL builder.
func ResolveSort(key string) Metric {
	if m, ok := metricsByKey[key]; ok {
		return m
	}
	return metricsByKey[DefaultSort]
}

// IsValidSort reports whether key names a known metric.
func IsValidSort(key string) bool {
	_, ok := metricsByKey[key]
	return ok
}
