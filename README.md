# gobuilder

## GitHub Actions

This repository has two workflows:

- `scheduler` is declared with a 3-minute cron and can also be started manually. It runs the Go scheduler in `cmd/scheduler`. GitHub may delay or throttle scheduled workflows, and its documented minimum interval is 5 minutes.
- `builder` is started by the scheduler and starts a LUCI Swarming bot.

The `builder` workflow declares `permissions: {}` so the default `GITHUB_TOKEN` has no repository permissions. The `scheduler` workflow grants `contents: read` to check out the Go source and `actions: write` to dispatch `builder` in the same repository with the built-in token.

The scheduler counts queued Go LUCI builds by querying Buildbucket, the backend used by https://ci.chromium.org/ui/p/golang. It counts `SCHEDULED` builds in the `golang/ci` builder bucket, then starts up to the configured maximum number of `builder` workflow runs.

- `--max-jobs`: maximum builder runs started per scheduler run, default `5`
- `--workflow`: GitHub Actions workflow file or ID to dispatch, default `builder.yml`
- `--buildbucket-project`: LUCI project to query, default `golang`
- `--buildbucket-bucket`: LUCI bucket to query, default `ci`
- `--buildbucket-builder-name`: optional builder-name substring to count; empty counts all builders

## LUCI builder setup

The `builder` workflow follows the Go dashboard builder setup by minting a LUCI machine token with `luci_machine_tokend` and starting the Swarming bot with `bootstrapswarm`. Before it can work, the builder must be approved and defined by the Go team in LUCI.

The `builder` workflow runs on a Windows ARM64 runner. It downloads `luci_machine_tokend.exe` and `bootstrapswarm.exe` from `go-builder-data`, matching the Azure Windows ARM64 setup in `golang/build`. The Swarming bot handles CIPD-managed payloads after it starts.

Required repository variables:

- `LUCI_BOT_HOSTNAME`: approved bot hostname, such as `<GOOS>-<GOARCH>-<github-handle>`
- `LUCI_BOT_CERT_PEM`: certificate PEM issued by the Go team

Required repository secrets:

- `LUCI_BOT_KEY_PEM`: private key PEM generated with `genbotcert`

The runner OS/architecture must match the LUCI builder you register.
