# Design Report

This document records the operational design decisions behind `gobuilder`. Security-specific decisions are documented separately in [DESIGN-SECURITY.MD](DESIGN-SECURITY.MD).

The system provides temporary Windows ARM64 LUCI Swarming capacity for the Go project by dispatching GitHub-hosted Windows ARM64 runners when queued LUCI work exists.

## Components

- `scheduler` workflow: runs every 5 minutes and can also be started manually. It builds and runs `cmd/scheduler` on Ubuntu.
- `builder-windows-arm64` workflow: starts a Windows ARM64 runner and registers it as a LUCI Swarming bot.
- `verify-dependency-pins` workflow: checks Dependabot pull requests that update the mirrored `pywin32` requirements file.
- `cmd/scheduler`: command-line entry point for querying Buildbucket and dispatching builder workflows.
- `internal/buildbucket`: Buildbucket queue client.
- `internal/githubactions`: GitHub Actions dispatch and active-run client.
- `internal/scheduler`: queue-depth, active-run, and slot allocation logic.

## Runtime Flow

1. GitHub starts the `scheduler` workflow on the 5-minute cron or by manual dispatch.
2. The workflow checks out the repository, sets up Go, builds `cmd/scheduler`, and runs it.
3. The scheduler queries Buildbucket for `SCHEDULED` Go LUCI builds.
4. The scheduler queries GitHub Actions for active runs of the target builder workflow.
5. The scheduler computes free builder slots and dispatches one builder workflow run per selected slot.
6. Each builder workflow run validates its `bot_index`, derives its LUCI hostname, prepares the Windows runner, mints a LUCI machine token, and starts `bootstrapswarm.exe`.

## Workflow Design

### Separate Scheduler And Builder Workflows

Decision: scheduling and builder registration are split into two workflows.

Rationale: the repository should have one scheduled workflow that decides when capacity is needed, rather than one scheduled workflow per builder slot or builder type. The builder workflow stays dispatch-only and owns the platform-specific LUCI registration steps.

Result: scheduling policy is centralized in one workflow, while builder workflows remain reusable capacity units that are only requested when the scheduler decides there is queued work and available capacity.

### Scheduler Cadence

Decision: the scheduler workflow uses `*/5 * * * *`.

Rationale: GitHub scheduled workflows do not support a practical cadence below 5 minutes. The scheduler therefore treats 5 minutes as the polling granularity and accepts GitHub scheduling delay or throttling.

Result: capacity reacts within the limits of GitHub scheduled workflow delivery. Manual dispatch remains available for testing or intervention.

### Builder Workflow Dispatch Only

Decision: `builder-windows-arm64` is started by `workflow_dispatch`.

Rationale: builder runs should be created by the scheduler or by an explicit manual operation, not by pushes or pull requests.

Result: the builder workflow is an on-demand capacity unit rather than a normal CI workflow.

## Scheduler Design

### Queue Source

Decision: queue depth comes from Buildbucket `SearchBuilds` against the Go LUCI project.

Default query settings:

- Project: `golang`
- Bucket: `ci`
- Status: `SCHEDULED`
- Builder-name filter: `windows-arm64` from the scheduler workflow input

Rationale: Buildbucket is the backend for the Go LUCI builders shown in the LUCI UI, so it is the source of queued work the temporary Swarming bots are meant to serve.

Result: the scheduler scales GitHub-hosted builder capacity from LUCI queue pressure rather than GitHub-side events.

### Active Run Accounting

Decision: active GitHub Actions builder runs count against the configured maximum before new runs are dispatched.

The GitHub client counts runs in these statuses:

- `requested`
- `queued`
- `in_progress`
- `waiting`
- `pending`

Rationale: queued or waiting GitHub Actions runs already represent requested capacity. Counting only running jobs would over-dispatch while GitHub is still placing jobs.

Result: `--max-jobs` means maximum active builder workflow slots, not maximum newly dispatched jobs per scheduler tick.

### Slot Allocation

Decision: scheduler-dispatched builders use two-digit slots from `01` through `--max-jobs`.

Rationale: stable slots make LUCI bot pages easier to follow while still allowing multiple concurrent temporary bots.

Result: the scheduler chooses the first free slots and passes each one as the `bot_index` workflow input.

Allocation rules:

- No queued LUCI work means no dispatch.
- No available active-run capacity means no dispatch.
- Unknown active builder slots mean no dispatch.
- Dispatch count is `min(queue depth, max jobs - active builder runs)`.
- Each dispatched workflow receives `bot_index` formatted as two digits.

### Unknown Active Builder Slots

Decision: if an active target workflow run has an unparseable slot in its run name, the scheduler dispatches no new builders.

Rationale: the scheduler cannot know which slot that run occupies. Continuing could reuse a slot that is already active.

