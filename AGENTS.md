# Agents Operating Guide

A local dashboard of your GitHub pull requests: one Go binary (`main.go`,
`github.go`, `model.go`, `reviewers.go`, `cli.go`) that embeds the page
(`index.html`, vanilla JS, no build step). See README.md for usage.

This repo is public. Commits, PR titles and PR descriptions must not name an
employer, its repos or its people; use generic examples (`feat(billing): …`).

## Commit messages

Conventional Commits, checked by commitlint in CI (`.commitlintrc.js`).

```
<type>(<scope>): <description>

<body>

<footer>
```

- **type**, lower-case, one of `build`, `chore`, `ci`, `docs`, `feat`,
  `fix`, `perf`, `refactor`, `revert`, `style`, `test`.
- **scope** is optional; name the area: `ui`, `server`, `cli`, `github`,
  `reviewers`, `groups`.
- **description** starts with a lower-case verb and has no final period:
  `feat(ui): name connected groups`, not `Feat: Named groups.`
- Several kinds of change in one commit: pick the dominant type for the
  header and list the rest in the body.
- Body and footer each follow one blank line. Bullets (`*`) are welcome;
  aim for ~72 characters per line. The body says why, not what the diff shows.

## Pull requests

- **Title** uses the same format as a commit header, and CI checks it:
  `feat(reviewers): request reviewers on several PRs at once`. A one-commit
  PR takes its commit's header.
- **Description**: a few bullets on what changed and why, then how it was
  verified. Link related PRs under `Related:` with full URLs.
- Before pushing: `gofmt -l .` prints nothing, `go vet ./...` and
  `go build ./...` pass.
