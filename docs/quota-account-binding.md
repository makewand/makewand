# Account-bound quota observations

Quota snapshot version 2 separates the selected `CredentialRoot`, the opaque
`AccountRef`, `IdentityKnown`, and the shared subscription `PoolRef`. Account
references hash the provider's plain identifier, rather than the directory or
any token. `SourceAt` is the observation time, never the cache write time.

Identity selection follows the CLI's selected home:

- Codex: `$CODEX_HOME` or `~/.codex`; only `auth.json`'s `tokens.account_id`.
- Claude: `$CLAUDE_CONFIG_DIR` or `~/.claude`; only
  `.claude.json`'s `oauthAccount.accountUuid`. At the default home the config
  file is `~/.claude.json`, outside the credentials directory.
- Gemini's agy source exposes only login state. Its cache and in-memory state
  are isolated by the resolved `~/.gemini` directory, with unknown account and
  no pool reference; login state cannot become quota percentage evidence.

Identity extraction skips secret fields and does not decode JWTs, access or
refresh tokens. The existing Claude usage reader still authenticates its usage
request using its selected credentials; identity extraction itself does not
read that token. Raw account identifiers and tokens are not written to quota
snapshots or execution telemetry.

Resolved symlinks share the same local directory identity. Known accounts share
one subscription pool across directories. Unknown identities use explicit
`provider:unknown:<directory hash>` isolation keys with no pool reference.
Different unknown keys do **not** establish independent quota pools.

Codex reads only the selected sessions directory, considering its eight newest
regular JSONL files (a FIFO or other special file is never opened and does not
count among them), and of each only the complete lines in its last 4 MiB plus
the log's first line, where Codex writes the session's `session_meta`, when
that line is complete within 64 KiB. The Python reader does the same; only
lines that mention `rate_limits` or `session_meta` are decoded. A quota event
must explicitly contain a matching `account_id` or `account_ref` at its root or
payload, or inherit that binding from an earlier `session_meta` among the lines
read, so a session whose transcript runs past the 4 MiB tail still binds its
recent events to its first line. An explicit conflicting or malformed identity
cannot inherit session metadata. Logs without such metadata remain history:
directory location alone does not prove which login produced an event.

`Windows` records each shared window and model sublimit separately, including
its pool, scope, duration, observation time and reset. The familiar scalar
fields summarize exact 300-minute, 10080-minute and 43200-minute windows.
Missing duration remains unknown rather than being guessed to be weekly.
Codex `model`, or a non-`codex` `limit_id`, is a scoped sublimit, not an
account-wide cap. Scoped limits are informational for the account-wide quota
ranking. They do not hard-block every model on the account.

The cache is keyed by provider plus account reference. Writers lock a stable
sidecar, merge observations per account and window, and atomically replace the
snapshot with a private temporary file. Refreshing another account does not
erase the first account; refreshing a weekly window does not erase a still
current short-window observation from another directory. Legacy entries can
remain in the cache as history, but known-account sources reject unbound data.
Go snapshot reads recheck the current plain identifier, so an in-process login
change cannot inherit a prior account's quota or its exhaustion seal. On Linux,
macOS and the BSDs a quota source reads its identity file again only when the
file's path, device, inode, size, modification time or change time (ctime)
changed, or while it was modified or changed less than 2 seconds before (a
coarse filesystem clock could hide a second write). A rewrite that keeps the
size and restores the modification time, or a new file that reuses the inode
number, still moves the change time, which no user can set. Where file
information has no change time, as on Windows, the source reads the file every
time, as admission and accounting do everywhere. Seals are account-bound,
remain in memory, and never enter the raw disk observations.

Go refreshes read the cache through per source. A source is read only when the
cache holds no read of its current login (provider plus account reference)
within the cache TTL, the refresh interval by default, or within 10 minutes when
that read found no current account-bound evidence; the other sources are served
from the cache. The reads are recorded in the snapshot's `reads` map (time, and
whether evidence was found) and never stand in for another account. A fresh cache
therefore lets `makewand quota`, the TUI and `serve` start without reading any
source, also when one source has no evidence (stock Codex logs, a missing agy, a
Claude login that names no account); evidence such a source gains shows up at most
10 minutes late. The background refresh of a running TUI or `serve` reads again,
at each tick, the sources with evidence that it read itself at the previous one;
reads another process made since are served from the cache. Writers merge the
`reads` map under the cache lock, keeping the newer read of each login. A binary
from before the `reads` map writes caches without it and drops it when it
rewrites one; the next refresh then reads every source again, as before.

Both readers reject observations older than 15 minutes and reset windows, even
if another account just updated the cache file or a log was touched. Each
window keeps its own observation time. Version 1 snapshots are never current
account evidence. Go's quota JSON output and Python's official quota result
include identity and window metadata.

Limitations for acceptance:

