# Execution contract

Go and Python retain their existing public entry points and share a versioned
execution wire contract, admission ledger and metadata-only event format. The
canonical fixtures are in `makewand/execution_contract.json`. Python and Go
tests exercise the same fixtures and share a real ledger across processes.
`scripts/check_execution_contract.py` is an offline CI and Makefile gate.

## Outcomes and compatibility

| Status | Exit code | Meaning |
| --- | ---: | --- |
| PASSED | 0 | Requested stage completed |
| INTERNAL_ERROR | 1 | Local coordinator failed |
| INVALID_REQUEST | 2 | Invalid parameters or incompatible policy |
| FAILED | 10 | Known rejection or failed check |
| UNVERIFIED | 11 | Missing tests, eligible engines or approval |
| CANCELLED | 12 | Cancelled execution |
| BUDGET_EXHAUSTED | 13 | Admission refused by the shared budget |
| APPLY_CONFLICT | 14 | Destination no longer matches its frozen baseline |
| SANDBOX_UNAVAILABLE | 15 | Required isolation is unavailable |
| TIMEOUT | 16 | Requested deadline expired |
| UNKNOWN | 17 | Result cannot be established after dispatch |

`PASSED` applies to one stage. It does not authorize delivery without the
baseline, artifact, test, review and apply checks in
[VERIFICATION_CONTRACT.md](VERIFICATION_CONTRACT.md). `outcome_known` distinguishes
a timeout before invocation from loss of a result after invocation. An unknown
remote result must not trigger automatic provider fallback or replay.

Provider-result `duration_ms` measures provider execution. Accounting and
telemetry use bounded independent cleanup so cancellation cannot erase an
attempt; cleanup can delay the return after a provider finished on time. Such
a known provider result remains known. The coordinator rechecks its overall
deadline before authorizing delivery. Provider events include accounting time
in their span duration; the independent runner measures total elapsed time.

Python `ExecutionResult` remains a three-element tuple `(success, output, error)`
with typed metadata and `to_dict()`. `run_pipeline()` keeps its boolean return;
`run_workflow()` exposes the typed final outcome. IPC version 1 retains its
accepted/query/cancel/output/exit protocol; result metadata is additive. Legacy
IPC transport codes remain transport codes rather than delivery authorization.

Requests include task/stage/engine, tier/model, read-only and repository trust,
API policy, selected account-root reference, deadline, timeout, budget and
workflow/risk. Account references are opaque bindings to the selected credential
root, not an assertion of account identity. Neither telemetry nor the admission
ledger stores prompts or credentials. API policy retains `subscription_only`
and `allow_paid`; library users supply their application's policy explicitly.

## Shared provider configuration

The Go and Python entry points read provider enablement from the same
`config.json`. `enabled_providers` overrides the legacy `active_providers`
allowlist. Local models require opt-in. Provider aliases use the same policy;
for example, the native `gemini` slot follows the Python `agy` setting.
`MAKEWAND_DISABLE_<PROVIDER>` and `MAKEWAND_ENABLE_<PROVIDER>` override the file,
then `MAKEWAND_ENABLE_PROVIDERS` replaces the result with its final allowlist.
Disabled providers are excluded from discovery probes, routing, API fallback
and quota probes. Native custom providers follow the same setting by name.
Saving Go preferences preserves the latest Python-owned enablement fields.

A missing optional configuration file uses the defaults. An existing
`config.json` that cannot be read, parsed or interpreted as a JSON object stops
execution and provider discovery. Neither frontend restores the default enabled
providers after such an error, and `doctor` reports a failing configuration check.
Provider controls also require their declared types: `enabled_providers` maps
names to JSON booleans, `active_providers` is a list of strings, and
`local_model_enabled` is a boolean. An entire field set to `null` retains the
missing-field behavior. Other invalid authorization types stop execution;
for example, use `false`, not the string `"false"`.
Python saves configuration and credentials by syncing a private temporary file
and replacing the destination atomically, so readers see complete JSON documents.

