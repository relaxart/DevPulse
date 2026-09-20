// Package github is a focused GitHub GraphQL client. It knows nothing about
// PostgreSQL and nothing about organizations other than the one it is asked
// about: every exported call takes an explicit organization login.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultEndpoint is the public GitHub GraphQL endpoint.
const DefaultEndpoint = "https://api.github.com/graphql"

// RateLimit is the point-based GraphQL budget reported alongside every query.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
	NodeCount int       `json:"nodeCount"`
}

// ErrRateLimitLow is returned when the remaining point budget dropped below the
// configured floor. Callers must stop synchronizing rather than retry in a loop.
var ErrRateLimitLow = errors.New("github graphql rate limit too low to continue")

// ErrNotFound indicates the organization or repository is not visible to the token.
var ErrNotFound = errors.New("github resource not found or not visible to this token")

// Client performs GraphQL requests against GitHub.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	log      *slog.Logger

	// minRemaining is the point floor below which requests are refused.
	minRemaining int

	mu        sync.Mutex
	last      RateLimit
	totalCost int
	calls     int
}

// Options configures a Client.
type Options struct {
	Endpoint     string
	Token        string
	HTTPClient   *http.Client
	Logger       *slog.Logger
	MinRemaining int
	Timeout      time.Duration
}

// New builds a Client. The token is stored in memory only; it is never logged.
func New(opts Options) *Client {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	hc := opts.HTTPClient
	if hc == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		endpoint:     endpoint,
		token:        opts.Token,
		http:         hc,
		log:          logger,
		minRemaining: opts.MinRemaining,
	}
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

func (e graphQLError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return e.Message
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// rateLimitProbe pulls the rateLimit block out of any response shape.
type rateLimitProbe struct {
	RateLimit *RateLimit `json:"rateLimit"`
}

// Stats reports client-side accounting for the status page and logs.
func (c *Client) Stats() (last RateLimit, totalCost, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.totalCost, c.calls
}

// RateLimit returns the most recent rate-limit snapshot.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// ResetCost clears the per-run cost accumulator.
func (c *Client) ResetCost() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalCost = 0
	c.calls = 0
}

// CheckBudget reports ErrRateLimitLow when the last observed budget is below the floor.
func (c *Client) CheckBudget() error {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last.Limit == 0 || c.minRemaining <= 0 {
		return nil
	}
	if last.Remaining < c.minRemaining {
		return fmt.Errorf("%w: remaining=%d floor=%d reset_at=%s",
			ErrRateLimitLow, last.Remaining, c.minRemaining, last.ResetAt.UTC().Format(time.RFC3339))
	}
	return nil
}

const maxAttempts = 4

// do executes one GraphQL operation and decodes data into out.
func (c *Client) do(ctx context.Context, op, query string, vars map[string]any, out any) error {
	if err := c.CheckBudget(); err != nil {
		return err
	}

	body, err := json.Marshal(graphQLRequest{Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("encode graphql request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			c.log.Warn("retrying github graphql operation",
				"operation", op, "attempt", attempt, "delay", delay.String(), "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		retryable, err := c.attempt(ctx, op, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("github graphql operation %s failed after %d attempts: %w", op, maxAttempts, lastErr)
}

func (c *Client) attempt(ctx context.Context, op string, body []byte, out any) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	// Authorization is set here and deliberately never logged or echoed.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "devpulse/1.0")

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("graphql transport: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return true, fmt.Errorf("read graphql response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// 403 is also used for secondary rate limits; distinguish by body text.
		if strings.Contains(strings.ToLower(string(raw)), "rate limit") {
			return true, fmt.Errorf("github secondary rate limit (status %d)", resp.StatusCode)
		}
		return false, fmt.Errorf("github rejected the token (status %d): check PAT scopes and organization SSO authorization", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, fmt.Errorf("github rate limited the request (status 429)")
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("github server error (status %d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("unexpected github status %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var envelope graphQLResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return true, fmt.Errorf("decode graphql envelope: %w", err)
	}

	rl := c.recordRateLimit(envelope.Data)
	c.log.Debug("github graphql call",
		"operation", op,
		"duration_ms", time.Since(start).Milliseconds(),
		"cost", rl.Cost,
		"remaining", rl.Remaining,
		"limit", rl.Limit,
		"reset_at", rl.ResetAt.UTC().Format(time.RFC3339),
	)

	if len(envelope.Errors) > 0 {
		if gqlErr, fatal := classifyErrors(envelope.Errors); fatal {
			return false, gqlErr
		} else if gqlErr != nil {
			return true, gqlErr
		}
	}
	if len(envelope.Data) == 0 {
		return true, errors.New("graphql response contained no data")
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return false, fmt.Errorf("decode graphql data for %s: %w", op, err)
		}
	}
	return false, nil
}

func classifyErrors(errs []graphQLError) (error, bool) {
	msgs := make([]string, 0, len(errs))
	fatal := true
	notFound := false
	for _, e := range errs {
		msgs = append(msgs, e.Error())
		switch e.Type {
		case "NOT_FOUND":
			notFound = true
		case "RATE_LIMITED":
			fatal = false
		case "INTERNAL", "SERVICE_UNAVAILABLE":
			fatal = false
		}
	}
	joined := strings.Join(msgs, "; ")
	if notFound {
		return fmt.Errorf("%w: %s", ErrNotFound, joined), true
	}
	return fmt.Errorf("graphql errors: %s", joined), fatal
}

func (c *Client) recordRateLimit(data json.RawMessage) RateLimit {
	var probe rateLimitProbe
	if len(data) > 0 {
		_ = json.Unmarshal(data, &probe)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if probe.RateLimit != nil {
		c.last = *probe.RateLimit
		c.totalCost += probe.RateLimit.Cost
	}
	return c.last
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
