package scenarios

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// RunSynBurst fires instantaneous high-concurrency connection bursts to overflow the SYN/Accept queues
func RunSynBurst(ctx context.Context, target string, burstSize int, waves int, waveInterval time.Duration) Summary {
	tracker := NewMetricTracker()

	// Strip http:// or https:// to obtain host:port
	hostPort := strings.TrimPrefix(target, "http://")
	hostPort = strings.TrimPrefix(hostPort, "https://")
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
	}

	fmt.Printf("[SYN-BURST] Target: %s | Burst Size: %d conns/wave | Waves: %d | Interval: %v\n",
		hostPort, burstSize, waves, waveInterval)
	fmt.Printf("[SYN-BURST] Inducing kernel listen backlog saturation (somaxconn / tcp_max_syn_backlog)\n")

	for wave := 1; wave <= waves; wave++ {
		select {
		case <-ctx.Done():
			return tracker.Snapshot()
		default:
		}

		fmt.Printf(" >>> Dispathing Burst Wave %d/%d (%d concurrent SYNs)...\n", wave, waves, burstSize)

		var wg sync.WaitGroup
		startWave := time.Now()

		for i := 0; i < burstSize; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dialStart := time.Now()

				// Short dial timeout to detect SYN drop retransmission stall
				d := net.Dialer{
					Timeout: 1500 * time.Millisecond,
				}

				conn, err := d.DialContext(ctx, "tcp", hostPort)
				if err != nil {
					tracker.RecordFailure(categorizeError(err))
					return
				}

				latency := time.Since(dialStart)
				tracker.RecordSuccess(latency)

				// Close connection immediately after handshake completed
				_ = conn.Close()
			}()
		}

		wg.Wait()
		waveDuration := time.Since(startWave)
		fmt.Printf(" <<< Wave %d finished in %v. Resting for %v...\n", wave, waveDuration, waveInterval)

		time.Sleep(waveInterval)
	}

	return tracker.Snapshot()
}