Result: malformed active runs temporarily block scale-up until they finish, which is preferable to slot collision.

### Dispatch Parallelism

Decision: when multiple builder runs are needed, `internal/scheduler` dispatches them concurrently and reports each successful slot.

Rationale: dispatching is network-bound and each selected slot is independent.

Result: a scheduler run can request multiple builders quickly. If one dispatch fails, successful dispatches are still counted and the first error is returned.

## LUCI Bot Naming

### Composite Bot IDs

Decision: builder runs register as `windows-arm64-azure-qmuntal--NN`.

Rationale: LUCI Swarming treats the suffix after `--` as the bot slot. The stable prefix keeps the host identity aligned with the LUCI bot certificate and bot group configuration, while the suffix distinguishes concurrent slots.

Result: LUCI UI pages are stable per slot and scheduler-dispatched runs can avoid active slot reuse.

### Manual Dispatch Slot

Decision: manual `builder-windows-arm64` dispatch defaults to slot `99`.

Rationale: manually started runs should not silently claim one of the normal low-numbered scheduler slots unless the operator chooses that explicitly.

Result: scheduler-dispatched runs use the configured range starting at `01`; ad hoc manual runs have a visible default outside the normal first few slots.

## Concurrency Design

### Scheduler Concurrency

Decision: the `scheduler` workflow uses one GitHub Actions concurrency group named `scheduler` with `cancel-in-progress: false`.

Rationale: overlapping scheduler runs could observe the same active-run state and request the same free slots. Queueing later scheduler runs preserves their work instead of cancelling them mid-flight.

Result: only one scheduler run allocates builder slots at a time.

### Builder Slot Concurrency

Decision: the builder workflow uses concurrency group `builder-windows-arm64-${{ inputs.bot_index }}` with `cancel-in-progress: false`.

Rationale: two builder runs with the same slot input should not register concurrently. Queueing is preferable to cancellation because a running builder may already be serving LUCI work.

Result: duplicate dispatches for the same slot serialize at the GitHub Actions layer.

### Scheduler And Builder Coordination

Decision: concurrency is enforced both by scheduler allocation and by GitHub Actions groups.

Rationale: scheduler allocation selects free slots, while GitHub Actions concurrency handles delayed or repeated dispatches for the same slot.

Result: the normal path avoids collisions proactively, and the workflow-level concurrency group provides a second coordination layer.

## Limits

### Maximum Jobs

Decision: `--max-jobs` defaults to `5` and must be between `0` and `99`.

Rationale: the workflow uses two-digit bot slots. `99` is the highest positive two-digit index, and `0` is useful as an explicit dry capacity setting.

Result: the scheduler refuses impossible three-digit slot plans.

### Builder Slot Input

Decision: `bot_index` must be exactly two digits and greater than zero.

Rationale: the builder workflow constructs a LUCI bot hostname from this value.

Result: malformed manual inputs fail early before token minting or Swarming startup.

### Step Timeouts

Decision: workflows use step-level timeouts for setup, build, dispatch, downloads, token minting, and builder registration.

Rationale: hosted runners should not remain stuck forever in setup or registration phases.

Result: the longest builder phase is `Register LUCI builder`, currently limited to 60 minutes.

### Pagination And Response Bounds

Decision: GitHub workflow-run pagination is capped, Buildbucket response bodies are size-limited, and repeated Buildbucket page tokens fail the scheduler run.

Rationale: API clients should have explicit bounds even when services behave unexpectedly.

Result: the scheduler fails with an error instead of looping indefinitely or reading unbounded responses.

## Dependency Decisions

### Go Toolchain

Decision: the scheduler workflow uses `actions/setup-go` with `go-version-file: go.mod`.

Rationale: `go.mod` is the repository's source of truth for the scheduler's Go version.

Result: workflow and local builds use the same declared Go toolchain line.

### Marketplace Actions

Decision: marketplace actions are pinned by commit SHA and keep a version comment next to the SHA.

Rationale: the SHA controls the exact implementation, while the comment preserves human-readable upgrade context.

Result: updates are explicit diffs in workflow files.

### `llvm-mingw`

Decision: the builder workflow installs `llvm-mingw` directly on the Windows ARM64 runner and caches `C:\llvm-mingw` by `LLVM_MINGW_VERSION`.

Rationale: Go's CGO tests on Windows need a MinGW-compatible C toolchain, and the GitHub-hosted runner does not provide the same CIPD-provisioned compiler layout as LUCI builders.

Result: the runner gets a known ARM64-compatible toolchain before Swarming starts.

### LUCI Tools

Decision: `luci_machine_tokend.exe` and `bootstrapswarm.exe` are downloaded from `go-builder-data` into `C:\golang`.

Rationale: these match the Windows ARM64 LUCI setup used by the Go builder infrastructure.

