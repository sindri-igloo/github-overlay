package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// claudeCLI runs the local Claude Code CLI for one-shot questions about PRs,
// under the user's own Claude login.
type claudeCLI struct {
	bin   string
	model func() string // read per call, so a settings change applies to the next one
	dir   string        // empty working directory, so no project files or CLAUDE.md get read
}

const claudeTimeout = 2 * time.Minute

// newClaudeCLI returns nil, which makes the Claude features unavailable, when
// bin is empty or not installed.
func newClaudeCLI(bin string, model func() string) *claudeCLI {
	if bin == "" {
		return nil
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		log.Printf("claude features unavailable: %s not found", bin)
		return nil
	}
	c := &claudeCLI{bin: path, model: model}
	if dir, err := os.UserCacheDir(); err == nil {
		c.dir = filepath.Join(dir, "github-overlay", "claude")
		os.MkdirAll(c.dir, 0o755)
	}
	return c
}

// ask sends input under the system prompt and returns the reply: the text,
// or with a JSON schema the structured output.
func (c *claudeCLI) ask(system, input, schema string) (string, json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()
	// No tools, MCP servers, user settings or saved session: just the prompt.
	args := []string{"-p", "--model", c.model(), "--effort", "low",
		"--tools", "", "--strict-mcp-config", "--setting-sources", "project",
		"--no-session-persistence", "--disable-slash-commands",
		"--output-format", "json", "--system-prompt", system}
	if schema != "" {
		args = append(args, "--json-schema", schema)
	}
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.dir
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	name := filepath.Base(c.bin)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	var res struct {
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
		IsError    bool            `json:"is_error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", nil, fmt.Errorf("reading %s output: %w", name, err)
	}
	if res.IsError {
		return "", nil, fmt.Errorf("%s: %s", name, res.Result)
	}
	return res.Result, res.Structured, nil
}

// signature identifies a set of PRs by their keys and titles, so cached
// answers about it go stale when a PR joins, leaves or is retitled.
func signature(prs []*PR) string {
	lines := make([]string, len(prs))
	for i, p := range prs {
		lines[i] = strings.ToLower(p.Key) + "\t" + p.Title
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:12])
}

// describe writes one PR for a prompt: where it lives, its title and
// branches, and the start of its description.
func describe(b *strings.Builder, id string, p *PR, maxBody int) {
	fmt.Fprintf(b, "%s %s #%d %q (branch %s into %s)\n", id, p.Repo, p.Number, p.Title, p.Head, p.Base)
	if body := strings.Join(strings.Fields(p.Body), " "); body != "" {
		if len(body) > maxBody {
			body = body[:maxBody] + "…"
		}
		fmt.Fprintf(b, "  %s\n", strings.ToValidUTF8(body, ""))
	}
}

// saveJSON writes v to path via a temp file. Failures only cost a re-ask
// after a restart, so they're logged and dropped.
func saveJSON(path string, v any) {
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		log.Printf("saving %s: %v", filepath.Base(path), err)
	}
}

// loadJSON reads path into v, leaving v alone when the file is missing or bad.
func loadJSON(path string, v any) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Printf("ignoring %s: %v", path, err)
	}
}

func configPath(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "github-overlay", name)
}
