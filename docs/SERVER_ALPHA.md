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
- WAL database requires careful backup procedures
- Concurrent agent execution can interfere with local workspace
- Remote clients may see stale session state

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
- **Recommended**: Use TLS-terminating reverse proxy (nginx, Caddy) for network access
- **SSH Tunnel**: Simplest secure remote access method

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
- `--enable-registration` (implies `--enable-users`) opens the public `/v1/users/register` endpoint. Self-registered accounts are created **inactive** and require an admin to activate (`makewand user activate`). Registration is rate-limited per-IP and globally, and password hashing is concurrency-bounded (returns 503 when saturated). Keep this **off** unless you control network access to the port
- `--trusted-proxy <CIDR|IP>` (repeatable): only when the direct peer matches one of these is an `X-Forwarded-For`/`X-Real-IP` header trusted for rate-limiting. By default client-supplied forwarding headers are ignored
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
  --state-db path/to/state.db         # SQLite state DB (users, tokens, usage)
  --data-dir path/to/dir              # Session/state directory (default ~/.config/makewand/server)
  --audit-log path/to/audit.jsonl     # JSONL audit log path
  --usage-log path/to/usage.jsonl     # JSONL usage ledger path
  --alert-webhook http://...          # Webhook for budget alerts
  --unsafe-no-tls                     # DANGER: allow plaintext on non-loopback (proxy/testing only)
```

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

- Backup your state.db before troubleshooting: `cp ~/.config/makewand/server/state.db{,.backup}`
- WAL files (`-wal`, `-shm`) are normal and should not be deleted
- Do not directly modify the database; use provided CLI commands
- Report backup/restore issues on GitHub

### Performance

- Server is single-threaded and designed for small teams
- Do not run high-concurrency workloads
- Monitor `/admin` dashboard for session and usage stats

## Feedback and Reporting Issues

If you encounter issues with the alpha server:

1. Gather diagnostics: `makewand doctor --json > diagnostics.json`
2. Collect relevant logs from `~/.config/makewand/server/audit.jsonl`
3. Report on GitHub with clear reproduction steps
4. Include version: `makewand --version`
