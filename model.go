package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Review struct {
	Login string `json:"login"`
	State string `json:"state"`
	Bot   bool   `json:"bot"`
}

type Check struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type CI struct {
	State   string  `json:"state"` // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or "" when no checks
	Failed  []Check `json:"failed"`
	Pending []Check `json:"pending"`
	Total   int     `json:"total"`
}

type PR struct {
	Key                string   `json:"key"` // owner/repo#123
	ID                 string   `json:"-"`   // GraphQL node id, for mutations
	MergeMethod        string   `json:"-"`   // SQUASH, MERGE or REBASE: what the repo allows, in that order of preference
	Number             int      `json:"number"`
	Title              string   `json:"title"`
	URL                string   `json:"url"`
	Repo               string   `json:"repo"`
	Author             string   `json:"author"`
	Head               string   `json:"head"`
	Base               string   `json:"base"`
	State              string   `json:"state"` // OPEN, DRAFT, MERGED, CLOSED
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
	ClosedAt           string   `json:"closedAt,omitempty"`
	Additions          int      `json:"additions"`
	Deletions          int      `json:"deletions"`
	Conflicting        bool     `json:"conflicting"`
	ReviewDecision     string   `json:"reviewDecision"`
	RequestedReviewers []string `json:"requestedReviewers"`
	Reviews            []Review `json:"reviews"`
	ChangesRequested   bool     `json:"changesRequested"`
	HumanThreads       int      `json:"humanThreads"` // unresolved
	BotThreads         int      `json:"botThreads"`   // unresolved
	CI                 CI       `json:"ci"`
	Links              []string `json:"links"`        // keys of PRs referenced from/to this one
	Bundle             int      `json:"bundle"`       // group of open PRs and what they link to
	ClosedBundle       int      `json:"closedBundle"` // group of closed PRs linked to each other

	MergeQueue bool        `json:"mergeQueue"` // the base branch has a merge queue
	Queued     *queueEntry `json:"queued"`     // null when not in the queue
}

type Snapshot struct {
	Viewer        string              `json:"viewer"`
	FetchedAt     string              `json:"fetchedAt"`
	Error         string              `json:"error,omitempty"`
	RateRemaining int                 `json:"rateRemaining"`
	PRs           map[string]*PR      `json:"prs"`
	Sections      map[string][]string `json:"sections"`
	Bundles       map[int][]string    `json:"bundles"`
}

