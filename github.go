package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const prFragment = `
fragment pr on PullRequest {
  id number title url isDraft state merged createdAt updatedAt closedAt mergedAt
  additions deletions mergeable reviewDecision body
  isMergeQueueEnabled isInMergeQueue mergeQueueEntry { position state }
  headRefName baseRefName
  author { login }
  repository { nameWithOwner squashMergeAllowed mergeCommitAllowed rebaseMergeAllowed }
  reviewRequests(first: 20) { nodes { requestedReviewer {
    __typename
    ... on User { login }
    ... on Team { slug }
    ... on Bot { login }
    ... on Mannequin { login }
  } } }
  latestReviews(first: 20) { nodes { state author { __typename login } } }
  reviewThreads(first: 50) { nodes { isResolved } }
  commits(last: 1) { nodes { commit { statusCheckRollup {
    state
    contexts(first: 50) { nodes {
      __typename
      ... on CheckRun { name status conclusion detailsUrl startedAt
        checkSuite { workflowRun { workflow { name } } } }
      ... on StatusContext { context state targetUrl }
    } }
  } } } }
  timelineItems(first: 25, itemTypes: [CROSS_REFERENCED_EVENT]) { nodes {
    ... on CrossReferencedEvent { source { __typename ... on PullRequest { url } } }
  } }
}`

// Closed PRs only need enough to list them and link them into bundles.
const prLiteFragment = `
fragment pr on PullRequest {
  number title url isDraft state merged createdAt updatedAt closedAt mergedAt
  additions deletions body headRefName baseRefName
  author { login }
  repository { nameWithOwner }
  latestReviews(first: 20) { nodes { state author { __typename login } } }
  timelineItems(first: 25, itemTypes: [CROSS_REFERENCED_EVENT]) { nodes {
    ... on CrossReferencedEvent { source { __typename ... on PullRequest { url } } }
  } }
}`

// Each section is its own request: one combined query takes longer than
// GitHub's ~10s GraphQL timeout and comes back as a 502.
const sectionQuery = `
query($q: String!) {
  viewer { login }
  rateLimit { remaining cost }
  search(query: $q, type: ISSUE, first: 50) { nodes { ...pr } }
}`

// Thread authors only matter for PRs with open threads, and asking for them in
// the section queries costs 25 of their 28 points, so they get their own query.
const threadsQuery = `
query($ids: [ID!]!) {
  rateLimit { cost }
  nodes(ids: $ids) { ... on PullRequest {
    id
    reviewThreads(first: 50) { nodes {
      isResolved
      comments(first: 1) { nodes { author { __typename login } } }
    } }
  } }
}`

// threadsChunk caps PRs per threadsQuery to keep its cost and runtime small.
const threadsChunk = 20

type actor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

type queueEntry struct {
	Position int    `json:"position"`
	State    string `json:"state"` // QUEUED, AWAITING_CHECKS, MERGEABLE, UNMERGEABLE, LOCKED
}

type rawPR struct {
	ID             string  `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	URL            string  `json:"url"`
	IsDraft        bool    `json:"isDraft"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	ClosedAt       *string `json:"closedAt"`
	MergedAt       *string `json:"mergedAt"`
	Additions      int     `json:"additions"`
	Deletions      int     `json:"deletions"`
	Mergeable      string  `json:"mergeable"`
	ReviewDecision *string `json:"reviewDecision"`
	Body           string  `json:"body"`
	HeadRefName    string  `json:"headRefName"`
	BaseRefName    string  `json:"baseRefName"`
	Author         *actor  `json:"author"`

	IsMergeQueueEnabled bool        `json:"isMergeQueueEnabled"`
	MergeQueueEntry     *queueEntry `json:"mergeQueueEntry"` // null when not queued

	Repository struct {
		NameWithOwner      string `json:"nameWithOwner"`
		SquashMergeAllowed bool   `json:"squashMergeAllowed"`
		MergeCommitAllowed bool   `json:"mergeCommitAllowed"`
		RebaseMergeAllowed bool   `json:"rebaseMergeAllowed"`
	} `json:"repository"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				Slug     string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestReviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author *actor `json:"author"`
		} `json:"nodes"`
	} `json:"latestReviews"`
	ReviewThreads reviewThreads `json:"reviewThreads"`
	Commits       struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Typename   string `json:"__typename"`
							Name       string `json:"name"`
							Status     string `json:"status"`
							Conclusion string `json:"conclusion"`
							DetailsURL string `json:"detailsUrl"`
							StartedAt  string `json:"startedAt"`
							CheckSuite *struct {
								WorkflowRun *struct {
									Workflow struct {
										Name string `json:"name"`
									} `json:"workflow"`
								} `json:"workflowRun"`
							} `json:"checkSuite"`
							Context   string `json:"context"`
							State     string `json:"state"`
							TargetURL string `json:"targetUrl"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	TimelineItems struct {
		Nodes []struct {
			Source *struct {
				Typename string `json:"__typename"`
				URL      string `json:"url"`
			} `json:"source"`
		} `json:"nodes"`
	} `json:"timelineItems"`
}

