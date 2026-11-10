package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// enqueueMutation adds the PR to its base branch's merge queue.
const enqueueMutation = `
mutation($id: ID!) {
  enqueuePullRequest(input: {pullRequestId: $id}) {
    mergeQueueEntry { position state }
  }
}`

// autoMergeMutation turns on GitHub auto-merge: the PR merges as soon as its
// requirements pass, and it can still be disabled until then. The repository
// must allow auto-merge; GitHub refuses otherwise and mutate surfaces the
// message. No commitHeadline or commitBody: the repo's default title and
// message settings apply.
const autoMergeMutation = `
mutation($id: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) {
    pullRequest { autoMergeRequest { enabledAt } }
  }
}`

const unautoMutation = `
mutation($id: ID!) {
  disablePullRequestAutoMerge(input: {pullRequestId: $id}) {
    actor { login }
  }
}`

// No commitHeadline or commitBody in mergeMutation either: the repo's default
// title and message settings apply.
const mergeMutation = `
mutation($id: ID!, $method: PullRequestMergeMethod!) {
  mergePullRequest(input: {pullRequestId: $id, mergeMethod: $method}) {
    pullRequest { state }
  }
}`

// mutate runs a GraphQL mutation. GitHub refuses with a GraphQL error when the
// PR isn't approved, checks are missing, branch protection blocks it or it
// conflicts; checkResponse turns the first into the message the page shows.
func (c *client) mutate(query string, vars map[string]any, out any) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := c.request("POST", "/graphql", map[string]any{"query": query, "variables": vars}, &res); err != nil {
		return err
	}
	// A failed mutation can carry data with a null field, so errors win.
	if len(res.Errors) > 0 {
		return c.checkResponse(res.Errors, false)
	}
	return json.Unmarshal(res.Data, out)
}

