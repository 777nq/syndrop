package scenarios

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// RunTimeWaitOverflow creates high-velocity client-initiated closes, exhausting ephemeral ports and filling tcp_max_tw_buckets
func RunTimeWaitOverflow(ctx context.Context, target string, concurrency int, duration time.Duration) Summary {
	tracker := NewMetricTracker()

	hostPort := strings.TrimPrefix(target, "http://")
	hostPort = strings.TrimPrefix(hostPort, "https://")
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
	}

	fmt.Printf("[TIME-WAIT] Target: %s | Concurrency: %d | Duration: %v\n", hostPort, concurrency, duration)
	fmt.Printf("[TIME-WAIT] Exhausting ephemeral port pool & tcp_max_tw_buckets\n")

	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)

	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			d := net.Dialer{
				Timeout: 2 * time.Second,
			}

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				start := time.Now()
				conn, err := d.DialContext(ctx, "tcp", hostPort)
				if err != nil {
					tracker.RecordFailure(categorizeError(err))
					time.Sleep(1 * time.Millisecond)
					continue
				}

				// Write minimal HTTP request
				_, err = conn.Write([]byte("GET /churn HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
				if err != nil {
					_ = conn.Close()
					tracker.RecordFailure("write_error")
					continue
				}

				// Active client close forces TIME_WAIT to dwell on the client socket side
				_ = conn.Close()

				tracker.RecordSuccess(time.Since(start))
			}
		}()
	}

	wg.Wait()
	return tracker.Snapshot()
}
