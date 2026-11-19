# Upstream

curio is a full-history copy of newscope by Umputun (MIT). The newscope code
stays in this repository and still builds as `cmd/newscope`.

| Field | Value |
| --- | --- |
| Upstream repo | [umputun/newscope](https://github.com/umputun/newscope) |
| Upstream branch | `master` |
| Local branch | `main` |
| Remote name | `upstream` (push is disabled) |
| Recorded commit | `96b7469e` (chore(deps): bump testify to 1.11.1) |
| Last check date | 2026-11-19 |

## What is new here

The curio code lives in new files only, so upstream merges stay simple:

- `cmd/curio/`: the curio command.
- `pkg/hn/`: Hacker News front page client (points, comments).
- `pkg/curio/`: config, SQLite store, weighted criteria fit, claude CLI scorer,
  refresh service and the loopback JSON API.
- `curio.example.yml`, `UPSTREAM.md`, a section at the top of `README.md`,
  and two lines in `.gitignore`.

curio reuses newscope's article text extractor (`pkg/content`) and its Go
modules. The Go module path stays `github.com/umputun/newscope` for the same reason.

## Check and merge

1. Set up the remote once, in a new clone:

   ```sh
   git remote add upstream https://github.com/umputun/newscope
   git remote set-url --push upstream DISABLED
   ```

2. Fetch and read the new commits:

   ```sh
   git fetch upstream
   git log --oneline 96b7469e..upstream/master
   ```

3. Merge on a branch and run the tests:

   ```sh
   git switch -c upstream-sync main
   git merge upstream/master
   go test ./...
   ```

4. Conflicts can occur only in `README.md`, `.gitignore`, `go.mod`,
   `go.sum` and `vendor/`. Keep both sides. For `vendor/`, run `go mod vendor`
   after the merge.
5. Update the recorded commit and the check date in this file. Merge the
   branch into `main`.
