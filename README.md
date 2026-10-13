# ephemeral-collapse

<p align="center">
  <strong>Diagnostic Harness & Kernel-Seam Failure Analysis for 100k+ RPS Microservice Meshes</strong><br>
  <em>A high-performance systems engineering testbed exposing and resolving sub-runtime Linux kernel failures beneath Kubernetes, Service Meshes, and Async Runtimes.</em>
</p>

<p align="center">
  <a href="#quick-start-1-command-runnable"><img src="https://img.shields.io/badge/run-docker%20compose%20up-blue?style=flat-square" alt="Docker Compose" /></a>
  <a href="#ebpf--bcc-diagnostic-probes"><img src="https://img.shields.io/badge/probes-eBPF%20%2F%20BCC-orange?style=flat-square" alt="eBPF BCC" /></a>
  <a href="#mathematical-tuning-formulas"><img src="https://img.shields.io/badge/tuning-sysctl%20formulas-green?style=flat-square" alt="Sysctl Formulas" /></a>
  <a href="docs/whitepaper.md"><img src="https://img.shields.io/badge/whitepaper-systems%20post--mortem-purple?style=flat-square" alt="Whitepaper" /></a>
</p>

---

## 📑 Post-Mortem Executive Summary

In high-scale Kubernetes and microservice architectures operating at or above **100,000 requests per second (100k+ RPS)**, silent cascading latency spikes often originate **beneath the runtime**:
- **Application runtimes** (Rust Tokio, Go Netpoll, C++ Envoy, Node.js) appear idle or lightly loaded (< 30% CPU).
- **APM traces** register sporadic 3-second connection timeouts (`ETIMEDOUT`) and dropped sessions (`ECONNRESET`).
- **Standard cloud metrics** report healthy node memory and CPU utilization.

**The Root Cause**: High-density microservice traffic creates impedance mismatches across Linux kernel subsystem boundaries—specifically **epoll starvation**, **listen backlog overflow (`tcp_v4_syn_recv_sock`)**, **Netfilter `nf_conntrack` hash table saturation**, and **TCP `TIME_WAIT` socket bucket overflow**.

`ephemeral-collapse` is a complete, containerized diagnostic harness and eBPF observability suite that reproduces, measures, and eliminates these kernel-seam failures in an isolated environment.

---

## 📐 Kernel Architecture & Visual Failure Seams

### 1. The TCP Handshake Split-Queue Seam: Silent SYN Drop Cascade

The Linux kernel does not have a single connection queue. It maintains two distinct queues: the **SYN Queue** (`net.ipv4.tcp_max_syn_backlog`) and the **Accept Queue** (`net.core.somaxconn`). When the Accept Queue fills up, Linux silently drops incoming ACKs by default, triggering an exponential backoff retransmission stall (1s, 3s, 6s).

```
CLIENT (Traffic Gen)                     LINUX KERNEL (TCP/IP Subsystem)                   SERVER (Tokio / Go)
        |                                                |                                          |
        |--- 1. TCP SYN [seq=100] ---------------------->|                                          |
        |                                                | [Check: SYN Queue < tcp_max_syn_backlog] |
        |<-- 2. TCP SYN-ACK [seq=200, ack=101] ----------| [Alloc: struct request_sock]             |
        |                                                |                                          |
        |--- 3. TCP ACK [seq=101, ack=201] ------------->|                                          |
        |    (Handshake completed from Client POV)       |                                          |
        |                                                | Hook: tcp_v4_syn_recv_sock()             |
        |                                                |                                          |
        |                                                | [CHECK ACCEPT QUEUE]                     |
        |                                                | Is sk_ack_backlog > sk_max_ack_backlog?  |
        |                                                |                                          |
        |                                                | +======================================+ |
        |                                                | | FAIL: ACCEPT QUEUE FULL (somaxconn)  | |
        |                                                | | ACTION: Kernel SILENTLY DROPS ACK!   | |
        |                                                | +======================================+ |
        |                                                |                                          |
        |--- 4. HTTP GET /bench (Data sent by Client) -->| [DROPPED: Socket still in SYN_RECV state]|
        |                                                |                                          |
        |    ... 1000ms Silence (Timer Backoff) ...      |                                          |
        |                                                |                                          |
        |<-- 5. TCP Retransmitted SYN-ACK ---------------| (Kernel retransmits; thinks ACK was lost)|
        |                                                |                                          |
        |--- 6. TCP ACK (Retransmit) ------------------->| [If Queue drained, socket moves to ACCEPT]
        |                                                |----------------------------------------->| 7. accept4()
        |                                                |                                          |
        +------------------------------------------------+------------------------------------------+
                    RESULT: Client experiences 1,000ms - 3,000ms latency spike on p99.9!
```

