#!/usr/bin/env bash
# ephemeral-collapse: eBPF Probe Runner
# Validates host tracing prerequisites and launches BCC/eBPF probe.

set -euo pipefail

echo "==============================================================================="
echo " ephemeral-collapse: Checking eBPF Tracing Prerequisites..."
echo "==============================================================================="

if [[ $EUID -ne 0 ]]; then
   echo "[WARN] eBPF kprobes require root / CAP_BPF / CAP_SYS_ADMIN privileges."
fi

# Ensure tracefs is mounted
if ! mount | grep -q tracefs; then
    echo "[+] Mounting tracefs..."
    mount -t tracefs nodev /sys/kernel/tracing 2>/dev/null || true
fi

# Check kernel version
KERNEL_VER=$(uname -r)
echo "[+] Detected Linux Kernel: $KERNEL_VER"

if command -v python3 >/dev/null 2>&1 && python3 -c "import bcc" >/dev/null 2>&1; then
    echo "[+] BCC environment detected. Launching eBPF kprobe tracing engine..."
    exec python3 "$(dirname "$0")/../services/ebpf-probe/trace.py"
else
    echo "[!] BCC module not found in host python environment."
    echo "[+] Launching universal /proc/net telemetry collector..."
    exec python3 "$(dirname "$0")/../services/ebpf-probe/fallback_collector.py"
fi
