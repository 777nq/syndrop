# ephemeral-collapse

<p align="center">
  <strong>Diagnostic Harness & Kernel-Seam Failure Analysis for 100k+ RPS Microservice Meshes</strong><br>
  <em>A high-performance systems engineering testbed exposing, visualizing, and resolving sub-runtime Linux kernel failures beneath Kubernetes, Service Meshes, and Async Runtimes.</em>
</p>

<p align="center">
  <a href="#quick-start-1-command-runnable"><img src="https://img.shields.io/badge/run-docker%20compose%20up-blue?style=flat-square" alt="Docker Compose" /></a>
  <a href="#live-terminal-dashboard-tui"><img src="https://img.shields.io/badge/tui-live%20terminal%20console-brightgreen?style=flat-square" alt="Live TUI" /></a>
  <a href="#break-vs-fix-remediation-playbook"><img src="https://img.shields.io/badge/playbook-break%20vs%20fix-red?style=flat-square" alt="Break vs Fix" /></a>
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

## 🖥️ Live Terminal Dashboard (TUI)

Monitor the failure seams live with the zero-dependency terminal console (`make tui`):

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  ephemeral-collapse: Kernel-Seam Real-Time Failure Console                  │
│  Target: http://backend:8080    System State: 🔴 LISTEN BACKLOG OVERFLOW     │
├──────────────────────────────────────────────────────────────────────────────┤
│ 📡 APPLICATION & RUNTIME LAYER                                               │
│   Throughput:     18,450 req/s  [█████████░░░░░░░░░░░░░░░]  36.9%            │
│   Active Sockets:    128 conns   Epoll Loop Lag:   1,840 µs                  │
├──────────────────────────────────────────────────────────────────────────────┤
│ 🛑 SEAM 1: LISTEN QUEUE & SYN DROPS (Server somaxconn: 128)                  │
│   Listen Drops Rate:    +3,412 drops/sec (Cumulative:  14,290)               │
│   Listen Overflows:     +3,412 /sec      (Cumulative:  14,290)               │
│   TCP SYN Retransmit:   ⚠️  ACTIVE (3,000ms Retransmit Penalty)              │
├──────────────────────────────────────────────────────────────────────────────┤
│ 🌐 SEAM 2: NETFILTER CONNTRACK TABLE                                         │
│   Table Entries:        1,840/2,048    [████████████████████░░]  89.8%       │
├──────────────────────────────────────────────────────────────────────────────┤
│ 🔌 SEAM 3: TIME_WAIT & EPHEMERAL PORT POOL (Client Range: 512 ports)         │
│   TIME_WAIT Sockets:       508 sockets [██████████████████████]  99.2%       │
│   TIME_WAIT Overflows:    +410 /sec    (Cumulative:   2,180)                 │
│   Port Exhaustion:      EADDRNOTAVAIL RISK HIGH                              │
└──────────────────────────────────────────────────────────────────────────────┘
  Press Ctrl+C to exit dashboard. Auto-refreshes every 500ms.
```

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
       [tcp_tw_reuse = 0 (Client Default)]                          [tcp_tw_reuse = 1]
                   |                                                           |
  Client 5-Tuple pinned for 60 seconds.                        Kernel checks TCP timestamp (PAWS).
  Port unavailable in ip_local_port_range.                     If timestamp increases, socket reused safely!
                   |                                                           |
  >500 RPS Churn = 28,000 ports exhausted                                      v
                   |                                               Zero Ephemeral Exhaustion
                   v
     DIAL ERROR: EADDRNOTAVAIL
     "Cannot assign requested address"
```

---

## 🔒 Container Sysctl Isolation vs Host Protection

A common pitfall in containerized kernel benchmarking is **host pollution**: modifying global sysctls inside Docker can accidentally throttle the developer's workstation (closing Slack, Spotify, or browser connections).

`ephemeral-collapse` implements strict separation between **namespaced** and **global** settings:

| Layer | Kernel Parameter | Scope | Container Target | Purpose in Harness |
| :--- | :--- | :--- | :--- | :--- |
| **Server** | `net.core.somaxconn` | **Network Namespaced** (Safe) | `backend` | Clamped to `128` to demonstrate Accept Queue overflow |
| **Server** | `net.ipv4.tcp_max_syn_backlog` | **Network Namespaced** (Safe) | `backend` | Clamped to `128` to demonstrate SYN Queue saturation |
| **Client** | `net.ipv4.ip_local_port_range` | **Network Namespaced** (Safe) | `traffic-generator` | Clamped to `32768 33279` (512 ports) to force client exhaustion |
| **Client** | `net.ipv4.tcp_tw_reuse` | **Network Namespaced** (Safe) | `traffic-generator` | Set to `0` to prohibit client socket recycling |
| **Global** | `net.netfilter.nf_conntrack_max`| **Global / Host Kernel** | *Guarded* | **Protected from accidental modification.** Isolated in netns scripts with `--clamp-conntrack` safety flags. |

---

## ⚡ Quick Start (1-Command Runnable)

No external API keys, cloud credits, or paid tools required.

### 1. Launch the Testbed

```bash
docker compose up --build
# or
make run
```

This boots:
1. **`ephemeral-backend`**: High-performance Rust Tokio microservice bound to port `8080`, with explicitly tuned kernel socket options (`SO_REUSEPORT`, `TCP_NODELAY`, custom listen backlog).
2. **`ephemeral-probe`**: Real-time eBPF & procfs kernel-seam diagnostic observer attached directly to the backend's network namespace.
3. **`ephemeral-generator`**: Synthetic load and failure orchestrator.

