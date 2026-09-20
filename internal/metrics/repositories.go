package metrics

// Sort directions accepted by the repositories table.
const (
	SortAsc  = "asc"
	SortDesc = "desc"
)

// RepositoryColumn documents one sortable column of the repositories table.
type RepositoryColumn struct {
	Key   string
	Label string
	// Title explains a column whose label is too short to be obvious.
	Title string
	// Numeric columns are right-aligned in the table.
	Numeric bool
	// SortExpr is the SQL expression used to order by this column. It comes from
	// this fixed table only and is never built from user input.
	SortExpr string
	// DefaultDir is the direction applied the first time a column is chosen:
	// biggest-first for counts, A-Z for names.
	DefaultDir string
}

// repositoryColumns is the complete allow-list, in table order. The repositories
// template renders one header per entry, so this list also fixes the column
// order of the page.
var repositoryColumns = []RepositoryColumn{
	{Key: "name", Label: "Repository", SortExpr: "lower(r.name)", DefaultDir: SortAsc},
	{Key: "contributors", Label: "Contributors", Numeric: true,
		SortExpr: "COALESCE(stats.contributors, 0)", DefaultDir: SortDesc},
	{Key: "commits", Label: "Commits", Numeric: true,
		SortExpr: "COALESCE(stats.commits, 0)", DefaultDir: SortDesc},
	{Key: "additions", Label: "+/−", Title: "Sort by lines added", Numeric: true,
		SortExpr: "COALESCE(stats.additions, 0)", DefaultDir: SortDesc},
	{Key: "prs_opened", Label: "PRs opened", Numeric: true,
		SortExpr: "COALESCE(stats.prs_opened, 0)", DefaultDir: SortDesc},
	{Key: "prs_merged", Label: "PRs merged", Numeric: true,
		SortExpr: "COALESCE(stats.prs_merged, 0)", DefaultDir: SortDesc},
	{Key: "reviews", Label: "Reviews", Numeric: true,
		SortExpr: "COALESCE(stats.reviews_submitted, 0)", DefaultDir: SortDesc},
	{Key: "last_activity", Label: "Last activity", Numeric: true,
		SortExpr: "stats.last_activity", DefaultDir: SortDesc},
	{Key: "status", Label: "Status", Title: "Sort active repositories before archived ones",
		SortExpr: "r.is_archived", DefaultDir: SortAsc},
}

// DefaultRepositorySort is applied when no (or an unknown) column is requested.
const DefaultRepositorySort = "commits"

var repositoryColumnsByKey = func() map[string]RepositoryColumn {
	m := make(map[string]RepositoryColumn, len(repositoryColumns))
	for _, c := range repositoryColumns {
		m[c.Key] = c
	}
	return m
}()

// RepositoryColumns returns every sortable column in table order.
func RepositoryColumns() []RepositoryColumn {
	out := make([]RepositoryColumn, len(repositoryColumns))
	copy(out, repositoryColumns)
	return out
}

// IsValidRepositorySort reports whether key names a known column.
func IsValidRepositorySort(key string) bool {
	_, ok := repositoryColumnsByKey[key]
	return ok
}

// ResolveRepositorySort maps a requested column and direction onto the
// allow-list. Unknown values fall back to the default, so a hand-edited URL can
// never reach the SQL builder.
func ResolveRepositorySort(key, dir string) (RepositoryColumn, string) {
	col, ok := repositoryColumnsByKey[key]
	if !ok {
		col = repositoryColumnsByKey[DefaultRepositorySort]
		return col, col.DefaultDir
	}
	switch dir {
	case SortAsc, SortDesc:
		return col, dir
	default:
		return col, col.DefaultDir
	}
}

// SQLDirection converts a resolved direction into a SQL keyword. Only the two
// allow-listed values can reach it.
func SQLDirection(dir string) string {
	if dir == SortAsc {
		return "ASC"
	}
	return "DESC"
}

// ToggleDirection is the direction a header link should request: the opposite of
// the current one when the column is already sorted, its default otherwise.
func (c RepositoryColumn) ToggleDirection(activeKey, activeDir string) string {
	if c.Key != activeKey {
		return c.DefaultDir
	}
	if activeDir == SortAsc {
		return SortDesc
	}
	return SortAsc
}
