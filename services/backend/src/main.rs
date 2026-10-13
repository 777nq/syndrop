//! ephemeral-backend: High-Throughput Kernel-Tuned Async Microservice
//!
//! Exposes kernel-level socket knobs via `socket2` and tracks epoll event loop lag,
//! active connections, and connection churn to expose kernel-seam degradation.

use bytes::{BufMut, BytesMut};
use socket2::{Domain, Protocol, Socket, Type};
use std::env;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

static TOTAL_REQUESTS: AtomicU64 = AtomicU64::new(0);
static ACTIVE_CONNECTIONS: AtomicUsize = AtomicUsize::new(0);
static CHURN_CONNECTIONS: AtomicU64 = AtomicU64::new(0);
static BYTES_RECEIVED: AtomicU64 = AtomicU64::new(0);
static BYTES_SENT: AtomicU64 = AtomicU64::new(0);
static EPOLL_LAG_MICROS: AtomicU64 = AtomicU64::new(0);

struct ServerConfig {
    port: u16,
    backlog: i32,
    reuse_port: bool,
    tcp_nodelay: bool,
    rcvbuf_size: Option<usize>,
    sndbuf_size: Option<usize>,
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            port: env::var("PORT")
                .unwrap_or_else(|_| "8080".to_string())
                .parse()
                .unwrap_or(8080),
            backlog: env::var("TCP_BACKLOG")
                .unwrap_or_else(|_| "128".to_string())
                .parse()
                .unwrap_or(128),
            reuse_port: env::var("SO_REUSEPORT")
                .unwrap_or_else(|_| "1".to_string())
                == "1",
            tcp_nodelay: env::var("TCP_NODELAY")
                .unwrap_or_else(|_| "1".to_string())
                == "1",
            rcvbuf_size: env::var("SO_RCVBUF").ok().and_then(|v| v.parse().ok()),
            sndbuf_size: env::var("SO_SNDBUF").ok().and_then(|v| v.parse().ok()),
        }
    }
}

fn create_kernel_tuned_listener(config: &ServerConfig) -> std::io::Result<std::net::TcpListener> {
    let addr: SocketAddr = format!("0.0.0.0:{}", config.port)
        .parse()
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidInput, e))?;
    let domain = Domain::IPV4;
    let socket_type = Type::STREAM;
    let protocol = Some(Protocol::TCP);

    let socket = Socket::new(domain, socket_type, protocol)?;

    socket.set_reuse_address(true)?;

    #[cfg(unix)]
    if config.reuse_port {
        if let Err(e) = socket.set_reuse_port(true) {
            eprintln!("[WARN] Failed to set SO_REUSEPORT: {}", e);
        }
    }

    if let Some(rcv) = config.rcvbuf_size {
        let _ = socket.set_recv_buffer_size(rcv);
    }
    if let Some(snd) = config.sndbuf_size {
        let _ = socket.set_send_buffer_size(snd);
    }

    // Set non-blocking mode required for Tokio async runtime
    socket.set_nonblocking(true)?;

    let sock_addr = socket2::SockAddr::from(addr);
    socket.bind(&sock_addr)?;

    // Listen with explicitly tuned backlog (key seam point for backlog drop testing)
    socket.listen(config.backlog)?;
    println!(
        "[INIT] Socket bound to {} with listen backlog = {} (SO_REUSEPORT={})",
        addr, config.backlog, config.reuse_port
    );

    Ok(socket.into())
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = ServerConfig::default();
    let start_time = Instant::now();

    println!("===============================================================");
    println!(" ephemeral-collapse: High-Throughput Kernel-Tuned Backend");
    println!(" Bound Port:       {}", config.port);
    println!(" Listen Backlog:   {} (somaxconn ceiling test target)", config.backlog);
    println!(" TCP_NODELAY:      {}", config.tcp_nodelay);
    println!(" SO_REUSEPORT:     {}", config.reuse_port);
    println!(" Worker Threads:   {}", tokio::runtime::Handle::current().metrics().num_workers());
    println!("===============================================================");

    // Spawn epoll latency / runtime lag monitor task
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_millis(50));
        loop {
            let tick_start = Instant::now();
            interval.tick().await;
            let elapsed = tick_start.elapsed();
            if elapsed > Duration::from_millis(50) {
                let lag = (elapsed - Duration::from_millis(50)).as_micros() as u64;
                EPOLL_LAG_MICROS.store(lag, Ordering::Relaxed);
            } else {
                EPOLL_LAG_MICROS.store(0, Ordering::Relaxed);
            }
        }
    });

    let std_listener = create_kernel_tuned_listener(&config)?;
    let listener = TcpListener::from_std(std_listener)?;
    let tcp_nodelay = config.tcp_nodelay;

    // Background metric reporter
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(Duration::from_secs(5));
        loop {
            ticker.tick().await;
            let reqs = TOTAL_REQUESTS.load(Ordering::Relaxed);
            let active = ACTIVE_CONNECTIONS.load(Ordering::Relaxed);
            let churn = CHURN_CONNECTIONS.load(Ordering::Relaxed);
            let lag = EPOLL_LAG_MICROS.load(Ordering::Relaxed);
            let uptime = start_time.elapsed().as_secs();
            println!(
                "[TELEMETRY] uptime={}s | reqs={} | active_conns={} | churn={} | epoll_lag={}us",
                uptime, reqs, active, churn, lag
            );
        }
    });

    loop {
        match listener.accept().await {
            Ok((mut socket, peer_addr)) => {
                if tcp_nodelay {
                    let _ = socket.set_nodelay(true);
                }

                ACTIVE_CONNECTIONS.fetch_add(1, Ordering::Relaxed);

                tokio::spawn(async move {
                    handle_connection(&mut socket, peer_addr).await;
                    ACTIVE_CONNECTIONS.fetch_sub(1, Ordering::Relaxed);
                });
            }
            Err(e) => {
                eprintln!("[ACCEPT_ERR] Socket accept failed: {}", e);
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        }
    }
}

