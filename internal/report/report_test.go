package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/metrics"
	"github.com/relaxart/dev-pulse/internal/models"
)

// newMeasuringPDF returns a document used only to measure text widths.
func newMeasuringPDF() *fpdf.Fpdf {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.AddPage()
	return pdf
}

// fakeStore records the filters it was asked for so the tests can assert that
// the report is scoped exactly like the dashboard.
type fakeStore struct {
	filters      []database.Filter
	sortKeys     []string
	contributors []models.ContributorStats
	repositories []models.RepositoryStats
	series       []models.TimePoint
	overview     models.OverviewStats
	err          error
}

func (f *fakeStore) Overview(ctx context.Context, filter database.Filter) (models.OverviewStats, error) {
	f.filters = append(f.filters, filter)
	return f.overview, f.err
}

func (f *fakeStore) TimeSeries(ctx context.Context, filter database.Filter, g metrics.Granularity, c, r *int64) ([]models.TimePoint, error) {
	f.filters = append(f.filters, filter)
	return f.series, f.err
}

func (f *fakeStore) ContributorRanking(ctx context.Context, filter database.Filter, sortKey string, limit, offset int) ([]models.ContributorStats, error) {
	f.filters = append(f.filters, filter)
	f.sortKeys = append(f.sortKeys, sortKey)
	if f.err != nil {
		return nil, f.err
	}
	if limit < len(f.contributors) {
		return f.contributors[:limit], nil
	}
	return f.contributors, nil
}

func (f *fakeStore) RepositoryRanking(ctx context.Context, filter database.Filter) ([]models.RepositoryStats, error) {
	f.filters = append(f.filters, filter)
	return f.repositories, f.err
}

func contributor(id int64, login, name string, commits, reviews int64) models.ContributorStats {
	return models.ContributorStats{
		Contributor:       models.Contributor{ID: id, Login: login, Name: name},
		Commits:           commits,
		Additions:         commits * 30,
		Deletions:         commits * 11,
		ChangedFiles:      commits * 3,
		PRsOpened:         commits / 4,
		PRsMerged:         commits / 5,
		PRsClosed:         1,
		ReviewsSubmitted:  reviews,
		Approvals:         reviews / 2,
		ChangesRequested:  reviews / 5,
		ReviewComments:    reviews * 2,
		UniquePRsReviewed: reviews - 1,
		ActiveDays:        12,
		Repositories:      2,
	}
}

func sampleStore() *fakeStore {
	last := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	return &fakeStore{
		overview: models.OverviewStats{
			ActiveContributors: 5, Commits: 555, Additions: 40000, Deletions: 12000,
			PRsOpened: 156, PRsMerged: 119, Reviews: 416, ActiveRepositories: 3,
		},
		series: []models.TimePoint{
			{Label: "2026-09-01", Commits: 12, PRsOpened: 3, PRsMerged: 2, ReviewsSubmitted: 7},
			{Label: "2026-09-02", Commits: 0, PRsOpened: 0, PRsMerged: 0, ReviewsSubmitted: 0},
			{Label: "2026-09-03", Commits: 31, PRsOpened: 9, PRsMerged: 8, ReviewsSubmitted: 22},
		},
		contributors: []models.ContributorStats{
			contributor(1, "alice", "Alice Nakamura", 143, 61),
			contributor(2, "bob", "Bob Ferreira", 91, 52),
			contributor(3, "carol", "", 78, 41),
		},
		repositories: []models.RepositoryStats{
			{Repository: models.Repository{ID: 1, Name: "api", PrimaryLang: "Go"},
				Contributors: 5, Commits: 240, Additions: 25400, Deletions: 8800,
				PRsOpened: 63, PRsMerged: 48, Reviews: 169, LastActivity: &last},
			{Repository: models.Repository{ID: 2, Name: "legacy", IsArchived: true, IsPrivate: true}},
		},
	}
}

