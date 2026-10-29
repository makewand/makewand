# Shared local capacity

The optional `capacity-policy.json`, beside the **default** `call-budget.json`,
coordinates actual makewand dispatches across Python, Go, and private budget
files. Copy `docs/capacity-policy.example.json` there to enable the policy.
All callers must use the same operator configuration directory
(`MAKEWAND_CONFIG_DIR`, or the normal XDG configuration directory). A private
`--call-budget-file` changes call accounting, never this capacity directory.
Removing the policy restores the existing per-ledger admission behaviour;
it does not delete leases, refund calls, or stop active workers.

The example limits supplemental model calls to three in flight, Codex to one,
AGY to two across aliases/models/accounts, Claude to one, and other providers
to one. API transports share their provider ceiling, with separate account
pools. The per-project ceiling is one; ordinary Git worktrees share their
repository identity. `MAKEWAND_PROJECT_ROOT` can explicitly group related
directories. A request ID cannot have two simultaneous dispatches.
AGY=2 is a local trial ceiling, not a vendor concurrency guarantee. Existing
cooldowns, quota protections, credits prohibition and task routing still apply.

Capacity and spend are separate. Each request still uses its existing shared
task ID, budget ledger and maximum (normally three actual model calls).
Neither waiting nor a busy refusal spends a call. A private ledger's daily
counters remain separate; this policy does not fabricate a vendor balance or
combine past spend. Unknown results and timeouts retain their existing retry
authorisation requirements.

`shared-capacity.json` contains only active lease references, short-lived queue
tickets and project scheduling turns. Admissions hold its lock before the
selected budget lock. A slot is durably registered before provider execution.
Current source leases supply worker identities and heartbeats; elapsed time
alone cannot release live owners, children or unobservable processes. Missing,
reset or unreadable active source files retain their cached slot until there
is positive settlement evidence. Never delete or edit these files to clear a
busy provider. The existing ledger registry imports visible legacy leases;
already-running old binaries cannot participate in the new admission protocol.
Let them finish and use the updated CLI for subsequent calls.
Dead-lease reapers preserve `lease_reaped: true` on the original charged
attempt, so a later refresh can remove its cached slot even after the source
lease is gone. For older reapers without that marker, recovery requires the
original attempt to remain present and the cached owner and all known workers
to be provably dead after expiry. Source checks use the same bounded effective
clock as ledger admission. Recovery never refunds or rewrites the call result.

Model queues prefer projects that have waited since a less recent admission, keeping
FIFO order within equally served projects. A blocked provider does not block
other eligible providers. The maximum wait is five minutes, reduced to one
third of the remaining execution deadline. `MAKEWAND_LEASE_WAIT_SECONDS` may
shorten it. Routing can still skip waiting when another usable provider exists.
Occupying projects, elapsed time and queue size appear on stderr; JSON stdout
is unchanged. Cancellation removes a ticket; abandoned tickets expire.

Heavy **local** work uses its own ceiling of two. The controlled entry points
are Python `run_local_tests` and non-provider `run_in_sandbox`. They track
actual subprocess trees, preserve the enclosing deadline, and spend no model
calls, with a separate FIFO queue. Linux admission requires at least 32 GiB available (a maximum 16 GiB job
plus 16 GiB headroom). The existing sandbox resource launcher keeps smaller
limits and clamps larger memory settings to 16 GiB; systemd scopes also set
`MemorySwapMax=0`. Hard memory enforcement requires the existing platform
resource launcher to be available. This does not control arbitrary Bash,
official CLI tool execution outside these entry points, already-running tools,
or old Go binaries. No provider sandbox receives writable coordinator files or
a wider worker-command whitelist.
If a tool's coordinator state disappears or becomes invalid during cleanup,
the trusted entry point reports validation as unavailable. Child teardown and
pipe cleanup still complete; successful command output alone cannot turn that
coordination failure into a passed validation.

Absent a policy file, existing library behaviour and limits are preserved.
Environment concurrency limits can tighten an enabled policy. Persisted lower
limits remain in force while their leases exist, then normal ledger compaction
removes unused limit keys. Do not clear active leases to raise capacity.
