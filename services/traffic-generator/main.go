package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ephemeral-collapse/traffic-generator/scenarios"
)

func main() {
	target := flag.String("target", "http://127.0.0.1:8080", "Target microservice URL or host:port")
	scenario := flag.String("scenario", "bench", "Scenario to execute: bench, syn-burst, conntrack-churn, time-wait, slowloris, http2-starve, all")
	concurrency := flag.Int("concurrency", 128, "Number of concurrent workers/connections")
	duration := flag.Duration("duration", 15*time.Second, "Duration for sustained scenarios")
	burstSize := flag.Int("burst-size", 2048, "Number of concurrent connections per wave in syn-burst")
	waves := flag.Int("waves", 4, "Number of waves in syn-burst")
	waveInterval := flag.Duration("wave-interval", 2*time.Second, "Interval between burst waves")

	flag.Parse()

	// Environment variable overrides for Docker Compose
	if envTarget := os.Getenv("TARGET_URL"); envTarget != "" {
		*target = envTarget
	}
	if envScenario := os.Getenv("SCENARIO"); envScenario != "" {
		*scenario = envScenario
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n[!] Received interruption signal, terminating scenario gracefully...")
		cancel()
	}()

	printBanner(*scenario, *target)

	switch strings.ToLower(*scenario) {
	case "bench":
		summary := scenarios.RunBench(ctx, *target, *concurrency, *duration)
		summary.Display("Sustained High-Throughput Baseline (Keep-Alive)")

	case "syn-burst", "syn":
		summary := scenarios.RunSynBurst(ctx, *target, *burstSize, *waves, *waveInterval)
		summary.Display("Listen Backlog Queue Saturation (somaxconn & tcp_max_syn_backlog)")

	case "conntrack-churn", "conntrack":
		summary := scenarios.RunConntrackChurn(ctx, *target, *concurrency, *duration)
		summary.Display("Netfilter nf_conntrack Table Saturation (5-Tuple Churn)")

	case "time-wait", "timewait":
		summary := scenarios.RunTimeWaitOverflow(ctx, *target, *concurrency, *duration)
		summary.Display("TCP TIME_WAIT Bucket Overflow & Port Exhaustion")

	case "slowloris", "starvation":
		summary := scenarios.RunSlowlorisHold(ctx, *target, *concurrency, *duration)
		summary.Display("Slowloris Socket Pinning & Epoll Event Churn")

	case "http2-starve", "h2":
		summary := scenarios.RunHTTP2Starve(ctx, *target, *concurrency, *duration)
		summary.Display("HTTP/2 Stream Multiplexing & Head-of-Line Blocking")

	case "all":
		fmt.Println(">>> Executing Comprehensive Failure Suite: All Scenarios Sequentially <<<")
		fmt.Println("\n--- Scenario 1/5: Sustained Baseline Bench ---")
		scenarios.RunBench(ctx, *target, *concurrency, 10*time.Second).Display("Baseline Bench")

		fmt.Println("\n--- Scenario 2/5: SYN Listen Backlog Burst ---")
		scenarios.RunSynBurst(ctx, *target, *burstSize, 3, 2*time.Second).Display("SYN Burst")

		fmt.Println("\n--- Scenario 3/5: Netfilter Conntrack Churn ---")
		scenarios.RunConntrackChurn(ctx, *target, *concurrency, 10*time.Second).Display("Conntrack Churn")

		fmt.Println("\n--- Scenario 4/5: TIME_WAIT Bucket Overflow ---")
		scenarios.RunTimeWaitOverflow(ctx, *target, *concurrency, 10*time.Second).Display("TIME_WAIT Churn")

		fmt.Println("\n--- Scenario 5/5: Slowloris Socket Buffer Pinning ---")
		scenarios.RunSlowlorisHold(ctx, *target, *concurrency, 10*time.Second).Display("Slowloris Pinning")

	default:
		fmt.Printf("Unknown scenario: '%s'. Choose from: bench, syn-burst, conntrack-churn, time-wait, slowloris, http2-starve, all\n", *scenario)
		os.Exit(1)
	}
}

func printBanner(scenario, target string) {
	fmt.Println("===============================================================")
	fmt.Println(" EPHEMERAL-COLLAPSE: KERNEL-SEAM REPRODUCIBLE FAILURE GENERATOR")
	fmt.Printf(" Active Target:   %s\n", target)
	fmt.Printf(" Target Scenario: %s\n", strings.ToUpper(scenario))
	fmt.Printf(" Timestamp:       %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Println("===============================================================")
}
