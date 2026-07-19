# Makewand AI Orchestration: MVP v2 and Evolution Plan

Status: Proposed  
Date: 2026-07-15  
Scope: Product architecture, MVP definition, evaluation gates, and staged evolution

## 1. Executive decision

Makewand should first become a trustworthy execution and verification layer for a
single coding agent. It should only become an AI planner, quota optimizer, and
multi-agent orchestrator after measured evidence shows that each additional layer
improves verified outcomes enough to justify its cost and complexity.

The MVP therefore does **not** attempt to prove that multi-agent orchestration is
better. It tests a smaller and more fundamental claim:

> Can Makewand take the work of one complete coding agent, capture the agent's
> entire workspace result, verify it safely and objectively, repair it within a
> bounded budget, and apply it without damaging the user's workspace?

The long-term responsibility split is:

- AI agents understand goals and produce candidate work.
- Makewand owns scope, permissions, budgets, routing, state, verification,
  approval, and stopping conditions.
- Machine-verifiable evidence takes precedence over AI self-evaluation.

This plan incorporates repository inspection and adversarial reviews performed
with Claude Code and AGY. Both reviews converged on four corrections to the
earlier architecture:

1. Verification quality is a higher priority than planning sophistication.
2. The complete worker workspace snapshot, not model-reported files, is the
   source of truth.
3. Static DAG planning and parallel writing agents should be postponed.
4. A fixed objective benchmark must prove value before advanced orchestration is
   built.

## 2. Product objective

The eventual product is a local-first control plane for AI development work:

```text
User goal
   |
   v
Admission and policy
   |
   +-- L0: direct answer or one-shot route
   +-- L1: one full agent + deterministic verification
   +-- L2: rolling planner + bounded execution (later)
   |
   v
Workspace snapshot and isolated execution
   |
   v
Oracle verification
   |
   +-- bounded repair
   +-- approval
   +-- discard
   |
   v
Transactional application + outcome ledger
```

The north-star objective is not raw subscription utilization. It is:

```text
verified and accepted tasks
-----------------------------------------------
cash cost + subscription opportunity cost + time
```

Optimization priorities are ordered as follows:

1. Safety and authority boundaries.
2. Verified task quality.
3. Availability and latency objectives.
4. Cash cost.
5. Reduction of avoidable subscription expiry.

Unused quota must never be consumed by low-value work merely to improve a usage
metric.

## 3. Current foundation and gaps

Makewand already contains useful building blocks:

- Adaptive routing, circuit breakers, and provider fallback in
  [`router`](../router/router.go).
- Soft quota bands and confirmed-exhaustion seals in
  [`router/quota.go`](../router/quota.go).
- Candidate execution and verification selection in
  [`internal/engine/candidate_service.go`](../internal/engine/candidate_service.go).
- Workspace checks and temporary project copies in
  [`internal/engine/workspace_verify.go`](../internal/engine/workspace_verify.go).
- Restricted command plans in
  [`internal/engine/sandbox.go`](../internal/engine/sandbox.go).
- Request-level usage records in
  [`serverusage`](../serverusage/usage.go).

The MVP must address these gaps before relying on the existing pieces as a
general orchestrator:

- A worker may edit one clone while verification reconstructs only the files the
  model reported in a different clone. Unreported changes can be omitted from
  verification.
- `ExtractedFile` cannot represent a complete deletion and rename model.
- A worktree or temporary clone isolates versions, but does not provide process,
  network, credential, CPU, or memory isolation.
- Verification without trusted tests can award confidence to code that merely
  parses or installs dependencies.
- A worker can weaken visible tests unless baseline tests and acceptance assets
  are protected from it.
- Provider quality statistics are currently coarse-grained by phase and
  provider, not actual model version, task features, language, or evidence
  strength.
- Quota observations are incomplete and sometimes depend on vendor-owned or
  undocumented data surfaces. They cannot be treated as exact truth.
- Randomized routing is not fully replayable unless the policy version,
  candidates, scores, and PRNG seed are recorded.

## 4. MVP hypotheses

### H1: A verified repair loop improves completion

For the same agent, this loop should outperform a single unverified invocation:

```text
edit -> full snapshot -> verify -> structured failure evidence -> repair
```

### H2: Makewand can contain side effects

The MVP must demonstrate that it can:

- keep the real workspace unchanged before approval;
- discover all worker changes, including unreported and untracked files;
- prevent modification of protected acceptance assets;
- block out-of-scope changes;
- bound time, process lifetime, output, model calls, and cost;
- discard failed work completely.

