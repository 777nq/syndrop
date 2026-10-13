//! syndrop-backend: Low-latency raw Tokio TCP HTTP server with socket2 kernel tuning.

use socket2::{Domain, Protocol, Socket, Type};
use std::env;
use std::io;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Instant;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

static ACTIVE_CONNECTIONS: AtomicUsize = AtomicUsize::new(0);
static TOTAL_REQUESTS: AtomicU64 = AtomicU64::new(0);

const BENCH_RESPONSE: &[u8] =
    b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK";

const CHURN_RESPONSE: &[u8] =
    b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\nConnection: close\r\n\r\nCHURN";

const NOT_FOUND_RESPONSE: &[u8] =
    b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";

#[derive(Debug)]
struct CliConfig {
    port: u16,
    backlog: i32,
    reuse_port: bool,
    tcp_nodelay: bool,
}

impl CliConfig {
    fn parse() -> Self {
        let args: Vec<String> = env::args().collect();
        let mut port = 8080u16;
        let mut backlog = 128i32;
        let mut reuse_port = true;
        let mut tcp_nodelay = true;

        if let Ok(p) = env::var("PORT") {
            if let Ok(val) = p.parse() {
                port = val;
            }
        }
        if let Ok(b) = env::var("TCP_BACKLOG") {
            if let Ok(val) = b.parse() {
                backlog = val;
            }
        }

        let mut i = 1;
        while i < args.len() {
            match args[i].as_str() {
                "--port" | "-p" => {
                    if i + 1 < args.len() {
                        if let Ok(val) = args[i + 1].parse() {
                            port = val;
                        }
                        i += 1;
                    }
                }
                "--backlog" | "-b" => {
                    if i + 1 < args.len() {
                        if let Ok(val) = args[i + 1].parse() {
                            backlog = val;
                        }
                        i += 1;
                    }
                }
                "--no-reuseport" => {
                    reuse_port = false;
                }
                "--no-nodelay" => {
                    tcp_nodelay = false;
                }
                "--help" | "-h" => {
                    println!("syndrop-backend options:");
                    println!("  --port, -p <n>       Listen port (default: 8080)");
                    println!("  --backlog, -b <n>    Listen queue backlog (default: 128)");
                    println!("  --no-reuseport       Disable SO_REUSEPORT");
                    println!("  --no-nodelay         Disable TCP_NODELAY");
                    std::process::exit(0);
                }
                _ => {}
            }
            i += 1;
        }

        CliConfig {
            port,
            backlog,
            reuse_port,
            tcp_nodelay,
        }
    }
}

fn create_listener(cfg: &CliConfig) -> io::Result<TcpListener> {
    let addr: SocketAddr = format!("0.0.0.0:{}", cfg.port)
        .parse()
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;

    let socket = Socket::new(Domain::IPV4, Type::STREAM, Some(Protocol::TCP))?;
    socket.set_reuse_address(true)?;

    #[cfg(unix)]
    if cfg.reuse_port {
        let _ = socket.set_reuse_port(true);
    }

    socket.set_nonblocking(true)?;
    let sock_addr = socket2::SockAddr::from(addr);
    socket.bind(&sock_addr)?;
    socket.listen(cfg.backlog)?;

    let std_listener: std::net::TcpListener = socket.into();
    TcpListener::from_std(std_listener)
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cfg = CliConfig::parse();
    let listener = create_listener(&cfg)?;

    eprintln!(
        "[syndrop-backend] Listening on 0.0.0.0:{} (backlog={}, SO_REUSEPORT={}, TCP_NODELAY={})",
        cfg.port, cfg.backlog, cfg.reuse_port, cfg.tcp_nodelay
    );

    let tcp_nodelay = cfg.tcp_nodelay;

    // Background reporting task
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(5));
        let start = Instant::now();
        loop {
            interval.tick().await;
            let active = ACTIVE_CONNECTIONS.load(Ordering::Relaxed);
            let total = TOTAL_REQUESTS.load(Ordering::Relaxed);
            eprintln!(
                "[syndrop-backend] uptime={:>4}s | active_conns={:>5} | total_reqs={:>9}",
                start.elapsed().as_secs(),
                active,
                total
            );
        }
    });

    loop {
        match listener.accept().await {
            Ok((stream, _)) => {
                if tcp_nodelay {
                    let _ = stream.set_nodelay(true);
                }
                ACTIVE_CONNECTIONS.fetch_add(1, Ordering::Relaxed);
                tokio::spawn(async move {
                    handle_client(stream).await;
                    ACTIVE_CONNECTIONS.fetch_sub(1, Ordering::Relaxed);
                });
            }
            Err(e) => {
                eprintln!("[syndrop-backend] Accept error: {}", e);
                tokio::time::sleep(std::time::Duration::from_millis(1)).await;
            }
        }
    }
}

async fn handle_client(mut stream: TcpStream) {
    let mut buf = [0u8; 1024];

    loop {
        match stream.read(&mut buf).await {
            Ok(0) => break, // Peer closed connection
            Ok(n) => {
                TOTAL_REQUESTS.fetch_add(1, Ordering::Relaxed);
                let req_slice = &buf[..n];

                // Fast zero-allocation routing check
                if req_slice.starts_with(b"GET /bench") || req_slice.starts_with(b"GET / ") {
                    if stream.write_all(BENCH_RESPONSE).await.is_err() {
                        break;
                    }
                } else if req_slice.starts_with(b"GET /churn") {
                    let _ = stream.write_all(CHURN_RESPONSE).await;
                    let _ = stream.shutdown().await;
                    break;
                } else if req_slice.starts_with(b"GET /healthz") {
                    if stream.write_all(BENCH_RESPONSE).await.is_err() {
                        break;
                    }
                } else {
                    let _ = stream.write_all(NOT_FOUND_RESPONSE).await;
                    break;
                }

                // If client specified Connection: close, terminate loop
                if is_connection_close(req_slice) {
                    break;
                }
            }
            Err(_) => break,
        }
    }
}

#[inline]
fn is_connection_close(slice: &[u8]) -> bool {
    slice
        .windows(17)
        .any(|w| w.eq_ignore_ascii_case(b"connection: close"))
}
