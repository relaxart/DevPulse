package metrics

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 20, 13, 45, 0, 0, time.UTC)

func TestResolveRangePresets(t *testing.T) {
	cases := []struct {
		period   string
		from, to string
		days     int
	}{
		{Period30d, "2026-08-22", "2026-09-20", 30},
		{Period3m, "2026-06-21", "2026-09-20", 92},
		{Period6m, "2026-03-21", "2026-09-20", 184},
		{Period12m, "2025-09-21", "2026-09-20", 365},
		{"", "2026-08-22", "2026-09-20", 30},
		{"nonsense", "2026-08-22", "2026-09-20", 30},
	}
	for _, c := range cases {
		t.Run(c.period, func(t *testing.T) {
			r, err := ResolveRange(c.period, "", "", now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.FromString() != c.from || r.ToString() != c.to {
				t.Errorf("range = %s..%s, want %s..%s", r.FromString(), r.ToString(), c.from, c.to)
			}
			if r.Days() != c.days {
				t.Errorf("Days() = %d, want %d", r.Days(), c.days)
			}
		})
	}
}

func TestResolveRangeCustom(t *testing.T) {
	r, err := ResolveRange(PeriodCustom, "2026-01-01", "2026-03-31", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Key != PeriodCustom {
		t.Errorf("Key = %q, want custom", r.Key)
	}
	if r.FromString() != "2026-01-01" || r.ToString() != "2026-03-31" {
		t.Errorf("range = %s..%s", r.FromString(), r.ToString())
	}
	if r.Days() != 90 {
		t.Errorf("Days() = %d, want 90", r.Days())
	}
}

func TestCustomRangeWithoutPeriodKey(t *testing.T) {
	// /contributors?from=...&to=... must behave like a custom range.
	r, err := ResolveRange("", "2026-01-01", "2026-01-31", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Key != PeriodCustom || r.Days() != 31 {
		t.Errorf("got %s %s..%s", r.Key, r.FromString(), r.ToString())
	}
}

func TestInvalidCustomRangeFallsBackSafely(t *testing.T) {
	cases := []struct{ name, from, to string }{
		{"reversed", "2026-03-31", "2026-01-01"},
		{"malformed from", "31/03/2026", "2026-04-01"},
		{"malformed to", "2026-03-31", "yesterday"},
		{"missing to", "2026-03-31", ""},
		{"too long", "2000-01-01", "2026-01-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := ResolveRange(PeriodCustom, c.from, c.to, now)
			if err == nil {
				t.Fatal("expected an explanatory error")
			}
			if r.Key != Period30d {
				t.Errorf("fallback should be the 30 day default, got %q", r.Key)
			}
		})
	}
}

func TestGranularity(t *testing.T) {
	cases := map[string]Granularity{
		Period30d: GranularityDay,
		Period3m:  GranularityWeek,
		Period6m:  GranularityWeek,
		Period12m: GranularityMonth,
	}
	for period, want := range cases {
		r, _ := ResolveRange(period, "", "", now)
		if got := r.Granularity(); got != want {
			t.Errorf("%s granularity = %s, want %s", period, got, want)
		}
	}
}

func TestQueryStringRoundTrip(t *testing.T) {
	r, _ := ResolveRange(Period6m, "", "", now)
	if got := r.QueryString(); got != "?period=6m" {
		t.Errorf("QueryString() = %q", got)
	}

	custom, _ := ResolveRange(PeriodCustom, "2026-01-01", "2026-03-31", now)
	q := custom.Query()
	if q.Get("period") != "custom" || q.Get("from") != "2026-01-01" || q.Get("to") != "2026-03-31" {
		t.Errorf("custom query = %v", q)
	}

	// A link built from the query must resolve back to the same window.
	again, err := ResolveRange(q.Get("period"), q.Get("from"), q.Get("to"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !again.From.Equal(custom.From) || !again.To.Equal(custom.To) {
		t.Errorf("round trip changed the range: %v vs %v", again, custom)
	}
}

func TestPreviousRangeIsAdjacentAndEquallyLong(t *testing.T) {
	r, _ := ResolveRange(Period30d, "", "", now)
	p := r.PreviousRange()
	if p.Days() != r.Days() {
		t.Errorf("previous range spans %d days, want %d", p.Days(), r.Days())
	}
	if !p.To.AddDate(0, 0, 1).Equal(r.From) {
		t.Errorf("previous range must end the day before the current one: %s vs %s", p.ToString(), r.FromString())
	}
}
