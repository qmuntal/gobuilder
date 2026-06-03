# gobuilder

## GitHub Actions

This repository has two workflows:

- `scheduler` is declared with a 3-minute cron and can also be started manually. It runs the Go scheduler in `cmd/scheduler`. GitHub may delay or throttle scheduled workflows, and its documented minimum interval is 5 minutes.
- `builder` is started by the scheduler and currently runs `go version`.

The `builder` workflow declares `permissions: {}` so the default `GITHUB_TOKEN` has no repository permissions. The `scheduler` workflow grants `contents: read` to check out the Go source and `actions: write` to dispatch `builder` in the same repository with the built-in token.

The scheduler uses a fixed queue that returns `2` queued jobs by default. Configure it with workflow inputs, repository variables, or command flags:

- `--fixed-jobs`: queue depth returned by the fixed queue, default `2`
- `--max-jobs`: maximum builder runs started per scheduler run, default `5`