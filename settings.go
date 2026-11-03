package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
)

// settings are the page's opt-in features, kept in settings.json next to
// groups.json. Everything here is off until the user turns it on.
type settings struct {
	ClaudeGrouping bool   `json:"claudeGrouping"` // group related open PRs with Claude
	ClaudeTitles   bool   `json:"claudeTitles"`   // title connected groups with Claude
	ClaudeModel    string `json:"claudeModel"`
}

// claudeModels are the models the settings offer; the first is the default.
var claudeModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}

type settingsStore struct {
	path string
	mu   sync.Mutex
	cur  settings
}

func loadSettings() *settingsStore {
	st := &settingsStore{path: configPath("settings.json"), cur: settings{ClaudeModel: claudeModels[0]}}
	loadJSON(st.path, &st.cur)
	if !slices.Contains(claudeModels, st.cur.ClaudeModel) {
		st.cur.ClaudeModel = claudeModels[0]
	}
	return st
}

func (st *settingsStore) get() settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur
}

func (st *settingsStore) set(v settings) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cur = v
	saveJSON(st.path, v)
}

type settingsResponse struct {
	settings
	Models []string `json:"models"`
	Claude string   `json:"claude"` // path of the Claude Code CLI, or "" when it isn't installed
}

func (s *server) settingsResponse() settingsResponse {
	r := settingsResponse{settings: s.settings.get(), Models: claudeModels}
	if s.cli != nil {
		r.Claude = s.cli.bin
	}
	return r
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settingsResponse())
}

func (s *server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var v settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&v); err != nil {
		httpError(w, errors.New("bad request body"), http.StatusBadRequest)
		return
	}
	if !slices.Contains(claudeModels, v.ClaudeModel) {
		httpError(w, errors.New("unknown model"), http.StatusBadRequest)
		return
	}
	s.settings.set(v)
	s.regroup() // apply at once: groups and titles follow the new settings
	writeJSON(w, s.settingsResponse())
}
