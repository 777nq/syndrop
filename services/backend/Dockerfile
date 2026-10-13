# Multi-stage Dockerfile for ephemeral-backend
FROM rust:1.78-slim-bookworm AS builder

WORKDIR /usr/src/backend

# Pre-compile dependencies for fast layer caching
COPY Cargo.toml ./
RUN mkdir src && echo "fn main() {}" > src/main.rs
RUN cargo build --release || true
RUN rm -rf src

# Copy real source code and build optimized release binary
COPY src ./src
RUN touch src/main.rs
RUN cargo build --release

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    iproute2 \
    procps \
    net-tools \
    tcpdump \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /usr/src/backend/target/release/ephemeral-backend /app/ephemeral-backend

ENV PORT=8080
ENV TCP_BACKLOG=128
ENV TCP_NODELAY=1
ENV SO_REUSEPORT=1

EXPOSE 8080

ENTRYPOINT ["/app/ephemeral-backend"]
