# Multi-stage Dockerfile for traffic-generator
FROM golang:1.22-bookworm AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download || true

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /traffic-generator main.go

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    iproute2 \
    procps \
    net-tools \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /traffic-generator /usr/local/bin/traffic-generator

ENV TARGET_URL=http://backend:8080
ENV SCENARIO=bench

ENTRYPOINT ["/usr/local/bin/traffic-generator"]
