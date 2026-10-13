# syndrop

A minimal harness and eBPF tracer for kernel-level socket drops, accept-queue overflows, and netfilter table exhaust under burst traffic (>100k RPS).

```
                      +-----------------------------+
                      |       100k+ RPS Burst       |
                      +-----------------------------+
                                     |
                       [NIC Ring Buffer / Softirq]
                                     |
               +---------------------+---------------------+
               |                     |                     |
               v                     v                     v
      [1. netfilter]        [2. SYN / Accept]       [3. TIME_WAIT]
      nf_conntrack:          sk_ack_backlog >       ip_local_port_range
      table full drops       sk_max_ack_backlog     exhaustion (5-tuple)
               |                     |                     |
               +---------------------+---------------------+
                                     |
                          [Silent Kernel Drops]
                                     |
               +-------------------------------------------+
               | User Space Runtime (Tokio / Go / Envoy)   |
               | (0% CPU, no errors logged, p99 = 3000ms)  |
               +-------------------------------------------+
```

## Why this exists

When microservices randomly return 502s or see 3-second p99 latency spikes, application metrics (Prometheus, APM) usually look completely fine because the packet was dropped in the kernel before the runtime (Tokio/epoll/Go runtime) ever saw it. Your userspace profilers show low CPU utilization, empty thread pools, and zero application exceptions, while clients time out waiting for connections to establish.

Existing tools either test HTTP apps (wrk, vegeta) or trace the kernel (bpftrace, dropwatch), but there is no zero-config harness that intentionally clamps specific kernel knobs in an isolated netns, drives exact traffic failure shapes, and measures the drop path in real time. `syndrop` isolates client and server network namespaces, reproduces these drops on demand, traces them with eBPF kprobes and tracepoints, and provides the exact sysctl math to fix them.

## Architecture & Failure Seams

```
+-----------------------------------------------------------------------------+
|                             LINUX KERNEL INGRESS                            |
|                                                                             |
|  [sk_buff] ---> PREROUTING ---> nf_conntrack_in()                           |
|                                        |                                    |
|                                        +---> [SEAM 2: CONNTRACK EXHAUST]    |
|                                        |     atomic_read(&net->ct.count)    |
|                                        |     >= nf_conntrack_max            |
|                                        v                                    |
|                                tcp_v4_rcv()                                 |
|                                        |                                    |
|                     +------------------+------------------+                 |
|                     |                                     |                 |
|            [TCP 3WHS in flight]                  [Established Data]         |
|                     |                                     |                 |
|          tcp_v4_syn_recv_sock()                           v                 |
|                     |                            [SEAM 4: EPOLL CHURN]      |
|                     v                            sk_rcvbuf pinning &        |
|        [SEAM 1: ACCEPT QUEUE FULL]               epmutex worker contention  |
|        sk_ack_backlog > sk_max_ack_backlog                |                 |
|        (Silent drop or TCP RST)                           v                 |
|                     |                            epoll_wait()               |
|                     v                                     |                 |
+---------------------+-------------------------------------+-----------------+
                      |                                     |
                      v                                     v
         +--------------------------+          +-------------------------+
         | Client Outbound Dial     |          | Application Runtime     |
         | [SEAM 3: TIME_WAIT /     |          | (Tokio / Go Netpoll)    |
         |  PORT EXHAUSTION]        |          +-------------------------+
         | EADDRNOTAVAIL (5-tuple)  |
         +--------------------------+
```

### 1. Accept Queue Overflow (`sk_ack_backlog > sk_max_ack_backlog`)

During the TCP three-way handshake, when the final ACK arrives from the client, the kernel executes `tcp_v4_syn_recv_sock()`. The socket transitions from the SYN queue to the Accept Queue.

```
Client                                Linux Kernel                           Server
  |                                         |                                  |
  |--- SYN [seq=0] ------------------------>|                                  |
  |                                         | Alloc request_sock               |
  |<-- SYN-ACK [seq=0, ack=1] --------------| Added to SYN backlog             |
  |                                         |                                  |
  |--- ACK [seq=1, ack=1] ----------------->|                                  |
  |    (Client marks connection ESTABLISHED)| tcp_v4_syn_recv_sock()           |
  |                                         |                                  |
  |                                         | if (sk_ack_backlog >             |
  |                                         |     sk_max_ack_backlog)          |
  |                                         |   DROP ACK SILENTLY              |
  |                                         |   (tcp_abort_on_overflow=0)      |
  |                                         |                                  |
  |--- HTTP GET (Data) -------------------->|                                  |
  |                                         | Ignored (Socket in SYN_RECV)     |
  |                                         |                                  |
  |    ... 1000ms Retransmission Timer ...  |                                  |
  |                                         |                                  |
  |<-- Retransmitted SYN-ACK ---------------| Kernel thinks ACK was lost       |
  |--- ACK (retransmit) ------------------->| Queue drained -> moves to ACCEPT |
  |                                         |--------------------------------->| accept4()
```