- Stock Codex logs may not expose plain account bindings; their unbound usage
  now becomes unknown. No identity is inferred from JWTs or file location.
- Identity and session schemas are vendor-owned, undocumented local surfaces.
  Missing fields fail to unknown; only static samples have been verified.
- Unknown quota remains a neutral routing signal under the existing policy.
  This change prevents treating historical quota as current evidence; it does
  not introduce a new rule that blocks every dispatch with unknown quota.
- Login files are rechecked at observation/consumption boundaries; coordinating
  concurrent login changes with provider launch requires the account lease
  work in P0-3. Multiple processes must support the platform's advisory file
  locks to share the cache safely; a lock failure skips cache writing.

Offline regressions cover interleaved accounts, same-account directories,
login changes, symlinks, unknown identity, old snapshots, stale observations,
per-window resets, all three window lengths, model sublimits, concurrent cache
writes, Claude plain UUID binding, and Go-written snapshots read by Python.

## Offline validation and baseline failures

Validation uses fake CLIs, fake credential/configuration files, static JSON
and a loopback server serving a static Claude usage response. No real model,
dispatch command, credential or dependency download is used. Proxy variables
are removed. `GOCACHE=/tmp/makewand-p0-tmp/gocache`,
`GOTMPDIR=/tmp/makewand-p0-tmp/gotmp`, `TMPDIR=/tmp`, `GOPROXY=off` and
`GOTOOLCHAIN=local` are set. Go process tests have a temporary HOME and fake
provider binaries; Python uses the repository's test isolation.

The affected Go packages are `./router` and `./cmd/makewand`. The complete run
has the nine pre-existing failures below. Each was reproduced with an overlay
of the original HEAD Go files in exactly the same isolated environment. The
overlay excludes new Go files and restores every changed Go file; it changes
neither the checkout nor production state. The remaining package tests and
the quota regressions are run separately. These failures are not repaired in
P0-2 because their fixture/process behavior also fails before this change.

After excluding those exact nine baseline test names, the package gate passes:
`router` has **558** passing top-level tests (**1083** including subtests), and
`cmd/makewand` has **116** (**211** including subtests). The quota-specific
regressions are also rerun after the final changes. `gofmt -l` is empty,
`go build ./...` passes, and `GOOS=linux`, `GOOS=windows`, and `GOOS=darwin`
`go vet ./...` all pass.
The race detector also passes the three selected concurrent-account cache,
concurrent snapshot read/seal, and account-switch/seal regressions:
`go test -race ./router -run '^(TestQuotaCacheConcurrentAccounts|TestSnapshotConcurrentReads|TestQuotaAccountCacheSharingSwitchAndSeal)$' -count=1`.

| Baseline test | Observed cause |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | Fake process does not create the expected readiness file. |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit` | Quiet/leader fake descendants do not reach readiness before the 15-second deadline. |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline` | Cancellation fixture does not reach readiness before its deadline. |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | Fake CLI exits 93 instead of the expected stderr outcome. |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | Fake CLI exits 93 instead of producing the retry fixture answer. |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | Fake CLI exits 93 before the expected retry/budget scenario. |
| `TestCustomCLIRefusalIsRetried_R4` | Fake CLI exits 93 instead of exercising the configured refusal/retry cases. |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | Fake Codex CLI exits 94. |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd` | Fake Codex CLI exits 94, causing provider-error responses. |

Python gate: `python3 -I -m pytest -q -p no:cacheprovider` on
`test_account_identity.py`, `test_quota_freshness.py`,
`test_quota_reserve_setting.py`, `test_execution_runtime.py`,
`test_benchmark_model_arm.py`, `test_health.py`, `test_fix_health_r4.py`,
`test_fix_health_reset_time.py`, `test_g3_reliability_cli.py`,
`test_g3_reliability_pacing.py`, and `test_multi_model_optimization.py` under
`tests/`: **227 passed, 298 subtests passed**. Quota/runtime fixtures now select
plain fake accounts and emit version 2 account bindings. Existing freshness,
reserve thresholds and circuit-breaker assertions remain in place.

Changed implementation files: `makewand/account_identity.py`,
`makewand/execution_runtime.py`, `makewand/health.py`, `router/quota_identity.go`,
`router/quota.go`, and `cmd/makewand/quota.go`.

Added tests: `tests/test_account_identity.py`, `router/quota_identity_test.go`.
Updated tests: `cmd/makewand/quota_test.go`, `router/quota_test.go`,
`router/quota_fix_test.go`, `tests/test_execution_runtime.py`,
`tests/test_benchmark_model_arm.py`, `tests/test_health.py`,
`tests/test_quota_freshness.py`, `tests/test_quota_reserve_setting.py`, and
`tests/test_multi_model_optimization.py`. This document records the schema,
behavior, limitations and acceptance evidence.
