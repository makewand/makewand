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
| APPLY_CONFLICT | 14 | Destination no longer matches its frozen baseline, already has an entry the candidate adds, or has an entry apply cannot write |
| SANDBOX_UNAVAILABLE | 15 | Required isolation is unavailable |
| TIMEOUT | 16 | Requested deadline expired |
| UNKNOWN | 17 | Result cannot be established after dispatch |

`PASSED` applies to one stage. It does not authorize delivery without the
baseline, artifact, test, review and apply checks in
[VERIFICATION_CONTRACT.md](VERIFICATION_CONTRACT.md). `outcome_known` distinguishes
a timeout before invocation from loss of a result after invocation. An unknown
remote result must not trigger automatic provider fallback or replay, with one
exception in the Go router: a read-only CLI run cannot have written the
workspace, so an exit the message catalog does not recognize stays `UNKNOWN` in
accounting while the router may try the next provider. Read-only runs are a
review in the bubblewrap sandbox, which mounts the workspace read-only, and a
remote-origin `serve` request without a WorkDir of its own, which runs in a
scratch directory removed afterwards. A review run on the host
(`MAKEWAND_UNSAFE_HOST_EXEC`) and a remote-origin request with a WorkDir have a
writable workspace and are not read-only. A provider CLI's own refusal of the
invocation, recognized by the shared message catalog
(`makewand/cli_message_catalog.json`: a login refusal, or agy rejecting its
arguments), is a known `FAILED` result and falls back like any other known
failure. In a run that could have written the workspace, a refusal counts only
when it is all the run shows: no answer on stdout, and the refusal is the first
line the CLI wrote. A login lost after the model started working reads the same,
so such an exit stays `UNKNOWN` and stops replay. In Power mode an `UNKNOWN`
generator or judge is never replayed, but it does not discard the answers the
other generators produced: the surviving or first answer is returned, and the
unknown attempt stays in the trace and the usage. A generator refused by
admission or accounting still ends the request.

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

