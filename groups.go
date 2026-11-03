package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// groupName is a name the user gave a connected group. It is keyed by member
// PRs, not a bundle id: ids renumber on every refresh and membership drifts,
// so the page matches a bundle to the newest entry that shares a PR with it.
type groupName struct {
	Name string   `json:"name"`
	PRs  []string `json:"prs"` // lowercased keys
	At   string   `json:"at"`  // RFC3339
}

type groupFile struct {
	Names []groupName `json:"names"`
}

// groupStore keeps names in a JSON file under the user config dir. They live
// outside the polled snapshot so a refresh never loses them.
type groupStore struct {
	path  string
	mu    sync.Mutex
	names []groupName
}

func loadGroups() *groupStore {
	g := &groupStore{names: []groupName{}}
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Printf("group names won't be saved: %v", err)
		return g
	}
	g.path = filepath.Join(dir, "github-overlay", "groups.json")
	b, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return g
	}
	var f groupFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		log.Printf("ignoring %s: %v", g.path, err)
		return g
	}
	if f.Names != nil {
		g.names = f.Names
	}
	return g
}

// list returns a copy, safe to encode while another request saves.
func (g *groupStore) list() []groupName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]groupName{}, g.names...)
}

// set names the group made of keys; an empty name just clears them. Every key
// leaves its old entries first, so one PR never counts toward two names.
func (g *groupStore) set(keys []string, name string) ([]groupName, error) {
	drop := map[string]bool{}
	prs := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !drop[k] {
			drop[k] = true
			prs = append(prs, k)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	next := []groupName{}
	for _, e := range g.names {
		var kept []string
		for _, k := range e.PRs {
			if !drop[k] {
				kept = append(kept, k)
			}
		}
		if len(kept) > 0 {
			e.PRs = kept
			next = append(next, e)
		}
	}
	if name != "" {
		next = append(next, groupName{Name: name, PRs: prs, At: time.Now().UTC().Format(time.RFC3339)})
	}
	if err := g.save(next); err != nil {
		return nil, err
	}
	g.names = next
	return append([]groupName{}, next...), nil
}

// save writes via a temp file in the same dir, so a crash never leaves a
// half-written groups.json.
func (g *groupStore) save(names []groupName) error {
	if g.path == "" {
		return errors.New("no config directory to save group names in")
	}
	dir := filepath.Dir(g.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(groupFile{Names: names}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "groups-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), g.path)
}

func (s *server) handleSetGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRs  []string `json:"prs"`
		Name string   `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, errors.New("bad request body"), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	switch {
	case utf8.RuneCountInString(name) > 80:
		httpError(w, errors.New("name is longer than 80 characters"), http.StatusBadRequest)
		return
	case len(req.PRs) == 0 || len(req.PRs) > 100:
		httpError(w, errors.New("need 1 to 100 PRs"), http.StatusBadRequest)
		return
	}
	names, err := s.groups.set(req.PRs, name)
	if err != nil {
		httpError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, groupFile{Names: names})
}