### H3: The benefit justifies the overhead

The system must record enough evidence to compare verified completion, latency,
model calls, token or quota consumption, and cash cost against direct use of the
same agents.

## 5. MVP scope

### 5.1 Included

- Go projects only for the first release.
- One repository and one writing agent per run.
- An initial attempt plus at most one repair by default.
- A hard configurable maximum of two repairs.
- A complete base workspace manifest.
- A complete candidate workspace manifest after every attempt.
- Add, modify, delete, and rename-aware change representation.
- Policy, syntax, build, static, and trusted test verification gates.
- Explicit user approval before applying changes to the real workspace.
- A minimal SQLite control-plane ledger.
- Claude, Codex, and AGY agent adapters.
- A fixed objective evaluation suite.
- Current routing as an optional provider-selection source, without a new
  scheduling algorithm.

### 5.2 Excluded

- AI-generated multi-step plans.
- Static PlanIR and DAG execution.
- Parallel writing agents.
- New quota forecasting or quota pacing.
- Automatic dependency downloads.
- Unapproved network access by project tools.
- Git commits, pushes, pull requests, deployments, or database migrations.
- Cross-repository tasks.
- Durable pause and resume of an agent's internal conversation.
- Automatic application of high-risk or weakly verified changes.
- AI authority to change validation, permissions, retry limits, or budgets.

## 6. User-facing workflow

Proposed commands:

```bash
makewand run "Fix the lost authentication session state"
makewand run "Fix the lost authentication session state" --provider claude
makewand run status <run-id>
makewand run inspect <run-id>
makewand run apply <run-id>
makewand run discard <run-id>
```

Execution lifecycle:

1. Parse the user goal into a deterministic `RunSpec`.
2. Scan the real workspace and create `BaseSnapshot`.
3. Run a verification preflight against the base workspace.
4. Create an isolated writable candidate workspace.
5. Launch one full coding agent with a bounded execution contract.
6. Allow the agent to inspect and edit only the candidate workspace.
7. Scan the entire candidate workspace independently of agent output.
8. Freeze `CandidateSnapshot` and derive a complete `ChangeSet`.
9. Run the Oracle ladder against that exact snapshot.
10. If verification fails, return structured evidence to the same agent and
    allow at most one repair by default.
11. Freeze and verify a new snapshot after repair.
12. Present the change set, verification evidence, cost, and risk to the user.
13. Before application, compare the current real workspace with
    `BaseSnapshot`.
14. If the real workspace changed, enter `STALE_WORKSPACE` instead of silently
    overwriting it.
15. Apply the verified change set transactionally after approval.

Agent prose is diagnostic output. The candidate filesystem is the source of
truth.

## 7. Core contracts

### 7.1 RunSpec

```json
{
  "schema_version": "makewand.run.v1",
  "run_id": "run_123",
  "intent": "Fix the lost authentication session state",
  "workspace_id": "workspace_abc",
  "base_snapshot": "sha256:...",
  "allowed_paths": ["internal/auth/**"],
  "immutable_paths": ["internal/auth/**/*_test.go"],
  "verifier_ids": ["go-format", "go-vet", "go-test"],
  "provider": "claude",
  "budget": {
    "max_agent_calls": 2,
    "max_wall_seconds": 900,
    "max_api_cost_usd": 1.0
  }
}
```

The MVP must not execute arbitrary shell text from this structure. Verifiers are
registered by stable ID and translate to typed command and argument plans owned
by Makewand.

### 7.2 SnapshotManifest

```json
{
  "snapshot_id": "snap_123",
  "base_snapshot": "sha256:...",
  "files": [
    {
      "path": "internal/auth/session.go",
      "mode": "0644",
      "size": 4381,
      "sha256": "..."
    }
  ]
}
```

A manifest covers:

- tracked and untracked files;
- additions and deletions;
- file modes;
- symbolic links;
- the effective ignore policy;
- an aggregate workspace hash.

Clean Git repositories may use a content-addressed Git tree. Dirty or non-Git
workspaces require a filesystem manifest so current user changes are preserved in
the base snapshot without modifying the user's index.

### 7.3 ChangeSet

```json
{
  "base_snapshot": "sha256:...",
  "candidate_snapshot": "sha256:...",
  "changes": [
    {
      "operation": "modify",
      "path": "internal/auth/session.go",
      "before_hash": "...",
      "after_hash": "..."
    },
    {
      "operation": "delete",
      "path": "internal/auth/legacy.go",
      "before_hash": "..."
    }
  ]
}
```