If `tcp_abort_on_overflow = 0` (Linux default), the kernel does not reset the connection. It drops the final ACK and leaves the socket in `SYN_RECV`. The client immediately sends application data, which the server kernel ignores because the socket is not fully established. The client stalls until its TCP retransmission timer fires (initial RTO is typically 1 second, backing off to 3 seconds).

### 2. Netfilter Conntrack Hash Exhaustion

Every TCP packet evaluated by `iptables` or connection tracking enters `nf_conntrack_in()`. Entries are stored in a fixed-size hash table of `struct nf_conn` structs:

```
Packet Arrival
      |
      v
nf_conntrack_in()
      |
      v
Hash 5-tuple -> Bucket linked list
      |
      +---> Bucket length > max depth? -> Softirq CPU burn traversing chains
      |
__nf_conntrack_alloc()
      |
      +---> atomic_read(&net->ct.count) >= nf_conntrack_max
            |
            v
      KERNEL DROP: "nf_conntrack: table full, dropping packet"
      (kretprobe:__nf_conntrack_alloc returns ENOMEM, packet dropped at PREROUTING)
```

No TCP RST or ICMP message is sent. The packet is dropped at `PREROUTING`. The client hangs until application-level connection timeouts trigger.

### 3. Port Starvation & TIME_WAIT Bucket Depletion

When clients perform active closes (sending FIN first), the local socket remains in `TIME_WAIT` for $2 \times \text{MSL}$ (60 seconds):

```
Client Active Close:
  close(fd) -> Sends FIN -> FIN-ACK exchange completes -> Enters TIME_WAIT (60s)
                                                                 |
           +-----------------------------------------------------+
           |
           v
  Next Outbound Dial:
    Kernel scans ephemeral port range (net.ipv4.ip_local_port_range).
    - If tcp_tw_reuse = 0: Port is locked for 60 seconds.
    - At >500 RPS without connection pooling, 28,000 ephemeral ports exhaust in <60s.
    - dial() syscall fails immediately: EADDRNOTAVAIL ("Cannot assign requested address").
    - If net.ipv4.tcp_max_tw_buckets is reached:
      Kernel logs "TCP: time wait bucket table overflow" and destroys socket without timer.
```

### 4. Epoll Read-Event Starvation Under Chunked Payloads

When hundreds of concurrent connections transmit chunked or partial request bodies (slowloris style), sockets remain in `ESTABLISHED` while consuming kernel receive buffers (`sk_rcvbuf`).

```
Worker 0 (epoll_wait)        Worker 1 (epoll_wait)        Kernel Socket Buffer
        |                            |                             |
        +--- epmutex Contention -----+                             |
        |                            |                             |
        v                            |                             |
  Woken by 1-byte read               |                             |
  Read 1 byte -> returns EAGAIN      |                             |
        |                            v                             |
        |                      Woken by 1-byte read                |
        |                      Read 1 byte -> returns EAGAIN       |
        |                            |                             |
        +----------------------------+                             |
        | Workers burn CPU handling single-byte I/O events         |
        | sk_buff allocations pin kernel slab memory (rmem_default)|
        +----------------------------------------------------------+
```

Worker threads spinning on partial socket reads experience epoll event loop lag, delaying `accept()` calls for new connections and directly precipitating Seam 1 (Accept Queue Overflow).

---

## Quickstart (Under 30 Seconds)

### 1. Build and Start the Harness

```bash
git clone https://github.com/your-org/syndrop.git
cd syndrop
docker compose up -d --build
```

### 2. Trigger Accept-Queue Overflow (`make break-backlog`)

Run a 4,096 connection burst against the backend with `somaxconn` clamped to 128:

```bash
make break-syn
```

### 3. Inspect Live Kernel Drop Traces

Attach to the eBPF probe output:

```bash
docker compose logs -f ebpf-probe
```

Raw tracer output:

