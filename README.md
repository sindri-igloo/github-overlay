# github-overlay

A local dashboard for your GitHub pull requests: which ones have a reviewer
requested, unresolved review threads (people and bots counted separately),
changes requested, CI status with links to failing checks, merge conflicts,
and bundles of PRs that reference each other.

It's a single Go binary with the page embedded. It authenticates with your
`gh` CLI token and polls GitHub's GraphQL API every minute while the page
is open and every 5 minutes while it's closed, so it opens close to current.
It pauses when the budget runs low or GitHub rate-limits it.

A refresh costs about 11 of your 5,000 hourly GraphQL points: 3 each for your
open PRs and your review requests, 1 for recently closed PRs, and about 4 more
to look up who started the unresolved review threads (skipped when there are
none). GitHub prices a query by the page sizes it asks for, not by what comes
back, so the cost barely moves with how many PRs you have. At the default
`-interval 1m` that is about 660 points an hour (13% of the budget) while the page is visible, and
at the default `-idle-interval 5m` about 130 (3%) while it's hidden or closed. Longer intervals
lower it proportionally; `-idle-interval 0` fetches nothing while the page is closed.

## Install

```sh
go install github.com/sindri-igloo/github-overlay@latest   # puts github-overlay in $(go env GOPATH)/bin
ln -s "$(go env GOPATH)/bin/github-overlay" ~/.local/bin/   # if GOPATH/bin isn't on PATH
```

Requires `gh auth login`. For development, `go install .` from a checkout
works too, or use the Makefile, which builds the checkout before each command:

```sh
make restart                         # rebuild and restart the background server
make start ARGS='-closed-days 14'    # pass server flags with ARGS
make stop / status / open / logs / serve
make check                           # gofmt, go vet, staticcheck, build and test, as in CI
```

## Usage

```sh
github-overlay start     # run in the background on http://prs.localhost:7777
github-overlay open      # open in the browser (starts the server if needed)
github-overlay status
github-overlay logs -f
github-overlay restart -closed-days 14
github-overlay stop
github-overlay serve     # run in the foreground
github-overlay update    # install the latest main from GitHub and restart the server
github-overlay version
```

Flags for `start`, `restart`, `open` and `serve`:

| Flag | Default | |
|---|---|---|
| `-addr` | `prs.localhost:7777` | listen address; a `*.localhost` name listens on both loopback addresses |
| `-interval` | `1m` | poll interval while the page is open and visible |
| `-idle-interval` | `5m` | poll interval while the page is closed or in a background tab; `0` fetches nothing then |
| `-closed-days` | `7` | how far back "Recently closed" goes |
| `-bots` | `igloo-code-reviewer,chatgpt-codex-connector,codex,copilot-pull-request-reviewer` | logins counted as bots, in addition to GitHub App accounts |
| `-claude` | `claude` | Claude Code CLI for the opt-in Claude features (see below); `''` hides them |

The CLI doesn't depend on DNS for `*.localhost`. If a browser can't reach http://prs.localhost:7777 (some VPNs and Tailscale DNS break `*.localhost` for browsers that use system DNS), use http://127.0.0.1:7777, or start with `-addr 127.0.0.1:7777`.

The PID file and log are kept in `~/Library/Caches/github-overlay/`.

## Tabs

- **Mine**: your open PRs and drafts, filterable by needs action, waiting on
  review, no reviewer, ready to merge, and drafts. Tick rows (or a connected group's Select) to request
  reviewers on several PRs at once. Approved PRs can be added to the merge
  queue, or merged where the repo has none, per row or for the selected PRs
  (merging asks for a second click).
- **To review**: open PRs where you or one of your teams is a requested reviewer.
- **Closed**: your PRs merged or closed in the last `-closed-days` days.

Each row shows CI (hover for failing checks), reviewer avatars ringed and
badged by state (dotted yellow requested, green approved, red changes requested,
gray commented; a PR with changes requested gets a red edge and names who asked,
until they are asked to review again), unresolved threads (people in red, bots as a gray `+N`), and age.
Press `r` to refresh.

When your PRs span more than one owner, chips next to the title narrow every
tab to the orgs you pick. The choice is remembered in the browser.

## Connected PRs

Unconnected PRs share one box at the top; below it, open PRs are grouped when one references another, either in its description
(a URL, `owner/repo#12`, `repo #12` for a repo under the same owner, or `#12`)
or through GitHub's cross-reference events. Closed PRs don't join bundles, but
they show as "Linked:" chips on the open PRs that reference them.

Two opt-in features use the [Claude Code CLI](https://code.claude.com), if it's installed and logged in.
Both are off until you turn them on under ⚙ (Settings) in the page header, which also picks the model
(default `claude-opus-5-5`):

- **Group related PRs**: per tab (Mine, To review), Claude reads the open PRs' titles, branches, links and
  the start of their descriptions, and groups them by the piece of work they belong to. Links are hints:
  PRs that only mention each other for context can end up in separate groups, and PRs that never
  reference each other can share one. A merged PR linked to open ones joins one of their groups. PRs
  opened since Claude last answered keep their link groups until it answers again. The "How to group"
  box in Settings holds the instruction Claude follows; edit it to change how it groups, or reset it.
- **Title groups**: each group gets a short title such as "Overdue invoice reminders".

While a feature is on, the server runs the CLI in the background, one call at a time, with no tools,
settings or MCP servers, and asks again only when a tab's open PRs or a group's PRs (or their titles)
change, or the grouping instruction does. Answers are cached in `pr-groups.json` and `group-titles.json`, and the toggles in
`settings.json`, next to `groups.json`. Turning a feature on sends those PR details to Anthropic under
your Claude account; turning it off stops the calls and puts the plain groups and names back at once.

Until a title arrives, or without the CLI, a group is named after the conventional-commit scopes in its
PR titles (up to three, most shared first), or the title of the bottom PR of the stack when no title has a scope. Click the name to rename the group;
the name is kept in `groups.json` under your config directory (`~/Library/Application Support/github-overlay/` on macOS), and clearing it reverts to the automatic one.

Each search returns at most 50 PRs.

## Browser extension

`extension/` shows the same dashboard in place of GitHub's own pull request
inbox at https://github.com/pulls/inbox, styled with GitHub's theme colors so
it follows light, dark and dimmed. It reads from the local server, so keep
`github-overlay start` running. A link above the dashboard switches back to
GitHub's inbox (remembered per browser). It expects the server on the default
`prs.localhost:7777`; for another `-addr`, change `SERVER` in
`extension/background.js` and `host_permissions` in `extension/manifest.json`.

The same folder works in Chrome and Firefox:

- **Chrome**: open `chrome://extensions`, turn on Developer mode, click
  "Load unpacked" and pick the `extension` folder.
- **Firefox** (128+): open `about:debugging#/runtime/this-firefox`, click
  "Load Temporary Add-on…" and pick `extension/manifest.json`. Temporary
  add-ons are removed when Firefox quits; to keep it, sign it as an unlisted
  add-on with `web-ext sign` or use Firefox Developer Edition with
  `xpinstall.signatures.required` off. If the dashboard says it can't reach
  the server, allow the extension's site access under `about:addons`.

The dashboard code (`extension/ui.js`, `extension/ui.css`) is shared: the Go
server embeds the same files for the standalone page, so changes show up in
both.
