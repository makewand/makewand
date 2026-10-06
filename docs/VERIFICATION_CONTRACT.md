# Verification and acceptance contract

## Go candidate verification

Candidate verification reports local diagnostic results. It does not treat output
from candidate-controlled code as an independent acceptance certificate.

- **Strength 0:** a check failed, isolation was unavailable, an expected baseline
  Go test was missing, or verification changed the input files or permissions.
- **Strength 1:** the local checks completed successfully. Human approval is
  required before applying the candidate, including when baseline tests ran.
- **Strength 2:** all isolated local checks and a configured independent trusted
  acceptance driver passed for the same frozen inputs and delivered artifact.
  Without that driver, local test success remains Strength 1.

Passing local tests alone does not authorize automatic file application. A
valid Strength 2 record can authorize autopilot; manual application remains
available for successful local diagnostics. Makewand still runs and ranks candidates,
shows check results, and prepares a candidate for approval. Even Go's structured
JSON test events can be forged by code executing in the test process; matching
every expected baseline test improves diagnostics but does not remove this limit.

Configure `MAKEWAND_TRUSTED_ACCEPTANCE_FILE` as a JSON file outside the project,
or use `ContextWithTrustedAcceptance(ctx, spec)` in the Go SDK. The `stdio-v1`
driver resolves a trusted runtime outside the candidate before generation and
binds its executable hash, command and arguments. A trusted parent compares
stdin-driven stdout, stderr and exit status to expectations kept out of the
candidate sandbox. Candidate output is application data, never an approval
protocol. Each behavioral case uses a read-only workspace, private home, and
separate PID, IPC and network namespaces through Linux Bubblewrap.

The record binds the policy, full baseline, full candidate tree and delivered
patch. This stricter input tree includes dependency and build directories;
symlinks, special files and unsealed inputs stop strong acceptance. Expected
results are hidden from candidate processes. The parent authenticates the
record with a private, in-process MAC; JSON export/import cannot recreate
approval. Restart requires fresh verification. Immediately before application,
the TUI checks the record and the baseline again under the application lock.
Unsafe host execution never grants Strength 2, even if a later check isolates
successfully. Configured acceptance errors or failures cannot fall back to
strong approval. The supplied cases establish only their specified behavior,
not arbitrary code safety or complete requirement coverage.

Example private specification (replace the fixed runtime path for the host):

```json
{
  "schema": 1,
  "command": "/usr/bin/python3",
  "args": ["-I", "app.py"],
  "timeout": "10s",
  "max_output_bytes": 65536,
  "cases": [{"name": "basic", "stdin": "2\n", "stdout": "4\n", "stderr": "", "exit_code": 0}]
}
```

The execution plan comes from the baseline project. Adding a `package.json` to a
Go candidate cannot replace `go test` with an npm command. The existing baseline
runner priority is `package.json` with a test script, `pytest.ini`, `go.mod`, then
`Cargo.toml`. Mixed-language projects use that single baseline runner; this is
not a claim that all language suites ran. Use a baseline test script that invokes
every required suite, or run the additional suites separately. Existing baseline
test definitions and the npm test script are restored for candidate checking.

Before checks start, Makewand seals the candidate's exact file bytes and Unix
permission bits and fingerprints the project input tree. Changes to existing
inputs, permissions, or newly generated project files invalidate the result.
Tool caches and designated build output directories are excluded from the input
tree fingerprint; candidate files themselves are checked separately even there.
Code-generation tests that change project inputs require review and another
verification pass. The delivered payload is the sealed pre-test version, never a
fresh read of the writable post-test tree. Its digest includes permissions and
is checked again before application. The TUI carries that structured payload
without converting it back through Markdown parsing.

File replacement uses a temporary sibling and an atomic, root-relative rename.
Existing executable permissions are retained, new files default to `0600`, and
checkpoint restoration restores the prior content and supported permissions.