var (
	urlRef  = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(?:pull|issues)/(\d+)`)
	fullRef = regexp.MustCompile(`(?:^|[\s(\[,])([\w.-]+/[\w.-]+)#(\d+)\b`)
	// "#12", or "rent-api #12" / "rent-api#12" naming a sibling repo without its owner.
	shortRef = regexp.MustCompile(`(?:^|[\s(\[,])(?:([\w.-]+) ?)?#(\d+)\b`)
)

func refKey(repo, num string) string { return strings.ToLower(repo) + "#" + num }

// bodyRefs returns keys of PRs/issues the body mentions. repos maps
// lowercased "owner/name" of every fetched repo, used to resolve a bare
// repo name in front of "#N" to a sibling repo under the same owner.
func bodyRefs(repo, body string, repos map[string]bool) []string {
	owner, _, _ := strings.Cut(repo, "/")
	var keys []string
	for _, m := range urlRef.FindAllStringSubmatch(body, -1) {
		keys = append(keys, refKey(m[1], m[2]))
	}
	for _, m := range fullRef.FindAllStringSubmatch(body, -1) {
		keys = append(keys, refKey(m[1], m[2]))
	}
	for _, m := range shortRef.FindAllStringSubmatch(body, -1) {
		target := repo
		if sibling := strings.ToLower(owner + "/" + m[1]); m[1] != "" && repos[sibling] {
			target = sibling
		}
		keys = append(keys, refKey(target, m[2]))
	}
	return keys
}

func isBot(a *actor, bots map[string]bool) bool {
	if a == nil {
		return false
	}
	login := strings.ToLower(a.Login)
	return a.Typename == "Bot" || strings.HasSuffix(login, "[bot]") || bots[login]
}

func convert(r rawPR, bots, repos map[string]bool) *PR {
	p := &PR{
		Key:       refKey(r.Repository.NameWithOwner, strconv.Itoa(r.Number)),
		ID:        r.ID,
		Number:    r.Number,
		Title:     r.Title,
		URL:       r.URL,
		Repo:      r.Repository.NameWithOwner,
		Head:      r.HeadRefName,
		Base:      r.BaseRefName,
		State:     r.State,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		Additions: r.Additions,
		Deletions: r.Deletions,
	}
	if r.Author != nil {
		p.Author = r.Author.Login
	}
	if r.IsDraft && r.State == "OPEN" {
		p.State = "DRAFT"
	}
	if r.ClosedAt != nil {
		p.ClosedAt = *r.ClosedAt
	}
	switch repo := r.Repository; {
	case repo.SquashMergeAllowed:
		p.MergeMethod = "SQUASH"
	case repo.MergeCommitAllowed:
		p.MergeMethod = "MERGE"
	case repo.RebaseMergeAllowed:
		p.MergeMethod = "REBASE"
	}
	p.MergeQueue = r.IsMergeQueueEnabled
	p.Queued = r.MergeQueueEntry
	p.Conflicting = r.Mergeable == "CONFLICTING"
	if r.ReviewDecision != nil {
		p.ReviewDecision = *r.ReviewDecision
	}

	p.RequestedReviewers = []string{}
	for _, n := range r.ReviewRequests.Nodes {
		if rr := n.RequestedReviewer; rr != nil {
			if rr.Typename == "Team" {
				p.RequestedReviewers = append(p.RequestedReviewers, "@"+rr.Slug)
			} else {
				p.RequestedReviewers = append(p.RequestedReviewers, rr.Login)
			}
		}
	}

	p.Reviews = []Review{}
	for _, n := range r.LatestReviews.Nodes {
		if n.Author == nil {
			continue
		}
		p.Reviews = append(p.Reviews, Review{Login: n.Author.Login, State: n.State, Bot: isBot(n.Author, bots)})
		if n.State == "CHANGES_REQUESTED" {
			p.ChangesRequested = true
		}
	}

	for _, t := range r.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		var a *actor
		if len(t.Comments.Nodes) > 0 {
			a = t.Comments.Nodes[0].Author
		}
		if isBot(a, bots) {
			p.BotThreads++
		} else {
			p.HumanThreads++
		}
	}

	p.CI = CI{Failed: []Check{}, Pending: []Check{}}
	if len(r.Commits.Nodes) > 0 {
		if roll := r.Commits.Nodes[0].Commit.StatusCheckRollup; roll != nil {
			// A commit keeps every run of a check, e.g. one cancelled when a
			// newer run of its workflow started. Only the latest run counts.
			nodes := roll.Contexts.Nodes
			keys := make([]string, len(nodes))
			latest := map[string]int{}
			for i, c := range nodes {
				if c.Typename != "CheckRun" {
					continue
				}
				keys[i] = c.Name
				if c.CheckSuite != nil && c.CheckSuite.WorkflowRun != nil {
					keys[i] = c.CheckSuite.WorkflowRun.Workflow.Name + "/" + c.Name
				}
				// A queued run has no start time yet and is the newest.
				if j, ok := latest[keys[i]]; !ok || c.StartedAt == "" || (nodes[j].StartedAt != "" && c.StartedAt > nodes[j].StartedAt) {
					latest[keys[i]] = i
				}
			}
			for i, c := range nodes {
				if c.Typename == "CheckRun" && latest[keys[i]] != i {
					continue
				}
				p.CI.Total++
				switch c.Typename {
				case "CheckRun":
					ch := Check{Name: c.Name, URL: c.DetailsURL}
					switch {
					case c.Status != "COMPLETED":
						p.CI.Pending = append(p.CI.Pending, ch)
					case c.Conclusion == "FAILURE", c.Conclusion == "TIMED_OUT", c.Conclusion == "CANCELLED",
						c.Conclusion == "ACTION_REQUIRED", c.Conclusion == "STARTUP_FAILURE":
						p.CI.Failed = append(p.CI.Failed, ch)
					}
				case "StatusContext":
					ch := Check{Name: c.Context, URL: c.TargetURL}
					switch c.State {
					case "PENDING", "EXPECTED":
						p.CI.Pending = append(p.CI.Pending, ch)
					case "FAILURE", "ERROR":
						p.CI.Failed = append(p.CI.Failed, ch)
					}
				}
			}
			// GitHub's rollup state counts superseded runs too, so derive it
			// from the latest runs. A required check that hasn't reported yet
			// only shows in the rollup state (PENDING or EXPECTED).
			switch {
			case p.CI.Total == 0:
				p.CI.State = roll.State
			case len(p.CI.Failed) > 0:
				p.CI.State = "FAILURE"
			case len(p.CI.Pending) > 0:
				p.CI.State = "PENDING"
			case roll.State == "PENDING" || roll.State == "EXPECTED":
				p.CI.State = roll.State
			default:
				p.CI.State = "SUCCESS"
			}
		}
	}

	seen := map[string]bool{p.Key: true}
	p.Links = []string{}
	refs := bodyRefs(p.Repo, r.Body, repos)
	for _, n := range r.TimelineItems.Nodes {
		if n.Source != nil && n.Source.Typename == "PullRequest" {
			if m := urlRef.FindStringSubmatch(n.Source.URL); m != nil {
				refs = append(refs, refKey(m[1], m[2]))
			}
		}
	}
	for _, k := range refs {
		if !seen[k] {
			seen[k] = true
			p.Links = append(p.Links, k)
		}
	}
	return p
}

func buildSnapshot(res *queryResult, bots map[string]bool) *Snapshot {
	s := &Snapshot{
		Viewer:        res.Viewer,
		RateRemaining: res.RateRemaining,
		PRs:           map[string]*PR{},
		Sections:      map[string][]string{},
		Bundles:       map[int][]string{},
	}
	sections := map[string][]rawPR{"mine": res.Mine, "review": res.Review, "closed": res.Closed}
	repos := map[string]bool{}
	for _, nodes := range sections {
		for _, r := range nodes {
			repos[strings.ToLower(r.Repository.NameWithOwner)] = true
		}
	}
	for name, nodes := range sections {
		keys := []string{}
		for _, r := range nodes {
			if r.Number == 0 { // non-PR search hit
				continue
			}
			p := convert(r, bots, repos)
			s.PRs[p.Key] = p
			keys = append(keys, p.Key)
		}
		s.Sections[name] = keys
	}

	// Two groupings share one id space in s.Bundles:
	//   Bundle: open PRs plus everything they link to, open or closed. A
	//     closed PR joins only through an open one; closed-to-closed links
	//     don't count here, since old merged PRs chain unrelated work together.
	//   ClosedBundle: closed PRs linked to each other, for the Closed tab.
	open := func(p *PR) bool { return p.State == "OPEN" || p.State == "DRAFT" }
	next := 1
	next = s.group(next, func(a, b *PR) bool { return open(a) || open(b) },
		func(p *PR, id int) { p.Bundle = id })
	s.group(next, func(a, b *PR) bool { return !open(a) && !open(b) },
		func(p *PR, id int) { p.ClosedBundle = id })
	return s
}

// group joins PRs linked in either direction where edge allows it, records
// each group of two or more in s.Bundles from id next on, calls assign for
// every member, and returns the next free id.
func (s *Snapshot) group(next int, edge func(a, b *PR) bool, assign func(*PR, int)) int {
	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		if parent[k] == "" || parent[k] == k {
			return k
		}
		parent[k] = find(parent[k])
		return parent[k]
	}
	for k, p := range s.PRs {
		for _, l := range p.Links {
			if q, ok := s.PRs[l]; ok && edge(p, q) {
				parent[find(k)] = find(l)
			}
		}
	}
	groups := map[string][]string{}
	for k := range s.PRs {
		root := find(k)
		groups[root] = append(groups[root], k)
	}
	var roots []string
	for r, members := range groups {
		if len(members) > 1 {
			roots = append(roots, r)
		}
	}
	sort.Strings(roots)
	for _, r := range roots {
		members := groups[r]
		sort.Strings(members)
		s.Bundles[next] = members
		for _, k := range members {
			assign(s.PRs[k], next)
		}
		next++
	}
	return next
}