---

### 2. Netfilter Conntrack Table Full & Silent Drop

Under high connection churn or sidecar proxies (Istio/Envoy), every new 5-tuple allocates an entry in the Netfilter connection tracking table. When `nf_conntrack_count` reaches `nf_conntrack_max`, new packets are discarded with zero TCP notifications.

```
Incoming IP Packet (SYN)
         |
         v
+-------------------------------+
|  PREROUTING (nf_conntrack_in) |
+-------------------------------+
         |
         v
+-------------------------------+      YES     +-----------------------------------------------+
| Hash 5-Tuple to Bucket Chain  |------------->| Linked List > Max Depth? -> CPU Softirq Spike |
+-------------------------------+              +-----------------------------------------------+
         |
         v
+-------------------------------+
| Call __nf_conntrack_alloc()   |
+-------------------------------+
         |
         +-------------------------------------------------+
         |                                                 |
[Table Has Space]                                 [Table Exceeded: nf_conntrack_max]
         |                                                 |
         v                                                 v
Allocate struct nf_conn                   +================================================+
Proceed to ip_local_deliver()             | KERNEL DROP: "nf_conntrack: table full,        |
                                          | dropping packet"                               |
                                          | eBPF Hook: kretprobe:__nf_conntrack_alloc      |
                                          +================================================+
                                                           |
                                                           v
                                          Silent Packet Blackhole (Client ETIMEDOUT)
```

---

### 3. Epoll Worker Wakeup Contention vs `SO_REUSEPORT`

```
  TRADITIONAL SINGLE-QUEUE (EPOLLET Contention)              MODERN SO_REUSEPORT (Kernel Multi-Queue)
  ===========================================              =======================================

           Incoming TCP Connections                                 Incoming TCP Connections
                      |                                                        |
                      v                                                        v
             [Single Listen Socket]                                [Kernel 4-Tuple BPF Hash]
                      |                                            /           |           \
             [Shared Epoll Set]                                   v            v            v
          /           |           \                         [Queue 0]      [Queue 1]    [Queue 2]
         v            v            v                            |              |            |
     Worker 0      Worker 1     Worker 2                    Worker 0       Worker 1     Worker 2
         \            |            /                        (Direct)       (Direct)     (Direct)
          +-----------+-----------+                             |              |            |
                      |                                         v              v            v
           [epmutex Lock Contention]                        No Lock       No Lock      No Lock
           Thundering Herd / Latency Spikes                 Contention    Contention   Contention
```

---

### 4. TIME_WAIT State Retention & Ephemeral Port Exhaustion

```
Active Close (FIN) -> FIN-ACK Handshake -> TIME_WAIT (2 * MSL = 60s)
                                                 |
                   +-----------------------------+-----------------------------+
                   |                                                           |
       [tcp_tw_reuse = 0 (Default)]                                 [tcp_tw_reuse = 1]
                   |                                                           |
  Socket 5-Tuple pinned for 60 seconds.                        Kernel checks TCP timestamp (PAWS).
  Port unavailable in ip_local_port_range.                     If timestamp increases, socket reused safely!
                   |                                                           |
  >500 RPS Churn = 28,000 ports exhausted                                      v
                   |                                               Zero Ephemeral Exhaustion
                   v
     DIAL ERROR: EADDRNOTAVAIL
     "Cannot assign requested address"
```