Renames may be represented explicitly when identity is reliable or derived from a
delete and add pair. Correct application must not depend on rename inference.

## 8. Oracle verification ladder

### Gate 0: preflight

Before invoking an agent:

- confirm the registered verifier is available;
- capture the baseline result;
- confirm dependencies are already available;
- check that results are reproducible;
- identify expected failing and passing tests.

Missing dependencies produce `BLOCKED_DEPENDENCY`, not an agent-quality failure.

### Gate 1: policy and integrity

Check that the candidate:

- modifies only allowed paths;
- does not alter immutable tests, CI definitions, or verifier scripts;
- does not access or copy credential files;
- does not introduce unsafe symbolic links;
- does not add unexpected binaries or oversized files;
- does not perform an unapproved deletion.

Worker-added tests are useful evidence, but cannot be the sole acceptance oracle.

### Gate 2: syntax and static checks

The initial Go verifier set is:

- formatting validation;
- Go parser validation;
- `go vet` where applicable;
- target package build or repository build.

### Gate 3: trusted behavior checks

At least one of the following is required for strong verification:

- immutable pre-existing tests;
- user-registered trusted acceptance commands;
- hidden benchmark tests that the worker cannot read or modify.

### Gate 4: regression checks

Later extensions may include:

- full repository tests;
- coverage non-regression;
- performance budgets;
- API compatibility checks;
- security scanners.

### Verification strength

```text
0 = no verification
1 = policy and syntax only
2 = build and static checks passed
3 = trusted target tests passed
4 = target and regression checks passed
```

Only strength 3 or higher may proceed automatically under `safe` or `autopilot`.
Lower strengths are reported as `UNVERIFIED` and require explicit human judgment.

An AI reviewer never raises verification strength by itself.

## 9. Isolation and authority

A worktree is version isolation, not a security sandbox. The MVP must distinguish:

1. the agent's control connection to its model provider; and
2. network access by project tools launched by the agent.

The provider control channel may be allowed through a restricted adapter. Project
tool network access is denied by default.

Minimum isolation requirements:

- mount the real workspace read-only;
- provide a separate writable candidate layer;
- do not mount general SSH, cloud, package-registry, or user-home credentials;
- provide only the minimum provider authentication required by the adapter;
- restrict environment variables;
- bound wall time, output size, and process lifetime;
- terminate the entire process group on cancellation;
- use network namespace, container, or equivalent enforcement where supported;
- record the adapter's actual isolation capability in an `AgentManifest`.

An adapter that cannot enforce its declared boundaries is not eligible for
`autopilot`. It may operate in manual mode only after an explicit warning.

Linux is the recommended first execution target because process and network
isolation can be enforced more consistently. Cross-platform behavior should be
added only after the Linux safety contract passes.

## 10. Minimal persistence

The MVP uses SQLite for control-plane measurement and audit, but does not build a
durable DAG runtime or persist model reasoning.

### task_runs

- run ID;
- intent hash;
- workspace and base snapshot IDs;
- state and terminal reason;
- budget limits and settlement;
- creation, update, and completion timestamps.

### attempts

- run ID and attempt number;
- requested provider and actual model identity;
- CLI and adapter versions;
- routing candidates and selection reason;
- strategy version and PRNG seed;
- quota snapshots before and after;
- tokens, estimated cash cost, duration, and classified error.

### snapshots

- snapshot ID and type;
- manifest and aggregate hashes;
- local artifact reference;
- change-set hash.

### oracle_results

- snapshot and verifier IDs;
- verifier version;
- typed command details;
- exit status and redacted output reference;
- evidence strength and duration.

Source content and complete prompts should not be stored in SQLite. Local patches
and artifacts are stored with mode `0600`, content-addressed hashes, explicit
retention, and a deletion command.

## 11. State machine

```text
CREATED
  |
  v
PREFLIGHT -- failure --> BLOCKED
  |
  v
RUNNING
  |
  v
SNAPSHOTTED
  |
  v
VERIFYING
  +-- failure and budget --> REPAIRING --> RUNNING
  +-- terminal failure --> FAILED
  |
  v
PASSED
  |
  v
AWAITING_APPROVAL
  +-- reject --> DISCARDED
  |
  v
APPLYING
  +-- workspace changed --> STALE_WORKSPACE
  +-- transaction failure --> APPLY_FAILED
  |
  v
APPLIED
```

