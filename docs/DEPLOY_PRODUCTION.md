# Production Deployment

`makewand serve` is best deployed as a single-tenant internal service on a VM or
container host. The recommended production shape is:

- dedicated service user
- `--enable-users`
- scoped `--auth-config`
- persisted `state.db`
- `/metrics` scraped by Prometheus
- regular backups of the state directory

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

## Monitoring

Use [`deploy/prometheus.yml`](../deploy/prometheus.yml) to scrape
`/metrics`. Pair it with an admin metrics token that only carries
`admin:metrics:read`.

## Operational Notes

- Prefer `127.0.0.1` + SSH tunnel or a private overlay network.
- Rotate admin tokens and session secrets on a schedule.
- Back up the state directory before upgrades.
- Run `makewand doctor --remote-check` after each deploy.
