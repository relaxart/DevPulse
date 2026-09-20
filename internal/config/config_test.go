package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"GITHUB_TOKEN": "github_pat_secret_value",
		"GITHUB_ORG":   "my-company",
		"DATABASE_URL": "postgres://devpulse:devpulse@postgres:5432/devpulse?sslmode=disable",
	}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(env(validEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != 30*time.Minute {
		t.Errorf("SyncInterval = %v, want 30m", cfg.SyncInterval)
	}
	if cfg.HistoryMonths != 12 {
		t.Errorf("HistoryMonths = %d, want 12", cfg.HistoryMonths)
	}
	if cfg.IncludeArchived {
		t.Error("IncludeArchived should default to false")
	}
	if cfg.GitHubOrg != "my-company" {
		t.Errorf("GitHubOrg = %q", cfg.GitHubOrg)
	}
}

func TestFromEnvRequiresEveryMandatoryVariable(t *testing.T) {
	for _, missing := range []string{"GITHUB_TOKEN", "GITHUB_ORG", "DATABASE_URL"} {
		t.Run(missing, func(t *testing.T) {
			m := validEnv()
			delete(m, missing)
			_, err := FromEnv(env(m))
			if err == nil {
				t.Fatalf("expected an error when %s is absent", missing)
			}
			if !errors.Is(err, ErrMissing) {
				t.Errorf("error should wrap ErrMissing, got %v", err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error should name %s, got %v", missing, err)
			}
		})
	}
}

func TestConfigErrorNeverContainsTheToken(t *testing.T) {
	m := validEnv()
	delete(m, "GITHUB_ORG")
	m["SYNC_INTERVAL"] = "not-a-duration"
	_, err := FromEnv(env(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "github_pat_secret_value") {
		t.Fatalf("configuration error leaked the token: %v", err)
	}
}

func TestRedactedHidesTheToken(t *testing.T) {
	cfg, err := FromEnv(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	red := cfg.Redacted()
	if red["github_token"] != "[redacted]" {
		t.Errorf("github_token = %v, want [redacted]", red["github_token"])
	}
	for k, v := range red {
		if s, ok := v.(string); ok && strings.Contains(s, "github_pat_secret_value") {
			t.Fatalf("field %s leaked the token", k)
		}
	}
}

func TestSingleOrganizationOnly(t *testing.T) {
	m := validEnv()
	m["GITHUB_ORG"] = "org-a,org-b"
	_, err := FromEnv(env(m))
	if err == nil {
		t.Fatal("a comma separated organization list must be rejected: DevPulse analyzes exactly one org")
	}
	if !strings.Contains(err.Error(), "single organization") {
		t.Errorf("error should explain the single-organization rule, got %v", err)
	}
}

func TestInvalidValuesAreReported(t *testing.T) {
	cases := map[string]map[string]string{
		"interval too short": {"SYNC_INTERVAL": "10s"},
		"bad interval":       {"SYNC_INTERVAL": "soon"},
		"bad history":        {"HISTORY_MONTHS": "0"},
		"huge history":       {"HISTORY_MONTHS": "500"},
		"bad bool":           {"INCLUDE_ARCHIVED": "sometimes"},
		"bad rate floor":     {"MIN_RATE_LIMIT_REMAINING": "-5"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			for k, v := range overrides {
				m[k] = v
			}
			if _, err := FromEnv(env(m)); err == nil {
				t.Fatalf("expected a validation error for %v", overrides)
			}
		})
	}
}

func TestParseExcludedUsers(t *testing.T) {
	got := ParseExcludedUsers(" dependabot[bot], Renovate[bot] ,, dependabot[bot] ")
	want := []string{"dependabot[bot]", "renovate[bot]"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(ParseExcludedUsers("")) != 0 {
		t.Error("an empty list must produce no exclusions")
	}
}

func TestHistoryStart(t *testing.T) {
	cfg, err := FromEnv(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	got := cfg.HistoryStart(now)
	want := time.Date(2025, 9, 20, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("HistoryStart = %s, want %s", got, want)
	}
}