Result: the workflow follows the existing LUCI bootstrap path rather than carrying local copies of the tools.

### `pywin32`

Decision: `pywin32` is pinned by `PYWIN32_VERSION`, installed wheel-only, and mirrored in `.github/requirements-builder-windows-arm64.txt` for Dependabot.

Rationale: `bootstrapswarm.exe` needs Python Windows integration, but the builder workflow should not check out the repository just to read a dependency file. The requirements file exists as update metadata.

Result: Dependabot can discover `pywin32` updates, and `verify-dependency-pins` requires Dependabot pull requests to keep the workflow pin synchronized.

### Dependency Update Flow

Decision: runtime dependency pins live in the workflow environment; Dependabot-visible metadata may mirror those pins when required by tooling.

Rationale: the builder workflow is intentionally self-contained. Update automation should not force the credentialed builder job to depend on checked-out repository files.

Result: updates to mirrored dependencies require both metadata and runtime workflow pin changes.

## Runner Preparation

### Dedicated Swarming User

Decision: the builder workflow creates or updates a local `swarming` user, prepares `C:\b`, and starts `bootstrapswarm.exe` under that account with `Start-Process -Credential -LoadUserProfile -UseNewEnvironment`. Tool paths needed by the Swarming process are written to Machine-scope `PATH` before launch. The LUCI machine token is written to `C:\luci_machine_tokend\token.json`, which is `bootstrapswarm`'s default Windows token path, so no `LUCI_MACHINE_TOKEN` environment variable is needed.

Rationale: this approximates the Go Azure builders' interactive `swarming` login model without rebooting the GitHub-hosted runner. `-UseNewEnvironment` avoids inheriting the GitHub runner account's profile variables, while `-LoadUserProfile` initializes the target user's Windows profile state.

Result: Swarming runs with the normal Windows profile paths for the `swarming` user, while build work still happens under `C:\b`.

### Tool Cleanup

Decision: the builder workflow removes known problematic x86-64 tools (`gdb`, `patch`, and `pkg-config`) from the runner path.

Rationale: these preinstalled tools can crash or misbehave under ARM64 emulation and cause unrelated Go tests to fail.

Result: dependent Go tests skip or avoid the broken tools instead of failing because the wrong architecture binary was found.

## Known Implementation Limitations

### Only Windows ARM64 Is Supported Today

Limitation: the current implementation only supports the `builder-windows-arm64` workflow.

Reason: the workflow, tool downloads, runner label, LUCI hostname prefix, toolchain setup, and cleanup steps are all tailored to Windows ARM64.

Effect: adding another platform would require a new builder workflow and likely platform-specific dependency, runner-preparation, and LUCI registration decisions.

### No Graceful Bot Shutdown

Limitation: the workflow does not currently ask the Swarming bot to drain or shut down gracefully.

Reason: the implementation starts `bootstrapswarm.exe` and lets the GitHub Actions step bound the run. No supported local lifecycle hook is installed, and the workflow does not patch Swarming bot code.

Effect: when the `Register LUCI builder` step reaches its timeout or the workflow is cancelled, the bot process is ended by the runner environment rather than by a controlled Swarming drain sequence.

### Bot Lifetime Is Bound By The Machine Token

Limitation: increasing the `Register LUCI builder` step timeout above 60 minutes would not extend the usable bot lifetime.

Reason: the minted LUCI machine token is valid for 60 minutes.

Effect: the 60-minute timeout is an upper bound aligned with the credential lifetime, not an idle policy.

### No Local Idle Detection

Limitation: the builder workflow does not detect whether the Swarming bot is idle.

Reason: the current implementation avoids local bot hooks and source patching.

Effect: a bot can remain registered until the step timeout even if it is not doing useful work.

### Active Run State Depends On GitHub Actions Metadata

Limitation: scheduler slot allocation depends on active GitHub Actions workflow runs and parseable run names.

Reason: GitHub Actions is the only local source of already-requested builder capacity.

Effect: delayed or malformed run metadata can temporarily reduce dispatch accuracy. If a target active run has an unparseable slot, the scheduler dispatches no new builders until that run is gone.

### Scheduled Wakeups Are Best Effort

Limitation: the scheduler cannot guarantee exact 5-minute execution.

Reason: GitHub scheduled workflows can be delayed, skipped, or throttled.

Effect: capacity may lag behind LUCI queue growth during GitHub scheduling delays.

## Non-Goals

- The scheduler does not attempt to predict individual LUCI build durations.
- The builder workflow does not patch Swarming bot source or local bot zip contents.
- The builder workflow does not implement a local idle detector today; the registration step timeout bounds the run.
- The scheduler does not manage capacity outside the configured GitHub Actions workflow and LUCI builder-name filter.