For Claude, Gemini and OpenAI, API settings use this precedence:
nonempty environment variables, `api_keys.json`, `config.json`, then provider
defaults. Shared configuration accepts the native flat fields, such as
`claude_api_key` and `claude_model`, and the nested `api` provider records.
The API key, model and base URL are resolved independently; an explicit model
or endpoint override does not discard a key supplied by a lower-priority source.
An explicit per-call model or a native routing mode's selected model takes
precedence over the provider default model. This configuration does not authorize
paid API use: `allow_paid` remains an explicit requirement.
An unreadable or malformed lower-priority `api_keys.json` produces a diagnostic;
valid environment and `config.json` fields remain available. It cannot override
the execution policy or authorize paid calls.

The native headless entry point and `chat`/`new` return `UNVERIFIED` (exit 11) when
no permitted backend can execute the task. Invalid mode/tier arguments return
`INVALID_REQUEST` (exit 2) before backend availability is checked. These failures
do not emit a successful model response.

## Admission and deadlines

Use the same absolute ledger path to share a maximum across native and Python
processes:

```bash
makewand --max-model-calls 3 --call-budget-file /tmp/task-budget.json run "fix boundary cases"
```

Admission locks the `.lock` sidecar with compatible Unix `flock` or Windows
`LockFileEx`, atomically replaces a private schema-1 JSON ledger, and preserves
unknown extension fields. A maximum cannot increase after creation, even after
a later client lowers it. Failed, cancelled and interrupted attempts still count.
No result, token or fee is inferred from an unfinished reservation.

Ledgers are limited to 16 MiB and must be regular files; devices, pipes, malformed
JSON and nonfinite values are rejected before invoking a provider. Budget lock
acquisition obeys the admission deadline. Accounting has a separate bounded
cleanup deadline so expired work cannot discard the record of a sent attempt.

Native accounting resolves existing operator-selected directory or file aliases
to their canonical path when creating an execution context. It remembers the
parent directory's file identity. Later admissions reject a replaced directory
or a newly inserted alias. Within each locked transaction, ledger reads,
temporary writes, atomic replacement and directory sync use the same open
directory handle, so a concurrent directory rename cannot redirect the write.
Existing accounting files must be single-link regular files owned by the current
user. Explicit absolute, relative and home-relative paths remain supported.

The Python SDK resolves operator-selected aliases at the start of each locked
transaction and keeps its canonical parent directory pinned through lock, read,
temporary write and atomic replacement. POSIX operations use that directory
file descriptor; Windows holds the canonical ancestor chain without delete
sharing. Unsafe file types, owners or multiple links are rejected before
permission changes or writes. The shared ledger and sidecar-lock protocol stays
the same for native and SDK callers.

Only `--max-model-calls` creates a task-specific ledger at the CLI entry point.
The execution library requires a ledger path when a maximum is set; it cannot
silently start a second independent budget. With neither option, admission is
unbounded. This is a dispatch limit, not a vendor billing or token limit. A
provider CLI may perform several internal tool turns during one admitted
invocation; Go's explicit CLI retries and Python's explicit HTTP retries after
known refusals consume new admissions. Read-only scope also constrains API
fallback, which cannot apply generated code to the workspace.

Nested Python SDK requests inherit the parent's effective deadline and ledger.
They cannot increase its maximum or select a different ledger. Dispatch attempt
IDs are recorded even without a configured limit, so validated provider spans
can count dispatches without inferring token use or fees.

Unused workflow capacity is stored as expiring `holds`, separate from attempts.
Ordinary dispatches cannot consume it. Claiming a hold decrements its remaining
capacity and creates exactly one attempt. Race reserves two contestant calls and
one judge call before either contestant begins; partial holds are released on
failure. An unused hold can expire; an actual attempt cannot be refunded.

`--total-timeout` bounds the Python workflow, including verification and review.
Child scopes can shorten a parent deadline, never extend it. Native Go propagates
context deadlines through dispatch. Cancellation terminates local work where the
adapter supports it; it cannot revoke a remote call already sent. Race leaves
25% of its total duration, capped at 60 seconds, for judging unless overridden
with `--judge-reserve-seconds`. This reserves opportunity, not successful review.

## Workflow selection

`--workflow auto|single|pipeline|race` and `--risk auto|low|high` apply to Python
workflows. Native commands reject these unsupported workflow options explicitly.
Budget flags apply to both entry points.

`run --workflow race` rejects `--model`, `--stream` and `--local-only` rather than
silently discarding their constraints. A local-only engineering workflow needs
an explicit low-risk single policy; unknown or high risk cannot silently enlist
a cloud reviewer.

