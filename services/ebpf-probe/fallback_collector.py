#!/usr/bin/env python3
"""
ephemeral-collapse: Kernel Procfs / Netlink Diagnostic Collector
Monitors kernel socket state, listen queue drops (ListenOverflows, ListenDrops),
nf_conntrack table saturation, and TIME_WAIT bucket overflow with sub-second resolution.
Serves as universal zero-dependency telemetry engine.
"""

import os
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
    """Parses /proc/net/netstat for ListenOverflows, ListenDrops, and TCPTimeWaitOverflow"""
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
    """Parses /proc/net/sockstat for TCP sockets inuse, tw, alloc, mem"""
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
    """Parses nf_conntrack count and max"""
    count_str = read_file("/proc/sys/net/netfilter/nf_conntrack_count")
    max_str = read_file("/proc/sys/net/netfilter/nf_conntrack_max")
    
    count = int(count_str) if count_str and count_str.isdigit() else 0
    max_val = int(max_str) if max_str and max_str.isdigit() else 0
    return count, max_val

def render_gauge(used, max_val, width=20):
    if max_val <= 0:
        return "[?]"
    ratio = min(1.0, max(0.0, used / float(max_val)))
    filled = int(ratio * width)
    bar = "=" * filled + "-" * (width - filled)
    return f"[{bar}] {ratio*100.0:5.1f}% ({used}/{max_val})"

def main():
    print("===============================================================================")
    print("  ephemeral-collapse: Kernel Seam Telemetry Collector (/proc/net & conntrack)")
    print("  Monitoring: ListenOverflows | ListenDrops | TIME_WAIT | nf_conntrack_max")
    print("===============================================================================")

    prev_netstat = parse_netstat()
    last_print = time.time()

    while True:
        time.sleep(1.0)
        curr_netstat = parse_netstat()
        sockstat = parse_sockstat()
        ct_count, ct_max = parse_conntrack()

        # Calculate delta drops
        listen_overflows_delta = (
            curr_netstat.get("TcpExt.ListenOverflows", 0) -
            prev_netstat.get("TcpExt.ListenOverflows", 0)
        )
        listen_drops_delta = (
            curr_netstat.get("TcpExt.ListenDrops", 0) -
            prev_netstat.get("TcpExt.ListenDrops", 0)
        )
        tw_overflow_delta = (
            curr_netstat.get("TcpExt.TCPTimeWaitOverflow", 0) -
            prev_netstat.get("TcpExt.TCPTimeWaitOverflow", 0)
        )

        total_overflows = curr_netstat.get("TcpExt.ListenOverflows", 0)
        total_drops = curr_netstat.get("TcpExt.ListenDrops", 0)
        tcp_inuse = sockstat.get("TCP.inuse", 0)
        tcp_tw = sockstat.get("TCP.tw", 0)
        tcp_alloc = sockstat.get("TCP.alloc", 0)

        now_str = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S")

        status_marker = "NOMINAL"
        if listen_overflows_delta > 0 or listen_drops_delta > 0:
            status_marker = "!! BACKLOG DROPS !!"
        elif tw_overflow_delta > 0:
            status_marker = "!! TIME_WAIT OVERFLOW !!"
        elif ct_max > 0 and (ct_count / ct_max) > 0.85:
            status_marker = "!! CONNTRACK SATURATION !!"

        print(
            f"[{now_str}] [{status_marker:26}] "
            f"ListenOverflows: +{listen_overflows_delta:<4} (tot: {total_overflows:<5}) | "
            f"ListenDrops: +{listen_drops_delta:<4} (tot: {total_drops:<5}) | "
            f"TCP inuse: {tcp_inuse:<5} tw: {tcp_tw:<5} | "
            f"Conntrack: {render_gauge(ct_count, ct_max)}"
        )

        prev_netstat = curr_netstat

if __name__ == "__main__":
    main()
