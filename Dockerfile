### Unified image for app placeholder + Postgres primary/replica
FROM golang:1.21 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/acecinema-app ./cmd/app-placeholder

FROM postgres:15-bookworm
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/acecinema-app /usr/local/bin/acecinema-app
COPY entrypoint.sh /usr/local/bin/acecinema-entrypoint.sh
RUN chmod +x /usr/local/bin/acecinema-entrypoint.sh

ENV PGDATA=/var/lib/postgresql/data
EXPOSE 8080 5432

ENTRYPOINT ["/usr/local/bin/acecinema-entrypoint.sh"]
