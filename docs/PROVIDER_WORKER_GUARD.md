# Provider worker query policy

When `MAKEWAND_PROVIDER_WORKER=1` is inherited, both entry points reject commands
before argument parsing or delegation. This prevents accidental recursive
provider dispatch and administrative operations. It only prevents misuse under
normal environment inheritance; it does not defend against malicious removal of
the marker, direct provider invocation, or modification of the executable/policy.

`makewand/command_contract.json` is the authoritative list of complete command
paths, allowed flags (including their value arity), and positional argument
limits. `scripts/generate_cli_contract.py` generates the Go copy; its `--check`
mode and the shared `tests/testdata/provider_worker_policy.json` cases keep the
Python and Go rules consistent. New commands, children, aliases, abbreviated
names/flags, and flags fail closed. Each addition requires a side-effect audit.
Options may use `--flag=value`, and `-Cdir` is the audited attached short form.
`--` ends option parsing: e.g. `search -- --probe` searches for literal text and
never probes. Help on a refused management command is also refused.

## Allowed paths and audit

| Path | Audited behavior |
| --- | --- |
| `search` | Searches the selected repository using bounded file reads in worker mode. It skips ripgrep, whose inherited configuration can launch a `--pre` process. No provider, credential/config reader, state refresh, or disk cache. Accepts only `-C`/`--cwd`, search bounds and help. |
| `repomap` | Reads source files and extracts symbols with AST/regex/optional local parsers. In worker mode it traverses files without Git: even `git ls-files` can execute a repository fsmonitor hook. Parser caches are in memory. Accepts only `-C`/`--cwd`, output bounds, JSON and help. |
| `help` | Prints static command help; no positional target or changed global option is allowed. |
| `completion bash`, `completion fish`, `completion powershell`, `completion zsh` | Print static shell completion scripts to stdout, without querying providers. All four accept help; only fish, powershell and zsh accept `--no-descriptions`. Executing a generated dynamic completion request (`__complete`) remains refused. |
| A single `-h`, `--help`, `-v`, or `--version` | Static root help/version only, with no accompanying command or option. Each engine still validates its supported spelling. |

Repository queries report content the caller can already read in the selected
repository. They do not read provider credentials or administrative state.
They bypass common configuration loading, inherited budget initialization,
daemon dispatch and observer setup. Static Go commands similarly skip budget
initialization. Python delegation cannot auto-build a Go cache in worker mode,
even with inherited `MAKEWAND_DEV=1`. No live provider is needed to use the policy.
Worker `repomap` can include untracked source files because its traversal does
not use the tracked-file optimization. Worker search can be slower than ripgrep
and retains the existing file/byte/time limits of the Python fallback.

## Removed permissions

- All `state`, `token`, `user` paths are refused, including list operations.
  Backups can export credentials and server sessions; token/user list paths may
  open/migrate SQLite state or authenticate to a remote admin API. Their write
  paths restore state, issue/revoke tokens, alter activation/roles/passwords or
  log users in.
- Python `status` and `quota` are refused: even loading their cached status
  ensures directories exist and opens `.status.lock` for writing. Go `quota`
  reads credentials, refreshes quota sources/cache, and can execute a provider CLI.
- `audit` and `usage` are refused: their local/remote modes can expose server
  metadata and use inherited admin credentials; SQLite/alert paths may open
  managed state. No complete safe path has been granted in this round.
- `candidates` and `inspect` are refused: listing ensures state directories exist,
  and inspection can persist application obstacles. `sessions` reports
  host-wide process/session metadata and is outside repository queries.
- Dispatch, probe, daemon, debug/trace, explicit budget flags, and every other
  non-listed command/option are refused. `--probe=false` is still an unapproved
  option. Abbreviations cannot reach argparse's permissive option matching.

Tests use isolated state, static repositories, fake hooks and test-built binaries.
The policy is not an operating-system sandbox: selected repository content,
local parser libraries and installed makewand components remain trusted, as before.
