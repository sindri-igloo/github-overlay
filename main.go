package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Shared with the browser extension, which loads them from its own folder.
//
//go:embed extension/ui.js
var uiJS []byte

//go:embed extension/ui.css
var uiCSS []byte

type server struct {
	client     *client
	bots       map[string]bool
	closedDays int

	candidates candidateCache
	groups     *groupStore
	titles     *titler  // nil when the Claude CLI is off
	grouping   *grouper // nil when Claude grouping is off

	fetchMu sync.Mutex // serializes fetches
	mu      sync.RWMutex
	snap    *Snapshot

	interval    time.Duration
	fetchedAt   time.Time // last successful fetch
	pausedUntil time.Time // no fetches before this (rate limited or saving budget)
	lastView    time.Time // last time the page asked for data
}

// lowBudget is the GraphQL points per hour left for gh and other tools: below
// it the overlay stops fetching until the hourly reset.
const lowBudget = 500

// refresh fetches from GitHub unless the data is younger than minAge or
// fetching is paused. One refresh costs roughly 130 GraphQL points.
func (s *server) refresh(minAge time.Duration) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	s.mu.RLock()
	skip := (s.snap != nil && time.Since(s.fetchedAt) < minAge) || time.Now().Before(s.pausedUntil)
	s.mu.RUnlock()
	if skip {
		return
	}
	if left, reset := s.client.budget(); left >= 0 && left < lowBudget && reset.After(time.Now()) {
		s.fail(fmt.Errorf("paused to leave GitHub API budget for other tools (%d points left); resuming at %s",
			left, reset.Local().Format("15:04")), reset)
		return
	}

	res, err := s.client.fetch(s.closedDays)
	if err != nil {
		var rl *RateLimitError
		until := time.Time{}
		if errors.As(err, &rl) {
			until = rl.Until
		}
		s.fail(err, until)
		return
	}

	left, _ := s.client.budget()
	log.Printf("refreshed (cost %d, %d GraphQL points left this hour)", res.Cost, left)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchedAt = time.Now()
	snap := buildSnapshot(res, s.bots)
	snap.FetchedAt = s.fetchedAt.UTC().Format(time.RFC3339)
	if len(res.Errors) > 0 {
		snap.Error = res.Errors[0]
	}
	snap.regroup(s.grouping.groups(snap))
	s.snap = snap
	s.grouping.update(snap)
	if !s.grouping.busy(snap) {
		s.titles.update(snap)
	}
}

// regroup applies a grouping that arrived after the snapshot was built, then
// titles the groups it made.
func (s *server) regroup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		return
	}
	s.snap.regroup(s.grouping.groups(s.snap))
	if !s.grouping.busy(s.snap) {
		s.titles.update(s.snap)
	}
}

// fail keeps the last good data, surfaces err, and pauses fetching until
// until (zero for no pause).
func (s *server) fail(err error, until time.Time) {
	log.Printf("refresh: %v", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		s.snap = &Snapshot{PRs: map[string]*PR{}, Sections: map[string][]string{}, Bundles: map[int][]string{}}
	}
	s.snap.Error = err.Error()
	s.pausedUntil = until
}

// poll refreshes on the interval, but only while the page is open: the page
// asks for data every 30s, so a quiet minute means nobody is looking.
func (s *server) poll() {
	for range time.Tick(s.interval) {
		s.mu.RLock()
		watched := time.Since(s.lastView) < time.Minute
		s.mu.RUnlock()
		if watched {
			s.refresh(s.interval / 2)
		}
	}
}

func (s *server) handlePRs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	idle := time.Since(s.lastView) > time.Minute
	s.lastView = time.Now()
	empty := s.snap == nil
	stale := time.Since(s.fetchedAt) > s.interval
	s.mu.Unlock()
	switch {
	case empty:
		s.refresh(0)
	case r.URL.Query().Get("refresh") == "1":
		s.refresh(20 * time.Second) // ignore repeated clicks
	case idle && stale:
		go s.refresh(s.interval) // page just reopened; it re-polls and gets the result
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if s.snap == nil {
		json.NewEncoder(w).Encode(s.snap)
		return
	}
	json.NewEncoder(w).Encode(struct {
		*Snapshot
		GroupNames  []groupName    `json:"groupNames"`
		GroupTitles map[int]string `json:"groupTitles"` // by bundle id
	}{s.snap, s.groups.list(), s.titles.titles(s.snap)})
}

