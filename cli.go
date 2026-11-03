package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

const usage = `github-overlay — local dashboard for your GitHub pull requests

Usage:
  github-overlay <command> [flags]

Commands:
  start     Start the server in the background
  stop      Stop the background server
  restart   Stop, then start with the given flags
  status    Show whether the server is running and where
  open      Open the dashboard in the browser (starts the server if needed)
  logs      Print the server log (-f to follow)
  update    Install the latest main from GitHub and restart the server
  version   Print the version of this binary
  serve     Run the server in the foreground

Server flags (start, restart, serve, open):
  -addr string         listen address (default "prs.localhost:7777")
  -interval duration   poll interval while the page is open (default 1m0s)
  -closed-days int     show PRs merged/closed in the last N days (default 7)
  -bots string         extra logins to treat as bots, comma-separated

State and logs live in %s
`

type state struct {
	PID     int      `json:"pid"`
	Addr    string   `json:"addr"`
	Version string   `json:"version,omitempty"` // of the binary the daemon runs, which may be older than the one on disk
	Args    []string `json:"args,omitempty"`    // flags it was started with, so update can restart it the same way
}

const modulePath = "github.com/sindri-igloo/github-overlay"

// version reports the module version for a `go install ...@vX` build, else
// the VCS revision for a build from a checkout.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, kv := range info.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

func stateDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "github-overlay")
}

func statePath() string { return filepath.Join(stateDir(), "server.json") }
func logPath() string   { return filepath.Join(stateDir(), "server.log") }

func writeState(s state) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(statePath(), b, 0o644)
}

func removeState() { os.Remove(statePath()) }

// readState returns the running server's state, or nil if none is running.
// A state file left behind by a dead process is cleaned up.
func readState() *state {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.PID <= 0 || syscall.Kill(s.PID, 0) != nil {
		removeState()
		return nil
	}
	return &s
}

// loopbackAddr maps a localhost name (localhost, *.localhost) to 127.0.0.1,
// since the server binds loopback directly and DNS may not agree: Tailscale's
// MagicDNS breaks *.localhost lookups.
func loopbackAddr(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, false
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return net.JoinHostPort("127.0.0.1", port), true
	}
	return addr, false
}

