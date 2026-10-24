# Bounded dispatch and review feedback

Delegate when independent judgment or parallel work adds value. Security,
concurrency, data loss, money and production releases require independent review.
Ordinary changes can use sufficient local verification; line count is not a
dispatch trigger. Q&A, read-only analysis and status queries need no model.

## One request across CLI processes

Generate a random request ID once per user request. Keep that ID across shells,
providers, subtasks and worktrees; unrelated requests need different IDs. The CLI
cannot infer conversational boundaries. Existing integrations may continue using
`MAKEWAND_TASK_ID`, `MAKEWAND_MAX_MODEL_CALLS` and the shared admission ledger.

```bash
makewand route review hard --risk high --self codex --not claude \
  --request-id request-EXAMPLE --caller codex --max-model-calls 3
```

The example excludes the named family; choose the actual implementation family.
With a request ID, route's candidate commands and escalation hints carry that ID,
caller, total cap and ledger path (including custom paths).
Using `--request-id` without an explicit cap defaults to 3. An integration using
only the legacy task-ID environment keeps its existing defaults; set both the ID
and `MAKEWAND_MAX_MODEL_CALLS` to establish a bounded user request. `--max-model-calls` is the
request's total, including failed calls, reviewers and repairs; it is not a fresh
allowance per command. Keep the same total on later commands: admission may
tighten an existing scope but does not silently raise it. Inherited task IDs and
limits cannot be replaced or relaxed by a child. Existing ledger retention and
machine/project daily limits still apply.

```bash
makewand budget request --request-id request-EXAMPLE --json
# Only after the user approves a total cap of 5:
makewand budget request --request-id request-EXAMPLE --set-limit 5 --max-model-calls 5
```

The explicit amendment changes only that task's maximum under the shared ledger
lock. It preserves charged calls, holds, account leases and project/machine caps.
Use the new total on later calls. Never generate a new ID to hide spent calls.
Status shows task capacity after its active holds; other limits can still refuse
admission. A missing request is reported, not silently created or reset.

## Review quality differs from code approval

For `outcome`, positional `pass` means the delegated work was accepted. An accurate
review finding confirmed defects is useful work even when the candidate fails:

```bash
makewand outcome codex 3 review pass --candidate-verdict fail \
  --candidate-hash SHA256_OF_THE_COMPLETE_CANDIDATE_DIFF \
  --model MODEL --effort EFFORT --seconds 42.5 --calls 1 \
  --caller agy --request-id request-EXAMPLE --max-model-calls 3
```

The appended record has `passed=true`, `review_validated=true` and
`candidate_approved=false`. Routing learns reviewer quality from the accepted
work. Approval still requires the complete, nonce-bound verdict and applicable
tests; this ledger never grants code approval. An unverified verdict cannot be
recorded as a validated review. New verdict records require measured seconds and
actual call counts. Existing records and unannotated legacy calls are retained
with their existing meaning; no migration guesses their intent.

## Stop repetitive operational failures

Record failed work with a typed `--failure-kind`: startup, auth, quota, timeout,
unknown, invalid_output or verification. Do not log prompts, raw errors or tokens
as metadata. Candidate hashes must be lowercase SHA256; caller values are bounded
tool names, and request IDs use the execution contract's identifier grammar.

Route avoids providers with startup/auth/quota failures recorded in this request
for five minutes. Recorded unknown/timeouts require explicit `route
--retry-approved` after user approval rather than expiring automatically. That
option does not bypass global pauses, quota marks or shared budgets. Other
requests remain unaffected. Successful work can supersede an earlier failure;
later failures cannot clear an unresolved unknown/timeout. Unreadable or corrupt
request history stops routing rather than pretending there were no failures.
candidate rejection and verification failures do not trigger operational cooling.
This is routing feedback from recorded outcomes, not a replacement for admission
or a blanket ban on explicitly requested direct CLI calls.

For quota exhaustion also use `makewand mark` with the observed reset interval.
Do not infer quota exhaustion merely from a quick failure. Avoid repeated model
probes: `status --probe` can make real calls. Gather fixes and run targeted tests
before reviewing the frozen candidate; reuse only results bound to the same
source scope and hash. One-account concurrency remains one.
