### Unified image for app placeholder + Postgres primary/replica
FROM golang:1.21 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/acecinema-api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/acecinema-scanner ./cmd/scanner

FROM debian:bookworm-slim
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/acecinema-api /usr/local/bin/acecinema-api
COPY --from=builder /out/acecinema-scanner /usr/local/bin/acecinema-scanner
COPY entrypoint.sh /usr/local/bin/acecinema-entrypoint.sh
RUN chmod +x /usr/local/bin/acecinema-entrypoint.sh

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/acecinema-entrypoint.sh"]
