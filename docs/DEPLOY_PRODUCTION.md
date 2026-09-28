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
as the starting point. Directory layout:

- `/var/lib/makewand` — all state the server writes, created by systemd
  (`StateDirectory=makewand`, mode `0700`, owned by the `makewand` user):
  `state.db`, sessions, the admin session secret, optional ledgers, **and the
  auth config `server_auth.json`**. `HOME` and `MAKEWAND_CONFIG_DIR` also point
  here because `ProtectHome=true` hides `/home`.
- `/etc/makewand/makewand.env` — read-only environment file for secrets such
  as provider API keys.
- `/srv/makewand` — working directory.

The auth config must stay writable. Revoking a bootstrap token, and password,
role, or active-state changes that revoke a user's file-based tokens, rewrite
`server_auth.json`; under `ProtectSystem=strict` a copy in `/etc` is read-only,
so those revocations fail and the mutation is refused. If a save still fails
(for example, wrong ownership), the server reports the error and keeps the
token listed as active, matching its real state. Migrating from an older unit
that used `/etc/makewand/server_auth.json`:

```bash
sudo systemctl stop makewand
sudo install -d -o makewand -g makewand -m 0700 /var/lib/makewand
sudo install -o makewand -g makewand -m 0600 /etc/makewand/server_auth.json /var/lib/makewand/server_auth.json
sudo systemctl daemon-reload && sudo systemctl start makewand
```

## Option 2: Docker Compose

Use [`deploy/docker-compose.yml`](../deploy/docker-compose.yml)
with [`deploy/Dockerfile`](../deploy/Dockerfile).

Example `.env`:

```bash
OPENAI_API_KEY=replace-me
MAKEWAND_SERVER_AUDIT_LOG=1
```

## Backup and Restore

Use `makewand state backup`, which snapshots the live `state.db` with
`VACUUM INTO` (transaction-consistent while the server runs) and adds the auth
config and any JSONL ledgers to a checksummed archive:

```bash
# Run as the service user so no root-owned state.db-wal/-shm files are created.
sudo -u makewand mkdir -p /var/lib/makewand/backups
sudo -u makewand makewand state backup /var/lib/makewand/backups/makewand-$(date -u +%Y%m%dT%H%M%SZ).tar.gz \
  --data-dir /var/lib/makewand --auth-config /var/lib/makewand/server_auth.json
# Then copy the archive off the host.
```

Restore with the service stopped:

```bash
sudo systemctl stop makewand
sudo -u makewand makewand state restore <archive.tar.gz> --force \
  --data-dir /var/lib/makewand --auth-config /var/lib/makewand/server_auth.json
sudo systemctl start makewand
```

[`backup_state.sh`](../scripts/backup_state.sh) and
[`restore_state.sh`](../scripts/restore_state.sh) are thin wrappers around
these commands; pass the auth config with `MAKEWAND_AUTH_CONFIG`.

> **Always pass `--auth-config`.** `state backup` includes the auth config only
> when it is given, and a state-only archive will not restore bootstrap tokens.
>
> **Never copy the live `state.db` file on its own.** It is a WAL database; most
> recent writes live in `state.db-wal` until a checkpoint, so a plain `cp` or
> `tar` of `state.db` can produce a copy that is missing tables or rows.

## Monitoring

Use [`deploy/prometheus.yml`](../deploy/prometheus.yml) to scrape
`/metrics`. Pair it with an admin metrics token that only carries
`admin:metrics:read`.

The `path` label uses route templates (for example
`/v1/admin/users/:id/role` or `/v1/sessions/:workspace`); any other path,
including unauthenticated 404 probes, is reported as `path="other"`, and
non-standard methods as `method="OTHER"`. The recorder also keeps at most 512
label combinations, so clients cannot grow the series set.

## Operational Notes

- Prefer `127.0.0.1` + SSH tunnel or a private overlay network.
- Rotate admin tokens and session secrets on a schedule.
- Back up the state directory before upgrades.
- Run `makewand doctor --remote-check` after each deploy.
