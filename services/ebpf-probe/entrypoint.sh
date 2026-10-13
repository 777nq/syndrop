#!/bin/bash
set -e

echo "==============================================================================="
echo " ephemeral-collapse: Initializing Kernel Diagnostic Engine"
echo " Kernel Release: $(uname -r)"
echo " Operating Mode: Auto-Detecting eBPF Kprobe Support vs Procfs/Netlink..."
echo "==============================================================================="

# Check if debugfs / tracefs / bpf is available
if [ -d "/sys/kernel/debug/tracing" ] || [ -d "/sys/kernel/tracing" ]; then
    echo "[+] Tracing debugfs/tracefs filesystem detected."
else
    echo "[!] /sys/kernel/debug/tracing not mounted or unavailable."
fi

# Attempt running real BCC eBPF probe
USE_EBPF=0
if python3 -c "import bcc" 2>/dev/null; then
    echo "[+] BCC Python module present. Testing eBPF kprobe attachment..."
    if python3 /app/trace.py --test 2>/dev/null; then
        USE_EBPF=1
    fi
fi

if [ "$USE_EBPF" -eq 1 ]; then
    echo "[+] Starting live Linux eBPF kprobe tracing engine..."
    exec python3 /app/trace.py
else
    echo "[!] Host environment is containerized without privileged BPF host mounts (or running on Docker Desktop VM)."
    echo "[+] Activating zero-dependency Kernel Seam Procfs/Netlink collector..."
    echo "[+] Telemetry resolution: 1000ms /proc/net/netstat (ListenOverflows, ListenDrops, nf_conntrack)."
    exec python3 /app/fallback_collector.py
fi
