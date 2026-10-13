.PHONY: all build run up down stop verify tui bench break-syn fix-syn break-timewait fix-timewait break-conntrack fix-conntrack break-slowloris chaos-syn chaos-conntrack chaos-timewait chaos-slowloris chaos-all logs-probe logs-backend clean

all: build

build:
	@echo "=== Building ephemeral-collapse microservice mesh & diagnostic probes ==="
	docker compose build

run:
	@echo "=== Starting ephemeral-collapse (1-Command Runnable Setup) ==="
	docker compose up --build

up:
	@echo "=== Starting services in background ==="
	docker compose up -d

down: stop

stop:
	@echo "=== Stopping ephemeral-collapse containers ==="
	docker compose down

verify:
	@echo "=== Verifying backend health and telemetry ==="
	@bash scripts/verify.sh http://127.0.0.1:8080

tui:
	@echo "=== Launching Live Terminal Dashboard (TUI) ==="
	@docker compose exec ebpf-probe python3 /app/tui.py || python3 services/ebpf-probe/tui.py

bench:
	@echo "=== Running Baseline High-Throughput Keep-Alive Benchmark ==="
	docker compose run --rm traffic-generator -scenario bench -concurrency 256 -duration 15s

# -----------------------------------------------------------------------------
# PAIRED BREAK vs FIX REMEDIATION TARGETS
# -----------------------------------------------------------------------------

break-syn:
	@echo "=== [BREAK] Inducing Listen Backlog Collapse (somaxconn=128) ==="
	@docker compose exec backend sysctl -w net.core.somaxconn=128 >/dev/null 2>&1 || true
	@docker compose exec backend sysctl -w net.ipv4.tcp_max_syn_backlog=128 >/dev/null 2>&1 || true
	docker compose run --rm traffic-generator -scenario syn-burst -burst-size 4096 -waves 4 -wave-interval 2s

fix-syn:
	@echo "=== [FIX] Applying Production Backlog Tuning (somaxconn=65535) ==="
	@docker compose exec backend sysctl -w net.core.somaxconn=65535
	@docker compose exec backend sysctl -w net.ipv4.tcp_max_syn_backlog=65535
	@echo "=== Re-testing identical 4,096 SYN burst with tuned queue (Expect: 0 drops) ==="
	docker compose run --rm traffic-generator -scenario syn-burst -burst-size 4096 -waves 4 -wave-interval 2s

break-timewait:
	@echo "=== [BREAK] Inducing Port Exhaustion (Pool: 512 ports, tcp_tw_reuse=0) ==="
	docker compose run --rm \
		--sysctl net.ipv4.ip_local_port_range="32768 33279" \
		--sysctl net.ipv4.tcp_tw_reuse=0 \
		traffic-generator -scenario time-wait -concurrency 256 -duration 15s

fix-timewait:
	@echo "=== [FIX] Applying Socket Recycling & Expanded Ephemeral Ports ==="
	@echo "Setting ip_local_port_range='1024 65535' and tcp_tw_reuse=1..."
	docker compose run --rm \
		--sysctl net.ipv4.ip_local_port_range="1024 65535" \
		--sysctl net.ipv4.tcp_tw_reuse=1 \
		traffic-generator -scenario time-wait -concurrency 256 -duration 15s

break-conntrack:
	@echo "=== [BREAK] Inducing Netfilter 5-Tuple Churn (Connection: close flood) ==="
	docker compose run --rm traffic-generator -scenario conntrack-churn -concurrency 300 -duration 15s

fix-conntrack:
	@echo "=== [FIX] Applying HTTP Keep-Alive & Connection Pooling Remediation ==="
	@echo "Re-running load with pooled HTTP/1.1 keep-alive (Expect: Minimal 5-tuple churn) ==="
	docker compose run --rm traffic-generator -scenario bench -concurrency 300 -duration 15s

break-slowloris:
	@echo "=== [BREAK] Inducing Socket Receive Buffer Pinning & Epoll Churn ==="
	docker compose run --rm traffic-generator -scenario slowloris -concurrency 500 -duration 15s

# Aliases for backward compatibility
chaos-syn: break-syn
chaos-conntrack: break-conntrack
chaos-timewait: break-timewait
chaos-slowloris: break-slowloris

chaos-all:
	@echo "=== Running Full Failure Scenario Battery ==="
	docker compose run --rm traffic-generator -scenario all -concurrency 256 -burst-size 4096

logs-probe:
	docker compose logs -f ebpf-probe

logs-backend:
	docker compose logs -f backend

clean:
	docker compose down -v --remove-orphans
	@echo "Clean complete."