---

## ⚡ Quick Start (1-Command Runnable)

No paid tools, cloud keys, or external prerequisites. Everything runs out of the box with Docker Compose or the included Makefile.

### Prerequisites
- Docker & Docker Compose (v2.0+) OR standard Linux host (kernel 5.4+)
- `make` (optional)

### 1. Launch the Testbed

```bash
docker compose up --build
```
*Or via Makefile:*
```bash
make run
```

This starts:
1. **`ephemeral-backend`**: High-performance Rust Tokio microservice bound to port `8080`, with explicitly tuned kernel socket options (`SO_REUSEPORT`, `TCP_NODELAY`, custom listen backlog).
2. **`ephemeral-probe`**: Real-time eBPF & procfs kernel-seam diagnostic observer attached directly to the network namespace.
3. **`ephemeral-generator`**: Synthetic load and failure orchestrator.

### 2. Verify Harness Health

In another terminal:
```bash
make verify
```
*Expected Output:*
```
[1/4] Probing /healthz... OK (HTTP 200)
[2/4] Testing high-speed /bench endpoint... OK -> {"status":"healthy","kernel_seam":"nominal"}
[3/4] Testing connection churn endpoint (/churn)... OK -> {"status":"churn_closed"}
[4/4] Querying /metrics... OK (Prometheus metrics active)
-------------------------------------------------------------------------------
ephemeral_active_connections 0
ephemeral_requests_total 3
ephemeral_connection_churn_total 1
ephemeral_epoll_lag_microseconds 0
-------------------------------------------------------------------------------
[SUCCESS] Harness is operational and ready for load & chaos injection!
```

---

## 💥 Reproducible Failure Scenarios

`ephemeral-collapse` includes dedicated scenarios engineered to induce sub-runtime kernel failures without saturating application CPU:

### 1. SYN Listen Backlog Queue Saturation (`make chaos-syn`)
Fires concurrent bursts of thousands of TCP handshakes in 50ms windows, exceeding `somaxconn` and `tcp_max_syn_backlog`.
```bash
make chaos-syn
```
*Observed Outcome*:
- In eBPF probe: `[KERNEL DROP] TCP SYN LISTEN BACKLOG OVERFLOW | Current Backlog: 129 > Max Allowed: 128`.
- In client: Sudden connection latency jump to `1.5s` and `ETIMEDOUT` / `ECONNREFUSED`.

### 2. Netfilter Conntrack Table Saturation (`make chaos-conntrack`)
Generates high-rate ephemeral 5-tuples without connection reuse to fill `nf_conntrack_max`.
```bash
make chaos-conntrack
```
*Observed Outcome*:
- In eBPF probe: `[KERNEL DROP] NETFILTER CONNTRACK ALLOCATION FAILED | nf_conntrack table full!`.
- Conntrack gauge reaches `100.0%`. Packets are dropped silently at the PREROUTING hook.

### 3. TCP TIME_WAIT Bucket Overflow & Port Exhaustion (`make chaos-timewait`)
Generates rapid client-side active closes against a bounded ephemeral port pool (`ip_local_port_range`).
```bash
make chaos-timewait
```
*Observed Outcome*:
- Kernel increments `TCPTimeWaitOverflow`.
- Client fails with `EADDRNOTAVAIL` (*"Cannot assign requested address"*).

### 4. Slowloris Socket Buffer Pinning & Epoll Starvation (`make chaos-slowloris`)
Dribbles single bytes across 500+ active connections, pinning kernel `sk_buff` receive memory and generating epoll event churn.
```bash
make chaos-slowloris
```
*Observed Outcome*:
- `ephemeral_epoll_lag_microseconds` spikes in Prometheus telemetry.
- Kernel `sockstat` shows elevated `TCP mem` allocation.

### 5. Baseline High-Throughput Keep-Alive Benchmark (`make bench`)
Runs clean, pipelined HTTP keep-alive traffic to establish baseline capability (>100k RPS capable).
```bash
make bench
```

### 6. Run All Scenarios Sequentially
```bash
make chaos-all
```

