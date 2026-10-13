#!/usr/bin/env python3
"""
ephemeral-collapse: Live Terminal Dashboard (TUI)
Renders high-frequency sub-runtime kernel metrics:
RPS, Accept Queue vs somaxconn, Dropped SYNs, Conntrack Saturation, and TIME_WAIT Depletion.
Zero-dependency ANSI rendering with auto-refresh.
"""

import os
import sys
import time
import urllib.request
from datetime import datetime

# ANSI Color Codes
RESET = "\033[0m"
BOLD = "\033[1m"
RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
BLUE = "\033[34m"
MAGENTA = "\033[35m"
CYAN = "\033[36m"
WHITE = "\033[37m"
BG_RED = "\033[41m"
BG_GREEN = "\033[42m"
CLEAR_SCREEN = "\033[2J\033[H"
HIDE_CURSOR = "\033[?25l"
SHOW_CURSOR = "\033[?25h"

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

def parse_backend_metrics(url):
    try:
        req = urllib.request.Request(f"{url}/metrics", headers={"User-Agent": "ephemeral-tui"})
        with urllib.request.urlopen(req, timeout=0.5) as resp:
            text = resp.read().decode("utf-8")
        metrics = {}
        for line in text.splitlines():
            if line.startswith("#") or not line.strip():
                continue
            parts = line.split()
            if len(parts) >= 2:
                try:
                    metrics[parts[0]] = float(parts[1])
                except ValueError:
                    pass
        return metrics
    except Exception:
        return {}

def render_bar(val, max_val, width=28, color=GREEN):
    if max_val <= 0:
        return f"[{'-' * width}]   0.0%"
    ratio = min(1.0, max(0.0, val / float(max_val)))
    filled = int(ratio * width)
    unfilled = width - filled

    bar_color = color
    if ratio > 0.85:
        bar_color = RED
    elif ratio > 0.60:
        bar_color = YELLOW

    bar = f"{bar_color}{'█' * filled}{RESET}{WHITE}{'░' * unfilled}{RESET}"
    return f"[{bar}] {ratio*100.0:5.1f}%"

