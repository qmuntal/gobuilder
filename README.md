# gobuilder

## GitHub Actions

This repository has two workflows:

- `scheduler` is declared with a 5-minute cron and can also be started manually. It runs the Go scheduler in `cmd/scheduler`. GitHub may delay or throttle scheduled workflows.
- `builder-windows-arm64` is started by the scheduler and starts a LUCI Swarming bot.

The `builder-windows-arm64` workflow grants only `id-token: write` to request GitHub OIDC tokens; its default `GITHUB_TOKEN` has no repository permissions. It prepares the authentication workspace directory without checking out the repository, and runner setup remains inline in the workflow. The `scheduler` workflow grants `contents: read` to check out the Go source and `actions: write` to dispatch the builder workflow in the same repository with the built-in token.

The `scheduler` workflow uses a single GitHub Actions concurrency group, so overlapping scheduled or manual scheduler runs do not allocate builder slots at the same time.

The scheduler counts queued Go LUCI builds by querying Buildbucket, the backend used by https://ci.chromium.org/ui/p/golang. It counts `SCHEDULED` builds in the `golang/ci` builder bucket, then starts up to the configured maximum number of `builder-windows-arm64` workflow runs.

- `--max-jobs`: maximum active builder workflow slots, default `5`
- `--workflow`: required GitHub Actions workflow file or ID to dispatch
- `--buildbucket-project`: LUCI project to query, default `golang`
- `--buildbucket-bucket`: LUCI bucket to query, default `ci`
- `--buildbucket-builder-name`: optional builder-name substring to count; empty counts all builders

Before dispatching, the scheduler counts active `builder-windows-arm64` workflow runs in GitHub Actions and reads their stable `bot_index` slots from the workflow run name. It starts only the remaining capacity up to `--max-jobs` and passes each new workflow the first free two-digit slot. The builder workflow uses that index as the suffix of the LUCI composite bot ID, keeping the bot pages stable in the LUCI UI while still allowing multiple concurrent slots. If an active workflow run does not expose a parseable slot, the scheduler dispatches no new builders rather than risk reusing an occupied bot ID.

Manual `builder-windows-arm64` workflow dispatches default to slot `99`; scheduler-dispatched runs use the first free slot in the configured active range.

Design details are documented in [DESIGN.md](DESIGN.md). Security hardening details are documented in [DESIGN-SECURITY.MD](DESIGN-SECURITY.MD).

## LUCI builder setup

The `builder-windows-arm64` workflow follows the Go dashboard builder setup by minting a LUCI machine token with `luci_machine_tokend` and starting the Swarming bot with `bootstrapswarm`. Before it can work, the builder must be approved and defined by the Go team in LUCI.

The `builder-windows-arm64` workflow runs on a Windows ARM64 runner. It downloads `luci_machine_tokend.exe` and `bootstrapswarm.exe` from `go-builder-data`, matching the Azure Windows ARM64 setup in `golang/build`. The Swarming bot handles CIPD-managed payloads after it starts.

The builder uses GitHub OIDC and Google Cloud Workload Identity Federation (WIF) with service account impersonation instead of a bot certificate/private key. `google-github-actions/auth` exports Application Default Credentials (ADC), which `luci_machine_tokend -google-auth` uses to call `MintMachineTokenForIdentity`. The machine token is written to `C:\luci_machine_tokend\token.json`, which is `bootstrapswarm`'s default Windows token path, and the local `swarming` user is granted read access to it. ADC variables are not copied into the bot's Machine-scope environment, and the authentication action removes its generated credentials file at the end of the job.

The pinned authentication action supports use without repository checkout: it needs the `GITHUB_WORKSPACE` directory to write its credentials file, not repository contents. It may print an informational notice about an empty workspace; that does not prevent credentials-file creation.

The workflow registers each GitHub Actions run as a LUCI composite bot ID of the form `windows-arm64-azure-qmuntal--NN`, where `NN` is the stable `bot_index` slot. LUCI Swarming uses the suffix after `--` as the bot slot identifier, while the host identity used for bot auth and bot group lookup is the base `windows-arm64-azure-qmuntal`. The requested machine FQDN is therefore `windows-arm64-azure-qmuntal.bots.golang.org`, without a slot suffix.

Required repository variables:

- `GCP_WIF_PROVIDER`: full provider resource name, `projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL/providers/PROVIDER`
- `LUCI_BOT_SERVICE_ACCOUNT`: dedicated Google service account email authorized to mint tokens for this builder host

No bot certificate or private-key repository secret is required.

### WIF rollout prerequisites

This workflow migration requires [luci-go CL 8549593](https://chromium-review.googlesource.com/c/infra/luci/luci-go/+/8549593) or equivalent deployed support. It is not enabled by adding repository variables alone:

1. Deploy compatible machine-token verifiers, the Token Server RPC and policy importer, and the audit-schema update. Any remaining legacy Python token consumers need equivalent verifier support.
2. Configure a WIF provider restricted to the trusted repository/owner IDs, workflow, branch and event, and grant that workload `roles/iam.workloadIdentityUser` on only the dedicated service account. The account does not need GCP resource roles merely to authenticate to Token Server.
3. Configure a Token Server `machine_token_identity_rules` entry authorizing that service account for the exact machine FQDN above, with a 3600-second token lifetime.
4. Configure the Swarming bot group with `require_luci_machine_token { service_account: "ACCOUNT_EMAIL" }`. Do not combine `service_account` and `ca_id` in one entry; use separate entries during migration if needed.
5. Publish a verified Windows ARM64 `luci_machine_tokend.exe` containing `-google-auth` and `-machine-fqdn`, then update `LUCI_MACHINE_TOKEND_SHA256` and its download URL if necessary.

The checked-in token-daemon hash still identifies the older binary. It is deliberately retained rather than replaced with an unverified hash. The workflow checks the downloaded binary's capabilities and stops with an explicit error until this pin is updated. `bootstrapswarm.exe` and its hash do not need to change.

After a successful cutover, remove the obsolete `LUCI_BOT_CERT_PEM` variable and `LUCI_BOT_KEY_PEM` secret. Generated `gha-creds-*.json` files are ignored by Git and must not be published as artifacts.

The machine token must remain valid through polling, execution and result reporting. The existing 60-minute bot-step timeout does not refresh credentials; leave expiry headroom or add a renewal mechanism before extending the usable lifetime.

The runner OS/architecture must match the LUCI builder you register.