`CandidateManager.apply_candidate` returns an `ApplyResult`, which still unpacks
as `(ok, files, message)` and adds `status`. `makewand apply` exits with that
status's code, and a multi-task DAG's delivery reports it, never inferring it from
the message's words: `APPLY_CONFLICT` for a workspace conflict, an entry apply
cannot write, or a target that changed between the conflict check and the write;
`UNVERIFIED` for the test harness's refusal and failed protected-file checks;
`TIMEOUT`; `UNKNOWN` when whether the application was committed cannot be
established; `FAILED` otherwise, a failed `--merge` synthesis included. Every
failed apply records its typed result.

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
paid API use: `allow_paid` remains an explicit requirement. Nor does a configured
key make an engine usable by itself: routing counts an engine whose CLI is limited,
logged out or missing as usable, and the claude and codex adapters relay its work
to the API, only under `allow_paid` (see [workflow selection](#workflow-selection)).
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
failure. The Winner-Take-All review uses the judge's hold, while a verdict
follow-up and the review of a judge's own-provider pick are admitted as ordinary
dispatches, so a tight `--max-model-calls` can end such a race
`BUDGET_EXHAUSTED`; it never approves without the review. An unused hold can
expire; an actual attempt cannot be refunded.

`--total-timeout` bounds the Python workflow, including verification and review.
For direct provider commands and `run`, `review`, `race`, `ensemble`, an omitted
`--timeout` inherits `--total-timeout`. With neither option, the per-call default
is 300 seconds (900 for ensemble). With both options, the per-call limit is their
minimum. Run without an explicit total budget uses max(900, 3 * per-stage limit);
sandbox retains its independent 120-second default.
The daemon follows the workflow budget with 30 seconds for startup/teardown. Its
six-hour safety cap bounds abandoned workers while covering three-hour routes.
Larger requests run standalone, or fail clearly with MAKEWAND_DAEMON_REQUIRE=1.
Dispatched children inherit their task deadline; the observer excludes tasks
within that deadline from hung classification, cleanup candidates and termination.
Child scopes can shorten a parent deadline, never extend it. Native Go propagates
context deadlines through dispatch. Cancellation terminates local work where the
adapter supports it; it cannot revoke a remote call already sent. A native
provider CLI's stream, also one that a cancellation or deadline ends, ends only
after its sandbox's credential write-back (at most 15 s by default), and a Router
fallback after an attempt's timeout starts once that stream has ended; streams of
API providers end at once. Race leaves
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
invalid. Race contestants must be distinct; unless one is named
(`run --workflow race --provider X` names contestant A), the race pairs any two
different enabled, healthy engines among claude, codex, grok, muse and local, and
agy when it is not known to fail, each slot in its own order of preference. The
judge must receive the whole judge prompt, and a provider other than both
contestants is preferred; a judge's pick of its own provider's candidate is
approved only after a read-only, nonce-bound review by another provider. A
Winner-Take-All win (the first candidate to pass its tests) needs the same review,
which asks once more for a missing verdict line; `FAILED` exits 10, a missing
reviewer or verdict 11. That reviewer follows the judge's rule: the first usable
engine in the judge order (agy, codex, claude, grok, muse, local) that is neither
contestant and receives the whole prompt, otherwise the other contestant, never
the winner's own provider. Missing eligible engines or evidence produces
`UNVERIFIED`.

When no enabled engine is usable (each is in an error state, or limited, logged
out or missing without an API key the API policy lets its adapter relay to; under
the default `subscription_only` no key counts), nothing is dispatched: no
pipeline, explain or question prompt, REPL turn or `ensemble`. Each engine's
reason is printed, and the outcome is `UNVERIFIED` (exit 11) with the error
`No usable execution provider: <engine>: <reason>; …`; with no enabled engine at
all it remains `No enabled execution provider`. `race` likewise dispatches
nothing when it cannot pair two engines, and lists every racer, disabled ones
included, and the other active engines, each with its reason.

A forced engine (`--provider X` of `run` and of the natural-language entry, or an
explicitly named race contestant) is decided before any workspace setup, routing
or dispatch, alike on the explain/question, pipeline, review and race paths. It
must pass what routing requires of any engine: enabled and without a live failure
verdict, unless its adapter relays that state to an API the policy allows; an
API-only engine (deepseek, qwen, glm, kimi, openrouter, siliconflow) needs a key
the policy lets it use. An unusable one is never dispatched: the outcome is
`UNVERIFIED` (exit 11) with the error `Requested engine X is not usable: <reason>`.
A usable one always runs, also when no other engine is usable (it is then the only
tool, under the single-tool review rules), and on the review path it is the only
reviewer. `auto` forces nothing. Limits: the check reads the recorded health, so an
engine without a recorded status (one never probed, for example) passes it even
when its CLI is missing, and its dispatch then fails; a question in the
interactive REPL after `/model X` goes to X without this check.

## Reviewer capacity

A review verdict counts only from a reviewer that received the complete prompt
with the complete diff. Diffs and prompts are measured as dispatched: an
undecodable file-name byte counts as its four-byte `\xNN` escape, for the review
cap, the whole-prompt limits and the local context window alike. A diff over the
review cap (`MAKEWAND_REVIEW_DIFF_MAX_BYTES`, default 262,144 bytes; an invalid
value falls back with one warning) is never reviewed truncated, in the pipeline
(first review, re-review, the auto-fix growth check), `makewand review`, the
Winner-Take-All review or the race judge. The
outcome is `UNVERIFIED` and the complete patch is saved as `unverified.patch`
(`git diff --binary --full-index --unified=3`) with one
`unverified.submodule-<n>.patch` per checked-out submodule, listed in
`manifest.json`; when it cannot be saved completely, the shadow worktree is kept.

The agy adapter passes a prompt over 32 KiB as a file that its agent reads in
paged views, so no review, judge or Winner-Take-All prompt over 31 KiB goes to
agy. The pipeline's
first review and the auto-fix re-review then append the next usable stand-in:
planned reviewers first, then active providers, never the coder (for a re-review,
nor the fixer); the coder only as a last resort where the plan allows
self-review; none in a single-tool run, `--local-only` included. A stand-in that
fails with a non-terminal status is replaced by the next allowed one; `UNKNOWN`,
`TIMEOUT`, `CANCELLED` and `BUDGET_EXHAUSTED` stop the review. Placeholder engines
without an executor (`cursor`, `copilot`) never review or stand in.
`makewand review` keeps its codex → grok → agy ladder (`local` with
`--local-only`; a review request through `run --provider X` asks X alone), so when
agy is the only usable engine there an oversized prompt ends `UNVERIFIED`.

Read-only local model calls (review, judging, questions) go to Ollama's native
`/api/chat` with `truncate: false` and `shift: false`. `num_ctx` is a token
estimate of the messages (half their dispatched UTF-8 size, each ASCII digit one
token) plus 512 template and 8,192 answer tokens, capped at the model's trained context
length from `/api/show`. The answer counts only when `prompt_eval_count` and
`eval_count` show that the prompt fitted with room for the system message and
the answer, and that at least one token per 6 bytes was processed. A prompt that
fills that window, or that Ollama refuses as too long, is sent once more with room
for one token per byte (`num_ctx` up to the prompt's bytes + 8,704, capped at the
trained length); when `/api/show` reports no context length nothing is retried
and only a prompt and answer within 2,048 tokens count. An endpoint without
Ollama's native API (vLLM, llama.cpp, LM Studio) is asked through its
OpenAI-compatible API and counts only when `usage.prompt_tokens` is at least the
prompt's bytes / 6. A context-length refusal, a missing count or a failed
`/api/show` probe is `UNVERIFIED`. Coding dispatches keep the OpenAI-compatible
path. OpenRouter requests send `transforms: []`, and its context-length refusals
are `UNVERIFIED`. Residual: a server that cuts a prompt yet reports at least one
token per 6 bytes (an Ollama that over-reports its trained length, a truncating
llama.cpp or LM Studio overflow policy, an OpenAI-compatible proxy in front of
Ollama) cannot be told from a whole prompt; prompts made of long runs of one
character can fail closed, and a retried window can need a large KV cache.

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