```
[2026-10-13 18:24:02.194821] [CPU 03] [kprobe:tcp_v4_syn_recv_sock] PID 48210 (tokio-runtime-w)
  saddr=172.28.0.3:48202 daddr=172.28.0.2:8080 state=TCP_SYN_RECV
  sk_ack_backlog=129 sk_max_ack_backlog=128
  action=SILENT_DROP_ACK (retransmission penalty ~1000ms)

[2026-10-13 18:24:02.195104] [CPU 01] [kprobe:tcp_v4_syn_recv_sock] PID 48210 (tokio-runtime-w)
  saddr=172.28.0.3:48204 daddr=172.28.0.2:8080 state=TCP_SYN_RECV
  sk_ack_backlog=135 sk_max_ack_backlog=128
  action=SILENT_DROP_ACK (retransmission penalty ~1000ms)

[2026-10-13 18:24:02.195312] [CPU 02] [tracepoint:skb:kfree_skb]
  skb=0xffff94812a81c300 location=tcp_v4_rcv+0x814 reason=SKB_DROP_REASON_TCP_BACKLOG
```

Client-side results from the generator:

```
===============================================================
 [SCENARIO RESULTS] Listen Backlog Queue Saturation
 Duration:        2.14s
 Total Requests:  4096 (1914.02 req/s)
 Successful:      128
 Failed/Dropped:  3968
 Failure Ratio:   96.88%
 Latency p50:     1.502s
 Latency p95:     1.505s
 Latency p99:     1.508s
 --- Kernel & Network Seam Error Breakdown ---
   * [ETIMEDOUT (SYN Retransmission / Blackholed Packet)]: 3968 occurrences
===============================================================
```

### 4. Apply Kernel Remediation (`make fix-backlog`)

Tune the server socket backlog and listen limits:

```bash
make fix-syn
```

Reruns the identical 4,096 connection burst:

```
===============================================================
 [SCENARIO RESULTS] Listen Backlog Queue Saturation (Remediated)
 Duration:        182ms
 Total Requests:  4096 (22505.49 req/s)
 Successful:      4096
 Failed/Dropped:  0
 Failure Ratio:   0.00%
 Latency p50:     4.2ms
 Latency p95:     11.8ms
 Latency p99:     18.1ms
===============================================================
```

---

## Kernel Telemetry: eBPF vs Fallback

The probe container runs two diagnostic pathways:

```
                           +---------------------------+
                           | syndrop Diagnostic Engine |
                           +---------------------------+
                                         |
                       Test BPF syscall & debugfs access
                                         |
                    +--------------------+--------------------+
                    |                                         |
         [Root / CAP_BPF granted]                  [Restricted / VM Container]
                    v                                         v
         +-----------------------+                 +------------------------+
         | eBPF Tracing Mode     |                 | Procfs Fallback Mode   |
         | (trace.py)            |                 | (fallback_collector.py)|
         +-----------------------+                 +------------------------+
```

### eBPF Mode

- **Hooks**:
  - `kprobe:tcp_v4_syn_recv_sock`: Inspects `sk->sk_ack_backlog` against `sk->sk_max_ack_backlog` on every completed handshake. Emits drops with full IP 5-tuple via BPF perf ring buffer.
  - `tracepoint/sock/inet_sock_set_state`: Tracks transitions between `TCP_SYN_RECV`, `TCP_ESTABLISHED`, `TCP_TIME_WAIT`, and `TCP_CLOSE`.
  - `kretprobe:__nf_conntrack_alloc`: Hooks return value; logs table exhaustion when allocation returns `NULL`.
  - `tracepoint/skb/kfree_skb`: Extracts `reason` code (`SKB_DROP_REASON_TCP_BACKLOG`, `SKB_DROP_REASON_SOCKET_RCVBUFF`).
- **Prerequisites**: Linux kernel headers, root or `CAP_BPF` + `CAP_NET_ADMIN`, mounted `/sys/kernel/debug/tracing`.

### Fallback Mode

Activated automatically if `bpf()` returns `EPERM` or kernel headers are missing (e.g. running in Docker Desktop VM on macOS or Windows).

- Polls `/proc/net/netstat` at 100ms intervals:
  - `TcpExt.ListenOverflows`: Counts times the accept queue filled and incoming connection was rejected.
  - `TcpExt.ListenDrops`: Counts total incoming packets dropped due to listen backlog saturation or memory exhaustion.
  - `TcpExt.TCPTimeWaitOverflow`: Counts times a socket could not enter TIME_WAIT because `tcp_max_tw_buckets` was exceeded.
- Polls `/proc/sys/net/netfilter/nf_conntrack_count` against `nf_conntrack_max`.
- Polls `/proc/net/sockstat` for real-time `TCP: inuse`, `tw`, and `alloc` counts.

---

## Production Playbook: The sysctl Math

### 1. Accept Queue & SYN Backlog Sizing

