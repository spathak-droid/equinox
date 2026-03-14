FROM golang:1.24-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bin/equinox-ui ./cmd/equinox-ui && \
    go build -o bin/indexer ./cmd/indexer

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates tzdata zstd && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/bin/ ./bin/
COPY --from=builder /app/equinox_markets_v2.db.zst ./equinox_markets_v2.db.zst

CMD sh -c 'if [ ! -f equinox_markets_v2.db ]; then echo "[startup] Decompressing DB (142MB -> ~1.9GB)..."; zstd -d equinox_markets_v2.db.zst -o equinox_markets_v2.db; echo "[startup] DB ready"; fi && ./bin/equinox-ui'
