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

On Linux, every verification step uses Bubblewrap with its own network
namespace. Bubblewrap enables loopback inside it for local test servers; host
loopback, host abstract sockets and the host default route remain inaccessible.
A dependency step is split so only downloading uses the network and no
project-controlled code runs while a step has it (owner decision, 2026-10-21).
The fetch — `pip download --only-binary=:all:` of strict PEP 508 **named**
requirements (never a requirement/constraint file itself, nor a buildable
`file:`/path/sdist/VCS/URL or `name @ …` target, wherever it is declared), with
the install's http(s) index options and those of the pip configuration it would
read (the project's `pip.conf`/`pip.ini`, the sandbox HOME's, site-wide and the
trusted pip's site file; a configured index is never replaced by PyPI, a source
the fetch cannot be given refuses it) and a constraints file of named pins
(nested `-c`/`-r` followed); `npm ci --ignore-scripts` of a registry-only
lockfile resolved and inspected first, only for `npm ci`/`npm install` without
package arguments; `cargo fetch` pinned to a named rustup toolchain; `go mod
download`/`tidy` — runs in a fresh, private fetch directory outside the
workspace and its sandbox HOME (`$XDG_CACHE_HOME/makewand/dep-fetch`), with the
workspace path covered by an empty tmpfs. The host copies in, through `os.Root`
handles and refusing symlinks and non-regular files, only what the fetch reads:
`package.json` and the lockfile; every `Cargo.toml`/`Cargo.lock` with empty
stubs at the target paths cargo needs; `go.mod`/`go.sum`/`go.work` (and those of
local replacement modules inside the workspace) with, for `go mod tidy`,
import-only stubs of the Go files (build constraints, package clause, import
paths, no code); the wheels of a local `find-links` directory. The fetch runs
the package manager resolved on the host outside the workspace (no symlink hop
or `#!` interpreter inside it), with a `PATH` of directories outside the
workspace only, gets outbound-only `slirp4netns` networking restricted to public
destinations, a fresh, empty HOME (`/tmp/makewand-fetch-home`) whose tool
config-file locations are neutralised (`GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0`, npm user/global config at two
distinct, non-existent paths, `PIP_CONFIG_FILE=/dev/null`, `GOENV=off`) and no
host module cache, and writes only the fetch directory and the shared,
host-owned caches (`GOMODCACHE` with `cache/vcs`, cargo's `CARGO_HOME`, npm's
cache), which every later loopback step binds read-only (the go module cache as
the first local `GOPROXY` entry, `CARGO_HOME` for cargo). The build/install that
may run project code (`setup.py`, a PEP 517 backend, `build.rs`) then runs
loopback-only in the workspace from what was fetched (`pip install --no-index
--find-links <wheelhouse>`, including `-e .`; `cargo build --offline`), and the
host puts the files the fetch produced that belong in the workspace there through
the workspace's `os.Root` handle (`go.mod`/`go.sum` after `tidy`, `Cargo.lock`,
the resolved `package-lock.json`, `node_modules` copied with its links
recreated, never followed). Every host-side write into a sandbox path (the
sandbox HOME, its go-telemetry `mode` file, the setuptools `egg_base`
configuration, the fetch directory) goes through the `os.Root` handle the path
was checked through, never a path reopened afterwards. Without the network
tools, or when the network cannot be attached, the plan runs unsplit and
loopback-only, as installs did before #80
([policy, overrides and limits](../SECURITY.md#dependency-installs--依赖安装)).
A failed pip fetch is reported as `ErrDependencyNetworkUnavailable`, an
environment limitation of fetch-only networking, only when the index the fetch
used shows releases matching the specifier and none of them offers a wheel the
interpreter can install (makewand reads the index's PEP 691/503 page from a
probe with the same user-mode network, running no code, and applies PEP 440); a
project or version the index lacks, a matching release with a usable wheel, or
an index without a usable answer is the candidate's failure. So is a failure of
the loopback build, unless a declared VCS or URL dependency whose environment
marker holds (evaluated before the build, for the fetch's interpreter) was never
fetched; a dependency that only surfaces once project code runs, or output the
build forges, is attributed to the candidate.
A failed fetch is reported as `ErrDependencyNetworkUnavailable`, an environment
problem rather than a candidate failure, only when makewand confirms the outage:
with network, its own connectivity check of the ecosystem's default registry
(for go, the sandbox's `GOPROXY` and checksum database, walked as the go command
walks them) gets no response; without network, the project's files show a
registry dependency that is not installed (for `go mod tidy`/`go mod download`:
`go.mod`, `go.sum`, the imports and both module caches). Package-manager output
alone never decides, except for a pip install of named requirements alone, which
runs no project code. When the suspicion is not confirmed (the registry answered,
the check could not run after two attempts, or nothing had to be downloaded),
the failure stays the candidate's and `ExecResult.Note` says why; verification
appends it to `DepsError` or `QuickCheckError`. After a confirmed outage the TUI
skips the tests instead of penalising the candidate or starting auto-fix.
Installs keep the input-integrity check meaningful: npm without a lockfile runs
with `--no-package-lock` (the lockfile its networked fetch resolves stays in
the fetch directory and never reaches the workspace); pip installs of a project with `pyproject.toml` or
`setup.py` use `--use-pep517` and write `*.egg-info` outside the tree; a Cargo
project with `Cargo.lock` runs every cargo step with `--locked`, reporting a
stale lockfile as a candidate failure, and a `Cargo.lock` that cargo generates
for a project without one is removed. The documented, acknowledged unsafe host
execution option still bypasses sandbox isolation; its connectivity check runs
on the host with the host's proxy settings.

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

Race baseline freezing uses the same host manifest policy as DAG staging: ignored
untracked logs/data and uncopied dependency trees are omitted, tracked files stay
bound, and both host checks share one digest cache. Each check enforces the
workspace snapshot file, byte and time limits before dispatch. A hash error,
limit violation or copied-input change aborts with `FAILED` and no provider call.

Missing detected Go/npm/Cargo test tools or malformed Node test configuration
produce `UNVERIFIED`; they are never silently skipped. A pipeline without
executed local test evidence stops before review and delivery. Python's
unittest fallback reporting zero tests is also unverified.
The Python runner is chosen inside the sandbox, not by the host interpreter: a
read-only probe there, `python -B -c "import pytest"`, decides whether pytest can
be imported, first with the interpreter of the run, then with the Python
interpreter that the first line of a host `pytest` script names (a user-site
pytest the sandbox hides cannot be imported, and a script whose interpreter the
sandbox cannot start, or whose first line names no Python interpreter, is ruled
out); otherwise unittest runs. The probe only imports pytest, so it loads no
pytest plugin and no `conftest.py`. Without bubblewrap, under the unsafe host
opt-in (`MAKEWAND_UNSAFE_HOST_EXEC=1` with the recorded acknowledgment), it runs
on the host like the tests themselves. Tests that cannot run are `UNVERIFIED`, not
failing tests: a pytest configuration without an importable pytest, pytest or
unittest exit code 5 (nothing collected or run), zero executed tests, and
pytest's own import error, with or without quotes, when pytest never started a
session (nothing on stdout, where pytest reports even without its header, and
the import error as the last line of stderr) or when unittest's only failures are
test modules that could not import pytest. A test that ran and failed is a
failing test, whatever it prints. A gate whose time budget runs
out before every suite produced a result is `TIMEOUT` (exit 16). Neither outcome
starts an auto-fix round or ends as `FAILED`, but failing tests an earlier suite
reported, and a run that changed tested inputs or left test output that could run
as code, still fail the gate; test output such a run created is removed as after
any other run. In `race` such a candidate has no test evidence and cannot win:
when no candidate passed its tests, the race ends `UNVERIFIED`, or `TIMEOUT` when
a candidate's gate ran out of time or the generation budget was spent before it
could start.
Saved candidates require explicit generation, test and review passes for normal
application. Missing SDK metadata remains unknown; it is never inferred from
generation success. An explicit human `--force` decision remains supported.

A race verdict must use the structured line `MAKEWAND_RACE_VERDICT_<nonce>`, bound
to a random per-task nonce and fenced diffs, so a verdict line a judge echoes from
a candidate diff is inert. An un-nonced `MAKEWAND_RACE_VERDICT:` line never counts,
nor does an un-nonced review verdict where a `MAKEWAND_VERDICT_<nonce>` line was
asked for; no environment setting or test harness re-enables them (the former
`MAKEWAND_ALLOW_LEGACY_VERDICT` and `MAKEWAND_STRICT_NONCE` switches are gone).
Rejection, missing verdicts and malformed verdicts cannot select a fastest
candidate as a fallback. The judge must receive the whole prompt; one independent
of both contestants is preferred, and a judge's pick of its own provider's
candidate is approved only after a read-only, nonce-bound review by another
provider. A Winner-Take-All win (the first candidate to pass its tests) needs the
same review: `FAILED` exits 10, no reviewer or no verdict exits 11, and records
saved before this review existed apply only with `--force`. That reviewer is
chosen by the judge's rule: the first usable engine in the judge order (agy,
codex, claude, grok, muse, local) that is neither contestant and can receive the
whole review prompt, otherwise the other contestant, never the winner's own
provider.
Candidates without detected tests remain unverified and cannot obtain normal
application approval. Race review includes each complete candidate diff; a diff
over the review cap (`MAKEWAND_REVIEW_DIFF_MAX_BYTES`, 256 KiB by default) remains
inspectable but is never reviewed truncated. Only the explicitly accepted, tested
candidate receives review approval. Coding, tests, judging and optional hybrid
verification share the race deadline.

Saved candidates seal content hashes, Unix permissions and the entire application
plan, including deletions. Application checks all three; changing Git metadata
cannot introduce extra deletions. Inside a Git work tree the sealed manifest binds
every file Git tracks or would add, also below dependency or build directory
names; pending records sealed by 3.2.0 (no `manifest_scope`) are still verified
with the earlier scope. A candidate that adds a file the workspace already
has (an ignored or untracked file its isolated copy never received) is a workspace
conflict, and so is uncommitted work on a file the candidate changes as
`git status` lists it: every untracked file counts, also one inside an untracked
directory, and names compare byte for byte. A target that changes between the
conflict check and the write is an `APPLY_CONFLICT` too, and nothing is written.
`--force` can override a human decision about test/review failures or workspace
conflicts, but cannot bypass artifact integrity, and an entry apply cannot write
(a directory, symlink or special file at a target, a non-directory above one, an
unwritable directory) is an `APPLY_CONFLICT` even with `--force`. `makewand apply`
exits with the status of its typed result
([outcomes](EXECUTION_CONTRACT.md#outcomes-and-compatibility)).
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

## Python host-mode delivery

A host-mode pipeline edits the workspace inside a transaction that snapshots it
before the task and rolls back on any refusal. Before delivery only the paths the
task created, modified or deleted are judged, on type, mode and content: a ctime
or inode change alone is not a modification. A change the reviewed diff does not
cover blocks the delivery: an ignored file the task created or changed, a binary
or executable artifact or an executable bit the diff does not show (ignored where
the filesystem gives every file one, such as WSL drvfs, CIFS or exFAT mounts), a
security-sensitive file (`.env*`, `.envrc`, `*.pth`, `sitecustomize.py`,
`usercustomize.py`, files under `.husky/` or `.githooks/`), a file in a submodule
or nested repository, new nested `.git` metadata, or a hook added to the hooks
directory of this checkout or of a checked-out submodule. Such a hook is moved to
`<artifacts>/rejected_*/quarantine/git-hooks/` and reported with an `mv` command
that restores it. A hooks directory shared with other checkouts (a linked
worktree, a repository with live linked worktrees, a `core.hooksPath` from the
user's configuration outside the repository) is reported once and never blocked,
quarantined or rolled back; `*.sample` files are ignored and `.git/config` is not
compared. The outcome is `UNVERIFIED` (exit 11) with the transaction's reason,
which names files that could not be restored; the rollback archive keeps new
untracked files, task commits and submodule patches, and quarantines what it
could not archive instead of deleting it. The review diff, the delivery check and
exported patches ignore the user's git configuration that would change them
(external diff and textconv drivers, colour, `diff.relative`, path prefixes,
submodule format, `diff.context`) and carry three context lines; rollback uses
literal pathspecs.
Undecodable path bytes are shown as `\xNN` in messages and artifacts.

Host-side git commands that run in a repository read attributes from the empty
tree (`--attr-source`), so no `.gitattributes` in the work tree or index selects
a filter, diff or merge driver, and each runs with `info/attributes` moved aside
in every git directory and common directory git may use for its working
directory: makewand reads the files git's repository discovery reads (a `.git`
file, a `.git` directory or an implicit bare repository there or in a parent)
and the `commondir` of each, whether `.git` is a file or a directory. A `.git`
file or `commondir` git cannot read makes the command fail closed (`Cannot
shield Git attributes`). The empty tree is named by the full object id of the
repository's own object format (SHA-1 or SHA-256), read from
`extensions.objectFormat` in the common directory's `config` exactly as git's
repository-format check reads it (no include is followed); a hex id of the other
length would be resolved as a ref name, so a branch named like the SHA-1 id
could hand a SHA-256 repository's attributes to a tree of its choosing. A config
git cannot parse, or an object format git does not know, also fails closed.

Apart from the commands listed below, no host git command starts a git inside a
nested repository: a child git there reads that repository's own config and
`info/attributes`, which the shield does not cover and an attribute source does
not replace, and would run a filter while hashing a tracked file.
Diffs run `--submodule=short --ignore-submodules=dirty` and `git status` runs
`--ignore-submodules=dirty`: the option, unlike `diff.ignoreSubmodules`, outranks a
`submodule.<name>.ignore` that `.gitmodules` (a work-tree file) sets. git still
reports a submodule whose commit pointer moved. `git add` reads no such setting,
so makewand's `git add -A` (`git_helper.add_all`, also for its private diff
indexes) marks each gitlink of the index assume-unchanged for the add, which then
never looks at it, unmarks it right after, and stages it with `git update-index`,
which only reads each nested `HEAD`; a gitlink below a symlinked directory is
dropped from the index unread, as `git add -A` drops it. Exclude pathspecs are not
used: `git add` refuses them once a directory above a gitlink is ignored or a
symlink. A gitlink the user marked assume-unchanged or skip-worktree is left
alone, as `git add -A` leaves it. A makewand process killed between the marking
and the unmarking leaves the gitlinks marked; `git update-index
--no-assume-unchanged <path>` clears them. The host rollback unstages with `git rm --cached -f`:
without `-f`, git rm's up-to-date check runs `git status` inside a staged nested
repository (and refuses a path whose staged content differs from both `HEAD` and
the file). The shadow copy checks each submodule out with `checkout -q`, and the
shadow delivery commits inside a submodule only when something is staged there:
reporting local changes after the switch, like the status `git commit` prints
when there is nothing to commit, runs `git status` in a nested submodule that
`.gitmodules` marks `ignore = none`. Every command also carries command-scope
configuration (`GIT_CONFIG_COUNT`, which git hands on to every git it starts):
`diff.ignoreSubmodules=dirty`, `submodule.recurse=false` (a user's
`submodule.recurse=true` made `checkout`, `reset --hard` and `read-tree` check a
submodule's files out through its own filters), `fetch.recurseSubmodules=false`
and `push.recurseSubmodules=no`. A clone runs `--no-checkout`; the files are
checked out afterwards in the new repository, with its own shield and attribute
source, so neither an untrusted repository's `.gitattributes` nor a filter the
user defines globally runs while cloning.

These host git commands still start a git inside a nested repository; none of
them can run a filter that a repository's own files define or select:

- `git submodule status --recursive`, which makewand uses to list submodules,
  starts `git describe` (up to four forms, naming the checked-out commit) and
  `git submodule--helper status --recursive` inside each initialized submodule.
  describe reads refs and objects; the helper compares commit pointers
  (`diff-files --ignore-submodules=dirty` limited to the gitlinks).
- The shadow copy's `git submodule update --init --recursive` clones each
  submodule into a new repository of the copy and checks it out there. That
  repository's config is the clone's own and its `info` comes from git's
  templates, and the checkout inherits the empty-tree attribute source
  (`--attr-source` exports `GIT_ATTR_SOURCE` to every git it starts), so no
  `.gitattributes` selects a filter.
- Transport with the user's submodules: the shadow copy fetches each submodule's
  checked-out commit from it (`git upload-pack` runs there), and the shadow
  delivery pushes each verified submodule commit to a new branch in it
  (`git receive-pack` runs there). Neither reads attributes or updates a work
  tree. A local transport hands neither `-c` nor `GIT_CONFIG_COUNT` settings on,
  so a receive hook the user installed in the destination runs, as on any push
  to it; the same holds for the push to the repository itself.

Where makewand relied on `git status` to see changed content inside a submodule,
it asks each submodule itself (`git_helper.dirty_submodule_paths`): every
checked-out submodule that `.gitmodules` registers for a gitlink of the index,
recursively through the submodules each of them registers, gets its own shielded
`git status`, and a dirty one is reported as the gitlink git status reported. This
covers the working-tree isolation check, the dirty-file lists (syntax gate,
candidate conflicts and change lists), the host transaction's task-start baseline
and rollback verification, the shadow delivery's clean checks and the DAG
staging copy's checkpoint and seal. Two observable differences: uncommitted
changes inside a nested repository that `.gitmodules` does not register are no
longer reported (a moved commit pointer still is), and `submodule.<name>.ignore`,
whether `.gitmodules` or the repository's own config sets it, no longer hides a
registered submodule's changes from these checks. A submodule that builds or
tests write untracked files into, which a repository may mark
`ignore = untracked`, now counts as dirty: the isolation check picks the shadow
copy, and the shadow delivery's clean checks and the DAG checkpoints refuse while
it stays so. `.gitmodules` is read with `git config -z`, so a submodule name or
path may hold any whitespace, quotes or non-ASCII characters. A registered
submodule below a symlinked directory or a junction is never asked (git status
takes it for deleted), and `git submodule status` refuses such a registration, so
the review diff and the shadow copy fail closed on it.

The auto-fix loop's capture and restore of dirty files
(`orchestrator.capture_dirty_files` / `restore_dirty_files`) reads plain
`git status` and is deliberately not given this per-submodule check: a submodule
whose only change is content inside it (no moved commit pointer) is neither
captured before an auto-fix attempt nor removed by the restore after a failed
one. Before, the restore deleted such a submodule's directory when the capture had
missed it; its content is now left as the attempt left it, and the review diff,
which reads each submodule's own diff, still shows it.

`git apply` cannot take an attribute source in work-tree mode (git segfaults) or
outside a repository, so every apply runs with each configured filter driver
disabled by command-scope configuration; its index modes also read attributes
from the empty tree. makewand lists the drivers right before git starts, with the
shield already in place; a process that writes the repository's configuration at
that very moment can still add a driver between the listing and git's own read,
since git has no setting that disables every filter driver whatever its name. The
delivery patch check and the generated `apply_delivery.sh` apply the same way:
the script's applies run through its private helper, which lists the
destination's drivers with the command and environment of the apply. The API
relay applies its diff blocks through `run_git_cmd`, with a git binary from
outside the workspace; that apply also carries `core.autocrlf=false`, so on a CRLF
checkout configured `core.autocrlf=true` an LF diff block may not apply.

`run_git_cmd` refuses a global option that would point git at another
repository, work tree, namespace, attribute source or set of git programs than
the ones it shields for its working directory (`-C`, `--git-dir`, `--work-tree`,
`--namespace`, `--bare`, `--attr-source`, `--exec-path`, `--config-env`, and `-c`
settings in an `include` or `includeIf` section): callers pass the working
directory instead. `GIT_CONFIG` (only `git config` reads it, in place of all other
configuration, so the filter driver listing would miss the drivers `git apply`
runs), `GIT_ATTR_SOURCE`, `GIT_CEILING_DIRECTORIES`,
`GIT_DISCOVERY_ACROSS_FILESYSTEM` and `GIT_NAMESPACE` are removed from the
inherited environment, like `GIT_DIR` and the other redirection variables.

Pre-existing files that are not backed up (dependency and cache directories,
files over the backup limits) get a SHA-256 digest at task start, smallest first,
within `MAKEWAND_FINGERPRINT_FILE_LIMIT` (default 64 MiB per file),
`MAKEWAND_FINGERPRINT_TOTAL_LIMIT` (512 MiB) and `MAKEWAND_FINGERPRINT_SECONDS`
(20 s). Every file left without a digest then gets a sampled fingerprint, 16
chunks of 64 KiB at fixed offsets (at most 1 MiB per file), in a separate budget
with the same limits, taken in turns across file sizes. Files are read in that
order, so running out of time leaves the same files without a digest or sample as
running out of bytes would, never a file only because its path sorts late. The
same limits bound the delivery check and the rollback verification, which re-hash
a file only when its inode or change time moved and its size, mtime and mode did
not. A full digest decides, and so does a sample that covers the whole file; a
partial sample clears a file only when a changed link count explains its stat
change (a hard link added elsewhere). A
file whose re-hash the budget refuses, or that has neither, is unconfirmed: it
blocks, naming the limit to raise, and is never reported as modified or
unrestorable, and the rollback report says when the baseline could not be
confirmed. Residual: an in-place patch of a file over 1 MiB placed between its
sampled chunks, with the mtime restored and a hard link added outside the
workspace, is delivered. When the time budget of the delivery check or the
rollback verification binds (slow disks), the changed files it has not re-hashed
yet block as unconfirmed.

Test output has one definition, `git_helper.is_test_output_path`, shared by the
test gate and the delivery check: reports, coverage data and tool caches at the
default locations of pytest (`--junitxml`), pytest-cov and coverage.py,
hypothesis, tox/nox, jest, vitest, c8, nyc, istanbul and Playwright. `coverage/`
and `playwright-report/` count only at a report root (the repository root, the
task's directory, or the root of a package with `package.json`, `pyproject.toml`
or `setup.py`) and never inside installed packages; Python source and test tool
configuration never count, and a file Git tracks is always a tested input. After
the suites, `run_local_tests` removes the test output its run created, except
output that could run as code (an executable bit, Python source, a
security-sensitive name) or a link, which fails the test gate, named with its
reason. Directories that existed before the run, empty ones included, are not
removed. An ignored directory (in a work tree one a `.gitignore` covers, elsewhere
a dependency or cache directory name) is recorded without being walked: it is kept
even when it held only ignored files the tests deleted, but the directories inside
it are not recorded, and one of them that held only such files is removed together
with the test output the run wrote there. If recording the directories, the
ignored ones included, exceeds `MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS` or
`MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES`, no directory is removed. A pre-existing
test-output file the run rewrote is kept only as plain data (JSON, XML, LCOV,
logs, report HTML/CSS and images, Playwright `.zip`/`.webm` in `test-results/` or
`playwright-report/`) or with identical bytes, and never inside `.tox`, `.nox` or
`.jest-cache`. The gate also accepts other gitignored paths, so a report at a
custom ignored location (`--junitxml=results/pytest-junit.xml`) passes the gate and
then blocks the host delivery as `UNVERIFIED`. Its `.gitignore` fallback, used only
where `git check-ignore` cannot run (outside a work tree, a path inside a
submodule), reads the tested directory's `.gitignore` as git does (lines split at
newlines, a UTF-8 BOM and a carriage return before the newline dropped, trailing
spaces trimmed unless escaped, a leading `\#` or `\!` literal) and matches with
git's own rules on the bytes of the path: the last matching pattern wins and `!`
re-includes, but nothing inside an ignored directory can be re-included; a pattern
with no slash but a trailing one matches a name at any depth, any other is
anchored; `*` and `?` never match `/`, `**/`, `/**` and `/**/` span directories,
and `[...]` sets take ranges, negation and `[:class:]`. It never treats a tracked
file as ignored. Matching uses memoized iterative frames and at most 100,000
work steps per candidate path (shared across its leading directories and rules).
If that bound is exhausted, or a name cannot be encoded, the path stays a tested
input. `.gitignore` lines remain bytes, so a non-UTF-8 comment cannot crash the
fallback on native Windows. Notes about inputs that changed between the test run
and the review snapshot name the files.

## Multi-stage DAG delivery

`makewand plan` decomposes a goal into a DAG of tasks; a single-task DAG runs as
an ordinary pipeline. A multi-task DAG runs every task in one private staging copy
of the repository root, each stage in the directory the DAG was started from,
and commits each passed code stage there, so later stages build on tested and
reviewed results. A read-only node (review, explain) sees every change since the
DAG base as uncommitted; whatever it writes, deletes or chmods is discarded and
listed, never committed or delivered. The cumulative result is delivered once, as
candidate `A` of a sealed `dg_…` record: applied with the candidate transaction
when the workspace is safe, otherwise left pending with the exact
`makewand apply` command. The staging copy carries the repository's
`.git/info/exclude` and `core.excludesFile`; like race copies it omits gitignored
files such as `node_modules`, which a test-related stage failure lists.

Every manifest the staging binds is limited by
`MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES` (100,000 files by default),
`MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES` (512 MiB) and
`MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS` (10 s per manifest, which a first read
on a slow disk can exceed). Files and bytes are counted before anything is hashed,
so a repository over a limit is refused before any copy is made: the outcome is
`UNVERIFIED`, no stage runs, and the message names the limit (for example
`工作区 <root> 中需要计算摘要的文件超过 536870912 字节 (MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES)`).
The freeze check of the workspace binds only what the copies receive: untracked
files that are gitignored or lie below dependency or build directory names are left
out, so a log written meanwhile in an ignored directory does not stop the DAG, while
tracked files stay bound wherever they are. Within one run a file whose device,
inode, size, mtime and change time are unchanged keeps its digest instead of being
read again, unless it had changed less than 2 s before it was read.

A result containing a symlink, a gitlink or an entry apply cannot write stops as
blocked and is sealed for inspection and manual recovery only. A failed stage, a
rejecting read-only review or an interrupt seals the verified part as a pending
partial candidate. The guidance runs the checks apply will run: commit
uncommitted work the DAG built on (stash cannot help), commit an uncommitted
deletion the DAG re-created, and use `--force` only for targets the copy never
had or files changed during the DAG; when a workspace entry blocks every apply,
no apply is offered. Native Windows refuses multi-task DAGs (use WSL2).

Isolated copies (such as race candidates and the DAG staging copy) and ephemeral
baseline repositories never write makewand's own metadata through links that
`git init` copied from a global `init.templateDir`: right after `git init` a linked
(or hard-linked) `config` becomes a regular file of the repository before makewand
sets its identity there (and the DAG its `core.excludesFile`), and the staging
copy's `info/exclude` and the ephemeral marker are written as new regular files,
replacing a link at that name or at a directory on the way.
`.git/info/attributes.mw_gate` is never created or opened through a link; one
planted there makes makewand's git commands in that repository fail closed
(`Cannot shield Git attributes`). A template-linked `info` directory is replaced
with an empty repository-owned directory immediately after `git init`, before
another git command can run its attribute shield. Windows metadata writes also
reject reparse roots and replace child junctions without writing their targets.
Known issue: `git init` itself still writes its core settings through a
template-linked `config` and changes that file's mode, as it does for every
repository created from that template.

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
than the review cap (`MAKEWAND_REVIEW_DIFF_MAX_BYTES`, 256 KiB by default) instead
of approving a truncated diff. Normal application requires both
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

### Host Git executable and three-way merge defaults

The executable resolver excludes the full enclosing workspace, including a parent repository when the caller starts in a subdirectory or nested repository. A cached executable and symlink target receive the same check. A repository cannot place `bin/git` on PATH above the current subdirectory and have the API relay execute it.

Three-way apply also overrides `merge.default`. Git lets user-defined drivers shadow names such as `text` and `binary`, so the filter/merge configuration is listed and a name outside all configured driver names is selected. Git then uses its internal text merge, preserving normal conflicts and nonconflicting combinations without a repository shell command. The standalone delivery environment follows the same rule. See Git's primary implementation, [find_ll_merge_driver](https://github.com/git/git/blob/master/merge-ll.c).

A plain-directory ignore probe uses a temporary repository outside the workspace. Its executable resolver retains the original workspace exclusion for every probe command as well as the temporary repository boundary, including cached executables and symlink targets. Changing the cache between probe commands cannot execute the workspace Git.