type reviewThreads struct {
	Nodes []struct {
		IsResolved bool `json:"isResolved"`
		Comments   struct {
			Nodes []struct {
				Author *actor `json:"author"`
			} `json:"nodes"`
		} `json:"comments"`
	} `json:"nodes"`
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type sectionResponse struct {
	Data *struct {
		Viewer    struct{ Login string } `json:"viewer"`
		RateLimit struct {
			Remaining int `json:"remaining"`
			Cost      int `json:"cost"`
		} `json:"rateLimit"`
		Search struct{ Nodes []rawPR } `json:"search"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

type threadsResponse struct {
	Data *struct {
		RateLimit struct {
			Cost int `json:"cost"`
		} `json:"rateLimit"`
		Nodes []*struct {
			ID            string        `json:"id"`
			ReviewThreads reviewThreads `json:"reviewThreads"`
		} `json:"nodes"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

type queryResult struct {
	Viewer        string
	RateRemaining int
	Cost          int // GraphQL points this refresh spent, as GitHub reports each query's own cost
	Mine          []rawPR
	Review        []rawPR
	Closed        []rawPR
	Errors        []string // partial errors from GitHub alongside data
}

var errUnauthorized = errors.New("github: unauthorized")

// RateLimitError means GitHub refused a call for rate limiting; calls
// should wait until Until.
type RateLimitError struct{ Until time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit hit; paused until " + e.Until.Local().Format("15:04")
}

type client struct {
	mu    sync.Mutex
	token string
	http  *http.Client

	// Last GraphQL budget GitHub reported; remaining is -1 until known.
	remaining int
	reset     time.Time
}

func newClient() *client {
	return &client{http: &http.Client{Timeout: 30 * time.Second}, remaining: -1}
}

// budget returns the GraphQL points left this hour and when they reset.
func (c *client) budget() (int, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remaining, c.reset
}

// rateLimited picks how long to wait: until the hourly reset when the budget
// is spent, otherwise a minute for GitHub's burst limits.
func (c *client) rateLimited(retryAfter string) *RateLimitError {
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 {
		return &RateLimitError{Until: time.Now().Add(time.Duration(secs) * time.Second)}
	}
	remaining, reset := c.budget()
	if remaining == 0 && reset.After(time.Now()) {
		return &RateLimitError{Until: reset}
	}
	return &RateLimitError{Until: time.Now().Add(time.Minute)}
}

func ghToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w (run `gh auth login`)", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *client) getToken(refresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || refresh {
		t, err := ghToken()
		if err != nil {
			return "", err
		}
		c.token = t
	}
	return c.token, nil
}

func (c *client) fetch(closedDays int) (*queryResult, error) {
	since := time.Now().AddDate(0, 0, -closedDays).Format("2006-01-02")
	sections := []struct {
		q, fragment string
		dst         *[]rawPR
	}{
		{"is:pr is:open author:@me archived:false sort:updated-desc", prFragment, nil},
		{"is:pr is:open review-requested:@me archived:false sort:updated-desc", prFragment, nil},
		{"is:pr is:closed author:@me closed:>=" + since + " sort:updated-desc", prLiteFragment, nil},
	}
	out := &queryResult{}
	sections[0].dst, sections[1].dst, sections[2].dst = &out.Mine, &out.Review, &out.Closed

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, s := range sections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.query(sectionQuery+s.fragment, s.q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			*s.dst = res.Data.Search.Nodes
			out.Viewer = res.Data.Viewer.Login
			out.Cost += res.Data.RateLimit.Cost
			if out.RateRemaining == 0 || res.Data.RateLimit.Remaining < out.RateRemaining {
				out.RateRemaining = res.Data.RateLimit.Remaining
			}
			for _, e := range res.Errors {
				out.Errors = append(out.Errors, e.Message)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	if err := c.fetchThreads(out); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchThreads fills in thread authors for open PRs with unresolved threads;
// the same PR can be in both Mine and Review.
func (c *client) fetchThreads(r *queryResult) error {
	byID := map[string][]*rawPR{}
	var ids []string
	for _, sec := range [][]rawPR{r.Mine, r.Review} {
		for i := range sec {
			p := &sec[i]
			open := false
			for _, t := range p.ReviewThreads.Nodes {
				open = open || !t.IsResolved
			}
			if !open || p.ID == "" {
				continue
			}
			if byID[p.ID] == nil {
				ids = append(ids, p.ID)
			}
			byID[p.ID] = append(byID[p.ID], p)
		}
	}
	for len(ids) > 0 {
		n := min(len(ids), threadsChunk)
		var res threadsResponse
		vars := map[string]any{"ids": ids[:n]}
		if err := c.request("POST", "/graphql", map[string]any{"query": threadsQuery, "variables": vars}, &res); err != nil {
			return err
		}
		if err := c.checkResponse(res.Errors, res.Data != nil); err != nil {
			return err
		}
		r.Cost += res.Data.RateLimit.Cost
		for _, node := range res.Data.Nodes {
			if node == nil {
				continue
			}
			for _, p := range byID[node.ID] {
				p.ReviewThreads = node.ReviewThreads
			}
		}
		ids = ids[n:]
	}
	return nil
}

func (c *client) query(q, search string) (*sectionResponse, error) {
	var out sectionResponse
	vars := map[string]any{"q": search}
	if err := c.request("POST", "/graphql", map[string]any{"query": q, "variables": vars}, &out); err != nil {
		return nil, err
	}
	if err := c.checkResponse(out.Errors, out.Data != nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// checkResponse turns rate limiting, or errors with no data, into an error.
func (c *client) checkResponse(errs []gqlError, hasData bool) error {
	for _, e := range errs {
		if strings.HasPrefix(e.Type, "RATE_LIMIT") || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			return c.rateLimited("")
		}
	}
	if !hasData {
		if len(errs) > 0 {
			return fmt.Errorf("github: %s", errs[0].Message)
		}
		return errors.New("github: empty response")
	}
	return nil
}

// request calls the GitHub API, retrying once with a fresh gh token on 401.
// out may be nil.
func (c *client) request(method, path string, body, out any) error {
	err := c.do(method, path, body, out, false)
	if errors.Is(err, errUnauthorized) {
		err = c.do(method, path, body, out, true)
	}
	return err
}

func (c *client) do(method, path string, body, out any, refreshToken bool) error {
	token, err := c.getToken(refreshToken)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-RateLimit-Resource") == "graphql" {
		rem, err1 := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
		reset, err2 := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		if err1 == nil && err2 == nil {
			c.mu.Lock()
			c.remaining, c.reset = rem, time.Unix(reset, 0)
			c.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")) {
		return c.rateLimited(resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
			Errors  []any  `json:"errors"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if strings.Contains(strings.ToLower(e.Message), "rate limit") {
			return c.rateLimited(resp.Header.Get("Retry-After"))
		}
		if e.Message == "" {
			e.Message = resp.Status
		}
		if len(e.Errors) > 0 {
			return fmt.Errorf("github: %s: %v", e.Message, e.Errors[0])
		}
		return fmt.Errorf("github: %s", e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
