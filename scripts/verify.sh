#!/usr/bin/env bash
# ephemeral-collapse: Automated Verification Script
# Validates running harness health, Prometheus metrics, and seam failure endpoints.

set -euo pipefail

TARGET_HOST="${1:-http://127.0.0.1:8080}"

echo "==============================================================================="
echo " ephemeral-collapse: Validating Testbed & Kernel Seam Health"
echo " Target: $TARGET_HOST"
echo "==============================================================================="

# 1. Check healthz endpoint
echo -n "[1/4] Probing /healthz... "
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$TARGET_HOST/healthz" || echo "FAIL")
if [ "$STATUS" = "200" ]; then
    echo "OK (HTTP 200)"
else
    echo "FAILED (Status: $STATUS)"
    exit 1
fi

# 2. Check /bench endpoint
echo -n "[2/4] Testing high-speed /bench endpoint... "
BENCH_RESP=$(curl -s "$TARGET_HOST/bench" || echo "FAIL")
if echo "$BENCH_RESP" | grep -q "nominal"; then
    echo "OK -> $BENCH_RESP"
else
    echo "UNEXPECTED RESPONSE: $BENCH_RESP"
    exit 1
fi

# 3. Check /churn endpoint
echo -n "[3/4] Testing connection churn endpoint (/churn)... "
CHURN_RESP=$(curl -s "$TARGET_HOST/churn" || echo "FAIL")
if echo "$CHURN_RESP" | grep -q "churn_closed"; then
    echo "OK -> $CHURN_RESP"
else
    echo "UNEXPECTED RESPONSE: $CHURN_RESP"
fi

# 4. Check Prometheus metrics endpoint
echo -n "[4/4] Querying /metrics... "
METRICS=$(curl -s "$TARGET_HOST/metrics" || echo "FAIL")
if echo "$METRICS" | grep -q "ephemeral_active_connections"; then
    echo "OK (Prometheus metrics active)"
    echo "-------------------------------------------------------------------------------"
    echo "$METRICS" | grep -E "ephemeral_(active_connections|requests_total|connection_churn_total|epoll_lag_microseconds)"
    echo "-------------------------------------------------------------------------------"
else
    echo "FAILED to retrieve metrics"
    exit 1
fi

echo "==============================================================================="
echo " [SUCCESS] Harness is operational and ready for load & chaos injection!"
echo "==============================================================================="
