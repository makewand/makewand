# Single-Tenant Internal Deployment (Alpha)

> **ALPHA.** The server component is alpha software provided without production
> support commitments — no HA, no backup automation, no cryptographically
> enforced multi-tenant isolation. Read [SERVER_ALPHA.md](SERVER_ALPHA.md)
> before deploying. This guide covers a single-tenant, internal (trusted
> network) deployment only; it is **not** a public-internet or hardened
> multi-tenant production guide.

`makewand serve` is best run as a single-tenant internal service on a VM or
container host. The recommended shape is:

- dedicated service user
- `--enable-users`
- scoped `--auth-config`
- persisted `state.db`
- `/metrics` scraped by Prometheus
- regular backups of the state directory (see caveats below)

## Option 1: VM + systemd

Use [`deploy/systemd.makewand.service`](../deploy/systemd.makewand.service)
as the starting point. Recommended directories:

- `/srv/makewand`
- `/var/lib/makewand`
- `/etc/makewand/server_auth.json`

## Option 2: Docker Compose

Use [`deploy/docker-compose.yml`](../deploy/docker-compose.yml)
with [`deploy/Dockerfile`](../deploy/Dockerfile).

Example `.env`:

```bash
OPENAI_API_KEY=replace-me
MAKEWAND_SERVER_AUDIT_LOG=1
```

## Backup and Restore

Use:

- [`backup_state.sh`](../scripts/backup_state.sh)
- [`restore_state.sh`](../scripts/restore_state.sh)

These scripts snapshot `state.db`, `server_auth.json`, and optional JSONL audit
or usage ledgers into a timestamped archive.

> **State lives in two places under the systemd layout.** The state directory
> (`/var/lib/makewand`, holding `state.db`) and the auth config
> (`/etc/makewand/server_auth.json`) are separate. `backup_state.sh` takes a
> single state directory, so back up the auth config alongside it — a state-only
> archive will not restore your tokens.
>
> **`state.db` is a live WAL database.** The scripts read the file while the
> server may be writing, which does not guarantee a transaction-consistent
> snapshot. For a consistent backup, either stop the service first, or use the
> `makewand state backup` subcommand, which checkpoints the WAL before copying.

## Monitoring

Use [`deploy/prometheus.yml`](../deploy/prometheus.yml) to scrape
`/metrics`. Pair it with an admin metrics token that only carries
`admin:metrics:read`.

## Operational Notes

- Prefer `127.0.0.1` + SSH tunnel or a private overlay network.
- Rotate admin tokens and session secrets on a schedule.
- Back up the state directory before upgrades.
- Run `makewand doctor --remote-check` after each deploy.
