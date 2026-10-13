package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	subcmd := os.Args[1]
	subArgs := os.Args[2:]

	switch subcmd {
	case "syn-burst":
		runSynBurstCmd(ctx, subArgs)
	case "bench":
		runBenchCmd(ctx, subArgs)
	case "time-wait":
		runTimeWaitCmd(ctx, subArgs)
	case "conntrack-churn":
		runConntrackChurnCmd(ctx, subArgs)
	case "-scenario": // Docker compose backward compatibility fallback
		dispatchLegacyFlag(ctx, subArgs)
	default:
		// Check if invoked with legacy flags like --target or -scenario
		if strings.HasPrefix(subcmd, "-") {
			dispatchLegacyFlag(ctx, os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subcmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("syndrop traffic generator:")
	fmt.Println("  syn-burst        Flood listen backlog with synchronized concurrent TCP SYNs")
	fmt.Println("  bench            Sustained high-throughput HTTP keep-alive benchmark")
	fmt.Println("  time-wait        High-churn client active close (exhausts ephemeral ports)")
	fmt.Println("  conntrack-churn  5-tuple churn with Connection: close (floods netfilter)")
}

func dispatchLegacyFlag(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("legacy", flag.ExitOnError)
	target := fs.String("target", "http://127.0.0.1:8080", "Target URL")
	scenario := fs.String("scenario", "bench", "Scenario")
	concurrency := fs.Int("concurrency", 128, "Concurrency")
	duration := fs.Duration("duration", 15*time.Second, "Duration")
	burstSize := fs.Int("burst-size", 4096, "Burst size")
	waves := fs.Int("waves", 4, "Waves")
	waveInterval := fs.Duration("wave-interval", 2*time.Second, "Interval")
	_ = fs.Parse(args)

	if env := os.Getenv("TARGET_URL"); env != "" {
		*target = env
	}
	if env := os.Getenv("SCENARIO"); env != "" {
		*scenario = env
	}

	switch *scenario {
	case "syn-burst", "syn":
		execSynBurst(ctx, *target, *burstSize, *waves, *waveInterval, 1500*time.Millisecond)
	case "time-wait", "timewait":
		execTimeWait(ctx, *target, *concurrency, *duration)
	case "conntrack-churn", "conntrack":
		execConntrackChurn(ctx, *target, *concurrency, *duration)
	default:
		execBench(ctx, *target, *concurrency, *duration)
	}
}

// -----------------------------------------------------------------------------
// SUBCOMMAND: syn-burst
// -----------------------------------------------------------------------------

func runSynBurstCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("syn-burst", flag.ExitOnError)
	target := fs.String("target", "127.0.0.1:8080", "Target host:port")
	burstSize := fs.Int("burst-size", 4096, "Number of concurrent connection attempts per wave")
	waves := fs.Int("waves", 4, "Number of burst waves to dispatch")
	interval := fs.Duration("interval", 2*time.Second, "Pause between burst waves")
	dialTimeout := fs.Duration("dial-timeout", 1500*time.Millisecond, "TCP handshake timeout")
	_ = fs.Parse(args)

	if env := os.Getenv("TARGET_URL"); env != "" {
		*target = env
	}

	execSynBurst(ctx, *target, *burstSize, *waves, *interval, *dialTimeout)
}

func execSynBurst(ctx context.Context, target string, burstSize, waves int, interval, timeout time.Duration) {
	hostPort := cleanHostPort(target)
	fmt.Printf("[syn-burst] Target: %s | Burst: %d conns/wave | Waves: %d | Handshake Timeout: %v\n",
		hostPort, burstSize, waves, timeout)

	var totalSuccess uint64
	var totalDropped uint64
	var totalRefused uint64
	var totalReset uint64

	for wave := 1; wave <= waves; wave++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var waveSuccess uint64
		var waveDropped uint64
		var waveRefused uint64
		var waveReset uint64

		// Barrier to release all workers simultaneously in the same millisecond window
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		waveStart := time.Now()

		for i := 0; i < burstSize; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-barrier // Wait for simultaneous release trigger

				d := net.Dialer{
					Timeout: timeout,
				}
				conn, err := d.DialContext(ctx, "tcp", hostPort)
				if err != nil {
					errMsg := err.Error()
					switch {
					case strings.Contains(errMsg, "i/o timeout"):
						atomic.AddUint64(&waveDropped, 1)
					case strings.Contains(errMsg, "connection refused"):
						atomic.AddUint64(&waveRefused, 1)
					case strings.Contains(errMsg, "reset by peer"):
						atomic.AddUint64(&waveReset, 1)
					default:
						atomic.AddUint64(&waveDropped, 1)
					}
					return
				}

				// Minimal linger delay: set linger to 0 to avoid client TIME_WAIT pin
				if tcpConn, ok := conn.(*net.TCPConn); ok {
					_ = tcpConn.SetLinger(0)
				}
				_ = conn.Close()
				atomic.AddUint64(&waveSuccess, 1)
			}()
		}

		// Trigger wave launch
		close(barrier)

		// Real-time progress monitoring loop while wave is in flight
		doneChan := make(chan struct{})
		go func() {
			wg.Wait()
			close(doneChan)
		}()

	pollLoop:
		for {
			select {
			case <-doneChan:
				break pollLoop
			case <-time.After(100 * time.Millisecond):
				s := atomic.LoadUint64(&waveSuccess)
				d := atomic.LoadUint64(&waveDropped)
				r := atomic.LoadUint64(&waveRefused)
				rst := atomic.LoadUint64(&waveReset)
				elapsed := time.Since(waveStart).Seconds()
				rate := float64(s+d+r+rst) / mathMax(0.001, elapsed)
				fmt.Printf("\r[syn-burst] Wave %d/%d | Established: %5d | Timeouts (Drops): %5d | RST/Refused: %4d | Rate: %6.0f/s",
					wave, waves, s, d, r+rst, rate)
			}
		}

		s := atomic.LoadUint64(&waveSuccess)
		d := atomic.LoadUint64(&waveDropped)
		r := atomic.LoadUint64(&waveRefused)
		rst := atomic.LoadUint64(&waveReset)
		atomic.AddUint64(&totalSuccess, s)
		atomic.AddUint64(&totalDropped, d)
		atomic.AddUint64(&totalRefused, r)
		atomic.AddUint64(&totalReset, rst)

		dur := time.Since(waveStart).Round(time.Millisecond)
		fmt.Printf("\r[syn-burst] Wave %d/%d finished in %v | Established: %d | Drops (Timeout): %d | Refused: %d | RST: %d\n",
			wave, waves, dur, s, d, r, rst)

		if wave < waves {
			time.Sleep(interval)
		}
	}

	fmt.Printf("\n=== SYN BURST COMPLETE ===\nTotal Attempted: %d | Total Established: %d | Total Listen Drops: %d (%.2f%% drop rate)\n",
		totalSuccess+totalDropped+totalRefused+totalReset,
		totalSuccess,
		totalDropped,
		(float64(totalDropped)/mathMax(1.0, float64(totalSuccess+totalDropped+totalRefused+totalReset)))*100.0,
	)
}