type serveOpts struct {
	addr           string
	interval       time.Duration
	closedDays     int
	bots           string
	claude         string
	claudeModel    string
	claudeGrouping bool
	args           []string // the raw flags, recorded in the state file for update
}

// serveFlags registers the server flags on fs. start and serve share them so
// start can forward its arguments to the background serve process verbatim.
func serveFlags(fs *flag.FlagSet) *serveOpts {
	o := &serveOpts{}
	fs.StringVar(&o.addr, "addr", "prs.localhost:7777", "listen address")
	fs.DurationVar(&o.interval, "interval", time.Minute, "GitHub poll interval while the page is open")
	fs.IntVar(&o.closedDays, "closed-days", 7, "show PRs merged/closed in the last N days")
	fs.StringVar(&o.bots, "bots", "igloo-code-reviewer,chatgpt-codex-connector,codex,copilot-pull-request-reviewer",
		"comma-separated logins to treat as bots (in addition to GitHub App accounts)")
	fs.StringVar(&o.claude, "claude", "claude",
		"Claude Code CLI that titles and groups related PRs; empty turns both off")
	fs.StringVar(&o.claudeModel, "claude-model", "claude-opus-5-5", "model the Claude CLI uses")
	fs.BoolVar(&o.claudeGrouping, "claude-grouping", true, "let Claude group related open PRs that don't link to each other")
	return o
}

// listen binds addr. A "localhost" or "*.localhost" host binds both loopback
// addresses: macOS resolves *.localhost to ::1 only, while browsers resolve it
// to 127.0.0.1 themselves.
func listen(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if h := strings.ToLower(host); h != "localhost" && !strings.HasSuffix(h, ".localhost") {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}
	l4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	ls := []net.Listener{l4}
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
		ls = append(ls, l6)
	}
	return ls, nil
}

// guard admits only API calls made by the overlay page itself. Any site open
// in the browser can send requests to prs.localhost, and the API acts with
// your GitHub token. Checking Host stops DNS rebinding; the custom header
// can't be sent cross-origin without a CORS preflight, which this server
// never approves.
func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalHost(r.Host) || r.Header.Get("X-Overlay") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func isLocalHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func serve(o *serveOpts, pidFile bool) error {
	botSet := map[string]bool{}
	for _, b := range strings.Split(o.bots, ",") {
		if b = strings.TrimSpace(strings.ToLower(b)); b != "" {
			botSet[b] = true
		}
	}
	s := &server{client: newClient(), bots: botSet, closedDays: o.closedDays, interval: o.interval,
		candidates: candidateCache{entries: map[string]cachedCandidates{}}, groups: loadGroups(),
	}
	cli := newClaudeCLI(o.claude, o.claudeModel)
	s.titles = newTitler(cli)
	if o.claudeGrouping {
		s.grouping = newGrouper(cli, s.regroup)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/prs", guard(s.handlePRs))
	mux.HandleFunc("GET /api/reviewers", guard(s.handleCandidates))
	mux.HandleFunc("POST /api/reviewers", guard(s.handleSetReviewers))
	mux.HandleFunc("POST /api/land", guard(s.handleLand))
	mux.HandleFunc("PUT /api/groups", guard(s.handleSetGroup))
	mux.HandleFunc("GET /ui.js", asset(uiJS, "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /ui.css", asset(uiCSS, "text/css; charset=utf-8"))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	srv := &http.Server{Handler: mux}
	ls, err := listen(o.addr)
	if err != nil {
		return err
	}

	if pidFile {
		if err := writeState(state{PID: os.Getpid(), Addr: o.addr, Version: version(), Args: o.args}); err != nil {
			return err
		}
		defer removeState()
	}

	go s.poll()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("github-overlay listening on http://%s", o.addr)
	errc := make(chan error, len(ls))
	for _, l := range ls {
		go func() { errc <- srv.Serve(l) }()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("github-overlay stopped")
	return nil
}

func asset(b []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(b)
	}
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
