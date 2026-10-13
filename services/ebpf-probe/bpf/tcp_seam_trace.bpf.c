// SPDX-License-Identifier: GPL-2.0
// ephemeral-collapse: eBPF Kernel-Seam Diagnostic Probe
// Traces listen backlog drops, conntrack table exhaustion, and socket buffer allocations.
// Compatible across Linux 5.x and 6.x+ kernel ABI layouts.

#include <uapi/linux/ptrace.h>
#include <linux/version.h>
#include <net/sock.h>
#include <net/tcp.h>
#include <linux/skbuff.h>
#include <linux/netfilter.h>

// Event emitted when a SYN packet is dropped due to listen backlog saturation
struct syn_drop_event_t {
    u64 timestamp_ns;
    u32 saddr;
    u32 daddr;
    u16 sport;
    u16 dport;
    u32 ack_backlog;
    u32 max_ack_backlog;
    u32 reason;
};

// Event emitted on netfilter conntrack allocation failure
struct conntrack_drop_event_t {
    u64 timestamp_ns;
    u32 current_count;
    u32 max_limit;
};

// BPF Perf Output streams for real-time userspace telemetry
BPF_PERF_OUTPUT(syn_drop_events);
BPF_PERF_OUTPUT(conntrack_drop_events);

// BPF Hash Maps for high-frequency metric aggregation without userspace lock overhead
BPF_HASH(syn_drops_total, u32, u64);
BPF_HASH(conntrack_drops_total, u32, u64);
BPF_HASH(tcp_state_histogram, u32, u64);
BPF_HASH(skb_drops_by_reason, u32, u64);

/**
 * Hook: tcp_v4_syn_recv_sock
 * Fired during 3-way handshake completion when server evaluates the socket.
 * If sk_ack_backlog > sk_max_ack_backlog, Linux drops the packet or responds with RST.
 */
int kprobe__tcp_v4_syn_recv_sock(struct pt_regs *ctx,
                                 struct sock *sk,
                                 struct sk_buff *skb,
                                 struct request_sock *req,
                                 struct dst_entry *dst,
                                 struct request_sock *req_unhash,
                                 bool *own_req)
{
    if (!sk)
        return 0;

    u32 ack_backlog = 0;
    u32 max_ack_backlog = 0;

    // Read socket backlog limits directly from kernel struct sock using safe probe reads
    bpf_probe_read_kernel(&ack_backlog, sizeof(ack_backlog), &sk->sk_ack_backlog);
    bpf_probe_read_kernel(&max_ack_backlog, sizeof(max_ack_backlog), &sk->sk_max_ack_backlog);

    // Detect queue saturation condition
    if (ack_backlog > max_ack_backlog) {
        u32 key = 0;
        u64 *val = syn_drops_total.lookup(&key);
        u64 zero = 0;
        if (!val) {
            syn_drops_total.update(&key, &zero);
            val = syn_drops_total.lookup(&key);
        }
        if (val) {
            __sync_fetch_and_add(val, 1);
        }

        struct syn_drop_event_t evt = {};
        evt.timestamp_ns = bpf_ktime_get_ns();
        evt.ack_backlog = ack_backlog;
        evt.max_ack_backlog = max_ack_backlog;
        evt.reason = 1; // LISTEN_BACKLOG_OVERFLOW

        // Extract IP 5-tuple from sk_buff network header
        if (skb) {
            struct iphdr *iph = (struct iphdr *)(skb->head + skb->network_header);
            bpf_probe_read_kernel(&evt.saddr, sizeof(evt.saddr), &iph->saddr);
            bpf_probe_read_kernel(&evt.daddr, sizeof(evt.daddr), &iph->daddr);

            struct tcphdr *th = (struct tcphdr *)(skb->head + skb->transport_header);
            bpf_probe_read_kernel(&evt.sport, sizeof(evt.sport), &th->source);
            bpf_probe_read_kernel(&evt.dport, sizeof(evt.dport), &th->dest);
            evt.sport = ntohs(evt.sport);
            evt.dport = ntohs(evt.dport);
        }

        syn_drop_events.perf_submit(ctx, &evt, sizeof(evt));
    }

    return 0;
}

/**
 * Hook: __nf_conntrack_alloc
 * Tracing Netfilter conntrack allocation failure (conntrack table full).
 */
int kretprobe____nf_conntrack_alloc(struct pt_regs *ctx)
{
    void *ret = (void *)PT_REGS_RC(ctx);
    if (ret == NULL || IS_ERR(ret)) {
        u32 key = 0;
        u64 *val = conntrack_drops_total.lookup(&key);
        u64 zero = 0;
        if (!val) {
            conntrack_drops_total.update(&key, &zero);
            val = conntrack_drops_total.lookup(&key);
        }
        if (val) {
            __sync_fetch_and_add(val, 1);
        }

        struct conntrack_drop_event_t evt = {};
        evt.timestamp_ns = bpf_ktime_get_ns();
        evt.current_count = 0;
        evt.max_limit = 0;

        conntrack_drop_events.perf_submit(ctx, &evt, sizeof(evt));
    }
    return 0;
}

/**
 * Stable Kernel Tracepoint: sock:inet_sock_set_state
 * Available across all Linux 5.x and 6.x kernels without struct layout dependency.
 * Tracks TCP transitions: SYN_RECV -> ESTABLISHED -> TIME_WAIT -> CLOSE.
 */
TRACEPOINT_PROBE(sock, inet_sock_set_state) {
    u32 newstate = args->newstate;
    u64 *val = tcp_state_histogram.lookup(&newstate);
    u64 zero = 0;
    if (!val) {
        tcp_state_histogram.update(&newstate, &zero);
        val = tcp_state_histogram.lookup(&newstate);
    }
    if (val) {
        __sync_fetch_and_add(val, 1);
    }
    return 0;
}

/**
 * Tracepoint: skb:kfree_skb
 * Uses version check for args->reason (introduced in Linux 5.17+).
 */
TRACEPOINT_PROBE(skb, kfree_skb) {
#if LINUX_VERSION_CODE >= KERNEL_VERSION(5, 17, 0)
    u32 reason = args->reason;
#else
    u32 reason = 0; // Legacy fallback for Linux < 5.17
#endif
    u64 *val = skb_drops_by_reason.lookup(&reason);
    u64 zero = 0;
    if (!val) {
        skb_drops_by_reason.update(&reason, &zero);
        val = skb_drops_by_reason.lookup(&reason);
    }
    if (val) {
        __sync_fetch_and_add(val, 1);
    }
    return 0;
}