On Linux, test/build/compile commands use Bubblewrap with a separate network
namespace. Bubblewrap enables loopback inside that namespace for local test
servers; the host loopback and host default network route remain inaccessible.
Dependency installation retains network access. The documented, acknowledged
unsafe host execution option still bypasses sandbox isolation.

The Go build wizard requires an explicit review approval. Review errors and
unresolved defects stop acceptance. One available provider performs a separate
review call rather than bypassing review, and review-generated fixes are reviewed
again. Passing checks or a review does not assert freedom from all defects.

## Go candidate resources and workspace copies

These settings apply to each native Go candidate selection. They do not change
the verification strength or human-approval requirement.

| Environment variable | Accepted values and behavior |
|---|---|
| `MAKEWAND_CANDIDATES` | `1`–`3`; caps the available provider attempts. Unset: up to three, or two for fix/review phases. |
| `MAKEWAND_CANDIDATE_CONCURRENCY` | `1`–`3`; caps concurrent clone, provider, and verification attempts. Unset: the selected candidate count. |
| `MAKEWAND_CANDIDATE_TIMEOUT` | Positive Go duration, such as `2m`; applies a cooperative context timeout. Unset: no additional selection timeout. |
| `MAKEWAND_CANDIDATE_BUDGET_USD` | Finite non-negative amount; `0` or unset disables this soft cost limit. A positive limit forces sequential attempts and stops new calls once reported cost reaches the limit. |

One call can exceed the soft cost limit. Unknown or zero reported subscription
cost cannot provide a monetary hard cap. Timeouts request cancellation between
copied files and through provider/check contexts; they cannot undo completed
writes or a provider request already accepted remotely. The candidate cost
setting is separate from the shared integer call ledger: native Go and Python
dispatches both join `--max-model-calls` / `--call-budget-file` admission.
Go callers can use
`ContextWithCandidateSelectionOptions(ctx, CandidateSelectionOptions{...})`;
context options replace the candidate environment settings, with zero fields
using defaults. Invalid resource settings are rejected.

On Linux, `MAKEWAND_WORKSPACE_REFLINK=auto` is the default. It attempts reflinks
when source and temporary workspace roots have matching filesystem identifiers
and are Btrfs or XFS; XFS also needs its reflink feature enabled. `1` explicitly
attempts reflinks on other filesystems, and `0` uses ordinary copies. Non-Linux
platforms use ordinary copies. A failed reflink resets offsets and removes
partial output before attempting a full ordinary copy; copy errors abort the
workspace clone. Unsupported device pairs are cached within that copy. Hardlinks
are never used to share mutable candidate inputs.
Performance depends on the filesystem and workload; these choices do not assert
a measured speedup on a filesystem that lacks reflink support.

Copies preserve supported file permission bits and omit symlinks and known
secret paths. Conventional `.env.example`, `.env.sample` and `.env.template`
files are retained; filtering uses paths and does not redact their contents.

Go file checkpoints use inode/device/link metadata on Unix. On Windows they
query a fixed handle for volume, file index and link count, rejecting reparse
points, missing identity information, and detected metadata changes. Supported
identity formats currently cover NTFS, FAT, FAT32 and exFAT. ReFS and unknown
filesystems are rejected: the 64-bit index is not guaranteed unique on ReFS.
[Microsoft documents this identity limitation](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/ns-fileapi-by_handle_file_information).
The `Architecture runtime` workflow exercises actual NTFS handles, file identity,
transaction recovery and permissions on `windows-latest`. Other filesystem
formats remain subject to their capability checks; this NTFS run does not certify
them or provide Linux sandbox isolation on Windows.

## Python pipeline and race candidates

Python fingerprints workspace inputs before and after local tests and before
review. Git-tracked files and deliverable untracked files remain covered even
inside cache-named directories. In a directory without Git metadata, only the
`.git` administrative directory is excluded. Python tests disable bytecode and
pytest caching; verification never writes workspace playbook state. All detected
suites and fallback commands share one deadline. Input changes invalidate the
result and require a new test and review pass. Before the first race seal, only
new, untracked bytecode that exactly matches trusted compilation of its source
may be removed; existing files and arbitrary cache-directory source remain inputs.

