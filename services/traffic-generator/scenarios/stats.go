package scenarios

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MetricTracker records low-overhead latency distributions and error categories
type MetricTracker struct {
	mu           sync.Mutex
	SuccessCount uint64
	FailureCount uint64
	ErrorMap     map[string]uint64
	latencies    []time.Duration
	StartTime    time.Time
}

func NewMetricTracker() *MetricTracker {
	return &MetricTracker{
		ErrorMap:  make(map[string]uint64),
		latencies: make([]time.Duration, 0, 100000),
		StartTime: time.Now(),
	}
}

func (m *MetricTracker) RecordSuccess(d time.Duration) {
	atomic.AddUint64(&m.SuccessCount, 1)
	m.mu.Lock()
	if len(m.latencies) < 500000 {
		m.latencies = append(m.latencies, d)
	}
	m.mu.Unlock()
}

func (m *MetricTracker) RecordFailure(reason string) {
	atomic.AddUint64(&m.FailureCount, 1)
	m.mu.Lock()
	m.ErrorMap[reason]++
	m.mu.Unlock()
}

type Summary struct {
	Duration   time.Duration
	TotalReqs  uint64
	RPS        float64
	Successes  uint64
	Failures   uint64
	P50        time.Duration
	P95        time.Duration
	P99        time.Duration
	P999       time.Duration
	ErrorBreak map[string]uint64
}

func (m *MetricTracker) Snapshot() Summary {
	m.mu.Lock()
	defer m.mu.Unlock()

	dur := time.Since(m.StartTime)
	total := m.SuccessCount + m.FailureCount
	var rps float64
	if dur.Seconds() > 0 {
		rps = float64(total) / dur.Seconds()
	}

	errCopy := make(map[string]uint64)
	for k, v := range m.ErrorMap {
		errCopy[k] = v
	}

	if len(m.latencies) == 0 {
		return Summary{
			Duration:   dur,
			TotalReqs:  total,
			RPS:        rps,
			Successes:  m.SuccessCount,
			Failures:   m.FailureCount,
			ErrorBreak: errCopy,
		}
	}

	sorted := make([]time.Duration, len(m.latencies))
	copy(sorted, m.latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 := sorted[int(float64(len(sorted))*0.50)]
	p95 := sorted[int(float64(len(sorted))*0.95)]
	p99 := sorted[int(float64(len(sorted))*0.99)]
	p999 := sorted[int(float64(len(sorted))*0.999)]

	return Summary{
		Duration:   dur,
		TotalReqs:  total,
		RPS:        rps,
		Successes:  m.SuccessCount,
		Failures:   m.FailureCount,
		P50:        p50,
		P95:        p95,
		P99:        p99,
		P999:       p999,
		ErrorBreak: errCopy,
	}
}

func (s Summary) Display(title string) {
	fmt.Println("===============================================================")
	fmt.Printf(" [SCENARIO RESULTS] %s\n", title)
	fmt.Printf(" Duration:        %v\n", s.Duration.Round(time.Millisecond))
	fmt.Printf(" Total Requests:  %d (%.2f req/s)\n", s.TotalReqs, s.RPS)
	fmt.Printf(" Successful:      %d\n", s.Successes)
	fmt.Printf(" Failed/Dropped:  %d\n", s.Failures)
	if s.TotalReqs > 0 {
		fmt.Printf(" Failure Ratio:   %.2f%%\n", (float64(s.Failures)/float64(s.TotalReqs))*100.0)
	}
	fmt.Printf(" Latency p50:     %v\n", s.P50)
	fmt.Printf(" Latency p95:     %v\n", s.P95)
	fmt.Printf(" Latency p99:     %v\n", s.P99)
	fmt.Printf(" Latency p99.9:   %v\n", s.P999)

	if len(s.ErrorBreak) > 0 {
		fmt.Println(" --- Kernel & Network Seam Error Breakdown ---")
		for errStr, count := range s.ErrorBreak {
			fmt.Printf("   * [%s]: %d occurrences\n", errStr, count)
		}
	}
	fmt.Println("===============================================================")
}
