# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

本项目所有重要变更记录于此。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- **Quota-aware routing / 配额感知路由.** Routing now reads each subscription's
  remaining usage (5-hour session window + weekly cap) and steers work toward the
  pool with headroom before a cap is hit.
  - Predicted headroom is a *soft* signal — a near-limit pool is deprioritized in
    candidate ranking (new `QuotaBand` sort key, orthogonal to the Thompson
    quality score) but never removed, so quota prediction alone cannot cause a
    routing failure.
  - Confirmed exhaustion (a real quota/429 error) *hard-blocks* the pool at the
    execution boundary until its window resets.
  - `makewand quota` (with `--json`) shows the current picture per provider.
  - Data is read locally or via already-stored credentials (Claude OAuth usage
    endpoint, Codex session logs, agy login state); nothing is uploaded, and an
    unreadable source degrades to neutral.
  - Quota reads are cached to disk (`~/.cache/makewand/quota-snapshot.json`,
    120s TTL) and shared across processes, so repeated invocations don't re-hit
    the rate-limited usage endpoints; a stale cache still serves as last-good
    when a source is momentarily unavailable. 429-seals are never persisted.
  - Disabled by default for library embedders: `NewRouterFromConfig` is quota-free
    unless `RouterConfig.Quota` is set.
- **Antigravity CLI (`agy`) support.** Personal Gemini subscriptions now route
  through `agy` (the successor to the retired individual `gemini` CLI OAuth). When
  installed, `agy` takes the Gemini provider slot automatically; `MAKEWAND_AGY_MODEL`
  overrides the model.
- **Untrusted-repository mode (`--repo-trust=trusted|untrusted`).** For cloned
  third-party repositories you do not control, `--repo-trust=untrusted` routes
  generation only to direct API providers (never a local repo-aware CLI) and fails
  closed if none is configured, stops injecting the repo's `.makewand/rules.md` as
  trusted instructions, and skips local-CLI health/quota probes in the repo's cwd.
  The flag is validated globally and honored by `serve`, `doctor`, and `quota`.
  Default is `trusted` (unchanged behavior). See SECURITY.md for the boundary.

### Security

- **Candidate verification is sandboxed and fail-closed.** Test/build/dependency
  commands for generated candidates run inside a bubblewrap sandbox (read-only
  root, writable workspace, cleared environment, `--unshare-net` for test/build
  steps); when strong isolation is unavailable, verification does not execute
  unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is set. Baseline `*_test.go` and npm test
  scripts are restored before judging, "no tests" cannot reach a passing strength,
  and `.git`/CI config/scripts are protected write paths. Static preview serves via
  `python3 -I` so a repo-local `sitecustomize.py`/`http` package cannot execute.
- **Router instance isolation.** Strategy/pricing tables and provider factories are
  per-`Router` (no shared mutable package state); overrides deep-merge at field
  granularity with strict validation, and hot-reload recomputes from immutable
  defaults.
- **Multi-user server authorization.** Remote sessions are namespaced per
  user/org/project; delegated tokens cannot widen scopes/allowlists/quota beyond
  their issuer; self-registration creates inactive accounts in a single write;
  login rate-limiting ignores forwarded-IP headers unless a `--trusted-proxy` is
  configured. Public registration is a separate opt-in (`--enable-registration`).
- Trust boundary documented in SECURITY.md (verification/preview are sandboxed;
  generation runs provider CLIs on the host), with a one-time runtime notice on
  first local-CLI generation.

### Changed

- The metered `gemini -p` path is now a fallback used only when `agy` is not
  installed, avoiding accidental pay-per-token charges on personal subscriptions.
