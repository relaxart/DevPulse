package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "github_pat_11ABCDEF_supersecrettokenvalue"

type recordedRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// newTestServer returns a GraphQL stub plus the requests it received.
func newTestServer(t *testing.T, handler func(w http.ResponseWriter, req recordedRequest, n int)) (*httptest.Server, *[]recordedRequest, *[]http.Header) {
	t.Helper()
	var reqs []recordedRequest
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rec recordedRequest
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Errorf("request body is not GraphQL JSON: %v", err)
		}
		reqs = append(reqs, rec)
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		handler(w, rec, len(reqs))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &headers
}

func rateLimitJSON(remaining int) string {
	return `"rateLimit":{"limit":5000,"cost":1,"remaining":` + strconv.Itoa(remaining) +
		`,"resetAt":"2026-09-20T12:00:00Z","nodeCount":10}`
}

func newTestClient(endpoint string, minRemaining int, logger *slog.Logger) *Client {
	return New(Options{
		Endpoint:     endpoint,
		Token:        testToken,
		Logger:       logger,
		MinRemaining: minRemaining,
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	})
}

func TestFetchOrganizationQueriesOnlyTheConfiguredLogin(t *testing.T) {
	srv, reqs, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"organization":{"id":"O_1","databaseId":1,"login":"my-company","name":"My Company"},`+rateLimitJSON(4999)+`}}`)
	})
	c := newTestClient(srv.URL, 100, slog.New(slog.NewTextHandler(io.Discard, nil)))

	org, err := c.FetchOrganization(context.Background(), "my-company")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if org.Login != "my-company" {
		t.Errorf("login = %q", org.Login)
	}
	if len(*reqs) != 1 {
		t.Fatalf("expected exactly one GraphQL call, got %d", len(*reqs))
	}
	if got := (*reqs)[0].Variables["login"]; got != "my-company" {
		t.Errorf("login variable = %v, want my-company", got)
	}
	// Nothing in the query may enumerate other organizations.
	for _, forbidden := range []string{"viewer", "organizations(", "search("} {
		if strings.Contains((*reqs)[0].Query, forbidden) {
			t.Errorf("organization query must not contain %q", forbidden)
		}
	}
}

func TestAuthorizationHeaderIsSentButNeverLogged(t *testing.T) {
	srv, _, headers := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"organization":{"id":"O_1","login":"acme"},`+rateLimitJSON(4000)+`}}`)
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newTestClient(srv.URL, 100, logger)

	if _, err := c.FetchOrganization(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if got := (*headers)[0].Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization header = %q", got)
	}
	out := logs.String()
	if out == "" {
		t.Fatal("expected debug logging of the GraphQL call")
	}
	if strings.Contains(out, testToken) || strings.Contains(strings.ToLower(out), "authorization") {
		t.Fatalf("logs leaked credentials: %s", out)
	}
	if !strings.Contains(out, "cost=1") || !strings.Contains(out, "remaining=4000") {
		t.Errorf("rate-limit fields must be logged, got: %s", out)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	srv, _, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"Bad credentials"}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := c.FetchOrganization(context.Background(), "acme")
	if err == nil {
		t.Fatal("expected an authentication error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked the token: %v", err)
	}
}

