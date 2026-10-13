package scenarios

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RunBench executes a high-throughput keep-alive benchmark against the target
func RunBench(ctx context.Context, target string, concurrency int, duration time.Duration) Summary {
	tracker := NewMetricTracker()

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

	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
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
					tracker.RecordFailure("request_creation_err")
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
					tracker.RecordFailure("status_non_200")
				}
			}
		}()
	}

	wg.Wait()
	return tracker.Snapshot()
}

func categorizeError(err error) string {
	if err == nil {
		return "none"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return "ECONNREFUSED (Listen Backlog Full or Port Unreachable)"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "i/o timeout"):
		return "ETIMEDOUT (SYN Retransmission / Blackholed Packet)"
	case strings.Contains(msg, "reset by peer"), strings.Contains(msg, "connection reset"):
		return "ECONNRESET (TCP RST on Backlog Overflow)"
	case strings.Contains(msg, "cannot assign requested address"):
		return "EADDRNOTAVAIL (Ephemeral Port Exhaustion)"
	case strings.Contains(msg, "no buffer space"):
		return "ENOBUFS (Kernel Socket Memory Exhaustion)"
	case strings.Contains(msg, "too many open files"):
		return "EMFILE (File Descriptor Exhaustion)"
	default:
		return "OTHER: " + err.Error()
	}
}
