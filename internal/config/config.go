// Package config loads and validates DevPulse runtime configuration from the
// environment. The GitHub token is held in memory only and is never logged,
// rendered or persisted.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated application configuration.
type Config struct {
	// GitHubToken is the Personal Access Token. Never log or render this.
	GitHubToken string
	// GitHubOrg is the one and only organization DevPulse is allowed to query.
	GitHubOrg string

	DatabaseURL string

	SyncInterval  time.Duration
	HistoryMonths int

	IncludeArchived bool
	// ExcludedUsers holds lower-cased logins excluded from rankings.
	ExcludedUsers []string

	ListenAddr      string
	GitHubAPIURL    string
	SyncOnStartup   bool
	MinRateLimit    int
	LogLevel        string
	RequestTimeout  time.Duration
	MaxRepositories int
}

// ErrMissing is returned (wrapped) when a required variable is absent.
var ErrMissing = errors.New("missing required configuration")

const (
	defaultSyncInterval   = 30 * time.Minute
	defaultHistoryMonths  = 12
	defaultListenAddr     = ":8080"
	defaultGitHubAPIURL   = "https://api.github.com/graphql"
	defaultMinRateLimit   = 200
	defaultRequestTimeout = 60 * time.Second
)

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	return FromEnv(os.Getenv)
}

// FromEnv builds a Config from an arbitrary lookup function, which keeps the
// validation rules testable without mutating the real process environment.
func FromEnv(get func(string) string) (*Config, error) {
	cfg := &Config{
		GitHubToken:     strings.TrimSpace(get("GITHUB_TOKEN")),
		GitHubOrg:       strings.TrimSpace(get("GITHUB_ORG")),
		DatabaseURL:     strings.TrimSpace(get("DATABASE_URL")),
		SyncInterval:    defaultSyncInterval,
		HistoryMonths:   defaultHistoryMonths,
		ListenAddr:      defaultListenAddr,
		GitHubAPIURL:    defaultGitHubAPIURL,
		MinRateLimit:    defaultMinRateLimit,
		RequestTimeout:  defaultRequestTimeout,
		SyncOnStartup:   true,
		LogLevel:        "info",
		IncludeArchived: false,
	}

	var errs []string
	if cfg.GitHubToken == "" {
		errs = append(errs, "GITHUB_TOKEN must be set (create a read-only Personal Access Token)")
	}
	if cfg.GitHubOrg == "" {
		errs = append(errs, "GITHUB_ORG must be set to exactly one organization login")
	} else if strings.ContainsAny(cfg.GitHubOrg, ", \t") {
		errs = append(errs, "GITHUB_ORG must name a single organization, not a list")
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, "DATABASE_URL must be set (postgres://user:pass@host:5432/db?sslmode=disable)")
	}

	if v := strings.TrimSpace(get("SYNC_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("SYNC_INTERVAL is not a duration (%q): use forms like 30m or 2h", v))
		case d < time.Minute:
			errs = append(errs, "SYNC_INTERVAL must be at least 1m to stay inside GitHub rate limits")
		default:
			cfg.SyncInterval = d
		}
	}

	if v := strings.TrimSpace(get("HISTORY_MONTHS")); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("HISTORY_MONTHS is not an integer (%q)", v))
		case n < 1 || n > 120:
			errs = append(errs, "HISTORY_MONTHS must be between 1 and 120")
		default:
			cfg.HistoryMonths = n
		}
	}

	if v := strings.TrimSpace(get("INCLUDE_ARCHIVED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("INCLUDE_ARCHIVED is not a boolean (%q)", v))
		} else {
			cfg.IncludeArchived = b
		}
	}

	cfg.ExcludedUsers = ParseExcludedUsers(get("EXCLUDED_USERS"))

	if v := strings.TrimSpace(get("LISTEN_ADDR")); v != "" {
		cfg.ListenAddr = v
	}
	if v := strings.TrimSpace(get("GITHUB_API_URL")); v != "" {
		cfg.GitHubAPIURL = v
	}
	if v := strings.TrimSpace(get("LOG_LEVEL")); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := strings.TrimSpace(get("SYNC_ON_STARTUP")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("SYNC_ON_STARTUP is not a boolean (%q)", v))
		} else {
			cfg.SyncOnStartup = b
		}
	}
	if v := strings.TrimSpace(get("MIN_RATE_LIMIT_REMAINING")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Sprintf("MIN_RATE_LIMIT_REMAINING must be a non-negative integer (%q)", v))
		} else {
			cfg.MinRateLimit = n
		}
	}
	if v := strings.TrimSpace(get("MAX_REPOSITORIES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Sprintf("MAX_REPOSITORIES must be a non-negative integer (%q)", v))
		} else {
			cfg.MaxRepositories = n
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%w:\n  - %s", ErrMissing, strings.Join(errs, "\n  - "))
	}
	return cfg, nil
}

// ParseExcludedUsers normalises a comma separated exclusion list.
func ParseExcludedUsers(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		login := strings.ToLower(strings.TrimSpace(p))
		if login == "" {
			continue
		}
		if _, dup := seen[login]; dup {
			continue
		}
		seen[login] = struct{}{}
		out = append(out, login)
	}
	return out
}

// HistoryStart is the earliest instant the initial synchronization reaches back to.
func (c *Config) HistoryStart(now time.Time) time.Time {
	return now.UTC().AddDate(0, -c.HistoryMonths, 0)
}

// Redacted renders the configuration for logging with the token removed.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"github_org":       c.GitHubOrg,
		"github_token":     "[redacted]",
		"sync_interval":    c.SyncInterval.String(),
		"history_months":   c.HistoryMonths,
		"include_archived": c.IncludeArchived,
		"excluded_users":   c.ExcludedUsers,
		"listen_addr":      c.ListenAddr,
		"min_rate_limit":   c.MinRateLimit,
	}
}