func TestRateLimitTrackingAndBudgetFloor(t *testing.T) {
	srv, _, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"organization":{"id":"O_1","login":"acme"},`+rateLimitJSON(50)+`}}`)
	})
	c := newTestClient(srv.URL, 200, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := c.CheckBudget(); err != nil {
		t.Fatalf("budget must be unknown (and therefore fine) before the first call: %v", err)
	}
	if _, err := c.FetchOrganization(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}

	rl := c.RateLimit()
	if rl.Limit != 5000 || rl.Remaining != 50 || rl.Cost != 1 {
		t.Errorf("rate limit snapshot = %+v", rl)
	}
	if rl.ResetAt.IsZero() {
		t.Error("resetAt must be recorded")
	}

	err := c.CheckBudget()
	if !errors.Is(err, ErrRateLimitLow) {
		t.Fatalf("CheckBudget() = %v, want ErrRateLimitLow", err)
	}
	// A depleted budget must also block the next request instead of looping.
	if _, err := c.FetchOrganization(context.Background(), "acme"); !errors.Is(err, ErrRateLimitLow) {
		t.Errorf("further calls must be refused, got %v", err)
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	srv, reqs, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"organization":null,`+rateLimitJSON(4000)+`},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to an Organization with the login of 'nope'."}]}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := c.FetchOrganization(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("a NOT_FOUND must not be retried, got %d calls", len(*reqs))
	}
}

func TestTransientServerErrorsAreRetried(t *testing.T) {
	srv, reqs, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "upstream error")
			return
		}
		io.WriteString(w, `{"data":{"organization":{"id":"O_1","login":"acme"},`+rateLimitJSON(4000)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := c.FetchOrganization(context.Background(), "acme"); err != nil {
		t.Fatalf("a 502 should be retried, got %v", err)
	}
	if len(*reqs) != 2 {
		t.Errorf("expected 2 attempts, got %d", len(*reqs))
	}
}

func TestRepositoryPaginationUsesFirstAndAfter(t *testing.T) {
	srv, reqs, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		if n == 1 {
			io.WriteString(w, `{"data":{"organization":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"CUR1"},
			"nodes":[{"id":"R_1","name":"api","nameWithOwner":"acme/api","owner":{"login":"acme"}}]}},`+rateLimitJSON(4000)+`}}`)
			return
		}
		io.WriteString(w, `{"data":{"organization":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":"CUR2"},
		"nodes":[{"id":"R_2","name":"web","nameWithOwner":"acme/web","owner":{"login":"acme"},"isArchived":true}]}},`+rateLimitJSON(3999)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	first, err := c.FetchRepositories(context.Background(), "acme", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if !first.PageInfo.HasNextPage || first.PageInfo.EndCursor != "CUR1" {
		t.Fatalf("page info = %+v", first.PageInfo)
	}
	if (*reqs)[0].Variables["after"] != nil {
		t.Errorf("the first page must not send an `after` cursor, got %v", (*reqs)[0].Variables["after"])
	}
	if (*reqs)[0].Variables["first"] != float64(50) {
		t.Errorf("first = %v, want 50", (*reqs)[0].Variables["first"])
	}

	second, err := c.FetchRepositories(context.Background(), "acme", 50, first.PageInfo.EndCursor)
	if err != nil {
		t.Fatal(err)
	}
	if (*reqs)[1].Variables["after"] != "CUR1" {
		t.Errorf("after = %v, want CUR1", (*reqs)[1].Variables["after"])
	}
	if second.PageInfo.HasNextPage {
		t.Error("the last page must report hasNextPage=false")
	}
	if !second.Nodes[0].IsArchived {
		t.Error("archived flag must be decoded")
	}
}

func TestCommitsQuerySendsSinceAndDecodesMissingAuthors(t *testing.T) {
	srv, reqs, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"repository":{"defaultBranchRef":{"target":{"history":{
		"pageInfo":{"hasNextPage":false,"endCursor":""},
		"nodes":[
		  {"oid":"abc","committedDate":"2026-09-01T10:00:00Z","additions":10,"deletions":2,"changedFilesIfAvailable":3,
		   "author":{"name":"Alice","email":"alice@example.com","user":{"__typename":"User","id":"U_1","login":"alice"}}},
		  {"oid":"def","committedDate":"2026-09-02T10:00:00Z","additions":1,"deletions":0,"changedFilesIfAvailable":null,
		   "author":{"name":"Deleted","email":"gone@example.com","user":null}}
		]}}}},`+rateLimitJSON(3900)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	page, err := c.FetchCommits(context.Background(), "acme", "api", since, nil, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := (*reqs)[0].Variables["since"]; got != "2026-08-01T00:00:00Z" {
		t.Errorf("since = %v", got)
	}
	if len(page.Nodes) != 2 {
		t.Fatalf("got %d commits", len(page.Nodes))
	}
	if page.Nodes[0].ChangedFiles() != 3 {
		t.Errorf("changed files = %d, want 3", page.Nodes[0].ChangedFiles())
	}
	if page.Nodes[1].ChangedFiles() != 0 {
		t.Errorf("a null changedFilesIfAvailable must decode to 0, got %d", page.Nodes[1].ChangedFiles())
	}
	if page.Nodes[1].Author.User != nil {
		t.Error("an unlinked commit author must decode to a nil user rather than failing")
	}
}

func TestEmptyRepositoryYieldsNoCommits(t *testing.T) {
	srv, _, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"repository":{"defaultBranchRef":null},`+rateLimitJSON(3800)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	page, err := c.FetchCommits(context.Background(), "acme", "empty", time.Now().Add(-time.Hour), nil, 100, "")
	if err != nil {
		t.Fatalf("a repository without a default branch is not an error: %v", err)
	}
	if len(page.Nodes) != 0 || page.PageInfo.HasNextPage {
		t.Errorf("expected an empty page, got %+v", page)
	}
}

func TestActorBotDetection(t *testing.T) {
	cases := []struct {
		actor Actor
		want  bool
	}{
		{Actor{TypeName: "User", Login: "alice"}, false},
		{Actor{TypeName: "Bot", Login: "dependabot"}, true},
		{Actor{TypeName: "User", Login: "renovate[bot]"}, true},
		{Actor{TypeName: "User", Login: "Dependabot[bot]"}, true},
		{Actor{TypeName: "Mannequin", Login: "imported-user"}, true},
		{Actor{TypeName: "User", Login: "github-actions"}, true},
	}
	for _, c := range cases {
		if got := c.actor.IsBot(); got != c.want {
			t.Errorf("Actor{%s,%s}.IsBot() = %v, want %v", c.actor.TypeName, c.actor.Login, got, c.want)
		}
	}
	var nilActor *Actor
	if nilActor.IsBot() {
		t.Error("a nil actor must not be reported as a bot")
	}
}

func TestStatsAccumulateCost(t *testing.T) {
	srv, _, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"organization":{"id":"O_1","login":"acme"},`+rateLimitJSON(4000-n)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.FetchOrganization(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	_, cost, calls := c.Stats()
	if calls != 3 || cost != 3 {
		t.Errorf("calls=%d cost=%d, want 3/3", calls, cost)
	}
	c.ResetCost()
	if _, cost, calls = c.Stats(); cost != 0 || calls != 0 {
		t.Errorf("ResetCost did not clear the accumulators: cost=%d calls=%d", cost, calls)
	}
}

// authorSelectionBlock extracts the `author { ... }` selection set from a query.
func authorSelectionBlock(query string) (string, bool) {
	start := strings.Index(query, "author {")
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(query); i++ {
		switch query[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return query[start : i+1], true
			}
		}
	}
	return "", false
}

