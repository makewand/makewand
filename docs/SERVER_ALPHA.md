# Makewand Server: Alpha Status

**ALPHA WARNING**: The Makewand server component is in **alpha** and is provided without production support commitments. Do not rely on this in production systems without thorough evaluation.

## Table of Contents

1. [Status and Limitations](#status-and-limitations)
2. [Personal Remote Mode](#personal-remote-mode)
3. [Security Considerations](#security-considerations)
4. [Configuration](#configuration)
5. [Troubleshooting](#troubleshooting)

## Status and Limitations

### What is Unsupported

- **Public internet deployment**: The server is designed for personal use or trusted networks only
- **High availability**: No HA support, backup automation, or failover
- **Multi-tenancy safety**: Isolation between users is not cryptographically enforced
- **Automatic dependency management**: Dependency installation and sandboxing remain incomplete
- **Authentication at scale**: Auth system is suitable for small teams only

### Known Risks

- Plaintext protocol without TLS terminates in `UNSAFE` mode only (requires explicit flag)
- WAL database requires careful backup procedures — use `makewand state backup`
  (VACUUM INTO snapshot), not a plain `cp`/`tar` of the live `state.db`
- The JSONL audit log is append-only by convention but not tamper-evident (no
  hash chain or signature): anyone with write access to the host can edit it.
  Ship it to external append-only storage if you need tamper evidence. Admin
  mutations record their actor and target (`action`, `target_user_id`,
  `target_organization_id`, `target_project_id`, `target_token_id`,
  `target_role`, `target_active`)
- Concurrent agent execution can interfere with local workspace
- Remote clients may see stale session state

### Accounting and Budget Behavior

Budget lookup errors reject admission, while usage-write errors are surfaced
and can reject non-streaming success responses in strict mode. Currency limits
depend on provider-reported costs; unknown costs cannot provide a monetary hard
cap:

- **Per-token quota counters persist across restarts.** A token's hourly/daily
  request counts and daily/monthly spend are flushed to the state DB
  periodically and once more at shutdown, and restored on startup (see
  `token_usage_counters`). Stale day/month windows self-heal on first use; a
  corrupt persisted window timestamp fails startup rather than silently resetting
  a counter. Scope limits: on a *clean* shutdown the final flush runs after
  in-flight requests drain, but a shutdown that times out (or an abnormal exit)
  can lose the last counter delta; this covers tokens stored in the state DB
  (issued via `token issue` or self-registration) — counters for static
  file-based `--auth-config` tokens remain in-memory only and reset on restart;
  and counters are per-process, so multiple server instances sharing one DB are
  not yet strongly consistent with each other.
- **SQLite reservations persist and serialize across processes.** The trusted
  server reserves integer micro-USD before dispatch for token day/month and
  organization/project month scopes. Integer account totals and usage settlement
  update in one transaction, avoiding repeated history scans. A duplicate usage
  callback does not charge twice. Confirmed cancellation before consumption
  refunds its reservation; an interrupted/unknown call conservatively keeps its
  reservation after restart. `--budget-reservation` still estimates one call:
  actual cost above that estimate can exceed the remaining money cap, and unknown
  provider cost cannot establish a monetary hard cap. JSONL-only deployments
  retain their documented process-local budget behavior.
- **Usage-write failures are surfaced, and optionally fatal.** A failed usage
  write always logs a warning (`usage accounting write failed`) so the loss is
  visible. With `--strict-accounting`, a non-streaming request whose usage cannot
  be recorded is rejected with 503 before its **success** response is written, so
  no successful response is returned unrecorded (and the server refuses to start
  with `--strict-accounting` but no usage sink). Scope: it does not apply to
  streaming responses (already on the wire when usage is known — they log at the
  tail), and it cannot un-spend a provider call already made — a request that
  failed *after* the provider ran still incurred cost, recorded best-effort and
  surfaced. A budget lookup that errors fails closed (HTTP 503
  `budget_unavailable`).
- **Budget alerts are asynchronous.** Usage entries are still written directly
  to SQLite or JSONL; only webhook notifications enter a bounded queue of 64.
  Deliveries use a 10-second timeout and at most three attempts, with an
  `Idempotency-Key` identifying the scope, month, and severity. Queue saturation,
  delivery failures, and state-write failures are logged and counted. Shutdown
  drains notifications before closing the stores, with a 20-second deadline.
  Pending notifications are held in memory and can be lost on an abnormal exit
  or when the shutdown drain deadline expires;
  receivers should honor the idempotency key because a retry can follow a
  successful delivery whose response was lost.

### Provider Concurrency

Each provider has a shared admission limit across request-scoped Router views:
8 concurrent API calls and 1 concurrent local/subscription CLI call by default.
`--api-concurrency`, `--cli-concurrency`, and `--provider-queue-timeout`
(default 30 seconds) configure these limits. Queue cancellation or expiry
does not dispatch a provider or consume the execution call ledger. Streaming
calls hold their slot until terminal completion or cancellation. Independent
server processes have separate provider slots; persistent currency reservations
still coordinate their monetary admission through SQLite.

### Remote Session Recovery

Session saves replace complete private files atomically. Cooperating writers hold
a shared store lock through the replacement; cleanup takes the exclusive lock.
Opening an existing store attempts cleanup without waiting and skips it while a
writer is active. `Store.Cleanup()` waits for writers when explicit maintenance
is needed. Cleanup removes regular `session-*.tmp` files left by interrupted
saves, preserving stored sessions, directories and symbolic links. Keep the
stable `.session.lock` file in place while the store is running.

Unix saves, deletions and cleanup also sync the directory after changing entries.
These guarantees require a local filesystem with working file locks and atomic
replacement. Native Windows uses the corresponding OS file lock; process-crash
recovery tests do not establish a hardware or power-loss durability guarantee.

### Login and User Storage

- User and browser-admin login share per-source and global admission limits:
  30 attempts per source and 300 across the process per 15-minute window.
  Five failures lock the account for 15 minutes across source addresses.
  IPv6 sources share a /64 bucket; forwarding headers are honored only for
  configured trusted proxies. Login state has a capacity of 4,096 keys per map;
  expired keys are reclaimed during subsequent requests.
- `--registration-concurrency` controls the process-wide password-hash limit
  shared by login and registration (default 2). Excess requests receive 503
  instead of waiting in an unbounded hash queue. Login and registration bodies
  are limited to 64 KiB, email addresses to 254 bytes, and passwords to 1,024
  bytes. Passwords must contain at least eight bytes when created or reset.
  This shared gate covers HTTP login and self-registration; administrative user
  creation/password reset and standalone CLI commands do not use it.
- SQLite remains the default multi-user store. When
  `MAKEWAND_SERVER_STATE_DB=off` selects the JSON fallback, user reads and writes
  use a stable sidecar file lock across objects and processes; writes sync a
  temporary file and replace `users.json` atomically. The fallback requires a
  local filesystem with file-lock and atomic-replacement support. Using this
  fallback does not make quotas, sessions, or browser logins shared across
  server processes.

### Usage Queries and Monitoring

- SQLite usage queries have indexes for time, organization, project, token,
  user, and request ID. Summary, billing, monthly-period, dashboard, budget, and
  alert calculations use SQL aggregation rather than loading event bodies into
  application memory. Existing databases receive the indexes at startup.
- `/v1/admin/usage/events` returns 100 events by default, accepts `limit` up to
  1,000 and a non-negative `offset`, and includes `pagination` metadata in JSON.
  CSV preserves its columns and follows the same pagination; `X-Total-Count`
  reports the matching event count. Tenant and user scope restrictions also
  constrain the count. Callers exporting a complete history must request
  subsequent pages. Offset pages and counts can shift when new events arrive.
  A fixed `until` excludes newer timestamps, but late writes for older requests
  can still change earlier pages.
- `/health` is a process liveness check. `/ready` uses a two-second context to
  check local SQLite availability when a state database is enabled; it returns
  503 when that check fails. It does not make model calls or prove provider
  credentials, quota, disk capacity, or write durability. JSONL-only mode has no
  SQLite readiness check.
- Protected `/metrics` includes HTTP request counts, latency histograms, active
  requests, provider/usage/webhook/readiness/database error counts, and usage
  database connection counts and wait duration. Database error counters cover
  readiness ping failures; other admin/database operations are not all
  separately instrumented. Provider labels describe the final HTTP invocation
  or streaming failure; successful fallback attempts do
  not expose every intermediate provider failure as a separate error counter.

## Personal Remote Mode

Run `makewand` on one main machine and continue from other computers by pointing them at that host. The server uses your local providers; remote clients use the HTTP facade plus centralized chat session storage.

### Setup

1. **Start the server on your main machine**

```bash
# Basic setup
makewand setup

# Health check
makewand doctor --strict --modes balanced,power

# Run the server (loopback only by default)
makewand serve --listen 127.0.0.1:8080 --enable-users
```

2. **Create a reverse proxy for safe remote access**

If you need to access from other machines, use an SSH tunnel or a TLS-terminating reverse proxy:

```bash
# SSH tunnel from remote machine
ssh -L 8080:127.0.0.1:8080 your-main-machine

# Then connect to localhost:8080
export MAKEWAND_REMOTE_URL=http://localhost:8080
makewand chat .
```

3. **Database and audit setup**

By default, `serve` keeps users, issued tokens, and usage in `~/.config/makewand/server/state.db`. JSONL audit logging is optional:

```bash
makewand serve \
  --listen 127.0.0.1:8080 \
  --enable-users \
  --alert-webhook http://your-webhook-endpoint \
  --audit-log ~/.config/makewand/server/audit.jsonl
```

### Usage on Remote Clients

```bash
# Set the server endpoint (reach it through an SSH tunnel or TLS proxy)
export MAKEWAND_REMOTE_URL=http://localhost:8080

# Bearer token issued by the server (makewand token issue / user login)
export MAKEWAND_REMOTE_TOKEN=your-token-from-setup

# Use makewand normally
makewand chat .
```

## Security Considerations

### Network Security

- **Default**: Loopback only (`127.0.0.1:8080`). Must use SSH tunnel or reverse proxy for remote access
- **Never**: Listen on `0.0.0.0` without TLS termination
- **Recommended**: Use TLS-terminating reverse proxy (nginx, Caddy) for network access, and pass the proxy address with `--trusted-proxy` so rate limits see real client addresses
- **SSH Tunnel**: Simplest secure remote access method
- **Public internet**: unsupported (see above). If you still expose the server, for example with the Cloudflare Tunnel in [CLOUDFLARE_WEBSITE_DEPLOYMENT.md](CLOUDFLARE_WEBSITE_DEPLOYMENT.md), read that guide's risk section and put an identity-aware access layer in front of `/admin` and `/v1/admin/`

```bash
# SSH tunnel example
ssh -L 8080:127.0.0.1:8080 -N user@remote-host &
# Then connect to localhost:8080
```

### Credential Handling

- **Never** pass passwords or API keys as command arguments
- **Never** store plaintext tokens in shell history
- Use environment variables or config files with restricted permissions (mode 0600)
- Rotate tokens regularly with `makewand token revoke`

### Authentication

- Multi-user mode is **disabled by default**; without `--enable-users` clients authenticate with scoped tokens only (`--token`, `--auth-config`, or tokens issued into the state DB)
- `--enable-users` enables user management, login, and the admin API **without** opening public registration. Sessions are namespaced per authenticated identity (user/org/project) and are not accessible across tenants
- `--enable-registration` (implies `--enable-users`) opens the public `/v1/users/register` endpoint. Self-registered accounts are created **inactive** and require an admin to activate (`makewand user activate`). Keep this **off** unless you control network access to the port. Registration limits:
  - The per-address limit is the primary control: `--registration-per-ip-limit` (default 5) registrations per client address per `--registration-window` (default `1h`). IPv6 clients are counted per `/64`, because one client normally controls a whole `/64`.
  - `--registration-global-limit` (default 30 per window) is a backstop on the total number of new, inactive accounts. When it is reached, the server logs a `warning: self-registration global limit reached` line and a `registration_global_limit` audit event, and rejects further sign-ups until the window ends. A distributed client can still exhaust it, so review pending accounts (`makewand user list`) when the alert fires, and raise the limit, or set it to `0` to disable the global cap and rely on the per-address limit.
  - Password hashing is concurrency-bounded by `--registration-concurrency` (default 2); excess requests get 503.
  - Inactive self-registered accounts are not deleted automatically.
- `--trusted-proxy <CIDR|IP>` (repeatable): only when the direct peer matches one of these are forwarding headers used for rate limiting. By default client-supplied forwarding headers are ignored. `X-Forwarded-For` is read **from the right**: hops that belong to a trusted proxy are skipped and the first untrusted hop is the client, so a client-supplied left-most value cannot change the limiter key. List every proxy hop that appends to the header (for example both the load balancer and nginx). The proxy must append to `X-Forwarded-For` (nginx `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`, Caddy and `cloudflared` do this by default) or overwrite it with the peer address. `X-Real-IP` is used only when no `X-Forwarded-For` header is present
- Login admission is limited per source and across the process; five failures also lock the account across source addresses for 15 minutes (see [Login and User Storage](#login-and-user-storage)). IPv6 clients share a `/64` bucket
- Sessions are stored locally and not replicated

## Configuration

### Server Flags

```bash
makewand serve \
  --listen 127.0.0.1:8080            # Address and port (loopback by default)
  --token <secret>                    # Single Bearer token for remote clients
  --auth-config path/to/auth.json     # Scoped-token auth config (alternative to --token)
  --enable-users                      # Enable multi-user auth, login, admin API (no public signup)
  --enable-registration               # Open public /v1/users/register (implies --enable-users; accounts need admin activation)
  --trusted-proxy <CIDR|IP>           # Trust XFF/X-Real-IP from these peers for rate limiting (repeatable)
  --registration-per-ip-limit 5       # Sign-ups per client address (IPv6 /64) per window
  --registration-global-limit 30      # Sign-ups from all clients per window; 0 disables the global cap
  --registration-window 1h            # Window for both registration limits
  --registration-concurrency 2        # Shared login/registration password hashes
  --state-db path/to/state.db         # SQLite state DB (users, tokens, usage)
  --data-dir path/to/dir              # Session/state directory (default ~/.config/makewand/server)
  --audit-log path/to/audit.jsonl     # JSONL audit log path
  --usage-log path/to/usage.jsonl     # JSONL usage ledger path
  --alert-webhook http://...          # Webhook for budget alerts
  --unsafe-no-tls                     # DANGER: allow plaintext on non-loopback (proxy/testing only)
```

### Environment Variables

Flags take precedence over these variables.

| Variable | Used when | Effect |
|---|---|---|
| `MAKEWAND_SERVER_TOKEN` | no `--token`, `--auth-config`, or `MAKEWAND_SERVER_AUTH_CONFIG` | Single bearer token with every scope |
| `MAKEWAND_SERVER_AUTH_CONFIG` | no `--auth-config` | Path to the scoped-token auth config (takes precedence over `--token`/`MAKEWAND_SERVER_TOKEN`); it must stay writable, because revocations rewrite it |
| `MAKEWAND_SERVER_STATE_DB` | no `--state-db` | Path to the SQLite state DB; `0`, `false`, or `off` disables it. Default `<data-dir>/state.db` |
| `MAKEWAND_SERVER_AUDIT_LOG` | no `--audit-log` | `1`/`true` writes `<data-dir>/audit.jsonl`; any other value is a path. Unset: no audit log |
| `MAKEWAND_SERVER_USAGE_LOG` | no `--usage-log` | `1`/`true` writes `<data-dir>/usage.jsonl`; `0`/`false`/`off`/`disabled` turns the JSONL ledger off; any other value is a path. Unset: off when the state DB is enabled, otherwise `<data-dir>/usage.jsonl` |
| `MAKEWAND_SERVER_ALERT_WEBHOOK` | no `--alert-webhook` | URL that receives budget alert webhooks |
| `MAKEWAND_SERVER_ALERT_STATE` | no `--alert-state` | Path of the alert delivery state. Default `<data-dir>/alert_state.json` |
| `MAKEWAND_API_POLICY` | always | `allow_paid` lets the server use provider API keys (billed); anything else, including the default `subscription_only`, ignores them. Required for API-key-only hosts such as the Docker image |
| `MAKEWAND_CONFIG_DIR` | always | makewand config directory (default `~/.config/makewand`); `--data-dir` defaults to `<config dir>/server` |

### Accessing Server Data

```bash
# List users and tokens
makewand token list --state-db ~/.config/makewand/server/state.db

# Revoke a token
makewand token revoke runner --state-db ~/.config/makewand/server/state.db

# View audit summary
makewand audit summary --path ~/.config/makewand/server/audit.jsonl

# View recent audit events
makewand audit events --path ~/.config/makewand/server/audit.jsonl --limit 20

# View usage summary
makewand usage summary --state-db ~/.config/makewand/server/state.db
```

## Troubleshooting

### Server won't start

- Check that port is not already in use: `lsof -i :8080`
- Verify permissions on `~/.config/makewand/`
- Check database integrity: `sqlite3 ~/.config/makewand/server/state.db ".integrity_check"`

### Client can't connect

- Verify server is running: `curl http://localhost:8080/health`
- Check network connectivity with `ping` or `nc -zv`
- For remote access, verify SSH tunnel or reverse proxy is configured
- Check `MAKEWAND_REMOTE_URL` (and `MAKEWAND_REMOTE_TOKEN`) environment variables are set correctly

### Database errors

- Back up before troubleshooting with `makewand state backup ~/makewand-backup.tar.gz --data-dir ~/.config/makewand/server` (add `--auth-config <path>` if you use one). It snapshots the live database with `VACUUM INTO`. Do **not** `cp state.db`: while the server runs, recent writes live in `state.db-wal`, so a copy of the main file alone can be missing tables and rows
- WAL files (`-wal`, `-shm`) are normal and should not be deleted
- Do not directly modify the database; use provided CLI commands
- Report backup/restore issues on GitHub

### Performance

- The server handles requests concurrently (Go `net/http`), but it is sized and tested for small teams: one process, one SQLite state DB, per-process rate-limit and quota counters, and no horizontal scaling or HA
- Password hashing across login and registration is capped by `--registration-concurrency`; rate-limit and quota state remains per process
- Monitor the `/admin` dashboard and `/metrics` for session and usage stats

## Feedback and Reporting Issues

If you encounter issues with the alpha server:

1. Gather diagnostics: `makewand doctor --json > diagnostics.json`
2. Collect relevant logs from `~/.config/makewand/server/audit.jsonl`
3. Report on GitHub with clear reproduction steps
4. Include version: `makewand --version`


## Load and recovery runtime gate

[`cmd/server-drill`](../cmd/server-drill) runs actual loopback HTTP traffic,
independent Router identities, real SQLite WAL backups and hard process
interruption/restart against deterministic local providers. Reports include
p50/p95 latency, throughput, unexpected error rate and acknowledged-record
RPO/RTO. It checks tenant budget contention, cancellation, persistent unknown
reservations, stream draining, archive corruption, foreign keys, supported
schema migration and multi-component restore recovery.

Run `make test-architecture` for the short profile or `make drill-long` for
10,000 requests. `Architecture runtime` runs a short profile on every push/PR
and archives the JSON report. Hardware, provider latency, deployment topology
and data size influence results. This gate provides repeatable measurements;
it does not promote Server to GA, assert a production SLA or replace the
three-release evidence window in the original optimization plan.
