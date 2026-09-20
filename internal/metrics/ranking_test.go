package metrics

import (
	"regexp"
	"testing"
)

// acceptanceSortKeys is the list the product requires the contributor ranking to
// support. A regression here would silently drop a sort option from the UI.
var acceptanceSortKeys = []string{
	"commits", "additions", "deletions", "changed_files",
	"prs_opened", "prs_merged", "prs_closed",
	"reviews_submitted", "approvals", "changes_requested", "review_comments",
	"unique_prs_reviewed", "active_days", "repositories",
}

func TestEverySupportedSortKeyExists(t *testing.T) {
	for _, key := range acceptanceSortKeys {
		if !IsValidSort(key) {
			t.Errorf("sort key %q is not available", key)
		}
		if got := ResolveSort(key); got.Key != key {
			t.Errorf("ResolveSort(%q).Key = %q", key, got.Key)
		}
	}
	if len(RankingMetrics()) != len(acceptanceSortKeys) {
		t.Errorf("RankingMetrics() has %d metrics, want %d", len(RankingMetrics()), len(acceptanceSortKeys))
	}
}

func TestUnknownSortKeysFallBackToTheDefault(t *testing.T) {
	// The sort key is interpolated into an ORDER BY clause, so anything outside
	// the allow-list must be discarded rather than passed through.
	for _, key := range []string{
		"", "score", "performance",
		"commits; DROP TABLE commits",
		"1) --",
		"(SELECT 1)",
	} {
		got := ResolveSort(key)
		if got.Key != DefaultSort {
			t.Errorf("ResolveSort(%q) = %q, want the default %q", key, got.Key, DefaultSort)
		}
		if IsValidSort(key) {
			t.Errorf("IsValidSort(%q) must be false", key)
		}
	}
}

// safeExpr matches a bare SQL identifier.
var safeExpr = regexp.MustCompile(`^[a-z_]+$`)

func TestSortExpressionsAreBareIdentifiers(t *testing.T) {
	for _, m := range RankingMetrics() {
		if !safeExpr.MatchString(m.SortExpr) {
			t.Errorf("metric %q has a non-identifier SQL expression %q", m.Key, m.SortExpr)
		}
	}
}

func TestNoCombinedPerformanceScoreExists(t *testing.T) {
	// The product rule is explicit: activity is reported as separate metrics and
	// never reduced to a single "performance" number.
	for _, m := range RankingMetrics() {
		switch m.Key {
		case "score", "performance", "productivity", "rating":
			t.Errorf("metric %q must not exist: DevPulse reports no overall score", m.Key)
		}
	}
}

func TestMetricGroupsCoverEveryMetric(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range MetricGroups() {
		if len(g.Metrics) == 0 {
			t.Errorf("group %q is empty", g.Name)
		}
		for _, m := range g.Metrics {
			if seen[m.Key] {
				t.Errorf("metric %q appears in more than one group", m.Key)
			}
			seen[m.Key] = true
		}
	}
	for _, key := range acceptanceSortKeys {
		if !seen[key] {
			t.Errorf("metric %q is missing from the sort menu groups", key)
		}
	}
}

func TestEveryMetricIsDocumented(t *testing.T) {
	for _, m := range RankingMetrics() {
		if m.Label == "" || m.Description == "" {
			t.Errorf("metric %q needs a label and a description for the UI glossary", m.Key)
		}
	}
}