Missing detected Go/npm/Cargo test tools or malformed Node test configuration
produce `UNVERIFIED`; they are never silently skipped. A pipeline without
executed local test evidence stops before review and delivery. Python's
unittest fallback reporting zero tests is also unverified.
Saved candidates require explicit generation, test and review passes for normal
application. Missing SDK metadata remains unknown; it is never inferred from
generation success. An explicit human `--force` decision remains supported.

A race verdict must use the structured `MAKEWAND_RACE_VERDICT` protocol. Rejection,
missing verdicts and malformed verdicts cannot select a fastest candidate as a
fallback. Candidates without detected tests remain unverified and cannot obtain
normal application approval. Race review includes each complete candidate diff;
diffs exceeding 64 KiB remain inspectable but do not trigger a truncated model
review. Only the explicitly accepted, tested candidate receives review approval.
Coding, tests, judging and optional hybrid verification share the race deadline.

Saved candidates seal content hashes, Unix permissions and the entire application
plan, including deletions. Application checks all three; changing Git metadata
cannot introduce extra deletions. `--force` can override a human decision about
test/review failures or workspace conflicts, but cannot bypass artifact integrity.
Candidates saved by older versions must be regenerated. Replacement and rollback
preserve supported permissions. POSIX uses directory handles; native Windows
uses pinned, non-reparse Win32 directory/file handles and atomic handle-based
replacement/deletion. Windows rejects junctions, alternate data streams, DOS
device names and ambiguous paths. DACLs and readonly attributes are retained;
Unix executable/owner bits are not emulated. Windows pipeline generation always
uses a shadow copy, including for a non-Git directory. The complete offline
generate/test/seal/review/apply/rollback flow runs in the native Windows CI job.
Testing generated code on Windows still requires explicit unsafe-host consent
when Linux sandbox isolation is unavailable; it cannot grant Strength 2.
Windows subprocesses in the Python runner start suspended, join a Job Object,
and then resume. Jobs bound process trees, output, deadline, process count and
memory; they do not isolate files, credentials or networking. Unknown batch
shims are rejected; recognized Node shims invoke the fixed JS entry directly.
The fixtures exercise runtime contracts, not every third-party model CLI.

Native Go command, Router CLI, raw CLI and preview processes also use shared
Windows Jobs, with suspended startup and no breakaway. Their combined raw
stdout/stderr limit is 10 MiB; overflow is an UNKNOWN outcome, never a successful
truncated response. Deadline cancellation, normal leader exit and preview Stop
reap inherited pipes and terminate supervised descendants. Unix process groups
retain their existing containment limits.

Python shadow delivery also freezes each repository's deliverable path set before
review, including submodules. After staging and committing, it compares the raw
Git blobs, symlink targets and Git executable bits of fixed commit IDs against the
reviewed input records. A clean working tree alone is insufficient. Hooks remain
disabled and Git replacement refs cannot alter this verification. Exported patches,
published branches and `delivery_manifest.json` use those same verified commit/tree
IDs, so a later change to HEAD cannot substitute unreviewed content. The manifest
records `verified_commit` and `verified_tree` for the main repository and exported
submodule patches.

For exported POSIX `apply_delivery.sh` scripts, the destination is frozen before
copying the shadow workspace and checked again before publishing the delivery.
Every exported patch must also pass Git's applicability check against that
destination before the delivery is published.
The snapshot binds the root directory identity and mode, file contents and modes,
symlink targets, directory entries, and repository/submodule HEAD and marker
identities. It includes ignored files and empty directories, excluding Git
administrative contents. The generated script embeds one copy of the same
standard-library checker and baseline, extracts them into a private directory,
and verifies their hashes before use. It needs only Python 3 and Git, without an
installed Makewand SDK.
Caller-supplied Git redirection and configuration variables are cleared.
Git's work tree is fixed explicitly to the approved destination.