The worker's internal state is not persisted. After a crash:

- a frozen snapshot may resume verification;
- an unfrozen candidate execution is discarded and may restart within budget;
- actions with external side effects are never replayed automatically.

## 12. Evaluation plan

### 12.1 Benchmark set

Create 20 fixed Go bug-fix tasks:

- 5 local logic defects;
- 5 cross-file behavioral defects;
- 4 concurrency or state defects;
- 3 error-handling defects;
- 3 API compatibility or regression defects.

Each task contains:

- a fixed base revision;
- a user-visible goal;
- hidden tests unavailable to the worker;
- allowed and immutable paths;
- registered verifier IDs;
- a known-correct patch used only to validate the benchmark itself.

### 12.2 Compared treatments

Run the same tasks through:

1. direct Claude execution;
2. direct Codex execution;
3. direct AGY execution;
4. one agent plus Makewand Oracle;
5. one agent plus Oracle and one repair;
6. the current Makewand ensemble as an experimental comparator.

Start with a five-task smoke test. The full comparison should eventually contain
at least 100 attempts because individual model runs are stochastic.

### 12.3 Metrics

- first-pass rate;
- verified completion rate;
- repair recovery rate;
- accepted completion rate;
- agent calls per verified completion;
- time to first verified completion;
- cash cost per verified completion;
- subscription consumption estimate;
- Oracle false-accept rate;
- unauthorized-change and isolation violation counts.

## 13. MVP release gates

All of the following must hold:

- Oracle rejection rate for seeded faulty patches is at least 95%.
- Unauthorized writes, secret exposure, and unapproved side effects are zero.
- Discovery of worker changes, including unreported changes, is 100%.
- Snapshots and change sets are reproducible.
- Recovery of already-frozen artifacts is at least 99%.
- L1 verified completion is non-inferior to the strongest direct agent within a
  two-percentage-point margin.
- One repair produces a measurable positive recovery rate for first-attempt
  failures.
- Control-plane overhead excluding model and verifier execution is no more than
  5% of total task time.
- Every route, policy rejection, and verification decision is explainable.

Failure to satisfy these gates blocks Planner, multi-agent, and autonomous
application work.

## 14. Implementation structure

Keep the public `router.Provider` interface compatible. Agentic execution is a
separate contract because a text provider and a repository-modifying agent have
different authority and result semantics.

Proposed packages:

```text
internal/runstore       minimal SQLite control-plane ledger
internal/snapshot       manifests, snapshots, and complete change sets
internal/agent          AgentExecutor and CLI adapters
internal/oracle         verifier registry and evidence ladder
internal/control        run state machine, budgets, and approval
internal/artifact       patch, log, retention, and cleanup policy
internal/eval           benchmark fixtures and experiment runner
cmd/makewand/run.go
cmd/makewand/eval.go
```

Core interfaces:

```go
type AgentExecutor interface {
    Manifest(ctx context.Context) (AgentManifest, error)
    Execute(ctx context.Context, req AttemptRequest) (AttemptResult, error)
}

type Verifier interface {
    ID() string
    Verify(ctx context.Context, snapshot Snapshot) VerificationResult
}
```

The first implementation should reuse existing candidate, workspace, sandbox,
and usage code where its semantics remain valid. It should not preserve the
current partial-file verification behavior merely for reuse.

### Recommended implementation slices

1. Define `SnapshotManifest`, `ChangeSet`, and deterministic hashing.
2. Add add/modify/delete support and stale-workspace detection.
3. Implement the verifier registry and Go Oracle ladder.
4. Build benchmark fixtures and an evaluation command.
5. Add the minimal SQLite ledger.
6. Define `AgentExecutor` and wrap one provider first.
7. Add bounded repair and approval.
8. Add the remaining provider adapters.
9. Run the benchmark and decide whether the next stage is justified.

## 15. Evolution after MVP

### Stage 1: capability registry and context affinity

Record per agent and model:

- read-only or editing capability;
- supported languages and task roles;
- context capacity;
- structured-output support;
- session continuation support;
- enforced sandbox level;
- actual model identity;
- recent workspace or conversation affinity.

Add a switching penalty:

```text
keep the current agent when

expected quality gain from switching
<= context loss + cache loss + switching latency
```

Context affinity is important only after the candidate meets a quality floor. It
must not outrank verified quality.