func sampleRequest() Request {
	rng, _ := metrics.ResolveRange(metrics.Period3m, "", "", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	return Request{
		Organization: "acme",
		Range:        rng,
		SortKey:      "reviews_submitted",
		Filter: database.Filter{
			OrganizationID: 7,
			From:           rng.From,
			To:             rng.To,
			ExcludedLogins: []string{"dependabot[bot]"},
		},
	}
}

func TestCollectScopesEveryQueryToTheOrganizationAndPeriod(t *testing.T) {
	store := sampleStore()
	req := sampleRequest()

	data, err := Collect(context.Background(), store, req)
	if err != nil {
		t.Fatal(err)
	}

	if len(store.filters) == 0 {
		t.Fatal("no queries were issued")
	}
	current, previous := 0, 0
	for _, f := range store.filters {
		if f.OrganizationID != req.Filter.OrganizationID {
			t.Errorf("a report query used organization %d, want %d", f.OrganizationID, req.Filter.OrganizationID)
		}
		if len(f.ExcludedLogins) != 1 || f.ExcludedLogins[0] != "dependabot[bot]" {
			t.Errorf("a report query dropped the exclusion list: %v", f.ExcludedLogins)
		}
		switch {
		case f.From.Equal(req.Range.From) && f.To.Equal(req.Range.To):
			current++
		case f.From.Equal(data.Previous.From) && f.To.Equal(data.Previous.To):
			previous++
		default:
			t.Errorf("a report query used an unexpected window %s..%s", f.From, f.To)
		}
	}
	if previous != 1 {
		t.Errorf("expected exactly one comparison-period query, got %d", previous)
	}

	for _, k := range store.sortKeys {
		if k != "reviews_submitted" {
			t.Errorf("ranking sorted by %q, want reviews_submitted", k)
		}
	}
	if data.Sort.Key != "reviews_submitted" {
		t.Errorf("Sort = %q", data.Sort.Key)
	}
}

func TestCollectFallsBackToADefaultSortKey(t *testing.T) {
	req := sampleRequest()
	req.SortKey = "commits; DROP TABLE commits"
	data, err := Collect(context.Background(), sampleStore(), req)
	if err != nil {
		t.Fatal(err)
	}
	if data.Sort.Key != metrics.DefaultSort {
		t.Errorf("Sort = %q, want the default %q", data.Sort.Key, metrics.DefaultSort)
	}
}

func TestCollectMarksTruncatedTables(t *testing.T) {
	store := sampleStore()
	req := sampleRequest()
	req.MaxContributors = 2
	req.MaxRepositories = 1

	data, err := Collect(context.Background(), store, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Contributors) != 2 || !data.ContributorsTruncated {
		t.Errorf("contributors = %d truncated=%v, want 2/true", len(data.Contributors), data.ContributorsTruncated)
	}
	if len(data.Repositories) != 1 || !data.RepositoriesTruncated {
		t.Errorf("repositories = %d truncated=%v, want 1/true", len(data.Repositories), data.RepositoriesTruncated)
	}
}

func TestCollectPropagatesErrors(t *testing.T) {
	store := sampleStore()
	store.err = errors.New("database is down")
	if _, err := Collect(context.Background(), store, sampleRequest()); err == nil {
		t.Fatal("a database failure must not produce a silently empty report")
	}
}

func TestRenderProducesAValidPDF(t *testing.T) {
	data, err := Collect(context.Background(), sampleStore(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output does not start with a PDF header: %q", out[:min(16, len(out))])
	}
	if !bytes.Contains(out[max(0, len(out)-64):], []byte("%%EOF")) {
		t.Error("output is not terminated with an EOF marker")
	}
	if len(out) < 2000 {
		t.Errorf("report is only %d bytes; it looks empty", len(out))
	}
}

func TestRenderHandlesAnEmptyOrganization(t *testing.T) {
	store := &fakeStore{}
	data, err := Collect(context.Background(), store, sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(data)
	if err != nil {
		t.Fatalf("a report with no data must still render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Error("empty report is not a PDF")
	}
}

func TestRenderHandlesLargeAndHostileInput(t *testing.T) {
	store := sampleStore()
	store.contributors = nil
	for i := 0; i < 250; i++ {
		store.contributors = append(store.contributors,
			contributor(int64(i+1), fmt.Sprintf("user-%d", i),
				strings.Repeat("A very long display name ", 4), int64(300-i), int64(200-i)))
	}
	// Names GitHub can legitimately return: non-Latin scripts, emoji, newlines
	// and a very long language string must not break the renderer.
	store.contributors[0].Name = "Ana Beatriz Gonçalves 张三 🚀\nsecond line"
	store.repositories[0].Name = "repo-with-a-very-long-name-that-will-not-fit-in-its-column-at-all"
	store.repositories[0].PrimaryLang = "Objective-C++ / Something"
	store.series = nil
	for i := 0; i < 60; i++ {
		store.series = append(store.series, models.TimePoint{
			Label: fmt.Sprintf("2026-%02d-%02d", 1+i/28, 1+i%28), Commits: int64(i * 3), ReviewsSubmitted: int64(i),
		})
	}

	data, err := Collect(context.Background(), store, sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Error("output is not a PDF")
	}
	// A 100-row table plus the glossary must span several pages.
	if pages := bytes.Count(out, []byte("/Type /Page\n")); pages < 2 {
		t.Errorf("expected a multi-page report, found %d page objects", pages)
	}
}

func TestSanitizeKeepsTextPrintableForTheCoreFonts(t *testing.T) {
	cases := map[string]string{
		"Alice":         "Alice",
		"Ana Gonçalves": "Ana Gon\xe7alves",
		"张三":            "??",
		"emoji 🚀":       "emoji ?",
		"line\nbreak":   "line break",
		"smart ’quote’": "smart 'quote'",
		"dash — here":   "dash - here",
		"ellipsis…":     "ellipsis...",
		"null\x00byte":  "nullbyte",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	for _, b := range []byte(sanitize("张三 🚀 Ana Gonçalves")) {
		if b > 0xff {
			t.Fatal("sanitize produced a byte outside Latin-1")
		}
	}
}

func TestFilenameIsSafeAndDescriptive(t *testing.T) {
	data, err := Collect(context.Background(), sampleStore(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	name := data.Filename()
	if !strings.HasPrefix(name, "devpulse-acme-") || !strings.HasSuffix(name, ".pdf") {
		t.Errorf("Filename() = %q", name)
	}
	if !strings.Contains(name, data.Range.FromString()) || !strings.Contains(name, data.Range.ToString()) {
		t.Errorf("Filename() should carry the period: %q", name)
	}

	// A hostile organization login must not be able to break out of the
	// Content-Disposition header.
	hostile := sampleRequest()
	hostile.Organization = `ac"me; rm -rf /` + "\r\nX-Injected: 1"
	data, err = Collect(context.Background(), sampleStore(), hostile)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"`, ";", "\r", "\n", " ", "/"} {
		if strings.Contains(data.Filename(), bad) {
			t.Errorf("Filename() %q contains %q", data.Filename(), bad)
		}
	}
}

func TestReportRangesCoverTheAdvertisedPeriods(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wantDays := map[string]int{
		metrics.Period7d:  7,
		metrics.Period30d: 30,
		metrics.Period3m:  92,
		metrics.Period6m:  184,
		metrics.Period12m: 365,
	}
	for key, days := range wantDays {
		rng, err := metrics.ResolveRange(key, "", "", now)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if rng.Days() != days {
			t.Errorf("period %s spans %d days, want %d", key, rng.Days(), days)
		}

		req := sampleRequest()
		req.Range = rng
		req.Filter.From, req.Filter.To = rng.From, rng.To
		data, err := Collect(context.Background(), sampleStore(), req)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if _, err := Render(data); err != nil {
			t.Errorf("rendering the %s report failed: %v", key, err)
		}
	}
}

// A table whose columns are wider than the printable area silently loses its
// last columns off the edge of the page, which is invisible in a byte-level
// assertion, so the widths are checked directly.
func TestTableWidthsFitThePage(t *testing.T) {
	tables := map[string][]column{
		"contributors": contributorColumns(),
		"repositories": repositoryColumns(),
	}
	for name, cols := range tables {
		var total float64
		for _, c := range cols {
			if c.width <= 0 {
				t.Errorf("%s column %q has a non-positive width", name, c.title)
			}
			total += c.width
		}
		if total != contentW {
			t.Errorf("%s columns sum to %.1fmm, want exactly %.1fmm of printable width", name, total, contentW)
		}
	}
}

// Every header label must fit inside its own column, otherwise headers collide.
func TestTableHeadersFitTheirColumns(t *testing.T) {
	pdf := newMeasuringPDF()
	for name, cols := range map[string][]column{
		"contributors": contributorColumns(),
		"repositories": repositoryColumns(),
	} {
		pdf.SetFont(fontFamily, "B", 7)
		for _, c := range cols {
			if w := pdf.GetStringWidth(c.title); w > c.width-1 {
				t.Errorf("%s header %q needs %.1fmm but its column is %.1fmm", name, c.title, w, c.width)
			}
		}
	}
}

func TestDiffCountersRenderZeroPlainly(t *testing.T) {
	cases := []struct {
		v          int64
		plus, less string
	}{
		{0, "0", "0"},
		{1, "+1", "-1"},
		{12400, "+12,400", "-12,400"},
	}
	for _, c := range cases {
		if got := plus(c.v); got != c.plus {
			t.Errorf("plus(%d) = %q, want %q", c.v, got, c.plus)
		}
		if got := minus(c.v); got != c.less {
			t.Errorf("minus(%d) = %q, want %q", c.v, got, c.less)
		}
	}
}

func TestEmptyReportIsStillAValidDocument(t *testing.T) {
	rng, _ := metrics.ResolveRange(metrics.Period30d, "", "", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	data := Empty("acme", rng)

	if !data.NoData {
		t.Error("an unsynchronized report must be flagged, so it is not mistaken for a quiet period")
	}
	if data.Sort.Key != metrics.DefaultSort {
		t.Errorf("Sort = %q, want the default", data.Sort.Key)
	}
	if !strings.HasPrefix(data.Filename(), "devpulse-acme-") {
		t.Errorf("Filename() = %q", data.Filename())
	}

	out, err := Render(data)
	if err != nil {
		t.Fatalf("an empty report must still render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Error("the empty report is not a PDF")
	}
}