async fn handle_connection(socket: &mut tokio::net::TcpStream, _peer_addr: SocketAddr) {
    let mut buf = BytesMut::with_capacity(2048);

    loop {
        match socket.read_buf(&mut buf).await {
            Ok(0) => break, // EOF / remote closed
            Ok(n) => {
                BYTES_RECEIVED.fetch_add(n as u64, Ordering::Relaxed);
                TOTAL_REQUESTS.fetch_add(1, Ordering::Relaxed);

                let req_slice = &buf[..];
                let is_churn = req_slice.windows(6).any(|w| w == b"/churn");
                let is_metrics = req_slice.windows(8).any(|w| w == b"/metrics");
                let is_leak = req_slice.windows(5).any(|w| w == b"/leak");

                if is_metrics {
                    let body = format!(
                        "# HELP ephemeral_active_connections Current active TCP connections\n\
                         # TYPE ephemeral_active_connections gauge\n\
                         ephemeral_active_connections {}\n\
                         # HELP ephemeral_requests_total Total HTTP requests served\n\
                         # TYPE ephemeral_requests_total counter\n\
                         ephemeral_requests_total {}\n\
                         # HELP ephemeral_connection_churn_total Total closed short-lived connections\n\
                         # TYPE ephemeral_connection_churn_total counter\n\
                         ephemeral_connection_churn_total {}\n\
                         # HELP ephemeral_epoll_lag_microseconds Event loop lag\n\
                         # TYPE ephemeral_epoll_lag_microseconds gauge\n\
                         ephemeral_epoll_lag_microseconds {}\n\
                         # HELP ephemeral_bytes_received_total Raw bytes received\n\
                         # TYPE ephemeral_bytes_received_total counter\n\
                         ephemeral_bytes_received_total {}\n\
                         # HELP ephemeral_bytes_sent_total Raw bytes sent\n\
                         # TYPE ephemeral_bytes_sent_total counter\n\
                         ephemeral_bytes_sent_total {}\n",
                        ACTIVE_CONNECTIONS.load(Ordering::Relaxed),
                        TOTAL_REQUESTS.load(Ordering::Relaxed),
                        CHURN_CONNECTIONS.load(Ordering::Relaxed),
                        EPOLL_LAG_MICROS.load(Ordering::Relaxed),
                        BYTES_RECEIVED.load(Ordering::Relaxed),
                        BYTES_SENT.load(Ordering::Relaxed),
                    );

                    let response = format!(
                        "HTTP/1.1 200 OK\r\n\
                         Content-Type: text/plain; version=0.0.4\r\n\
                         Content-Length: {}\r\n\
                         Connection: keep-alive\r\n\r\n{}",
                        body.len(),
                        body
                    );

                    if socket.write_all(response.as_bytes()).await.is_err() {
                        break;
                    }
                    BYTES_SENT.fetch_add(response.len() as u64, Ordering::Relaxed);
                } else if is_churn {
                    // Force immediate socket close to produce TIME_WAIT on server/client
                    CHURN_CONNECTIONS.fetch_add(1, Ordering::Relaxed);
                    let body = r#"{"status":"churn_closed"}"#;
                    let response = format!(
                        "HTTP/1.1 200 OK\r\n\
                         Content-Type: application/json\r\n\
                         Content-Length: {}\r\n\
                         Connection: close\r\n\r\n{}",
                        body.len(),
                        body
                    );
                    let _ = socket.write_all(response.as_bytes()).await;
                    BYTES_SENT.fetch_add(response.len() as u64, Ordering::Relaxed);
                    let _ = socket.shutdown().await;
                    break;
                } else if is_leak {
                    // Simulate slow drain / buffer pinning
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    let body = r#"{"status":"buffer_retained"}"#;
                    let response = format!(
                        "HTTP/1.1 200 OK\r\n\
                         Content-Type: application/json\r\n\
                         Content-Length: {}\r\n\
                         Connection: keep-alive\r\n\r\n{}",
                        body.len(),
                        body
                    );
                    if socket.write_all(response.as_bytes()).await.is_err() {
                        break;
                    }
                    BYTES_SENT.fetch_add(response.len() as u64, Ordering::Relaxed);
                } else {
                    // Ultra-fast zero-alloc bench response
                    let body = r#"{"status":"healthy","kernel_seam":"nominal"}"#;
                    let response = format!(
                        "HTTP/1.1 200 OK\r\n\
                         Content-Type: application/json\r\n\
                         Content-Length: {}\r\n\
                         Connection: keep-alive\r\n\r\n{}",
                        body.len(),
                        body
                    );
                    if socket.write_all(response.as_bytes()).await.is_err() {
                        break;
                    }
                    BYTES_SENT.fetch_add(response.len() as u64, Ordering::Relaxed);
                }

                buf.clear();
            }
            Err(_) => break,
        }
    }
}