// resolvesToLoopback reports whether the system resolver maps host to
// loopback addresses only.
func resolvesToLoopback(host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, s := range ips {
		if ip := net.ParseIP(s); ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

func healthy(addr string) bool {
	// Dial loopback directly for localhost names; the URL and Host header
	// keep the name.
	t := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: time.Second}
	t.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		if lb, ok := loopbackAddr(a); ok {
			a = lb
		}
		return d.DialContext(ctx, network, a)
	}
	c := &http.Client{Timeout: time.Second, Transport: t}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Printf(usage, stateDir())
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "start":
		return cmdStart(rest)
	case "stop":
		return cmdStop()
	case "restart":
		if err := cmdStop(); err != nil {
			return err
		}
		return cmdStart(rest)
	case "status":
		return cmdStatus()
	case "update":
		return cmdUpdate()
	case "version", "-v", "--version":
		fmt.Println(version())
		return nil
	case "open":
		return cmdOpen(rest)
	case "logs":
		return cmdLogs(rest)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		o := serveFlags(fs)
		fs.Parse(rest)
		if s := readState(); s != nil {
			return fmt.Errorf("a background server is already running (pid %d on %s); run `github-overlay stop` first", s.PID, s.Addr)
		}
		return serve(o, false)
	case "_daemon": // internal: the detached process spawned by start
		fs := flag.NewFlagSet("_daemon", flag.ExitOnError)
		o := serveFlags(fs)
		fs.Parse(rest)
		o.args = rest
		return serve(o, true)
	case "help", "-h", "--help":
		fmt.Printf(usage, stateDir())
		return nil
	default:
		fmt.Fprintf(os.Stderr, usage, stateDir())
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	o := serveFlags(fs)
	fs.Parse(args)

	if s := readState(); s != nil {
		fmt.Printf("already running (pid %d) at http://%s\n", s.PID, s.Addr)
		return nil
	}
	if healthy(o.addr) {
		return fmt.Errorf("%s is already serving something; stop it or pass -addr", o.addr)
	}
	if _, err := ghToken(); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	c := exec.Command(self, append([]string{"_daemon"}, args...)...)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal closing
	if err := c.Start(); err != nil {
		return err
	}
	pid := c.Process.Pid
	c.Process.Release()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if healthy(o.addr) {
			fmt.Printf("started (pid %d) at http://%s\n", pid, o.addr)
			return nil
		}
		if syscall.Kill(pid, 0) != nil {
			return fmt.Errorf("server exited during startup; see `github-overlay logs`")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server did not become healthy within 10s; see `github-overlay logs`")
}

func cmdStop() error {
	s := readState()
	if s == nil {
		fmt.Println("not running")
		return nil
	}
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", s.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(s.PID, 0) != nil {
			removeState()
			fmt.Printf("stopped (pid %d)\n", s.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(s.PID, syscall.SIGKILL)
	removeState()
	fmt.Printf("killed (pid %d) after it ignored SIGTERM\n", s.PID)
	return nil
}

func cmdStatus() error {
	s := readState()
	if s == nil {
		fmt.Println("not running")
		return nil
	}
	health := "healthy"
	if !healthy(s.Addr) {
		health = "not responding"
	}
	running := s.Version
	if running == "" {
		running = "unknown" // started by a build that didn't record it
	}
	fmt.Printf("running (pid %d) at http://%s — %s, version %s\n", s.PID, s.Addr, health, running)
	if v := version(); v != running {
		fmt.Printf("installed: %s; run github-overlay restart\n", v)
	}
	fmt.Printf("logs: %s\n", logPath())
	return nil
}

// goInstallDir is where `go install` puts binaries: GOBIN, else the first
// GOPATH entry's bin.
func goInstallDir(goBin string) (string, error) {
	out, err := exec.Command(goBin, "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 2 {
		if lines[0] != "" {
			return lines[0], nil
		}
		if p := filepath.SplitList(lines[1]); len(p) > 0 {
			return filepath.Join(p[0], "bin"), nil
		}
	}
	return "", errors.New("could not determine where go installs binaries")
}

// cmdUpdate installs main from GitHub and restarts the daemon with its saved
// flags. The restart runs the new binary: this process is the old one.
func cmdUpdate() error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("update needs Go installed and on PATH (https://go.dev/dl)")
	}
	fmt.Printf("current version: %s\n", version())

	c := exec.Command(goBin, "install", modulePath+"@main")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	// The module proxy caches @main for a while; direct sees a fresh merge at once.
	c.Env = append(os.Environ(), "GOPROXY=direct")
	if err := c.Run(); err != nil {
		return fmt.Errorf("go install: %w", err)
	}

	dir, err := goInstallDir(goBin)
	if err != nil {
		return err
	}
	installed := filepath.Join(dir, "github-overlay")
	// The new binary only reaches the daemon if it is the one `start` runs.
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	inst, ierr := filepath.EvalSymlinks(installed)
	if err != nil || ierr != nil || self != inst {
		fmt.Printf("warning: the updated binary is at %s but this command runs %s; not restarting.\n", installed, self)
		return nil
	}

	s := readState()
	if s == nil {
		fmt.Printf("installed %s. Start it with: github-overlay start\n", installed)
		return nil
	}
	if err := cmdStop(); err != nil {
		return err
	}
	for _, args := range [][]string{append([]string{"start"}, s.Args...), {"version"}} {
		n := exec.Command(inst, args...)
		n.Stdout, n.Stderr = os.Stdout, os.Stderr
		if err := n.Run(); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
	}
	return nil
}

func cmdOpen(args []string) error {
	s := readState()
	if s == nil {
		if err := cmdStart(args); err != nil {
			return err
		}
		if s = readState(); s == nil {
			return errors.New("server started but its state file is missing")
		}
	}
	url := "http://" + s.Addr
	// Browsers that use the system resolver (Safari) can't reach a *.localhost
	// name when VPN or Tailscale DNS hijacks it. Page state (tab, filters) is
	// saved per origin, so it won't carry over between the two URLs.
	if lb, ok := loopbackAddr(s.Addr); ok {
		host, _, _ := net.SplitHostPort(s.Addr)
		if !resolvesToLoopback(host) {
			url = "http://" + lb
			fmt.Printf("%s doesn't resolve to this machine (VPN or Tailscale DNS?); opening %s\n", host, url)
		}
	}
	return exec.Command("open", url).Run()
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow the log")
	fs.Parse(args)
	tailArgs := []string{"-n", "100"}
	if *follow {
		tailArgs = append(tailArgs, "-f")
	}
	c := exec.Command("tail", append(tailArgs, logPath())...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
