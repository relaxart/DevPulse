// Package metrics turns user-facing filter choices into validated date windows,
// documents what each metric means and defines the allowed sort keys.
package metrics

import (
	"fmt"
	"net/url"
	"time"
)

// Period keys accepted by every page via the `period` query parameter.
const (
	Period30d    = "30d"
	Period3m     = "3m"
	Period6m     = "6m"
	Period12m    = "12m"
	PeriodCustom = "custom"
)

// DefaultPeriod is used when no filter is supplied.
const DefaultPeriod = Period30d

// Granularity controls chart bucket width.
type Granularity string

// Supported chart granularities.
const (
	GranularityDay   Granularity = "day"
	GranularityWeek  Granularity = "week"
	GranularityMonth Granularity = "month"
)

// DateLayout is the layout used by custom range query parameters.
const DateLayout = "2006-01-02"

// maxCustomSpanDays caps custom ranges so a hand-typed URL cannot ask for a
// scan of an unbounded window.
const maxCustomSpanDays = 366 * 5

// Range is a resolved, inclusive [From, To] window of UTC calendar days.
type Range struct {
	Key   string
	Label string
	From  time.Time
	To    time.Time
}

// PeriodOption describes a selectable preset for the UI.
type PeriodOption struct {
	Key   string
	Label string
}

// PeriodOptions lists the presets shown in the date selector.
func PeriodOptions() []PeriodOption {
	return []PeriodOption{
		{Key: Period30d, Label: "Last 30 days"},
		{Key: Period3m, Label: "Last 3 months"},
		{Key: Period6m, Label: "Last 6 months"},
		{Key: Period12m, Label: "Last 12 months"},
		{Key: PeriodCustom, Label: "Custom range"},
	}
}

// ResolveRange converts the `period`, `from` and `to` query parameters into an
// inclusive UTC day range. Unknown period keys fall back to the default rather
// than failing, but a malformed custom range is reported so the UI can explain it.
func ResolveRange(period, from, to string, now time.Time) (Range, error) {
	today := truncateDay(now)

	if period == PeriodCustom || (period == "" && (from != "" || to != "")) {
		return resolveCustom(from, to, today)
	}

	switch period {
	case Period3m:
		return Range{Key: Period3m, Label: "Last 3 months", From: today.AddDate(0, -3, 0).AddDate(0, 0, 1), To: today}, nil
	case Period6m:
		return Range{Key: Period6m, Label: "Last 6 months", From: today.AddDate(0, -6, 0).AddDate(0, 0, 1), To: today}, nil
	case Period12m:
		return Range{Key: Period12m, Label: "Last 12 months", From: today.AddDate(0, -12, 0).AddDate(0, 0, 1), To: today}, nil
	case Period30d, "":
		return Range{Key: Period30d, Label: "Last 30 days", From: today.AddDate(0, 0, -29), To: today}, nil
	default:
		return Range{Key: Period30d, Label: "Last 30 days", From: today.AddDate(0, 0, -29), To: today}, nil
	}
}

func resolveCustom(from, to string, today time.Time) (Range, error) {
	fallback := Range{Key: Period30d, Label: "Last 30 days", From: today.AddDate(0, 0, -29), To: today}

	if from == "" || to == "" {
		return fallback, fmt.Errorf("a custom range needs both from and to (YYYY-MM-DD)")
	}
	f, err := time.ParseInLocation(DateLayout, from, time.UTC)
	if err != nil {
		return fallback, fmt.Errorf("from=%q is not a YYYY-MM-DD date", from)
	}
	t, err := time.ParseInLocation(DateLayout, to, time.UTC)
	if err != nil {
		return fallback, fmt.Errorf("to=%q is not a YYYY-MM-DD date", to)
	}
	if t.Before(f) {
		return fallback, fmt.Errorf("from (%s) must not be after to (%s)", from, to)
	}
	if t.Sub(f) > maxCustomSpanDays*24*time.Hour {
		return fallback, fmt.Errorf("custom range is longer than the %d day maximum", maxCustomSpanDays)
	}
	return Range{
		Key:   PeriodCustom,
		Label: fmt.Sprintf("%s to %s", f.Format(DateLayout), t.Format(DateLayout)),
		From:  f,
		To:    t,
	}, nil
}

// Days is the inclusive length of the range.
func (r Range) Days() int {
	return int(r.To.Sub(r.From).Hours()/24) + 1
}

// Granularity picks a sensible chart bucket width for the range length.
func (r Range) Granularity() Granularity {
	switch days := r.Days(); {
	case days <= 45:
		return GranularityDay
	case days <= 200:
		return GranularityWeek
	default:
		return GranularityMonth
	}
}

// FromString and ToString render the bounds for query parameters and inputs.
func (r Range) FromString() string { return r.From.Format(DateLayout) }

// ToString renders the upper bound.
func (r Range) ToString() string { return r.To.Format(DateLayout) }

// Query renders the filter as query parameters so links keep the selection.
func (r Range) Query() url.Values {
	v := url.Values{}
	if r.Key == PeriodCustom {
		v.Set("period", PeriodCustom)
		v.Set("from", r.FromString())
		v.Set("to", r.ToString())
		return v
	}
	v.Set("period", r.Key)
	return v
}

// QueryString renders the filter as a "?period=..." suffix for templates.
func (r Range) QueryString() string {
	return "?" + r.Query().Encode()
}

// PreviousRange returns the equally long window immediately before this one,
// which the dashboard uses for period-over-period deltas.
func (r Range) PreviousRange() Range {
	days := r.Days()
	to := r.From.AddDate(0, 0, -1)
	return Range{
		Key:   r.Key,
		Label: "previous " + r.Label,
		From:  to.AddDate(0, 0, -(days - 1)),
		To:    to,
	}
}

func truncateDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