func (c *client) enqueue(id string) (*queueEntry, error) {
	var out struct {
		Enqueue *struct {
			Entry *queueEntry `json:"mergeQueueEntry"`
		} `json:"enqueuePullRequest"`
	}
	if err := c.mutate(enqueueMutation, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	if out.Enqueue == nil || out.Enqueue.Entry == nil {
		return nil, fmt.Errorf("github: enqueue returned no queue entry")
	}
	return out.Enqueue.Entry, nil
}

func (c *client) merge(id, method string) error {
	var out struct{}
	return c.mutate(mergeMutation, map[string]any{"id": id, "method": method}, &out)
}

func (c *client) autoMerge(id, method string) error {
	var out struct {
		Enable *struct {
			PR *struct {
				Auto *autoMergeReq `json:"autoMergeRequest"`
			} `json:"pullRequest"`
		} `json:"enablePullRequestAutoMerge"`
	}
	if err := c.mutate(autoMergeMutation, map[string]any{"id": id, "method": method}, &out); err != nil {
		return err
	}
	if out.Enable == nil || out.Enable.PR == nil || out.Enable.PR.Auto == nil {
		return fmt.Errorf("github: auto-merge was not enabled")
	}
	return nil
}

func (c *client) unauto(id string) error {
	var out struct {
		Disable *struct{} `json:"disablePullRequestAutoMerge"`
	}
	if err := c.mutate(unautoMutation, map[string]any{"id": id}, &out); err != nil {
		return err
	}
	if out.Disable == nil {
		return fmt.Errorf("github: auto-merge was not disabled")
	}
	return nil
}

type landResult struct {
	Key    string `json:"key"`
	PR     *PR    `json:"pr"`               // null when the request failed or the PR left the snapshot
	Action string `json:"action,omitempty"` // "queued", "merged", "auto" or "unauto"
	Error  string `json:"error,omitempty"`
}

// maxLand bounds one request; each PR is one mutation.
const maxLand = 50

// handleLand lands many PRs in one request so the page triggers a single
// refresh: it queues those whose base branch has a merge queue and merges or
// auto-merges the rest. With mode "auto", a PR whose checks are still running
// gets auto-merge, which merges it when the requirements pass. With mode
// "unauto" — used to cancel auto-merge from its pill — the reverse. Checks
// that already pass merge directly instead, so a repository without the
// auto-merge setting still lands its green PRs. On a merge-queue branch a
// refused enqueue falls back to auto-merge — GitHub's merge-when-ready — so
// the queue takes the PR once its requirements pass. A failure on one PR
// lands in its result and does not stop the rest.
func (s *server) handleLand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRs  []string `json:"prs"`
		Mode string   `json:"mode"` // "", "auto" or "unauto"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	if n := len(req.PRs); n == 0 || n > maxLand {
		httpError(w, fmt.Errorf("prs must hold 1 to %d items, got %d", maxLand, n), http.StatusBadRequest)
		return
	}
	if req.Mode != "" && req.Mode != "auto" && req.Mode != "unauto" {
		httpError(w, fmt.Errorf("unknown mode %q", req.Mode), http.StatusBadRequest)
		return
	}

	results := make([]landResult, 0, len(req.PRs))
	anyOK := false
	for _, key := range req.PRs {
		res := landResult{Key: key}
		// The node id lives only in the snapshot, so a PR the page no longer
		// has can't be landed.
		var id, method, reason string
		var queue, auto bool
		s.mu.RLock()
		if s.snap != nil {
			if p := s.snap.PRs[strings.ToLower(key)]; p != nil {
				id, method, queue = p.ID, p.MergeMethod, p.MergeQueue
				// The page checks this too, but the server is what acts with
				// the token, and a direct merge can't be undone.
				switch {
				case p.State != "OPEN":
					reason = "not open"
				case req.Mode == "unauto":
					if !p.AutoMerge {
						reason = "auto-merge isn't on"
					}
				case !p.Approved:
					reason = "not approved"
				case p.Queued != nil:
					reason = "already in the merge queue"
				case !strings.EqualFold(p.Author, s.snap.Viewer):
					reason = "not your pull request"
				}
				// With mode "auto", a merge-queue branch queues and a PR with
				// running checks turns auto-merge on; anything else merges,
				// whatever the page asked for. Checking the CI state here keeps
				// green PRs in an auto batch on the direct merge path, which
				// works even where the auto-merge setting is off.
				auto = req.Mode == "auto" && !queue &&
					(p.CI.State == "PENDING" || p.CI.State == "EXPECTED")
			}
		}
		s.mu.RUnlock()
		if id == "" {
			res.Error = "not found"
			results = append(results, res)
			continue
		}
		if reason != "" {
			res.Error = reason
			results = append(results, res)
			continue
		}

		// entry stays nil unless a queue took the PR; the snapshot update
		// later keys off res.Action.
		var entry *queueEntry
		var err error
		switch {
		case req.Mode == "unauto":
			res.Action = "unauto"
			err = s.client.unauto(id)
		case queue:
			res.Action = "queued"
			entry, err = s.client.enqueue(id)
			// GitHub only queues a PR that meets every requirement right
			// now — with checks still running it refuses. GitHub's own
			// "merge when ready" turns auto-merge on instead, and the queue
			// takes the PR once the requirements pass. If that fails too
			// (the repository may ban auto-merge), the queue error is the
			// one to show.
			if err != nil && method != "" {
				if serr := s.client.autoMerge(id, method); serr == nil {
					res.Action, entry, err = "auto", nil, nil
				}
			}
		case method == "":
			err = fmt.Errorf("the repository allows no merge method")
		case auto:
			res.Action = "auto"
			err = s.client.autoMerge(id, method)
		default:
			res.Action = "merged"
			err = s.client.merge(id, method)
		}
		if err != nil {
			res.Action, res.Error = "", err.Error()
			results = append(results, res)
			continue
		}
		anyOK = true

		s.mu.Lock()
		if s.snap != nil {
			if p := s.snap.PRs[strings.ToLower(key)]; p != nil {
				switch res.Action {
				case "queued":
					p.Queued = entry
				case "merged":
					p.State = "MERGED"
				case "auto":
					p.AutoMerge = true
				case "unauto":
					p.AutoMerge = false
				}
				updated := *p
				res.PR = &updated
			}
		}
		s.mu.Unlock()
		results = append(results, res)
	}
	if anyOK {
		go s.refresh(0)
	}

	writeJSON(w, map[string]any{"results": results})
}
