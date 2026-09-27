# Verification and acceptance contract

## Go candidate verification

Candidate verification reports local diagnostic results. It does not treat output
from candidate-controlled code as an independent acceptance certificate.

- **Strength 0:** a check failed, isolation was unavailable, an expected baseline
  Go test was missing, or verification changed the input files or permissions.
- **Strength 1:** the local checks completed successfully. Human approval is
  required before applying the candidate, including when baseline tests ran.
- **Strength 2:** reserved for a trusted acceptance driver independent of the
  candidate process. No current local test runner grants this strength.

This changes the previous autopilot behavior: passing local tests no longer
authorizes automatic file application. Makewand still runs and ranks candidates,
shows check results, and prepares a candidate for approval. Even Go's structured
JSON test events can be forged by code executing in the test process; matching
every expected baseline test improves diagnostics but does not remove this limit.

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

## Python pipeline and race candidates

Python fingerprints workspace inputs before and after local tests and before
review. Git-tracked files and deliverable untracked files remain covered even
inside cache-named directories. In a directory without Git metadata, only the
`.git` administrative directory is excluded. Pytest runs without writing its
cache or Python bytecode. Input changes invalidate the result and require a new
test and review pass.

A race verdict must use the structured `MAKEWAND_RACE_VERDICT` protocol. Rejection,
missing verdicts and malformed verdicts cannot select a fastest candidate as a
fallback. Only the explicitly accepted candidate receives review approval.

Saved candidates seal content hashes, Unix permissions and the entire application
plan, including deletions. Application checks all three; changing Git metadata
cannot introduce extra deletions. `--force` can override a human decision about
test/review failures or workspace conflicts, but cannot bypass artifact integrity.
Candidates saved by older versions must be regenerated. Replacement and rollback
preserve permissions and use POSIX directory handles; native Windows users must
run the full apply workflow in WSL2.

Python shadow delivery also freezes each repository's deliverable path set before
review, including submodules. After staging and committing, it compares the raw
Git blobs, symlink targets and Git executable bits of fixed commit IDs against the
reviewed input records. A clean working tree alone is insufficient. Hooks remain
disabled and Git replacement refs cannot alter this verification. Exported patches,
published branches and `delivery_manifest.json` use those same verified commit/tree
IDs, so a later change to HEAD cannot substitute unreviewed content. The manifest
records `verified_commit` and `verified_tree` for the main repository and exported
submodule patches.