---

## 🔍 eBPF / BCC Diagnostic Probes

The diagnostic engine is located in [`services/ebpf-probe`](services/ebpf-probe/). It compiles a real C eBPF tracing program ([`bpf/tcp_seam_trace.bpf.c`](services/ebpf-probe/bpf/tcp_seam_trace.bpf.c)) hooking critical kernel symbols:

```c
// Hook into TCP SYN processing to catch listen queue drops:
int kprobe__tcp_v4_syn_recv_sock(struct pt_regs *ctx, struct sock *sk, struct sk_buff *skb) {
    u32 ack_backlog = sk->sk_ack_backlog;
    u32 max_ack_backlog = sk->sk_max_ack_backlog;

    if (ack_backlog > max_ack_backlog) {
        // Submit drop event to userspace ring buffer with 5-tuple
        syn_drop_events.perf_submit(ctx, &evt, sizeof(evt));
    }
}
```

### Live Probe Output

```
===============================================================================
  ephemeral-collapse: Live Linux Kernel Seam eBPF Observability Probe
  Hooks active: tcp_v4_syn_recv_sock | __nf_conntrack_alloc | kfree_skb
===============================================================================
[+] Attached kprobe:tcp_v4_syn_recv_sock (sk_ack_backlog > sk_max_ack_backlog)
[+] Attached kretprobe:__nf_conntrack_alloc (conntrack table saturation)
[+] Attached kprobe:tcp_set_state (TIME_WAIT state transitions)

[20:34:12.104] [KERNEL DROP] TCP SYN LISTEN BACKLOG OVERFLOW | 172.28.0.3:54210 -> 172.28.0.2:8080 | Current Backlog: 132 > Max Allowed: 128
[20:34:12.106] [KERNEL DROP] TCP SYN LISTEN BACKLOG OVERFLOW | 172.28.0.3:54212 -> 172.28.0.2:8080 | Current Backlog: 135 > Max Allowed: 128
[20:34:14.001] SUMMARY: Total Backlog SYN Drops=1482 | Total Conntrack Drops=0
```

> **Universal Compatibility Note**: When executed in non-privileged container engines or virtual machines (such as Docker Desktop on macOS/Windows) where host kernel headers or kprobes are inaccessible, the probe automatically falls back to the high-frequency Netlink/Procfs collector ([`fallback_collector.py`](services/ebpf-probe/fallback_collector.py)), monitoring `/proc/net/netstat` (`ListenOverflows`, `ListenDrops`, `TCPTimeWaitOverflow`) with sub-second precision.

---

## 📐 Mathematical Sizing & Tuning Formulas

### 1. Netfilter Conntrack Memory Consumption

Kernel slab memory allocated for connection tracking:

$$\text{Memory}_{\text{conntrack}} = (\text{nf\_conntrack\_max} \times 320\text{ bytes}) + (\text{nf\_conntrack\_buckets} \times 8\text{ bytes})$$

*Formula Rule*: Maintain a **1:4** ratio between buckets and maximum connections:
$$\text{nf\_conntrack\_buckets} = \frac{\text{nf\_conntrack\_max}}{4}$$

| `nf_conntrack_max` | `nf_conntrack_buckets` | Kernel Slab RAM Allocated |
| :--- | :--- | :--- |
| `262,144` | `65,536` | ~80 MB |
| `1,048,576` | `262,144` | ~322 MB |
| `2,097,152` | `524,288` | ~644 MB |

---

### 2. Listen Queue Sizing for Microservice Bursts

To prevent silent SYN drops during instantaneous traffic spikes, the Listen Queue capacity $Q_{\text{accept}}$ must absorb the burst rate $\lambda_{\text{burst}}$ across the maximum thread scheduling delay $\Delta t_{\text{accept}}$:

$$Q_{\text{accept}} \ge \lambda_{\text{burst}} \times \Delta t_{\text{accept}}$$