### Stage 2: quota-aware routing in shadow mode

Compute but do not apply a new score. Record what it would have selected.

Decision order:

```text
safety hard filters
-> capability match
-> minimum quality floor
-> context affinity
-> latency and cash cost
-> subscription headroom soft reward
```

Quota state should include:

- observation confidence;
- data freshness;
- reset windows;
- expected task consumption;
- reserved capacity for likely high-priority work;
- API shadow price.

Unknown quota remains a low-confidence soft signal. Only confirmed exhaustion
causes a hard seal.

Promotion gate:

- verified completion is non-inferior within two percentage points;
- API cost per verified task falls by at least 20%;
- quota-related failures fall materially;
- no increase in worthless or unverifiable calls.

### Stage 3: rolling Planner in shadow mode

Do not generate a complete static DAG. Plan only the current step and a small
horizon:

```json
{
  "next_step": {
    "kind": "inspect",
    "read_scope": ["router/**"],
    "write_scope": [],
    "capabilities": ["go", "large_context"],
    "verifier_ids": []
  },
  "remaining_hypotheses": [
    "The defect may be in quota seal recovery"
  ]
}
```

The Planner may declare abstract capabilities and verifier references. It may
not:

- select a provider brand;
- emit arbitrary executable shell;
- increase scope, authority, budget, or retries;
- waive failed verification;
- declare the overall task complete without platform evidence.

Promotion requires high schema validity, zero unblocked privilege expansion, and
a measured benefit over direct single-agent execution.

### Stage 4: limited dual roles

First support one writing worker plus a read-only reviewer from a different
provider family. The reviewer returns structured findings and cannot edit or
approve.

Next, optionally support two independent candidates:

```text
Worker A -> Snapshot A -> Oracle
Worker B -> Snapshot B -> Oracle
```

The Oracle filters first. AI comparison is only a tie-breaker when objective
evidence cannot distinguish successful candidates.

### Stage 5: durable multi-step control plane

Only after rolling planning proves useful, add:

- `StepRun` persistence;
- attempt leases and idempotent settlement;
- budget reservations;
- approval gates;
- process-crash recovery;
- pause until quota reset;
- HTTP and TUI run monitoring.

### Stage 6: limited DAG execution

DAG execution is gated on:

- a stable single-agent loop;
- a useful rolling Planner;
- complete workspace snapshots;
- mature Oracle coverage;
- a measured multi-agent gain of roughly ten percentage points on the complex
  task subset, with acceptable resource consumption.

Parallel writes remain disabled initially. They may be enabled only when declared
write sets are disjoint and the merged workspace passes the full Oracle again.
Textual conflict freedom alone is insufficient; semantic integration must be
reverified.

## 16. Decisions AI must never own

AI may propose work, but it must never decide:

- its own filesystem, network, credential, or tool authority;
- whether it may expand scope;
- whether a failed trusted test can be ignored;
- whether it may modify or weaken acceptance assets;
- its retry, token, cash, or wall-time ceiling;
- whether unverified work may be applied automatically;
- whether to perform push, deploy, migration, publication, or external messaging;
- whether sensitive project data may be sent to another provider;
- whether an irreversible action may be replayed after a crash.

These are owned by user configuration, Makewand policy, and explicit approval.

## 17. Long-term default behavior

```text
simple question
-> L0 direct routing

normal code or repair task
-> L1 single agent + Oracle (default development path)

complex exploratory task
-> rolling Planner + single agent

high-value, high-uncertainty task
-> independent reviewer or two verified candidates

multi-agent DAG
-> rare, evidence-gated use only
```

The sequence matters:

> First make one agent trustworthy, then make routing measurable, then make
> planning adaptive, and only then consider multi-agent orchestration.

## 18. Definition of MVP done

The MVP is complete when:

1. `makewand run` can execute one supported agent in an isolated candidate
   workspace without changing the real workspace.
2. Makewand independently captures every candidate file change.
3. The Go Oracle verifies the exact frozen candidate snapshot.
4. One bounded repair can consume structured failure evidence.
5. The user can inspect, apply, or discard a verified change set.
6. Application detects stale workspace state and cannot partially apply.
7. Minimal run, attempt, snapshot, and verification metadata is persisted.
8. The benchmark report compares direct agents, the verified loop, and the
   current ensemble using objective metrics.
9. All MVP release gates have recorded results.
10. Advanced orchestration remains disabled until those results justify it.