For a node handling burst traffic, the queue must hold all connections arriving during the maximum application event loop latency $\Delta t$:

$$\text{Queue Capacity} \ge \lambda_{\text{burst}} \times \Delta t_{\text{accept}}$$

Example: A burst of $40,000\text{ conns/sec}$ where the Tokio/Go event loop experiences a $50\text{ms}$ GC or scheduling pause:

$$\text{Capacity} \ge 40000 \times 0.050 = 2,000\text{ connections}$$

Set both system and socket limits:
```bash
# Host / Container sysctl
sysctl -w net.core.somaxconn=8192
sysctl -w net.ipv4.tcp_max_syn_backlog=8192
```

In your application code, pass the corresponding backlog argument:
```rust
// Rust (socket2)
socket.listen(8192)?;
```

### 2. Conntrack Memory Overhead

Every conntrack entry consumes unswappable kernel slab memory (`struct nf_conn`):

$$\text{Memory (bytes)} = (\text{nf\_conntrack\_max} \times 320) + (\text{nf\_conntrack\_buckets} \times 8)$$

Always size buckets to a 1:4 ratio with max entries:

$$\text{nf\_conntrack\_buckets} = \frac{\text{nf\_conntrack\_max}}{4}$$

Memory sizing reference:
- $262,144$ entries ($65,536$ buckets) $\approx 84\text{ MB}$ kernel RAM.
- $1,048,576$ entries ($262,144$ buckets) $\approx 337\text{ MB}$ kernel RAM.
- $2,097,152$ entries ($524,288$ buckets) $\approx 675\text{ MB}$ kernel RAM.

Set via sysctl:
```bash
sysctl -w net.netfilter.nf_conntrack_max=2097152
# Bucket count must be set via module parameter or sysfs:
echo 524288 > /sys/module/nf_conntrack/parameters/hashsize
```

### 3. `tcp_tw_recycle` vs `tcp_tw_reuse`

- **Do NOT enable `net.ipv4.tcp_tw_recycle = 1`**: It was completely removed in Linux 4.12. It caches per-host TCP timestamps via PAWS (Protection Against Wrapped Sequence Numbers). Multiple clients behind a single NAT gateway share an IP but have un-synchronized TCP timestamps. Incoming SYNs with lower timestamps are dropped silently, breaking NAT clients.
- **DO enable `net.ipv4.tcp_tw_reuse = 1`**: Safe for outbound client connections. Reuses a socket in `TIME_WAIT` only if the new timestamp is strictly greater than the last timestamp recorded for that specific 5-tuple.

```bash
sysctl -w net.ipv4.tcp_tw_reuse=1
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
```

### 4. The Kubernetes Unprivileged Pod Trap

In Kubernetes, worker nodes may have `net.core.somaxconn = 65535` configured on the host, but unprivileged pods instantiate their own network namespace with `somaxconn` defaulted to `128`.

Unless explicitly configured, your backend application will run with a backlog limit of 128 regardless of host capacity.

To fix, set namespaced sysctls directly in the Pod spec:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: backend-service
spec:
  securityContext:
    sysctls:
      - name: net.core.somaxconn
        value: "8192"
      - name: net.ipv4.tcp_max_syn_backlog
        value: "8192"
      - name: net.ipv4.ip_local_port_range
        value: "1024 65535"
      - name: net.ipv4.tcp_tw_reuse
        value: "1"
```

> **Note**: Your kubelet must be started with `--allowed-unsafe-sysctls 'net.core.somaxconn,net.ipv4.tcp_max_syn_backlog'` if using older Kubernetes versions that do not treat these as safe sysctls.

---

## Caveats & Edge Cases

1. **SYN Cookies Masking Drops**: If `net.ipv4.tcp_syncookies = 1`, the kernel will not drop SYNs when `tcp_max_syn_backlog` fills. Instead, it computes cryptographic syncookies. While connections survive, TCP options (window scaling, SACK, timestamps) are stripped, severely degrading throughput on high-bandwidth connections.
2. **`SO_REUSEPORT` Uneven Distribution**: Under heavy traffic, the kernel uses a 4-tuple hash to distribute connections across `SO_REUSEPORT` listeners. If single client IPs send large volumes of requests, specific worker queues receive disproportionate load while other workers sit idle.
3. **WSL2 / Docker Desktop Tracing**: WSL2 kernels do not always bundle `/lib/modules/$(uname -r)` or kernel headers by default. `syndrop` will fall back to reading `/proc/net/netstat` in these environments.

---

## License

[MIT](LICENSE)