// -----------------------------------------------------------------------------
// SUBCOMMAND: bench (HTTP keep-alive)
// -----------------------------------------------------------------------------

func runBenchCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	target := fs.String("target", "http://127.0.0.1:8080", "Target HTTP URL")
	concurrency := fs.Int("concurrency", 128, "Concurrent worker connections")
	duration := fs.Duration("duration", 10*time.Second, "Benchmark duration")
	_ = fs.Parse(args)

	if env := os.Getenv("TARGET_URL"); env != "" {
		*target = env
	}

	execBench(ctx, *target, *concurrency, *duration)
}

func execBench(ctx context.Context, target string, concurrency int, duration time.Duration) {
	targetURL := cleanHTTPURL(target) + "/bench"
	fmt.Printf("[bench] Target: %s | Concurrency: %d | Duration: %v\n", targetURL, concurrency, duration)

	tr := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}

	var totalReqs uint64
	var totalErrors uint64
	deadline := time.Now().Add(duration)
	startTime := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
				if err != nil {
					atomic.AddUint64(&totalErrors, 1)
					continue
				}

				resp, err := client.Do(req)
				if err != nil {
					atomic.AddUint64(&totalErrors, 1)
					continue
				}

				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode == http.StatusOK {
					atomic.AddUint64(&totalReqs, 1)
				} else {
					atomic.AddUint64(&totalErrors, 1)
				}
			}
		}()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reqs := atomic.LoadUint64(&totalReqs)
				errs := atomic.LoadUint64(&totalErrors)
				elapsed := time.Since(startTime).Seconds()
				rps := float64(reqs) / mathMax(0.001, elapsed)
				fmt.Printf("\r[bench] Time: %4.1fs | Reqs: %7d | RPS: %7.0f | Errors: %4d", elapsed, reqs, rps, errs)
			}
		}
	}()

	wg.Wait()
	elapsed := time.Since(startTime).Seconds()
	reqs := atomic.LoadUint64(&totalReqs)
	errs := atomic.LoadUint64(&totalErrors)
	fmt.Printf("\n=== BENCHMARK COMPLETE ===\nTotal Time: %.2fs | Reqs: %d | Throughput: %.1f RPS | Errors: %d\n",
		elapsed, reqs, float64(reqs)/mathMax(0.001, elapsed), errs)
}

// -----------------------------------------------------------------------------
// SUBCOMMAND: time-wait
// -----------------------------------------------------------------------------

func runTimeWaitCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("time-wait", flag.ExitOnError)
	target := fs.String("target", "127.0.0.1:8080", "Target host:port")
	concurrency := fs.Int("concurrency", 256, "Concurrent dial workers")
	duration := fs.Duration("duration", 10*time.Second, "Test duration")
	_ = fs.Parse(args)

	if env := os.Getenv("TARGET_URL"); env != "" {
		*target = env
	}

	execTimeWait(ctx, *target, *concurrency, *duration)
}

func execTimeWait(ctx context.Context, target string, concurrency int, duration time.Duration) {
	hostPort := cleanHostPort(target)
	fmt.Printf("[time-wait] Target: %s | Workers: %d | Active close churn\n", hostPort, concurrency)

	var successCount uint64
	var addrNotAvail uint64
	var otherErrors uint64
	deadline := time.Now().Add(duration)
	startTime := time.Now()

	var wg sync.WaitGroup
	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := net.Dialer{Timeout: 2 * time.Second}

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				conn, err := d.DialContext(ctx, "tcp", hostPort)
				if err != nil {
					if strings.Contains(err.Error(), "cannot assign requested address") {
						atomic.AddUint64(&addrNotAvail, 1)
					} else {
						atomic.AddUint64(&otherErrors, 1)
					}
					time.Sleep(1 * time.Millisecond)
					continue
				}

				_, _ = conn.Write([]byte("GET /churn HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
				_ = conn.Close() // Active client close puts socket in client TIME_WAIT
				atomic.AddUint64(&successCount, 1)
			}
		}()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s := atomic.LoadUint64(&successCount)
				e := atomic.LoadUint64(&addrNotAvail)
				o := atomic.LoadUint64(&otherErrors)
				elapsed := time.Since(startTime).Seconds()
				rate := float64(s) / mathMax(0.001, elapsed)
				fmt.Printf("\r[time-wait] Time: %4.1fs | Dials: %6d (%.0f/s) | EADDRNOTAVAIL (Exhaustion): %5d | Other: %3d",
					elapsed, s, rate, e, o)
			}
		}
	}()

	wg.Wait()
	fmt.Printf("\n=== TIME_WAIT RUN COMPLETE ===\nSuccessful Dials: %d | Port Depletion (EADDRNOTAVAIL): %d\n",
		atomic.LoadUint64(&successCount), atomic.LoadUint64(&addrNotAvail))
}

// -----------------------------------------------------------------------------
// SUBCOMMAND: conntrack-churn
// -----------------------------------------------------------------------------

func runConntrackChurnCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("conntrack-churn", flag.ExitOnError)
	target := fs.String("target", "http://127.0.0.1:8080", "Target URL")
	concurrency := fs.Int("concurrency", 256, "Concurrent workers")
	duration := fs.Duration("duration", 10*time.Second, "Test duration")
	_ = fs.Parse(args)

	if env := os.Getenv("TARGET_URL"); env != "" {
		*target = env
	}

	execConntrackChurn(ctx, *target, *concurrency, *duration)
}

func execConntrackChurn(ctx context.Context, target string, concurrency int, duration time.Duration) {
	churnURL := cleanHTTPURL(target) + "/churn"
	fmt.Printf("[conntrack-churn] Flooding 5-tuples against %s | Workers: %d\n", churnURL, concurrency)

	var successCount uint64
	var errCount uint64
	deadline := time.Now().Add(duration)
	startTime := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &http.Transport{
				DisableKeepAlives: true, // Forces brand new 5-tuple for every request
				DialContext: (&net.Dialer{
					Timeout: 2 * time.Second,
				}).DialContext,
			}
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				req, _ := http.NewRequestWithContext(ctx, "GET", churnURL, nil)
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddUint64(&errCount, 1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				atomic.AddUint64(&successCount, 1)
			}
		}()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s := atomic.LoadUint64(&successCount)
				e := atomic.LoadUint64(&errCount)
				elapsed := time.Since(startTime).Seconds()
				rate := float64(s) / mathMax(0.001, elapsed)
				fmt.Printf("\r[conntrack-churn] Time: %4.1fs | New 5-Tuples: %6d (%.0f/s) | Errors/Drops: %4d",
					elapsed, s, rate, e)
			}
		}
	}()

	wg.Wait()
	fmt.Printf("\n=== CONNTRACK CHURN COMPLETE ===\nAllocated 5-Tuples: %d | Drops: %d\n",
		atomic.LoadUint64(&successCount), atomic.LoadUint64(&errCount))
}

// -----------------------------------------------------------------------------
// Utilities
// -----------------------------------------------------------------------------

func cleanHostPort(target string) string {
	s := strings.TrimPrefix(target, "http://")
	s = strings.TrimPrefix(s, "https://")
	if idx := strings.Index(s, "/"); idx != -1 {
		s = s[:idx]
	}
	if !strings.Contains(s, ":") {
		s += ":80"
	}
	return s
}

func cleanHTTPURL(target string) string {
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return "http://" + target
	}
	return strings.TrimRight(target, "/")
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
