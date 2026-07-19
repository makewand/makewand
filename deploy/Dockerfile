FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN go build -trimpath -o /out/makewand ./cmd/makewand

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /srv/makewand
COPY --from=build /out/makewand /usr/local/bin/makewand
RUN useradd -m -r -s /bin/bash makewand \
  && mkdir -p /srv/makewand/data \
  && chown -R makewand:makewand /srv/makewand
USER makewand
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/makewand"]
# The server refuses plaintext (non-TLS) listening on non-loopback addresses.
# Inside a container we must bind 0.0.0.0 for Docker port publishing to work,
# so --unsafe-no-tls is required for the container to boot at all. The traffic
# is still plaintext: NEVER publish this port on a public interface. Publish it
# on the host loopback only (see deploy/docker-compose.yml, which uses
# "127.0.0.1:8080:8080") and reach it via an SSH tunnel, or put a
# TLS-terminating reverse proxy (nginx, Caddy) in front.
#
# Auth is fail-closed: with no --token/--auth-config and an empty state DB,
# every authenticated endpoint is rejected until a token exists (the unauthenticated
# /health probe still responds). The running server reads its tokens at startup and
# holds them in memory, so issue tokens BEFORE first start (or restart after issuing):
#   docker compose run --rm makewand token issue --state-db /srv/makewand/data/state.db
#   docker compose up -d          # (re)start so the new token is loaded
# or set MAKEWAND_SERVER_TOKEN in the environment before starting.
#
# --enable-users enables auth + login + admin API WITHOUT opening public signup.
# Public self-registration is a separate opt-in (--enable-registration, off here);
# --trusted-proxy <CIDR> makes the login limiter honor X-Forwarded-For only from
# that peer. See docs/SERVER_ALPHA.md for the full flag set.
CMD ["serve", "--listen", "0.0.0.0:8080", "--unsafe-no-tls", "--enable-users", "--data-dir", "/srv/makewand/data"]
