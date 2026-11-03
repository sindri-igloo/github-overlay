package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const enqueueMutation = `
mutation($id: ID!) {
  enqueuePullRequest(input: {pullRequestId: $id}) {
    mergeQueueEntry { position state }
  }
}`

// No commitHeadline or commitBody: the repo's default title and message
// settings apply.
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

type landResult struct {
	Key    string `json:"key"`
	PR     *PR    `json:"pr"`               // null when the request failed or the PR left the snapshot
	Action string `json:"action,omitempty"` // "queued" or "merged"
	Error  string `json:"error,omitempty"`
}

// maxLand bounds one request; each PR is one mutation.
const maxLand = 50

// handleLand lands many PRs in one request so the page triggers a single
// refresh: it queues those whose base branch has a merge queue and merges the
// rest. A failure on one PR lands in its result and does not stop the rest.
func (s *server) handleLand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRs []string `json:"prs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, err, http.StatusBadRequest)
		return
	}
	if n := len(req.PRs); n == 0 || n > maxLand {
		httpError(w, fmt.Errorf("prs must hold 1 to %d items, got %d", maxLand, n), http.StatusBadRequest)
		return
	}

	results := make([]landResult, 0, len(req.PRs))
	anyOK := false
	for _, key := range req.PRs {
		res := landResult{Key: key}
		// The node id lives only in the snapshot, so a PR the page no longer
		// has can't be landed.
		var id, method string
		var queue bool
		s.mu.RLock()
		if s.snap != nil {
			if p := s.snap.PRs[strings.ToLower(key)]; p != nil {
				id, method, queue = p.ID, p.MergeMethod, p.MergeQueue
			}
		}
		s.mu.RUnlock()
		if id == "" {
			res.Error = "not found"
			results = append(results, res)
			continue
		}

		var entry *queueEntry
		var err error
		if queue {
			res.Action = "queued"
			entry, err = s.client.enqueue(id)
		} else if method == "" {
			err = fmt.Errorf("the repository allows no merge method")
		} else {
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
				if queue {
					p.Queued = entry
				} else {
					p.State = "MERGED"
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