### 2. Open Live TUI in Another Shell

```bash
make tui
```

---

## 🛠️ Break vs Fix: The Remediation Playbook

`ephemeral-collapse` bridges the gap between an exploit tool and an actionable production runbook with paired **Break $\to$ Fix** commands:

### 1. Listen Backlog Saturation (`somaxconn`)
```bash
# 1. TRIGGER THE COLLAPSE (Clamps somaxconn=128 and fires 4,096 SYN burst)
make break-syn
# Observation: Heavy ListenDrops in TUI, client ETIMEDOUT, latency jumps to 1,500ms

# 2. APPLY PRODUCTION REMEDIATION (Tuning somaxconn=65535 & tcp_max_syn_backlog=65535)
make fix-syn
# Observation: Zero drops under the IDENTICAL 4,096 SYN burst! Sub-millisecond latency.
```

### 2. TIME_WAIT & Ephemeral Port Exhaustion
```bash
# 1. TRIGGER PORT DEPLETION (Client port pool: 512 ports, tcp_tw_reuse=0)
make break-timewait
# Observation: Client crashes with EADDRNOTAVAIL ("Cannot assign requested address")

# 2. APPLY SOCKET RECYCLING (Enables tcp_tw_reuse=1 and expands port range)
make fix-timewait
# Observation: 100% success rate under high connection churn without errors.
```

### 3. Netfilter Conntrack Table Flooding
```bash
# 1. TRIGGER 5-TUPLE CHURN (Connection: close flood)
make break-conntrack
# Observation: Conntrack table utilization jumps rapidly towards 100%

# 2. APPLY HTTP KEEP-ALIVE CONNECTION POOLING
make fix-conntrack
# Observation: Re-runs identical RPS with persistent pools; conntrack allocations drop by 99%.
```

---

## 🔍 eBPF Diagnostic Probes & Kernel Portability

The eBPF engine is located in [`services/ebpf-probe`](services/ebpf-probe/). It compiles a C program ([`bpf/tcp_seam_trace.bpf.c`](services/ebpf-probe/bpf/tcp_seam_trace.bpf.c)) designed for portability across **Linux 5.x and 6.x+**:

- **Stable Tracepoints**: Employs `tracepoint/sock/inet_sock_set_state` to track TCP state transitions without relying on kernel struct layout offsets that drift between kernel versions.
- **Kernel Version Guarding**: Uses compile-time macros for `kfree_skb` drop reason arguments (introduced in Linux 5.17+):
  ```c
  TRACEPOINT_PROBE(skb, kfree_skb) {
  #if LINUX_VERSION_CODE >= KERNEL_VERSION(5, 17, 0)
      u32 reason = args->reason;
  #else
      u32 reason = 0; // Legacy fallback
  #endif
      ...
  }
  ```
- **Dual-Mode Engine**: If BPF compilation fails or is restricted by virtualization (e.g. Docker Desktop on macOS/Windows), the probe **automatically switches** to the universal Procfs/Netlink collector ([`fallback_collector.py`](services/ebpf-probe/fallback_collector.py)), sampling `/proc/net/netstat` with sub-second resolution.

---

## 📐 Mathematical Sizing & Tuning Formulas

### 1. Netfilter Conntrack Memory Sizing

Kernel slab memory allocated for connection tracking:

$$\text{Memory}_{\text{conntrack}} = (\text{nf\_conntrack\_max} \times 320\text{ bytes}) + (\text{nf\_conntrack\_buckets} \times 8\text{ bytes})$$

*Golden Rule*: Maintain a **1:4** ratio between buckets and maximum connections:
$$\text{nf\_conntrack\_buckets} = \frac{\text{nf\_conntrack\_max}}{4}$$

| `nf_conntrack_max` | `nf_conntrack_buckets` | Kernel Slab RAM Allocated |
| :--- | :--- | :--- |
| `262,144` | `65,536` | ~80 MB |
| `1,048,576` | `262,144` | ~322 MB |
| `2,097,152` | `524,288` | ~644 MB |

---

### 2. Burst Backlog Sizing Equation

To prevent silent SYN drops during instantaneous traffic spikes, the Listen Queue capacity $Q_{\text{accept}}$ must absorb the burst arrival rate $\lambda_{\text{burst}}$ across the maximum thread scheduling delay $\Delta t_{\text{accept}}$:

$$Q_{\text{accept}} \ge \lambda_{\text{burst}} \times \Delta t_{\text{accept}}$$

For a service handling a $50,000\text{ conn/s}$ burst with a $100\text{ms}$ ($0.1\text{s}$) event loop processing lag:
$$Q_{\text{accept}} \ge 50000 \times 0.1 = 5,000 \implies \mathbf{\texttt{net.core.somaxconn} \ge 8192}$$

---

### 3. Maximum Connection Churn Rate (TIME_WAIT Ceiling)

Without socket reuse (`tcp_tw_reuse = 0`), the maximum sustainable connection establishment rate is bounded by the ephemeral port pool divided by the 2MSL duration ($60\text{s}$):

$$\text{RPS}_{\text{max}} = \frac{|P_{\text{ephemeral}}|}{2 \times \text{MSL}} = \frac{\text{port\_max} - \text{port\_min} + 1}{60}$$

For default Linux ports ($32768\text{--}60999 = 28,232\text{ ports}$):
$$\text{RPS}_{\text{max}} = \frac{28232}{60} \approx \mathbf{470\text{ RPS}}$$

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