Before any patch is applied, the script rejects destination drift, including
changes outside patch hunks, new files, permission changes, branch switches and
directory replacement. After application it checks the expected patch postimage
over the frozen destination. Git patches preserve executable/non-executable mode
semantics for changed regular files; unchanged entries retain exact modes.
Inspection defaults to 100,000 entries, 512 MiB of file content and ten seconds.
Larger projects can explicitly set `MAKEWAND_DELIVERY_MAX_ENTRIES`,
`MAKEWAND_DELIVERY_MAX_BYTES` and `MAKEWAND_DELIVERY_MAX_SECONDS` before starting
the task. Their upper bounds are 1,000,000 entries, 16 GiB and 300 seconds.
For example, the following allows a 1 GiB, 120-second inspection:

```bash
MAKEWAND_DELIVERY_MAX_BYTES=1073741824 MAKEWAND_DELIVERY_MAX_SECONDS=120 makewand run --timeout 300 "your task"
```

The SDK's remaining task deadline can shorten this time. These limits are frozen
into the delivery, so later environment
changes cannot weaken its checks. Embedded metadata remains limited to 32 MiB.
Exceeding a limit stops delivery and reports the limit and scan progress.

These checks do not lock external editors. Stop other writes while applying a
delivery. The exported shell script applies and reverses patches sequentially;
it does not provide simultaneous multi-file visibility or durable crash recovery.
After each patch, a checkpoint records only its touched paths, their ancestors
and the relevant root/repository identities and HEAD. Rollback checks their full
content, types and exact permissions against that postimage. The complete
destination checks still run before and after delivery, but an unrelated large
cache cannot exhaust the patch checkpoint or rollback scan. If the postimage or
its bound identities have changed, or a checkpoint cannot be established, the
script preserves it for manual recovery instead of reversing over newer edits.
Saved candidates use the separate persistent application journal described above.

## Python daemon execution contract

The optional Unix daemon supervises one independent Python process per request.
Workers use the same CLI parser as standalone execution. Each request supplies
the original invocation directory, complete argument list, stdin text and client
environment snapshot. The daemon's startup environment does not supply task
policies or credentials. Workers load Makewand from its trusted installation
path using Python's isolated mode. Process isolation does not replace repository
locks, Bubblewrap, review or delivery verification. Each request includes worker
startup overhead; there is no shared task interpreter or sub-15 ms guarantee.

The NDJSON execution protocol uses version `1` and a UUID hex `request_id`.
A client checks the server version before transmitting a task. The server sends
`accepted` before starting the worker, streams bounded `out`/`err` messages and
sends one final `exit` result. A missing final result is an IPC error, never a
successful execution. Standalone fallback is allowed only before transmitting
an execution request. Once transmission begins, an uncertain result never causes
automatic replay. Duplicate accepted IDs are refused while their records remain
in the same daemon instance.

Defaults are four execution workers, twelve connections, a 1 MiB request,
16 MiB combined worker output and a 600-second execution deadline capped at
3600 seconds. Reading an initial request has a separate five-second absolute
deadline; slow or incomplete clients cannot hold connections indefinitely.
Completed request metadata is kept in memory for one hour, with at most 4096
records. Reaching that limit refuses new execution requests instead of removing
deduplication records early. Full outputs and client environment snapshots are
not retained in result records. Restarting the daemon loses these records; an
unknown result does not prove the task was never executed.

Python callers can use `query_daemon_request(request_id)` to inspect a retained
status, or pass `cancel=True` to request cancellation. A client disconnect,
execution deadline or explicit cancellation terminates the associated local
worker and discovered descendants. On Linux, a request environment marker also
identifies descendants that created separate sessions or outlived their worker.
Cancellation cannot undo prior file writes or cancel an already accepted remote
model request. Check task artifacts and recorded results before a manual retry.
Provider-internal turns and remote billing are not counted by the IPC protocol.

