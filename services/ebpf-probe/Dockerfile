FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 \
    python3-pip \
    iproute2 \
    procps \
    net-tools \
    curl \
    ca-certificates \
    bpfcc-tools \
    python3-bpfcc \
    libbpf-dev \
    kmod \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY bpf ./bpf
COPY trace.py ./
COPY fallback_collector.py ./
COPY entrypoint.sh ./

RUN chmod +x /app/entrypoint.sh /app/trace.py /app/fallback_collector.py

ENTRYPOINT ["/app/entrypoint.sh"]