def main():
    target_url = os.getenv("TARGET_URL", "http://127.0.0.1:8080")
    sys.stdout.write(HIDE_CURSOR)
    sys.stdout.flush()

    prev_time = time.time()
    prev_netstat = parse_netstat()
    prev_req_total = 0.0

    try:
        while True:
            time.sleep(0.5)
            now = time.time()
            dt = max(0.001, now - prev_time)

            curr_netstat = parse_netstat()
            sockstat = parse_sockstat()
            ct_count, ct_max = parse_conntrack()
            backend_metrics = parse_backend_metrics(target_url)

            # Deltas & Rates
            listen_overflows_delta = curr_netstat.get("TcpExt.ListenOverflows", 0) - prev_netstat.get("TcpExt.ListenOverflows", 0)
            listen_drops_delta = curr_netstat.get("TcpExt.ListenDrops", 0) - prev_netstat.get("TcpExt.ListenDrops", 0)
            tw_overflow_delta = curr_netstat.get("TcpExt.TCPTimeWaitOverflow", 0) - prev_netstat.get("TcpExt.TCPTimeWaitOverflow", 0)

            drops_per_sec = int(listen_drops_delta / dt)
            overflows_per_sec = int(listen_overflows_delta / dt)
            tw_overflow_per_sec = int(tw_overflow_delta / dt)

            curr_req_total = backend_metrics.get("ephemeral_requests_total", 0.0)
            rps = int(max(0.0, curr_req_total - prev_req_total) / dt) if prev_req_total > 0 else 0
            active_conns = int(backend_metrics.get("ephemeral_active_connections", sockstat.get("TCP.inuse", 0)))
            epoll_lag = int(backend_metrics.get("ephemeral_epoll_lag_microseconds", 0))

            tcp_inuse = sockstat.get("TCP.inuse", 0)
            tcp_tw = sockstat.get("TCP.tw", 0)

            # Determine Health Status
            status_text = f"{GREEN}NOMINAL / STABLE{RESET}"
            if drops_per_sec > 0 or overflows_per_sec > 0:
                status_text = f"{BG_RED}{WHITE}{BOLD} 🔴 LISTEN BACKLOG OVERFLOW (somaxconn full) {RESET}"
            elif tw_overflow_per_sec > 0 or tcp_tw > 500:
                status_text = f"{BG_RED}{WHITE}{BOLD} ⚠️  TIME_WAIT EXHAUSTION (port pool clamped) {RESET}"
            elif ct_max > 0 and (ct_count / ct_max) > 0.85:
                status_text = f"{BG_RED}{WHITE}{BOLD} 💥 NETFILTER CONNTRACK FULL {RESET}"
            elif epoll_lag > 5000:
                status_text = f"{YELLOW}{BOLD} ⚠️  EPOLL EVENT LOOP STALL (>5ms) {RESET}"

            # Frame buffer rendering
            out = []
            out.append(CLEAR_SCREEN)
            out.append(f"{CYAN}┌──────────────────────────────────────────────────────────────────────────────┐{RESET}")
            out.append(f"{CYAN}│{RESET}  {BOLD}ephemeral-collapse: Kernel-Seam Real-Time Failure Console{RESET}              {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}  Target: {target_url:<22} System State: {status_text:<25} {CYAN}│{RESET}")
            out.append(f"{CYAN}├──────────────────────────────────────────────────────────────────────────────┤{RESET}")
            out.append(f"{CYAN}│{RESET} {BOLD}📡 APPLICATION & RUNTIME LAYER{RESET}                                                {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   Throughput:   {BOLD}{rps:>7} req/s{RESET}  {render_bar(rps, 50000, 24, CYAN)}   {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   Active Sockets: {active_conns:>5} conns   Epoll Loop Lag: {epoll_lag:>6} µs                 {CYAN}│{RESET}")
            out.append(f"{CYAN}├──────────────────────────────────────────────────────────────────────────────┤{RESET}")
            out.append(f"{CYAN}│{RESET} {BOLD}🛑 SEAM 1: LISTEN QUEUE & SYN DROPS (Server somaxconn: 128){RESET}                  {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   Listen Drops Rate:    {RED}{BOLD}+{drops_per_sec:>6} drops/sec{RESET} (Cumulative: {curr_netstat.get('TcpExt.ListenDrops', 0):>6})         {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   Listen Overflows:     {YELLOW}+{overflows_per_sec:>6} /sec{RESET}     (Cumulative: {curr_netstat.get('TcpExt.ListenOverflows', 0):>6})         {CYAN}│{RESET}")
            retrans_msg = f"{RED}⚠️  ACTIVE (3,000ms Retransmit Penalty){RESET}" if drops_per_sec > 0 else f"{GREEN}None (Nominal RTT){RESET}"
            out.append(f"{CYAN}│{RESET}   TCP SYN Retransmit:   {retrans_msg:<40}      {CYAN}│{RESET}")
            out.append(f"{CYAN}├──────────────────────────────────────────────────────────────────────────────┤{RESET}")
            out.append(f"{CYAN}│{RESET} {BOLD}🌐 SEAM 2: NETFILTER CONNTRACK TABLE{RESET}                                           {CYAN}│{RESET}")
            ct_label = f"{ct_count}/{ct_max}" if ct_max > 0 else f"{ct_count} (unbounded)"
            out.append(f"{CYAN}│{RESET}   Table Entries:        {ct_label:<14} {render_bar(ct_count, max(1, ct_max), 24, MAGENTA)}   {CYAN}│{RESET}")
            out.append(f"{CYAN}├──────────────────────────────────────────────────────────────────────────────┤{RESET}")
            out.append(f"{CYAN}│{RESET} {BOLD}🔌 SEAM 3: TIME_WAIT & EPHEMERAL PORT POOL (Client Range: 512 ports){RESET}           {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   TIME_WAIT Sockets:    {tcp_tw:>5} sockets   {render_bar(tcp_tw, 512, 24, YELLOW)}   {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   TIME_WAIT Overflows:  {RED}+{tw_overflow_per_sec:>5} /sec{RESET}    (Cumulative: {curr_netstat.get('TcpExt.TCPTimeWaitOverflow', 0):>6})         {CYAN}│{RESET}")
            out.append(f"{CYAN}│{RESET}   Port Exhaustion:      {RED if tcp_tw > 450 else GREEN}{'EADDRNOTAVAIL RISK HIGH' if tcp_tw > 450 else 'Pool Nominal':<24}{RESET}                             {CYAN}│{RESET}")
            out.append(f"{CYAN}└──────────────────────────────────────────────────────────────────────────────┘{RESET}")
            out.append(f"  {WHITE}Press Ctrl+C to exit dashboard. Auto-refreshes every 500ms.{RESET}")

            sys.stdout.write("\n".join(out) + "\n")
            sys.stdout.flush()

            prev_time = now
            prev_netstat = curr_netstat
            prev_req_total = curr_req_total

    except KeyboardInterrupt:
        sys.stdout.write(SHOW_CURSOR + "\n")
        sys.stdout.flush()
        print("\nDashboard exited.")

if __name__ == "__main__":
    main()
