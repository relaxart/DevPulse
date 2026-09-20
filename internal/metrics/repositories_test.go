package metrics

import (
	"regexp"
	"strings"
	"testing"
)

// repositoryTableColumns is the set of columns the repositories page must let a
// user sort by. A regression here silently removes a header link.
var repositoryTableColumns = []string{
	"name", "contributors", "commits", "additions",
	"prs_opened", "prs_merged", "reviews", "last_activity", "status",
}

func TestEverySortableRepositoryColumnExists(t *testing.T) {
	for _, key := range repositoryTableColumns {
		if !IsValidRepositorySort(key) {
			t.Errorf("repository column %q is not sortable", key)
		}
		col, _ := ResolveRepositorySort(key, "")
		if col.Key != key {
			t.Errorf("ResolveRepositorySort(%q).Key = %q", key, col.Key)
		}
	}
	if len(RepositoryColumns()) != len(repositoryTableColumns) {
		t.Errorf("RepositoryColumns() has %d entries, want %d",
			len(RepositoryColumns()), len(repositoryTableColumns))
	}
}

func TestRepositoryColumnsKeepTableOrder(t *testing.T) {
	cols := RepositoryColumns()
	for i, want := range repositoryTableColumns {
		if cols[i].Key != want {
			t.Fatalf("column %d is %q, want %q: the header order must match the table body",
				i, cols[i].Key, want)
		}
	}
}

func TestUnknownRepositorySortFallsBackToTheDefault(t *testing.T) {
	// The column and the direction are both interpolated into an ORDER BY
	// clause, so anything outside the allow-list must be discarded.
	for _, key := range []string{
		"", "score", "r.name; DROP TABLE repositories",
		"(SELECT 1)", "1) --",
	} {
		col, dir := ResolveRepositorySort(key, "desc")
		if col.Key != DefaultRepositorySort {
			t.Errorf("ResolveRepositorySort(%q) = %q, want the default %q", key, col.Key, DefaultRepositorySort)
		}
		if dir != SortDesc {
			t.Errorf("ResolveRepositorySort(%q) direction = %q", key, dir)
		}
		if IsValidRepositorySort(key) {
			t.Errorf("IsValidRepositorySort(%q) must be false", key)
		}
	}
}

func TestUnknownDirectionFallsBackToTheColumnDefault(t *testing.T) {
	for _, dir := range []string{"", "sideways", "ASC; DROP TABLE repositories", "descending"} {
		if _, got := ResolveRepositorySort("name", dir); got != SortAsc {
			t.Errorf("direction %q on name resolved to %q, want the column default asc", dir, got)
		}
		if _, got := ResolveRepositorySort("commits", dir); got != SortDesc {
			t.Errorf("direction %q on commits resolved to %q, want the column default desc", dir, got)
		}
	}
	// A valid direction is honoured on every column.
	for _, key := range repositoryTableColumns {
		if _, got := ResolveRepositorySort(key, SortAsc); got != SortAsc {
			t.Errorf("%s did not accept an explicit ascending sort", key)
		}
	}
}

func TestSQLDirectionOnlyEverEmitsTwoKeywords(t *testing.T) {
	if got := SQLDirection(SortAsc); got != "ASC" {
		t.Errorf("SQLDirection(asc) = %q", got)
	}
	for _, dir := range []string{SortDesc, "", "nonsense", "ASC --"} {
		if got := SQLDirection(dir); got != "DESC" && got != "ASC" {
			t.Errorf("SQLDirection(%q) = %q, which is not a bare SQL keyword", dir, got)
		}
	}
}

// safeOrderExpr allows the column references and COALESCE wrappers the table
// uses, and nothing that could carry an injection.
var safeOrderExpr = regexp.MustCompile(`^(lower\()?(r|stats)\.[a-z_]+\)?$|^COALESCE\((r|stats)\.[a-z_]+, 0\)$`)

func TestRepositorySortExpressionsAreFixedColumnReferences(t *testing.T) {
	for _, c := range RepositoryColumns() {
		if !safeOrderExpr.MatchString(c.SortExpr) {
			t.Errorf("column %q has an unexpected SQL expression %q", c.Key, c.SortExpr)
		}
		for _, bad := range []string{";", "--", "/*", "'"} {
			if strings.Contains(c.SortExpr, bad) {
				t.Errorf("column %q expression contains %q", c.Key, bad)
			}
		}
	}
}

func TestToggleDirection(t *testing.T) {
	commits, _ := ResolveRepositorySort("commits", "")
	name, _ := ResolveRepositorySort("name", "")

	// An inactive column starts at its own default.
	if got := commits.ToggleDirection("name", SortAsc); got != SortDesc {
		t.Errorf("inactive commits toggles to %q, want desc", got)
	}
	if got := name.ToggleDirection("commits", SortDesc); got != SortAsc {
		t.Errorf("inactive name toggles to %q, want asc", got)
	}
	// The active column flips.
	if got := commits.ToggleDirection("commits", SortDesc); got != SortAsc {
		t.Errorf("active commits desc toggles to %q, want asc", got)
	}
	if got := commits.ToggleDirection("commits", SortAsc); got != SortDesc {
		t.Errorf("active commits asc toggles to %q, want desc", got)
	}
}

func TestEveryRepositoryColumnIsLabelled(t *testing.T) {
	for _, c := range RepositoryColumns() {
		if c.Label == "" {
			t.Errorf("column %q has no header label", c.Key)
		}
		if c.DefaultDir != SortAsc && c.DefaultDir != SortDesc {
			t.Errorf("column %q has an invalid default direction %q", c.Key, c.DefaultDir)
		}
	}
}
