# Provider execution evidence (schema 1)

The Go and Python runners keep one atomic JSON record per provider attempt in
`<user cache>/makewand/execution-evidence/<attempt_id>.json`. Set
`MAKEWAND_EXECUTION_EVIDENCE_DIR` to select another directory. User cache follows
Go's platform convention: XDG_CACHE_HOME or ~/.cache on Linux, ~/Library/Caches
on macOS, and LocalAppData on Windows. Files are private (0600 / private Windows
ACL) and use the existing pinned-directory accounting writers and sidecar locks.
Records are retained until the operator removes them; there is no automatic
retention policy yet.

The attempt ID is the same ID used by the P0 call ledger and provider events.
The ledger's `account_ref` and `model` remain **admission/request** metadata;
settlement can join this record by attempt ID to obtain observed metadata. No
budget or optional execution-events setting is needed to keep evidence.

For a Python invocation that retries an explicit API refusal or relays a CLI
refusal to an API, the outer `ExecutionResult.attempt_id` remains the **first
admission** owned by that invocation. Its `status`/output describe the final
invocation result. Each subsequent transport gets a separate admission and
evidence record under the same `task_id`; the first refused attempt stays
`FAILED` even if a later attempt is `PASSED`. This stable outer ID preserves
the invocation's start/end telemetry correlation and avoids relabelling the
first charged call. Consumers must use `task_id` to inspect all attempts and
must not assume the record at the outer ID has the aggregate result's status.
Schema 1 does not expose a separate final-attempt ID or an explicit retry edge;
adding those is a future contract change. Unknown outcomes still forbid replay.

The record contains only these projected fields:

- `schema`, `task_id`, `attempt_id`, `engine`: record identity.
- `account_ref`, `identity_known`, `pool_ref`: a hash of the provider's plain
  account identifier and its subscription pool, or an explicitly unknown
  identity. Token values and JWT claims are never evidence. Codex's private
  `auth.json` and Claude's private `.claude.json` are projected again before
  cleanup. A changed private login is recorded under the observed account;
  deleted, invalid or symlink-replaced metadata clears identity, pool and
  account-bound windows. The private paths are fixed during preparation,
  independent of later host login/symlink changes. Claude metadata is a frozen
  copy even when mounted read-only. The default host login uses `~/.claude.json`;
  explicitly setting `CLAUDE_CONFIG_DIR=~/.claude` copies that same admitted
  metadata inside the selected private directory where the child reads it.
  A different custom directory uses its own `.claude.json`. Missing private
  metadata never falls back to another copy or to the initial identity.
- `actual_model`: optional, observed in a protocol envelope / private session
  metadata. Requested models are never substituted. A single Claude modelUsage
  entry can establish its model; multiple entries do not select an arbitrary one.
- `source`, `observed_at_ms`: collection source (`selected_login`, `private_home`,
  `cli_json`, `private_home+cli_json`, `api`, or `unavailable`) and collection time.
- `windows`: at most 32 Codex rate-limit observations, each with `model`,
  `window_key`, `window_minutes`, `used_percent`, `observed_at_ms`, `reset_at_ms`.
  Window observation/reset times come from the original event, not file mtime.
  `reset_at_ms: 0` means unknown. Pool-wide and model limits remain distinct.
- `process_exit_code`: nullable local process exit code, independent of the
  upstream result. `result_status`, `outcome_known`: runner classification.
  `tokens`: nullable measured usage; no token usage is inferred from text.

A durable initial UNKNOWN record precedes execution. Before deleting a private
home, the runner extracts only the selected root's bounded regular JSONL files
(Codex `sessions`, Claude `projects`) and saves a pending snapshot. Completion
then writes the runner's result. Private paths, prompts, answer bodies, tool
output, raw errors, and credential documents are never copied into records.
Protocol parsers inspect only allowlisted event envelopes; they do not search
inside model text. Private symlinks and oversized files are rejected.

Ephemeral Codex executions can expose evidence through their JSON output even
when no session log is written. Python non-streaming Codex now requests `--json`
and returns the extracted agent message to existing callers. Plain text streams
and local Codex `review` output are not parsed as protocol evidence. If a CLI
version emits no actual-model, account, quota, or usage metadata, those fields
stay unknown. Other providers currently have outcome records but no documented
account/quota projection; their account, model and quotas stay unknown. API
transport attempts cannot borrow a subscription login identity.

P0 quota readers in both runtimes also consume these records. They require an
exact known account + pool match, retain the existing 15-minute TTL, reject
future observations and expired windows, and merge each window independently.
They inspect the newest 64 records; older evidence remains available for audit
but may no longer contribute to live quota selection. An UNKNOWN attempt may
still contain a valid quota observation; its result is never promoted to known
on that basis.

Timeout, cancellation, termination, and generic nonzero model-process exits
remain unknown and stop automatic replay. Explicit quota refusals retain their
existing known-rejection handling. Failure to persist evidence blocks a launch
or stops delivery/replay after a launch. A host process killed before cleanup can
leave only the initial UNKNOWN record: this is deliberately insufficient to
claim completion. Evidence is local CLI telemetry, not an independently signed
provider billing receipt. Live provider format compatibility remains unverified;
tests use fake CLIs and static samples only.