Unknown risk defaults to a cross-provider pipeline. Prompt keywords alone never
prove low risk. Trusted callers may provide a complete bounded scope with no
sensitive interfaces or external effects. An explicit low-risk single workflow
uses one coder and a separate read-only review call, possibly through the same
provider. It retains the local tests, complete diff and artifact binding.
High-risk work requires distinct-provider review; a high-risk single request is
invalid. Race contestants must be distinct; a third provider is preferred for
judging but an independent read-only call through a contestant's provider is
allowed. Missing eligible engines or evidence produces `UNVERIFIED`.

## Protected task files

Use repeatable `--protect PATH` with `run`, `race` or `apply`, for example:

```sh
makewand run "Fix the parser" --protect tests/test_parser.py --protect requirements.lock
makewand race "Fix the parser" --protect tests/test_parser.py
makewand apply RACE_ID --candidate A --force --protect tests/test_parser.py
```

Paths are relative to the task working directory. They must name existing
regular files and cannot traverse symlinks, `..` or `.git`. The SDK accepts
`protected_paths=[...]`; omitted declarations inherit the JSON array in
`MAKEWAND_TASK_PROTECTED_PATHS`. An explicit empty list suppresses that
inheritance for a new task. It cannot remove a saved candidate's frozen policy.

The policy records SHA-256 and the complete Unix permission mode. Nonempty
protection requires POSIX no-follow directory and file descriptors; unsupported
platforms fail closed. Empty policies retain existing behavior. ACLs and other
extended file attributes are outside this policy.

A protected pipeline generates in a private shadow workspace. It checks both
the generated copy and the original workspace after each generation or repair,
after local tests and before delivery. A violation returns `UNVERIFIED` and
stops testing, review and delivery. Deadline expiry returns `TIMEOUT`. Successful
work remains in its reviewed shadow delivery; use its generated
`apply_delivery.sh` to apply the patch. That script embeds the frozen policy,
checks destination files before writing and after application, and rolls back
its patches if the final check fails. It needs Python 3 and Git, without an
installed Makewand SDK.

Race copies and saved A/B/M candidates carry the same policy. Application checks
candidate and destination files before any writes, including `--dry-run` and
`--force`. Extra apply declarations add restrictions. Hybrid creation, testing,
sealing and approval also enforce the saved policy. Corrupt saved policies fail
closed.

This constrains Makewand's managed delivery. It is not a security boundary
against arbitrary external processes or direct manual filesystem/Git writes.
File declarations are explicit; prompt text alone does not establish a policy.
The benchmark runner declares common visible overlay files through the same
policy. Its protected single-model arms still make one generation call without
inventing test or review evidence; they generate privately and use the sealed
atomic applier. Protected pipeline and race arms retain their verification gates.

## Stage measurements

Set `MAKEWAND_EXECUTION_EVENTS_FILE` to enable private JSONL events. Each span's
start/end records share `event_id` and `task_id`; provider spans also bind to
`attempt_id`. Events record the stage, engine, read-only flag, timestamps,
duration, outcome, safe error category and optional artifact digest. Stages
include prepare, copy, generation, verification, review, merge, apply, workflow
and provider. Nested stages are reported separately and must not be summed into
a total duration. The benchmark parent independently measures elapsed time.

Events exclude prompt text, source paths, output, raw errors and credentials.
Observed tokens and fees may be recorded; estimates must remain separate from
observed amounts. Unmeasured token, monetary and memory values stay `null`.
Memory sampling across arbitrary vendor process trees is not implemented in
this revision. An incomplete span has no final duration. Telemetry failure must
not replay a model task or invalidate an already completed operation.

The fixed 12-task suite uses independent trusted acceptance scripts, fixed risk
metadata and repeated trials. Its offline correct/incorrect/early-exit checks
validate the evaluator, not the quality or speed of live models. The previously
authorized 12 live dispatch reservations remain a separate, exhausted budget
(12/12). This phase adds zero live calls. Its 72 offline trials (12 fixtures,
three stubs, two repeats) accepted 24 correct, zero incorrect and zero early-exit
results. They establish evaluator sanity; live workflow or model effectiveness
was not remeasured.
