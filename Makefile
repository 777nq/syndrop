.PHONY: all build run up down stop verify bench chaos-syn chaos-conntrack chaos-timewait chaos-slowloris chaos-all logs-probe logs-backend clean

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

bench:
	@echo "=== Running Baseline High-Throughput Keep-Alive Benchmark ==="
	docker compose run --rm traffic-generator -scenario bench -concurrency 256 -duration 15s

chaos-syn:
	@echo "=== Running Failure Scenario: SYN Listen Backlog Queue Saturation ==="
	docker compose run --rm traffic-generator -scenario syn-burst -burst-size 4096 -waves 5 -wave-interval 2s

chaos-conntrack:
	@echo "=== Running Failure Scenario: Netfilter nf_conntrack Table Saturation ==="
	docker compose run --rm traffic-generator -scenario conntrack-churn -concurrency 300 -duration 15s

chaos-timewait:
	@echo "=== Running Failure Scenario: TCP TIME_WAIT Bucket Overflow & Port Exhaustion ==="
	docker compose run --rm traffic-generator -scenario time-wait -concurrency 300 -duration 15s

chaos-slowloris:
	@echo "=== Running Failure Scenario: Slowloris Socket Buffer Pinning & Epoll Churn ==="
	docker compose run --rm traffic-generator -scenario slowloris -concurrency 500 -duration 15s

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
