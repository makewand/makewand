# Ordinary-user Windows desktop verification

The reusable [native Windows workflow](../.github/workflows/native-windows.yml)
blocks release publication until the same commit passes the five Python native
modules, the engine/TUI/process/session/router/raw/ledger groups, and
`go test -race -json -count=1 -timeout=10m ./...`. Every discovered Go package
must finish, and every Python method must have a result. Required native tests
cannot become green by skipping a missing capability. Raw stdout, stderr,
explicit skips, compiler/runtime proof and the summary remain in the workflow
artifact even when verification fails. This hosted-runner gate does not certify
interactive desktop behavior or real providers.

Desktop acceptance below is a separate human check. A report starts at
`pending_manual`. Windows PE, Wine, SYSTEM, elevated Administrator tokens,
non-NTFS storage/temp directories and redirected terminal input/output are
rejected. Use a normal, non-elevated Windows 10/11 user in Windows Terminal or
another interactive terminal. No desktop pass is implied by Linux unit tests,
cross compilation or previous WinPE tests.

## Windows path limits

Makewand supports deep file paths through the native Windows file APIs while
keeping logical paths and sealed manifest names unchanged. Git operations
require a canonical local-drive workspace root shorter than 260 UTF-16 code
units. An unsupported source or shadow root is rejected before cloning or
copying; command-local `core.longpaths=true` handles long files within supported
roots. Keep the checkout and private state roots short. Private shadow cleanup
checks the original directory identity and refuses reparse points and hardlinks
before deleting files, including readonly Git administration.

## Prepare the exact inputs

Check out the release commit in a clean source directory. Untracked inputs are
rejected; keep the extracted release and verification output outside that
checkout. Install the Go version from `go.mod`, Python 3.12 and Git. Extract the
Windows release archive with `makewand.exe` and its complete `lib` directory,
and verify the archive/checksums through the release's trusted distribution.
Do not use a standalone executable without its library.

Run these PowerShell commands in the clean source checkout after verifying and
extracting the release archive. Replace the commit and binary path with the
intended values. The release publishes archive checksums; calculate the
executable digest from that verified extraction to bind this acceptance run.
The two tools are built from this checkout; they are diagnostics rather than
application packages.

```powershell
$commit = 'FULL_40_CHARACTER_RELEASE_COMMIT'
$binary = 'C:\qa\release\makewand.exe'
$expectedBinarySha = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
$out = 'C:\qa\desktop-result-new'
$tools = 'C:\qa\desktop-tools'
New-Item -ItemType Directory -Force $tools | Out-Null
$env:CGO_ENABLED = '0'
go build -o "$tools\runtime-proof.exe" scripts/windows_runtime_proof.go
if ($LASTEXITCODE -ne 0) { throw 'proof build failed' }
go build -o "$tools\codex.exe" scripts/windows_desktop_cli_stub.go
if ($LASTEXITCODE -ne 0) { throw 'stub build failed' }
python -I scripts/verify_windows_desktop.py prepare --output $out --source . --commit $commit --binary $binary --expected-binary-sha256 $expectedBinarySha --proof "$tools\runtime-proof.exe" --stub "$tools\codex.exe"
if ($LASTEXITCODE -ne 0) { throw 'desktop preparation refused' }
```

Preparation binds the commit, tracked source hashes, executable/library hashes,
proof/stub hashes, actual OS/build, hashed user SID and actual Go/Python-created
NTFS temp directories. It creates private state and a private executable stub.
Only this synthetic Codex frontend is enabled; other providers and paid API use
are disabled. Account, endpoint, Git redirection and Python injection variables
are removed from the child environment. The fixture only answers `--version`
and `auth status`. Any attempted generation exits 97 and fails acceptance.
Its audit records contain safe categories and exit codes, never prompt text.
This scope is recorded as `stub_frontend_only`, with
`live_provider_verified=false`.

## Observe the terminal and record what happened

```powershell
python -I scripts/verify_windows_desktop.py launch --output $out
```

In the actual UI, perform all four checks without submitting a natural-language
task:

1. `launch_and_help`: verify startup renders correctly; submit the UI's `/help`
   command and inspect its help display.
2. `navigation_and_resize`: navigate the help/UI controls, press Esc to return,
   shrink and enlarge the terminal, and check that the display redraws without
   missing controls or corrupted text.
3. `unicode_edit_without_submission`: type Chinese text in the editor, move the
   cursor, delete and restore characters, then clear/cancel it. Do not submit
   this text to a model.
4. `clean_exit_and_terminal_restore`: press Ctrl+C to exit and verify the shell
   prompt, cursor, echo and terminal input work normally afterward.

Record each real observation. A failure should be recorded as `fail`; do not
turn missing observations into `pass`. Replace the example observation text
with what you actually saw, without personal names, credentials or prompt text.

```powershell
python -I scripts/verify_windows_desktop.py record --output $out --case launch_and_help --result pass --observation 'Startup and help were readable; Esc returned to the editor.'
python -I scripts/verify_windows_desktop.py record --output $out --case navigation_and_resize --result pass --observation 'Observed keyboard navigation and resize redraw in the named terminal.'
python -I scripts/verify_windows_desktop.py record --output $out --case unicode_edit_without_submission --result pass --observation 'Observed unsent Chinese editing and cancellation without display corruption.'
python -I scripts/verify_windows_desktop.py record --output $out --case clean_exit_and_terminal_restore --result pass --observation 'Observed Ctrl+C exit and normal shell input afterward.'
python -I scripts/verify_windows_desktop.py finalize --output $out
```

All operations recheck inputs and desktop capabilities; `launch` also checks
again after the real UI process exits. Finalization requires a successful real
UI run, actual stub invocation evidence, exactly four valid human observations,
and no blocked generation. Exit 0 with `manual_desktop_passed` records this
limited manual scope; exit 2 means observations are incomplete, and exit 1
means failure/refusal. Changing the binary, library, source, tools or user
requires a new preparation and new observations.

Share `desktop-result.json` and, if requested, `stub-events.jsonl`. Keep
`local-launch-plan.json` private: it contains local paths. The report's source
paths are relative and user identity is hashed, but review your freeform
observations before sharing. This record cannot replace a real-provider smoke
test or prove that an unperformed human check passed.
