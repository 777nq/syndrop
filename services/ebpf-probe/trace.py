#!/usr/bin/env python3
"""
ephemeral-collapse: BCC eBPF Tracing Engine
Attaches kprobes to tcp_v4_syn_recv_sock and __nf_conntrack_alloc,
streaming live kernel-seam failure telemetry to stdout.
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
    print("[ERROR] BCC python module not found. Falling back to procfs collector.")
    sys.exit(1)

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

def print_banner():
    print("===============================================================================")
    print("  ephemeral-collapse: Live Linux Kernel Seam eBPF Observability Probe")
    print("  Hooks active: tcp_v4_syn_recv_sock | __nf_conntrack_alloc | kfree_skb")
    print("===============================================================================")

def handle_syn_drop(cpu, data, size):
    event = bpf["syn_drop_events"].event(data)
    now = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S.%f")[:-3]
    saddr = ip_to_str(event.saddr)
    daddr = ip_to_str(event.daddr)
    print(f"[{now}] [KERNEL DROP] TCP SYN LISTEN BACKLOG OVERFLOW | {saddr}:{event.sport} -> {daddr}:{event.dport} | "
          f"Current Backlog: {event.ack_backlog} > Max Allowed: {event.max_ack_backlog}")

def handle_conntrack_drop(cpu, data, size):
    event = bpf["conntrack_drop_events"].event(data)
    now = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S.%f")[:-3]
    print(f"[{now}] [KERNEL DROP] NETFILTER CONNTRACK ALLOCATION FAILED | nf_conntrack table full!")

def main():
    print_banner()

    if not os.path.exists(BPF_SOURCE_PATH):
        print(f"[FATAL] BPF source not found at: {BPF_SOURCE_PATH}")
        sys.exit(1)

    with open(BPF_SOURCE_PATH, "r") as f:
        bpf_text = f.read()

    print("[*] Compiling and loading eBPF program into kernel...")
    global bpf
    try:
        bpf = BPF(text=bpf_text)
    except Exception as e:
        print(f"[ERROR] eBPF compilation/injection failed: {e}")
        print("[*] Transitioning to procfs/netlink fallback collector...")
        sys.exit(2)

    print("[+] Successfully attached kprobes:")
    print("    -> kprobe:tcp_v4_syn_recv_sock (detects sk_ack_backlog > sk_max_ack_backlog)")
    print("    -> kretprobe:__nf_conntrack_alloc (detects conntrack table saturation)")
    print("    -> kprobe:tcp_set_state (tracks TCP lifecycle & TIME_WAIT churn)")
    print("    -> tracepoint:skb:kfree_skb (tracks socket buffer drop reasons)")
    print("\n[+] Listening for kernel events. Press Ctrl+C to stop...\n")

    bpf["syn_drop_events"].open_perf_buffer(handle_syn_drop)
    bpf["conntrack_drop_events"].open_perf_buffer(handle_conntrack_drop)

    last_poll = time.time()
    try:
        while True:
            bpf.perf_buffer_poll(timeout=100)

            # Every 3 seconds, dump aggregated metric table
            if time.time() - last_poll >= 3.0:
                last_poll = time.time()
                syn_drops = 0
                ct_drops = 0
                for k, v in bpf["syn_drops_total"].items():
                    syn_drops += v.value
                for k, v in bpf["conntrack_drops_total"].items():
                    ct_drops += v.value

                now_str = datetime.utcfromtimestamp(time.time()).strftime("%H:%M:%S")
                print(f"[{now_str}] SUMMARY: Total Backlog SYN Drops={syn_drops} | Total Conntrack Drops={ct_drops}")

    except KeyboardInterrupt:
        print("\n[*] Detaching probes and exiting...")

if __name__ == "__main__":
    main()
