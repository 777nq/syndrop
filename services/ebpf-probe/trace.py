#!/usr/bin/env python3
"""
syndrop: BCC eBPF Tracing Engine
Attaches to tcp_v4_syn_recv_sock, inet_sock_set_state, and kfree_skb.
Streams real-time drops and prints aggregated kernel counters.
"""

import os
import sys
import time
import socket
import struct
from datetime import datetime

try:
    from bcc import BPF
except ImportError:
    print("[syndrop-tracer] BCC python module not found. Launching /proc fallback reader...")
    fallback_path = os.path.join(os.path.dirname(__file__), "fallback_collector.py")
    os.execv(sys.executable, [sys.executable, fallback_path])

BPF_SOURCE_PATH = os.path.join(os.path.dirname(__file__), "bpf", "tcp_seam_trace.bpf.c")

TCP_STATES = {
    1: "ESTABLISHED",
    2: "SYN_SENT",
    3: "SYN_RECV",
    4: "FIN_WAIT1",
    5: "FIN_WAIT2",
    6: "TIME_WAIT",
    7: "CLOSE",
    8: "CLOSE_WAIT",
    9: "LAST_ACK",
    10: "LISTEN",
    11: "CLOSING",
    12: "NEW_SYN_RECV",
}

def ip_to_str(ip_int):
    return socket.inet_ntoa(struct.pack("<I", ip_int))

def handle_syn_drop(cpu, data, size):
    event = bpf["syn_drop_events"].event(data)
    now = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S.%f")[:-3]
    saddr = ip_to_str(event.saddr)
    daddr = ip_to_str(event.daddr)
    print(
        f"[{now}] [kprobe:tcp_v4_syn_recv_sock] CPU {cpu:<2} | "
        f"saddr={saddr}:{event.sport:<5} -> daddr={daddr}:{event.dport:<5} | "
        f"backlog={event.ack_backlog} > max={event.max_ack_backlog} | "
        f"ACTION: SILENT_DROP_ACK (1s-3s RTO penalty)"
    )

def handle_conntrack_drop(cpu, data, size):
    event = bpf["conntrack_drop_events"].event(data)
    now = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S.%f")[:-3]
    print(f"[{now}] [kretprobe:__nf_conntrack_alloc] KERNEL DROP: Netfilter conntrack table full!")

def main():
    print("===============================================================================")
    print("  syndrop: Kernel Socket & Listen Queue eBPF Tracer")
    print("  Hooks: tcp_v4_syn_recv_sock | __nf_conntrack_alloc | inet_sock_set_state | kfree_skb")
    print("===============================================================================")

    if not os.path.exists(BPF_SOURCE_PATH):
        print(f"[FATAL] BPF source not found at: {BPF_SOURCE_PATH}")
        sys.exit(1)

    with open(BPF_SOURCE_PATH, "r") as f:
        bpf_text = f.read()

    global bpf
    try:
        bpf = BPF(text=bpf_text)
    except Exception as e:
        print(f"[ERROR] eBPF injection failed: {e}")
        print("[*] Transitioning to /proc fallback reader...")
        fallback_path = os.path.join(os.path.dirname(__file__), "fallback_collector.py")
        os.execv(sys.executable, [sys.executable, fallback_path])

    print("[+] Attached kprobe:tcp_v4_syn_recv_sock (detects sk_ack_backlog > sk_max_ack_backlog)")
    print("[+] Attached kretprobe:__nf_conntrack_alloc (detects nf_conntrack table full)")
    print("[+] Attached tracepoint:sock:inet_sock_set_state (tracks TCP transitions)")
    print("[+] Attached tracepoint:skb:kfree_skb (tracks sk_buff drop reasons)")
    print("\n[+] Tracing active. Press Ctrl+C to stop...\n")

    bpf["syn_drop_events"].open_perf_buffer(handle_syn_drop)
    bpf["conntrack_drop_events"].open_perf_buffer(handle_conntrack_drop)

    last_report = time.time()
    try:
        while True:
            bpf.perf_buffer_poll(timeout=100)

            # Every 2 seconds print aggregated summary
            now = time.time()
            if now - last_report >= 2.0:
                last_report = now
                syn_drops = 0
                for k, v in bpf["syn_drops_total"].items():
                    syn_drops += v.value
                ct_drops = 0
                for k, v in bpf["conntrack_drops_total"].items():
                    ct_drops += v.value

                time_str = datetime.utcfromtimestamp(now).strftime("%H:%M:%S")
                print(f"[{time_str}] COUNTERS: Cumulative Backlog Drops={syn_drops} | Conntrack Drops={ct_drops}")

    except KeyboardInterrupt:
        print("\nDetaching eBPF probes and exiting.")

if __name__ == "__main__":
    main()
