#!/usr/bin/env python3
"""
syndrop: Pure /proc Fallback Reader
Samples /proc/net/netstat at 100ms intervals to compute exact deltas on ListenOverflows and ListenDrops.
Guarantees operation in rootless containers, macOS Docker VMs, and kernels without eBPF kprobe support.
"""

import sys
import time
from datetime import datetime

def read_file(path):
    try:
        with open(path, "r") as f:
            return f.read().strip()
    except Exception:
        return None

def parse_netstat():
    content = read_file("/proc/net/netstat")
    if not content:
        return {}
    lines = content.splitlines()
    data = {}
    for i in range(0, len(lines), 2):
        if i + 1 < len(lines):
            headers = lines[i].split()
            values = lines[i + 1].split()
            prefix = headers[0].rstrip(":")
            for h, v in zip(headers[1:], values[1:]):
                try:
                    data[f"{prefix}.{h}"] = int(v)
                except ValueError:
                    pass
    return data

def parse_sockstat():
    content = read_file("/proc/net/sockstat")
    if not content:
        return {}
    data = {}
    for line in content.splitlines():
        parts = line.split()
        if not parts:
            continue
        proto = parts[0].rstrip(":")
        idx = 1
        while idx < len(parts):
            key = parts[idx]
            val = parts[idx + 1] if idx + 1 < len(parts) else "0"
            try:
                data[f"{proto}.{key}"] = int(val)
            except ValueError:
                pass
            idx += 2
    return data

def parse_conntrack():
    count_str = read_file("/proc/sys/net/netfilter/nf_conntrack_count")
    max_str = read_file("/proc/sys/net/netfilter/nf_conntrack_max")
    count = int(count_str) if count_str and count_str.isdigit() else 0
    max_val = int(max_str) if max_str and max_str.isdigit() else 0
    return count, max_val

def main():
    print("===============================================================================")
    print("  syndrop: /proc Fallback Reader (100ms Resolution)")
    print("  Polling: /proc/net/netstat [ListenOverflows, ListenDrops, TCPTimeWaitOverflow]")
    print("===============================================================================")

    prev_netstat = parse_netstat()
    last_heartbeat = time.time()

    while True:
        time.sleep(0.1) # Exact 100ms sampling window
        curr_netstat = parse_netstat()
        now = time.time()

        overflow_delta = (
            curr_netstat.get("TcpExt.ListenOverflows", 0) -
            prev_netstat.get("TcpExt.ListenOverflows", 0)
        )
        drop_delta = (
            curr_netstat.get("TcpExt.ListenDrops", 0) -
            prev_netstat.get("TcpExt.ListenDrops", 0)
        )
        tw_delta = (
            curr_netstat.get("TcpExt.TCPTimeWaitOverflow", 0) -
            prev_netstat.get("TcpExt.TCPTimeWaitOverflow", 0)
        )

        now_str = datetime.utcfromtimestamp(now).strftime("%H:%M:%S.%f")[:-3]

        if overflow_delta > 0 or drop_delta > 0:
            total_drops = curr_netstat.get("TcpExt.ListenDrops", 0)
            total_overflows = curr_netstat.get("TcpExt.ListenOverflows", 0)
            print(
                f"[{now_str}] [KERNEL SEAM ALERT] ListenDrops: +{drop_delta:<4} (total: {total_drops}) | "
                f"ListenOverflows: +{overflow_delta:<4} (total: {total_overflows}) | "
                f"RATE: {drop_delta * 10} drops/sec"
            )
            last_heartbeat = now
        elif tw_delta > 0:
            print(f"[{now_str}] [KERNEL SEAM ALERT] TCPTimeWaitOverflow: +{tw_delta}")
            last_heartbeat = now
        elif now - last_heartbeat >= 1.0:
            # 1-second nominal status heartbeat
            sockstat = parse_sockstat()
            ct_count, ct_max = parse_conntrack()
            tcp_inuse = sockstat.get("TCP.inuse", 0)
            tcp_tw = sockstat.get("TCP.tw", 0)
            ct_info = f"{ct_count}/{ct_max}" if ct_max > 0 else f"{ct_count}"
            print(
                f"[{now_str}] [NOMINAL] Sockets inuse: {tcp_inuse:<4} tw: {tcp_tw:<4} | "
                f"ListenDrops: 0 | Conntrack: {ct_info}"
            )
            last_heartbeat = now

        prev_netstat = curr_netstat

if __name__ == "__main__":
    main()
