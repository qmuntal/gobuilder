# gobuilder

## GitHub Actions

This repository has two workflows:

- `scheduler` is declared with a 3-minute cron and can also be started manually. It runs the Go scheduler in `cmd/scheduler`. GitHub may delay or throttle scheduled workflows, and its documented minimum interval is 5 minutes.
- `builder` is started by the scheduler and currently runs `go version`.

The `builder` workflow declares `permissions: {}` so the default `GITHUB_TOKEN` has no repository permissions. The `scheduler` workflow grants `contents: read` to check out the Go source and `actions: write` to dispatch `builder` in the same repository with the built-in token.

The scheduler counts queued Go LUCI builds by querying Buildbucket, the backend used by https://ci.chromium.org/ui/p/golang. It counts `SCHEDULED` builds in the `golang/ci` builder bucket, then starts up to the configured maximum number of `builder` workflow runs.

- `--max-jobs`: maximum builder runs started per scheduler run, default `5`
- `--workflow`: GitHub Actions workflow file or ID to dispatch, default `builder.yml`
- `--buildbucket-project`: LUCI project to query, default `golang`
- `--buildbucket-bucket`: LUCI bucket to query, default `ci`
- `--buildbucket-builder-name`: optional builder-name substring to count; empty counts all builders