For a service handling $50,000\text{ conn/s}$ burst with a $100\text{ms}$ ($0.1\text{s}$) event loop processing lag:
$$Q_{\text{accept}} \ge 50000 \times 0.1 = 5,000 \implies \mathbf{\texttt{net.core.somaxconn} \ge 8192}$$

---

### 3. Maximum Connection Churn Rate (TIME_WAIT Limit)

Without socket reuse (`tcp_tw_reuse = 0`), the maximum sustainable connection establishment rate is bounded by the ephemeral port pool divided by the 2MSL duration ($60\text{s}$):

$$\text{RPS}_{\text{max}} = \frac{|P_{\text{ephemeral}}|}{2 \times \text{MSL}} = \frac{\text{port\_max} - \text{port\_min} + 1}{60}$$

For default Linux ports ($32768\text{--}60999 = 28,232\text{ ports}$):
$$\text{RPS}_{\text{max}} = \frac{28232}{60} \approx \mathbf{470\text{ RPS}}$$
*Tuning*: Expanding port range to `1024 65535` and enabling `tcp_tw_reuse = 1` removes this ceiling.

---

## 🛡️ Production Hardened sysctl Profile

Deploy to `/etc/sysctl.d/99-kubernetes-mesh-seam.conf` on Kubernetes worker nodes:

```ini
# ==============================================================================
# EPHEMERAL-COLLAPSE: Production sysctl Profile for 100k+ RPS Kubernetes Nodes
# ==============================================================================

# Listen Queue & SYN Backlog
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.ipv4.tcp_abort_on_overflow = 0
net.ipv4.tcp_syncookies = 1

# Ephemeral Port Pool & Socket Recycling
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_max_tw_buckets = 2000000
net.ipv4.tcp_fin_timeout = 15

# Netfilter Connection Tracking Sizing
net.netfilter.nf_conntrack_max = 2097152
net.netfilter.nf_conntrack_tcp_timeout_established = 600
net.netfilter.nf_conntrack_tcp_timeout_time_wait = 30
net.netfilter.nf_conntrack_tcp_timeout_close_wait = 15

# Socket Buffer Memory Sizing (16MB max window for high BDP)
net.core.rmem_default = 262144
net.core.rmem_max = 16777216
net.core.wmem_default = 262144
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
net.core.netdev_max_backlog = 100000

# File Descriptors
fs.file-max = 2097152
fs.nr_open = 2097152
```

---

## 🚫 Systems Anti-Patterns to Avoid

| # | Anti-Pattern | Fatal Mechanism | Correct Architectural Pattern |
| :-: | :--- | :--- | :--- |
| **1** | **Default Kubelet `somaxconn`** | Pods inherit `somaxconn=128`. Burst traffic drops SYNs even on a 96-core node. | Configure `securityContext.sysctls` in Pod specs or daemonset init containers. |
| **2** | **Disabling HTTP Keep-Alive** | Each RPC initiates a new 3-way handshake, exhausting conntrack & TIME_WAIT tables. | Enforce HTTP connection pooling with persistent keep-alive and idle connection reaping. |
| **3** | **Unbounded HTTP/2 Streams** | Multiplexing >1,000 streams over 1 TCP socket causes single packet loss to stall all streams. | Cap max concurrent streams at 100 and maintain a pool of 5–10 TCP connections. |
| **4** | **Unmonitored Sidecar Redirection** | Istio/Envoy iptables `PREROUTING` doubles conntrack entry allocation per RPC. | Monitor `nf_conntrack_count` via Prometheus and size `nf_conntrack_max` with $4\times$ headroom. |
| **5** | **Single-Thread Accept Loops** | One thread calls `accept()`, causing epoll lock contention and worker queue imbalances. | Bind listener with `SO_REUSEPORT` across all worker threads. |

---

## 📚 Deep Dive Architecture Whitepaper

For the comprehensive 20-page architectural analysis, including kernel source code walkthroughs of `tcp_v4_syn_recv_sock`, `nf_conntrack_in`, and NAPI softirq polling, read the [Architectural Whitepaper](docs/whitepaper.md).

---

## 📜 License

Licensed under the [Apache 2.0 License](LICENSE).