Lifecycle startup uses a private singleton lock and authenticated PID/socket
matching; stale PID files never authorize signalling another process. Socket,
PID and log state are private. Signal handlers request shutdown without waiting
on the serving thread, and cleanup only removes files still owned by that
instance. A live daemon using an older protocol must be stopped or bypassed
before executing tasks through the new client.

## Hybrid review and call admission

Race A/B and their frozen baseline are sealed before synthesis. Hybrid M uses a
complete three-way merge, retaining module-level assignments, decorators,
deletions and file modes. Conflicts fail conservatively instead of discarding
unrecognized source. `inspect` is read-only; `makewand merge <race-id>` explicitly
creates and tests M. Missing tests leave M unverified. Test results and review
approval are separate Boolean or null fields, never tuple truthiness.

`makewand review --race-id <race-id> --candidate M` independently reviews the
sealed hybrid, rejects mutation during review, and binds a valid positive verdict
to exactly that manifest and application plan. Review compares the fixed baseline
commit, ignores Git replacement refs, and rejects complete hybrid diffs larger
than 64 KiB instead of approving a truncated diff. Normal application requires both
tests and review to be exactly true. Human `--force` may override those decisions,
but never integrity checks or an invalid frozen baseline.

Both native Go and Python `--max-model-calls N --call-budget-file PATH` use locked, atomic admission
reservations across processes. Coding, review, verdict follow-ups, direct CLI
commands, race judging and real-model health probes count; failures still occupy
a slot. Reservations persist through caller crashes. The file's maximum cannot
be raised by a later client's environment. This counts Makewand task dispatches,
not provider-internal tool turns or HTTP requests. Token use and monetary cost
remain unknown when providers do not supply measurements. Built-in Go providers
and Router dispatches join the same ledger. External providers called through
Router or `ChatProvider` participate; a caller invoking its own implementation
directly is responsible for admission. Unused workflow holds expire or are
released, while actual attempts are never refunded. Race reserves two coder
slots and a judge slot before starting, and leaves time for the judge.

The schema-1 request/result/event fixtures and stable outcome codes are shared
through `makewand/execution_contract.json` and parity tests in both languages.
See [execution contract](EXECUTION_CONTRACT.md) for selection policy, deadline
and telemetry semantics. Stage completion never replaces the sealed delivery
verification described above.

The command registry in `makewand/command_contract.json` drives Python dispatch
and generated Go delegation tables. Generation drift and help-routing checks
are part of release validation. Public help must reach the command's actual
parser, not merely return a successful root help response.


## Interrupted application recovery

Go TUI delivery uses `Project.ApplyFilesTransactional(ctx, files, validate)`:
seal bounded preimages and fixed sibling scratch names in private state outside
the workspace, sync the journal, replace files through a held root, then publish
a committed marker. `OpenProject` and `NewProject` recover a pending application;
SDK callers can invoke `RecoverInterruptedApply(ctx)` explicitly. Recovery checks
workspace identity, every preimage and all current paths before changing any
file. A conflicting external edit preserves the workspace and journal and
returns `ErrStaleApply`. The private state defaults to the makewand config's
`apply-journals` directory; `MAKEWAND_APPLY_STATE_DIR` overrides it. Low-level
`WriteFile`/`WriteFiles` remain direct write APIs; their callers should use the
transaction API for recoverable multi-file delivery.

Python saves an application journal before each mutation and recovers pending
applications on the next apply under the same workspace/apply locks. SDK users
can call `CandidateManager.recover_interrupted_applications(base_cwd)` explicitly.
Recovery validates all journals and backups before beginning, refuses later
workspace changes, and remains resumable if interrupted again. Committed and
rolled-back markers are durable; Python keeps preimages and journals as evidence.

These are recoverable transactions, not instantaneous filesystem-wide atomic
visibility. External editors do not join the writer lock. The crash tests cover
process termination and restart; they do not certify hardware power-loss or
network-filesystem durability. Windows flushes file data and journal headers
without claiming POSIX directory-fsync semantics.
