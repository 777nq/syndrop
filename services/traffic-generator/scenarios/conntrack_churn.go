package scenarios

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// RunConntrackChurn spawns high-rate ephemeral HTTP/1.1 connections without reuse,
// deliberately saturating the Linux Netfilter nf_conntrack table.
func RunConntrackChurn(ctx context.Context, target string, workers int, duration time.Duration) Summary {
	tracker := NewMetricTracker()

	fmt.Printf("[CONNTRACK-CHURN] Target: %s | Workers: %d | Duration: %v\n", target, workers, duration)
	fmt.Printf("[CONNTRACK-CHURN] Generating unique 5-tuple churn to fill nf_conntrack_max\n")

	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Disabling Keep-Alives forces a brand new TCP handshake and conntrack entry per request
			transport := &http.Transport{
				DisableKeepAlives: true,
				DialContext: (&net.Dialer{
					Timeout: 2 * time.Second,
				}).DialContext,
			}
			client := &http.Client{
				Transport: transport,
				Timeout:   3 * time.Second,
			}

			churnURL := target + "/churn"

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}

				reqStart := time.Now()
				req, err := http.NewRequestWithContext(ctx, "GET", churnURL, nil)
				if err != nil {
					tracker.RecordFailure("request_init_error")
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
					tracker.RecordSuccess(time.Since(reqStart))
				} else {
					tracker.RecordFailure(fmt.Sprintf("status_%d", resp.StatusCode))
				}
			}
		}()
	}

	wg.Wait()
	return tracker.Snapshot()
}
