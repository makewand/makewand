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
CMD ["serve", "--listen", "0.0.0.0:8080", "--enable-users", "--data-dir", "/srv/makewand/data"]