// GitHub types a pull request and review author as the `Actor` interface, which
// exposes only login, avatarUrl, url and resourcePath. Selecting `id` or `name`
// directly on it makes GitHub reject the whole query with
// "Field 'id' doesn't exist on type 'Actor'", which previously failed every
// pull request and review sync.
func TestActorAuthorsAreSelectedThroughInlineFragments(t *testing.T) {
	queries := map[string]string{
		"pullRequests":       pullRequestsQuery,
		"repositoryReviews":  repoReviewsQuery,
		"pullRequestReviews": prReviewsQuery,
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			block, ok := authorSelectionBlock(query)
			if !ok {
				t.Fatalf("%s selects no author", name)
			}
			// Everything before the first inline fragment is selected on the
			// interface itself and must stay within the Actor fields.
			head := block
			if idx := strings.Index(block, "... on"); idx >= 0 {
				head = block[:idx]
			} else {
				t.Fatalf("%s selects the author without any inline fragment:\n%s", name, block)
			}
			for _, field := range []string{"id", "name"} {
				for _, token := range strings.Fields(head) {
					if token == field {
						t.Errorf("%s selects %q directly on the Actor interface; it only exists on the concrete types:\n%s",
							name, field, block)
					}
				}
			}
			for _, required := range []string{"... on User { id name }", "... on Bot { id }"} {
				if !strings.Contains(block, required) {
					t.Errorf("%s is missing the inline fragment %q", name, required)
				}
			}
		})
	}
}

// A commit author is reached through commit.author.user, which is a concrete
// User, so it may select those fields directly.
func TestCommitAuthorSelectsUserFieldsDirectly(t *testing.T) {
	if !strings.Contains(commitsQuery, "user { __typename id login name avatarUrl url }") {
		t.Error("the commit query should select User fields directly")
	}
}

// FetchPullRequests must decode the shape GitHub returns for an inline-fragment
// author selection, including a Bot author that carries no name.
func TestPullRequestAuthorsDecodeFromInlineFragments(t *testing.T) {
	srv, _, _ := newTestServer(t, func(w http.ResponseWriter, req recordedRequest, n int) {
		io.WriteString(w, `{"data":{"repository":{"pullRequests":{
		"pageInfo":{"hasNextPage":false,"endCursor":""},
		"nodes":[
		  {"id":"PR_1","number":1,"title":"Human PR","state":"MERGED",
		   "createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-02T10:00:00Z",
		   "author":{"__typename":"User","login":"alice","avatarUrl":"https://a.test/a.png","url":"https://github.test/alice","id":"U_1","name":"Alice"}},
		  {"id":"PR_2","number":2,"title":"Bot PR","state":"OPEN",
		   "createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-02T10:00:00Z",
		   "author":{"__typename":"Bot","login":"dependabot","avatarUrl":"https://a.test/b.png","url":"https://github.test/apps/dependabot","id":"BOT_1"}},
		  {"id":"PR_3","number":3,"title":"Ghost PR","state":"CLOSED",
		   "createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-02T10:00:00Z",
		   "author":null}
		]}},`+rateLimitJSON(4000)+`}}`)
	})
	c := newTestClient(srv.URL, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	page, err := c.FetchPullRequests(context.Background(), "acme", "api", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Nodes) != 3 {
		t.Fatalf("got %d pull requests", len(page.Nodes))
	}
	if got := page.Nodes[0].Author; got == nil || got.ID != "U_1" || got.Name != "Alice" || got.IsBot() {
		t.Errorf("user author = %+v", got)
	}
	if got := page.Nodes[1].Author; got == nil || got.ID != "BOT_1" || !got.IsBot() {
		t.Errorf("bot author = %+v", got)
	}
	if page.Nodes[1].Author.Name != "" {
		t.Error("a Bot has no name field; it must decode as empty, not fail")
	}
	// A deleted account leaves author null; the pull request is still imported.
	if page.Nodes[2].Author != nil {
		t.Error("a null author must decode as nil")
	}
}
