package scenarios

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// RunHTTP2Starve tests multiplexed streams over constrained connections, demonstrating stream head-of-line blocking
func RunHTTP2Starve(ctx context.Context, target string, concurrentStreams int, duration time.Duration) Summary {
	tracker := NewMetricTracker()

	fmt.Printf("[HTTP2-STARVE] Target: %s | Concurrent Streams: %d | Duration: %v\n", target, concurrentStreams, duration)
	fmt.Printf("[HTTP2-STARVE] Pushing high-density multiplexed streams to expose TCP HoL blocking\n")

	// Configure HTTP/2 transport
	h2Transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}

	client := &http.Client{
		Transport: h2Transport,
		Timeout:   4 * time.Second,
	}

	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)

	for s := 0; s < concurrentStreams; s++ {
		wg.Add(1)
		go func(streamID int) {
			defer wg.Done()
			reqURL := target + "/bench"

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
				if err != nil {
					tracker.RecordFailure("h2_req_err")
					continue
				}

				resp, err := client.Do(req)
				if err != nil {
					tracker.RecordFailure(categorizeError(err))
					continue
				}

				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode == http.StatusOK {
					tracker.RecordSuccess(time.Since(start))
				} else {
					tracker.RecordFailure(fmt.Sprintf("h2_status_%d", resp.StatusCode))
				}
			}
		}(s)
	}

	wg.Wait()
	return tracker.Snapshot()
}
