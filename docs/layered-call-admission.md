# Layered subscription call admission

Bounded Go and Python CLI invocations use the same stable
`MAKEWAND_CONFIG_DIR/call-budget.json` (otherwise
`XDG_CONFIG_HOME/makewand/call-budget.json`, or `~/.config/makewand/call-budget.json`).
`--call-budget-file` selects an isolated deployment ledger; all participating
entry points must use the same path. No dollar budget substitutes for these counts.

`--max-model-calls` / `MAKEWAND_MAX_MODEL_CALLS` limits one `MAKEWAND_TASK_ID`.
The dispatcher generates an ID and passes it through daemon workers, retries,
repairs and reviews. Independent entry points generate independent IDs.
`MAKEWAND_PROJECT_DAILY_CALLS` (default 200) and
`MAKEWAND_MACHINE_DAILY_CALLS` (default 1000) configure separate daily limits.
These are initial conservative local policies, not vendor quota predictions.
`MAKEWAND_PROJECT_ROOT` groups canonical directories/worktrees into one project;
otherwise the canonical invocation directory is the project identity.

The three scope maps are stored separately **inside one transaction file**.
A sidecar OS lock plus durable atomic replacement commits admission together.
Limits can only tighten within the same scope; a task never rewrites a project
or machine limit. Daily buckets start at local midnight, and a clock that moves
back keeps counting into the latest bucket (see
[Daily buckets and the budget clock](#daily-buckets-and-the-budget-clock)).

Reservations cross an explicit launch boundary. Unstarted reservations can be
released; launched failures, cancellation, crashes and unknown outcomes remain
charged exactly once. Completion is idempotent. Review capacity holds occupy
all three scopes, and admission rechecks the current daily buckets when a hold
crosses midnight. A hold is bound to its task.

Library callers can opt in with `MAKEWAND_LAYERED_BUDGET=1` or either daily limit.
Legacy schema-1 lifetime ledgers remain supported. On opt-in their old lifetime
limit becomes an additional conservative cap; existing spent calls are never
refunded. Use a fresh shared ledger to start a new layered deployment.

This local file protocol is not a tamper-proof authorization store. Operators
must share one ledger across participating entry points; separate files/users
or direct vendor CLI invocations cannot be counted by it. Ledger size is capped
at 16 MiB and exhaustion fails closed. Each read/write compacts settled
attempts older than 48 hours under the same lock; active/reserved attempts and
attempts with lifecycle leases survive regardless of age. An older Go completion
uses its start timestamp plus elapsed seconds to date settlement. A legacy
settled record without that proof starts its retention period at upgrade.
Recent settled attempts and all their scope references remain intact. Old daily
counters disappear only when unreferenced; task counters disappear after 48 hours
without use and with no retained attempt/hold reference. Expired holds and unused
concurrency keys are removed. Lifetime ledgers retain `archived_used`, so pruning
never refunds their cap, including after layered migration. Normal layered caps
bound retained history over time; unprovable live leases can still occupy space.
An already oversized/corrupt ledger still requires operator recovery with all
workers stopped; do not delete a live ledger to bypass budgets or lease fences
(a clock that was ahead needs only `makewand budget clock --repair`).
Unknown project/worktree ownership is not inferred.

## Daily buckets and the budget clock

Daily buckets start at local midnight: the process's local time zone, or
`MAKEWAND_BUDGET_TIMEZONE` (an IANA name such as `Asia/Shanghai` or `UTC`).
Go and Python compute the same bucket; set the variable for every participating
process when their local zones may differ. An invalid name refuses admission
with `INTERNAL_ERROR` (exit 1) and charges nothing. On Windows every name other
than `UTC` also needs a time zone database: `ZONEINFO` for Go, the `tzdata`
package for Python. Buckets used to start at Asia/Shanghai midnight on every
host; `MAKEWAND_BUDGET_TIMEZONE=Asia/Shanghai` keeps that boundary.

A clock that moves back into an earlier day no longer refuses dispatch:
admission keeps counting into the latest bucket already seen, so counts are never
reset. The ledger's `scopes.clock` is a ratchet against a clock that steps back,
trusted and stored at most 300 seconds ahead of real time. Lease and capacity
hold expiry, compaction and the clock itself use that capped value, so a ledger
written while the system clock was ahead (an RTC read in the wrong time zone, a
VM snapshot, a manual date change) cannot stall lease cleanup: a lease admitted
or renewed after the correction expires at most 300 seconds plus its 30-second
TTL after real time, and the next admission re-dates a lease stored further
ahead as if it had just renewed. A dead worker's lease is then reclaimable once
real time passes that expiry; liveness evidence is still required, as below.
Backward clock movement never resets counts, and the budget clock moves back only
when it is more than 300 seconds ahead of real time. Each process warns once on
stderr when it meets a clock or a day bucket ahead of real time.

`makewand budget clock` shows the ledger's clock lead, its latest day bucket
against today's, and how many leases and holds carry expiries written while the
clock was ahead; it writes nothing. `makewand budget clock --repair` (`--json`
for a machine-readable report, `--call-budget-file` for another ledger) clears
the skew under the ledger lock, with workers running or not: the clock and the
counters' timestamps return to real time, leases expire one TTL from now (live
owners renew them), holds are capped at their 24-hour maximum, and the latest day
moves back to today. Calls counted in a later day's bucket stay counted there and are
added to today's bucket as well, so nothing is refunded. Provider workers cannot
run it.

## Account and quota-pool concurrency

Admission also reserves provider, account and subscription-pool concurrency in
that same commit. `MAKEWAND_PROVIDER_CONCURRENCY` defaults to 64;
`MAKEWAND_ACCOUNT_CONCURRENCY` and `MAKEWAND_POOL_CONCURRENCY` default to 1.
Known identities use P0-2 account references and their shared subscription pool.
Unknown directories share a conservative provider-wide unknown pool, which
also conflicts with known accounts of that provider; directory hashes never
establish independent accounts. Limits tighten while a concurrency key has
active leases. Once that key is quiescent, its stored limit is discarded and the
next admission uses the configured value, allowing an operator to raise a limit
without deleting the budget ledger.

Both languages share the provider-alias capacity namespaces: `agy`, `gemini`,
`google`, `antigravity` → `agy`; `codex`, `openai` → `codex`; `claude`,
`anthropic` → `claude`, plus the remaining `PROVIDER_ALIASES` mappings.
Names are case-insensitive. Normalize **before** adding the `:api` suffix.
API transports use `agy:api:provider`, `agy:api:unknown` and
`agy:api:unknown:subscription`; the last suffix is the schema-1 pool-key slot,
not a claim that an API key has a subscription. The same API key therefore
cannot acquire separate Python/Go locks through different adapter names.
API keys do not establish account identity: different keys also share this
conservative unknown API pool. Subscription leases remain separate.

Account contention raises a distinct concurrency-busy error. When another usable
candidate/provider remains, routing immediately tries it without sleeping, even
if a wait cap is configured. Otherwise admission waits outside the ledger lock,
with backoff from 50 ms to 1 second, rechecking all budget scopes atomically each
time. `MAKEWAND_LEASE_WAIT_SECONDS` caps the wait (default 30 seconds; zero refuses
immediately). With a deadline, the wait also leaves one second for subsequent
steps, or half the remaining time if less than two seconds remain.
An unsuccessful capacity wait returns known `FAILED` with
`error_kind=concurrency_busy` (exit 10), permitting workflow/provider fallback;
it never starts a provider or spends a call. A deadline already expired before
admission remains `TIMEOUT`, and explicit cancellation remains `CANCELLED`.
Real budget exhaustion remains `BUDGET_EXHAUSTED` (exit 13) and is refused
immediately. Python's busy exception subclasses `BudgetError` for existing
low-level callers, and the runtime catches it before budget errors. Cancelled
race contestants stop waiting without acquiring a lease or consuming a call.

Both runtimes parse the wait cap as an ASCII decimal, allowing signs, fractions,
exponents and surrounding ASCII whitespace. An unset or empty value defaults to
30 seconds; whitespace alone, negative/nonfinite values, hex, underscores,
Unicode digits and values whose nanoseconds reach 2^63 are rejected before
admission, including when no ledger is configured. Invalid settings return
`INTERNAL_ERROR` (exit 1), without charging a call. Busy refusals never count
against provider reliability. Go also excludes admission cancellation/deadlines
from provider reliability and circuit-breaker failure accounting.
Candidate lists remain single-pass: a busy candidate skipped for a fallback is
not retried after that fallback fails. A candidate with no usable alternative
still receives the bounded capacity wait described above.

Go stops heartbeats on failed startup and failed settlement. Unstarted releases
and completion writes retry with independent five-second lock budgets and
exponential backoff (100 ms up to 30 seconds) until the ledger recovers or the
reservation disappears. A temporarily missing pinned directory is retried too.
These retries only
settle accounting; they never dispatch a provider. A failed Start also refunds
a persisted `started` transition when no successful Start has returned, covering
a directory-fsync failure after rename. Releasing a running attempt through
`ReleaseUnstarted` remains a no-op and preserves its heartbeat.

Leases renew every 5 seconds with a 30-second expiry. Expiry alone never releases
a live dispatcher or worker. Linux checks boot ID, PID start ticks and process
group members (including children left running after their leader exits).
Cancellation and unknown outcomes preserve the spent call, and release concurrency
only after the process runner has joined/cleaned its worker tree.

The launch fence is written **before** spawning and replaced with the worker's
process identity afterward. Cleanup retries a failed fence-clearing write three
times with independent lock deadlines and reports a persistent failure. An
expired fence can be reclaimed once the owner and registered workers are
provably dead. A completed attempt with no launch fence and no live workers
can also be reclaimed after expiry even if its daemon owner remains alive.
A crash never refunds the call. Platform lifecycle observations are:

- Linux: boot ID, PID start ticks and surviving process-group members.
- Windows: `OpenProcess`, `WaitForSingleObject` and `GetProcessTimes` identify
  a process lifetime. A signalled handle or a missing PID is dead; a different
  creation time is PID reuse. Access denial or an unavailable observation is
  inconclusive. `Signal(0)` is not used: its Windows unsupported error says
  nothing about liveness. The supervised Windows runners register workers with
  `job=owner-kill-on-close`: the lease owner holds the only non-inheritable Job
  handle, with no breakaway. A provably dead/reused owner therefore proves its
  marked tree was terminated, allowing expired leases to be reclaimed after a
  crash or reboot. A dead worker leader alone does **not** prove tree death.
  Legacy/unmarked worker refs and inaccessible owner observations remain
  conservative; this change does not invent proof for old uncertain leases.
- macOS: `/bin/ps` under `LC_ALL=C`, `TZ=UTC` supplies the PID's start time and
  process group. Start time is compared and surviving group members are checked.
  `lstart` has one-second resolution, so reuse within that second is ambiguous.
  If start time cannot be obtained, PID/group existence is the fallback;
  reuse then cannot be distinguished and may conservatively retain capacity.
  Permission errors and unavailable observations never prove death.
- Other systems retain leases when lifecycle identity cannot be established.

Go and Python serialize the same Windows creation-time token (decimal FILETIME)
and macOS start-time token (normalized UTC `ps lstart` text). Normal runner
cleanup still releases the lease. Operators must verify
all old processes are stopped before clearing uncertain leases; do not clear them
merely because a heartbeat expired. Independently detached descendants that evade
the tracked process group need stronger OS supervision for reliable recovery.

Go binds the selected login once the attempt holds its account lease, after any
lease wait, so tokens a lease holder refreshed meanwhile are the ones bound, and
compares it with the login admitted before the wait by account (codex
`tokens.account_id`, Claude `oauthAccount.accountUuid`). From then on a token
file that names its account, codex `auth.json` with `tokens.account_id`, may be
refreshed by that account; any other token file is bound by its hash: Claude
keeps its tokens in `.credentials.json` apart from the account in `.claude.json`,
so a change of them cannot be proven the same account's, and neither can one of
an `auth.json` without an account. From `.claude.json` only the account (hashed
reference plus existence) is projected, so normal startup/project/cost state
writes do not change the binding. The sandbox copies the login into its private
home one file at a time and then reads it again before launch. A switch to
another account during the lease wait or before transport invocation refuses
launch and releases call capacity; the refusal is no provider failure, and it
ends a Power request. An invoked transport is never refunded. Direct-mode
post-run account switches still produce UNKNOWN.
