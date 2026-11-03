package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Candidate struct {
	Login     string `json:"login"` // user login, or team slug for teams
	Name      string `json:"name,omitempty"`
	Suggested bool   `json:"suggested,omitempty"`
}

type Candidates struct {
	Users []Candidate `json:"users"`
	Teams []Candidate `json:"teams"`
}

const candidatesQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      author { login }
      suggestedReviewers { reviewer { login name } }
    }
    assignableUsers(first: 100) { nodes { login name } }
  }
}`

// Listing a repo's teams over REST needs admin on the repo, so offer the
// org's teams instead. One without access to the repo gets ignored by GitHub
// when requested, and setReviewers reports that.
const teamsQuery = `
query($owner: String!) {
  organization(login: $owner) { teams(first: 100, orderBy: {field: NAME, direction: ASC}) { nodes { slug name } } }
}`

var prKeyRe = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

type prRef struct {
	owner, repo string
	number      int
}

func parsePRKey(key string) (prRef, error) {
	m := prKeyRe.FindStringSubmatch(key)
	if m == nil {
		return prRef{}, fmt.Errorf("bad PR key %q", key)
	}
	n, _ := strconv.Atoi(m[3])
	return prRef{m[1], m[2], n}, nil
}

// candidateCache keeps each repo's assignable users and teams for a while;
// they rarely change and the picker opens often.
type candidateCache struct {
	mu      sync.Mutex
	entries map[string]cachedCandidates
}

type cachedCandidates struct {
	users, teams []Candidate
	at           time.Time
}

const candidateTTL = 10 * time.Minute

func (c *client) candidates(cache *candidateCache, pr prRef) (*Candidates, error) {
	var res struct {
		Data *struct {
			Repository *struct {
				PullRequest *struct {
					Author             *actor `json:"author"`
					SuggestedReviewers []struct {
						Reviewer struct {
							Login string `json:"login"`
							Name  string `json:"name"`
						} `json:"reviewer"`
					} `json:"suggestedReviewers"`
				} `json:"pullRequest"`
				AssignableUsers struct {
					Nodes []Candidate `json:"nodes"`
				} `json:"assignableUsers"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	vars := map[string]any{"owner": pr.owner, "name": pr.repo, "number": pr.number}
	if err := c.request("POST", "/graphql", map[string]any{"query": candidatesQuery, "variables": vars}, &res); err != nil {
		return nil, err
	}
	if res.Data == nil || res.Data.Repository == nil || res.Data.Repository.PullRequest == nil {
		if len(res.Errors) > 0 {
			return nil, fmt.Errorf("github: %s", res.Errors[0].Message)
		}
		return nil, fmt.Errorf("pull request %s/%s#%d not found", pr.owner, pr.repo, pr.number)
	}
	repo := res.Data.Repository

	repoKey := strings.ToLower(pr.owner + "/" + pr.repo)
	cache.mu.Lock()
	cached, ok := cache.entries[repoKey]
	cache.mu.Unlock()
	if !ok || time.Since(cached.at) > candidateTTL {
		cached = cachedCandidates{users: repo.AssignableUsers.Nodes, at: time.Now()}
		// A personal repo has no org, and a token without read:org can't list
		// teams; both come back as errors and mean no teams.
		var teams struct {
			Data *struct {
				Organization *struct {
					Teams struct {
						Nodes []struct {
							Slug string `json:"slug"`
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"teams"`
				} `json:"organization"`
			} `json:"data"`
		}
		q := map[string]any{"query": teamsQuery, "variables": map[string]any{"owner": pr.owner}}
		if err := c.request("POST", "/graphql", q, &teams); err == nil && teams.Data != nil && teams.Data.Organization != nil {
			for _, t := range teams.Data.Organization.Teams.Nodes {
				cached.teams = append(cached.teams, Candidate{Login: t.Slug, Name: t.Name})
			}
		}
		cache.mu.Lock()
		cache.entries[repoKey] = cached
		cache.mu.Unlock()
	}

	author := ""
	if repo.PullRequest.Author != nil {
		author = strings.ToLower(repo.PullRequest.Author.Login)
	}
	suggested := map[string]bool{}
	byLogin := map[string]Candidate{}
	for _, s := range repo.PullRequest.SuggestedReviewers {
		suggested[strings.ToLower(s.Reviewer.Login)] = true
		byLogin[strings.ToLower(s.Reviewer.Login)] = Candidate{Login: s.Reviewer.Login, Name: s.Reviewer.Name}
	}
	for _, u := range cached.users {
		byLogin[strings.ToLower(u.Login)] = u
	}

	out := &Candidates{Users: []Candidate{}, Teams: []Candidate{}}
	for k, u := range byLogin {
		if k == author { // GitHub rejects requesting the author
			continue
		}
		u.Suggested = suggested[k]
		out.Users = append(out.Users, u)
	}
	sort.Slice(out.Users, func(i, j int) bool {
		a, b := out.Users[i], out.Users[j]
		if a.Suggested != b.Suggested {
			return a.Suggested
		}
		return strings.ToLower(a.Login) < strings.ToLower(b.Login)
	})
	out.Teams = append(out.Teams, cached.teams...)
	sort.Slice(out.Teams, func(i, j int) bool { return out.Teams[i].Login < out.Teams[j].Login })
	return out, nil
}

type reviewerSet struct {
	Users []string `json:"users"`
	Teams []string `json:"teams"`
}

func (r reviewerSet) empty() bool { return len(r.Users) == 0 && len(r.Teams) == 0 }

// setReviewers adds and removes review requests on a PR and returns the
// requests GitHub now has, teams as "@slug". GitHub answers 2xx even when it
// skips a reviewer (an unknown login, a team without repo access), so a
// requested add missing from the result comes back in ignored. The POST and
// DELETE answer with the pull request, so its requested lists are read from
// the last change made; only a change with nothing to add or remove costs a
// GET.
func (c *client) setReviewers(pr prRef, add, remove reviewerSet) (requested, ignored []string, err error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", pr.owner, pr.repo, pr.number)
	body := func(r reviewerSet) map[string]any {
		return map[string]any{"reviewers": nonNil(r.Users), "team_reviewers": nonNil(r.Teams)}
	}
	type pull struct {
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
		RequestedTeams []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
	}
	var now pull
	// The last call's response is the freshest, so remove goes first.
	switch {
	case !remove.empty() && !add.empty():
		if err := c.request("DELETE", path, body(remove), nil); err != nil {
			return nil, nil, err
		}
		fallthrough
	case !add.empty():
		if err := c.request("POST", path, body(add), &now); err != nil {
			return nil, nil, err
		}
	case !remove.empty():
		if err := c.request("DELETE", path, body(remove), &now); err != nil {
			return nil, nil, err
		}
	default:
		// This GET answers {users, teams}, not a pull request.
		var cur struct {
			Users []struct {
				Login string `json:"login"`
			} `json:"users"`
			Teams []struct {
				Slug string `json:"slug"`
			} `json:"teams"`
		}
		if err := c.request("GET", path, nil, &cur); err != nil {
			return nil, nil, err
		}
		now = pull{RequestedReviewers: cur.Users, RequestedTeams: cur.Teams}
	}
	have := map[string]bool{}
	requested = []string{}
	for _, u := range now.RequestedReviewers {
		requested = append(requested, u.Login)
		have[strings.ToLower(u.Login)] = true
	}
	for _, t := range now.RequestedTeams {
		requested = append(requested, "@"+t.Slug)
		have["@"+strings.ToLower(t.Slug)] = true
	}
	for _, u := range add.Users {
		if !have[strings.ToLower(u)] {
			ignored = append(ignored, u)
		}
	}
	for _, t := range add.Teams {
		if !have["@"+strings.ToLower(t)] {
			ignored = append(ignored, "@"+t)
		}
	}
	return requested, ignored, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	pr, err := parsePRKey(r.URL.Query().Get("pr"))
	if err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	cands, err := s.client.candidates(&s.candidates, pr)
	if err != nil {
		httpError(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, cands)
}

type reviewChange struct {
	PR     string      `json:"pr"`
	Add    reviewerSet `json:"add"`
	Remove reviewerSet `json:"remove"`
}

type reviewResult struct {
	Key     string   `json:"key"`
	PR      *PR      `json:"pr"` // null when the change failed or the PR left the snapshot
	Ignored []string `json:"ignored"`
	Error   string   `json:"error,omitempty"`
}

// maxReviewChanges bounds one request; each change costs a few API calls.
const maxReviewChanges = 50

// handleSetReviewers applies many PRs' changes in one request so the page
// triggers a single refresh instead of one per PR. A failure on one PR lands in
// its result and does not stop the rest.
func (s *server) handleSetReviewers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Changes []reviewChange `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	if n := len(req.Changes); n == 0 || n > maxReviewChanges {
		httpError(w, fmt.Errorf("changes must hold 1 to %d items, got %d", maxReviewChanges, n), http.StatusBadRequest)
		return
	}

	results := make([]reviewResult, 0, len(req.Changes))
	anyOK := false
	for _, ch := range req.Changes {
		res := reviewResult{Key: ch.PR, Ignored: []string{}}
		pr, err := parsePRKey(ch.PR)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		requested, ignored, err := s.client.setReviewers(pr, ch.Add, ch.Remove)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		anyOK = true
		res.Ignored = nonNil(ignored)

		// Reflect the change right away; the next poll brings in the rest.
		s.mu.Lock()
		if p := s.snap.PRs[strings.ToLower(ch.PR)]; p != nil {
			p.RequestedReviewers = requested
			updated := *p
			res.PR = &updated
		}
		s.mu.Unlock()
		results = append(results, res)
	}
	if anyOK {
		go s.refresh(0)
	}

	writeJSON(w, map[string]any{"results": results})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
