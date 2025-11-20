package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rom8726/chaoskit"
	"github.com/rom8726/chaoskit/injectors"
	"github.com/rom8726/chaoskit/validators"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("=== Floxy Stress Test with ChaosKit ===")
	log.Println("Testing Floxy workflow engine with advanced chaos injection")
	log.Println()

	repeatStr := os.Getenv("REPEAT")
	repeat := 100
	if repeatStr != "" {
		var err error
		repeat, err = strconv.Atoi(repeatStr)
		if err != nil {
			log.Fatalf("Failed to parse REPEAT environment variable: %v", err)
		}
	}

	// Database connection configuration
	// Can use direct connection or via ToxiProxy
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5432"
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "floxy"
	}
	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "floxy"
	}
	dbPassword := os.Getenv("DB_PASSWORD")

	directConnString := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", dbUser, dbPassword, dbHost, dbPort, dbName)
	connString := directConnString

	useToxiProxy := os.Getenv("USE_TOXIPROXY") == "true"
	toxiproxyHost := os.Getenv("TOXIPROXY_HOST")
	if toxiproxyHost == "" {
		toxiproxyHost = "localhost:8474"
	}

	// Setup ToxiProxy client for managing toxins (latency, bandwidth, timeout)
	// Note: Proxy is already configured via toxiproxy.json, we just use it for connection
	var toxiproxyClient *injectors.ToxiProxyClient
	if useToxiProxy {
		log.Println("[Setup] Initializing ToxiProxy client for database connection chaos...")
		toxiproxyClient = injectors.NewToxiProxyClient(toxiproxyHost)
		log.Printf("[Setup] Using ToxiProxy for database connections: %s:%s (proxy configured via toxiproxy.json)", dbHost, dbPort)
		connString = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", dbUser, dbPassword, "floxy-stress-toxiproxy", 6432, dbName)
	} else {
		log.Printf("[Setup] Connecting directly to PostgreSQL: %s:%s", dbHost, dbPort)
	}

	// Create a Floxy target
	log.Println("[Setup] Creating Floxy target...")
	floxyTarget, err := NewFloxyStressTarget(directConnString, connString)
	if err != nil {
		log.Fatalf("Failed to create Floxy target: %v", err)
	}

	// Register workflows
	log.Println("[Setup] Registering workflows...")
	if err := RegisterWorkflows(directConnString); err != nil {
		log.Fatalf("Failed to register workflows: %v", err)
	}

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a direct database pool for validators (bypasses ToxiProxy)
	log.Println("[Setup] Creating direct database pool for validators...")
	validatorPool, err := pgxpool.New(ctx, directConnString)
	if err != nil {
		log.Fatalf("Failed to create validator pool: %v", err)
	}
	defer validatorPool.Close()

	// Statistics reporter
	//go func() {
	//	ticker := time.NewTicker(10 * time.Second)
	//	defer ticker.Stop()
	//
	//	for {
	//		select {
	//		case <-ctx.Done():
	//			return
	//		case <-ticker.C:
	//			stats := floxyTarget.GetStats()
	//			log.Printf("[Stats] %+v", stats)
	//		}
	//	}
	//}()

	// Build injectors
	log.Println("[Setup] Creating chaos injectors...")

	// 1. Monkey patching injectors for handlers
	// Note: Must pass pointer to function for monkey patching
	//monkeyPanicTargets := []injectors.PatchTarget{
	//	{
	//		Func:        &processPaymentHandler,
	//		Probability: 0.05, // 5% chance of panic
	//		FuncName:    "processPaymentHandler",
	//	},
	//	{
	//		Func:        &processInventoryHandler,
	//		Probability: 0.05,
	//		FuncName:    "processInventoryHandler",
	//	},
	//	{
	//		Func:        &processShippingHandler,
	//		Probability: 0.05,
	//		FuncName:    "processShippingHandler",
	//	},
	//}
	//
	//monkeyPanicInjector := injectors.MonkeyPatchPanic(monkeyPanicTargets)
	//
	//monkeyDelayTargets := []injectors.DelayPatchTarget{
	//	{
	//		Func:        &processPaymentHandler,
	//		MinDelay:    50 * time.Millisecond,
	//		MaxDelay:    200 * time.Millisecond,
	//		Probability: 0.2, // 20% chance of delay
	//		DelayBefore: true,
	//		FuncName:    "processPaymentHandler",
	//	},
	//	{
	//		Func:        &processInventoryHandler,
	//		MinDelay:    30 * time.Millisecond,
	//		MaxDelay:    150 * time.Millisecond,
	//		Probability: 0.15,
	//		DelayBefore: true,
	//		FuncName:    "processInventoryHandler",
	//	},
	//}
	//
	//monkeyDelayInjector := injectors.MonkeyPatchDelay(monkeyDelayTargets)

	// 2. Failpoint injector (requires build with -tags failpoint)
	failpointNames := []string{
		"payment-handler-panic",
		"inventory-handler-panic",
		"shipping-handler-panic",
	}
	failpointInjector := injectors.FailpointPanic(failpointNames, 0.03, 500*time.Millisecond)

	// 3. ToxiProxy injectors for database network chaos
	var latencyInjector *injectors.ToxiProxyLatencyInjector
	var bandwidthInjector *injectors.ToxiProxyBandwidthInjector
	var timeoutInjector *injectors.ToxiProxyTimeoutInjector

	if useToxiProxy && toxiproxyClient != nil {
		// Latency for database calls
		// Proxy name "pg" matches the name in toxiproxy.json
		latencyInjector = injectors.ToxiProxyLatency(
			toxiproxyClient,
			"pg",
			100*time.Millisecond, // 100ms latency
			20*time.Millisecond,  // ±20ms jitter
		)

		// Bandwidth limiting (simulates slow network)
		bandwidthInjector = injectors.ToxiProxyBandwidth(
			toxiproxyClient,
			"pg",
			500, // 500 KB/s limit
		)

		// Connection timeouts
		timeoutInjector = injectors.ToxiProxyTimeout(
			toxiproxyClient,
			"pg",
			2*time.Second, // 2 second timeout
		)
	}

	// Create ChaosKit scenario
	log.Println("[Setup] Building chaos scenario...")
	scenarioBuilder := chaoskit.NewScenario("floxy-stress-test").
		WithTarget(floxyTarget).
		Step("execute-workflow", RunFloxyWorkflow).
		Inject("context-delay", injectors.RandomDelayWithProbability(time.Millisecond*20, time.Millisecond*100, 0.15)).
		//Inject("context-panic", injectors.PanicProbability(0.15)).
		Inject("context-error", injectors.ErrorWithProbability("chaos error", 0.35))
	//// Monkey patching injectors (always active)
	//Inject("monkey-panic", monkeyPanicInjector).
	//Inject("monkey-delay", monkeyDelayInjector)

	// Failpoint injector (may fail if not built with -tags failpoint)
	if err := failpointInjector.Inject(ctx); err != nil {
		if errors.Is(err, injectors.ErrFailpointDisabled) {
			log.Println("[Warning] Failpoint injector disabled (build with -tags failpoint to enable)")
		} else {
			log.Printf("[Warning] Failpoint injector error: %v", err)
		}
	} else {
		log.Println("[Setup] Failpoint injector enabled")
		scenarioBuilder = scenarioBuilder.Inject("failpoint-panic", failpointInjector)
	}

	// ToxiProxy injectors (conditional)
	if useToxiProxy && latencyInjector != nil {
		scenarioBuilder = scenarioBuilder.
			Inject("db-latency", latencyInjector).
			Inject("db-bandwidth", bandwidthInjector).
			Inject("db-timeout", timeoutInjector)
		log.Println("[Setup] ToxiProxy injectors enabled")
	}

	scenario := scenarioBuilder.
		// Validators
		Assert("recursion-depth", validators.RecursionDepthLimit(10)).
		Assert("goroutine-leak", validators.GoroutineLimit(100)).
		Assert("no-slow-iteration", validators.NoSlowIteration(5*time.Second)).
		Assert("no-infinite-loops", validators.NoInfiniteLoop(10*time.Second)).
		// Database consistency validators (use direct connection pool, bypassing ToxiProxy)
		Assert("queue-empty", NewQueueEmptyValidator(validatorPool)).
		Assert("completed-instances-consistency", NewCompletedInstancesValidator(validatorPool)).
		Assert("failed-instances-consistency", NewFailedInstancesValidator(validatorPool)).
		Repeat(repeat).
		Build()

	// Create executor with ContinueOnFailure policy
	executor := chaoskit.NewExecutor(
		chaoskit.WithFailurePolicy(chaoskit.ContinueOnFailure),
	)

	// Run in the background
	log.Println("[Main] Starting chaos scenario...")

	go func() {
		// Wait for a signal
		<-sigCh
		log.Println("\n[Main] Shutting down...")
		cancel()
	}()

	if err := executor.Run(ctx, scenario); err != nil {
		log.Printf("Scenario error: %v", err)
	}

	// Stop injectors
	log.Println("[Cleanup] Stopping injectors...")

	// Wait a bit for cleanup
	time.Sleep(2 * time.Second)

	// Get verdict and generate a report
	thresholds := chaoskit.DefaultThresholds()
	report, err := executor.Reporter().GetVerdict(thresholds)
	if err != nil {
		log.Fatalf("Failed to generate report: %v", err)
	}

	// Print detailed report
	log.Println("\n=== Final Chaos Test Report ===")
	log.Println(executor.Reporter().GenerateTextReport(report))

	// Print Floxy stats
	stats := floxyTarget.GetStats()
	log.Printf("\n=== Floxy Statistics ===")
	log.Printf("Total workflows: %d", stats["total_workflows"])
	log.Printf("Successful runs: %d", stats["successful_runs"])
	log.Printf("Failed runs: %d", stats["failed_runs"])
	log.Printf("Rollback count: %d", stats["rollback_count"])
	log.Printf("Max rollback depth: %d", stats["max_rollback_depth"])
	log.Printf("Metrics: %+v", stats["metrics"])

	log.Println("\n=== Final Verdict ===")
	log.Printf(report.Verdict.String())

	// Exit with verdict code
	//os.Exit(report.Verdict.ExitCode())
}

// createProxyIfNotExists creates a proxy via ToxiProxy API if it doesn't exist
func createProxyIfNotExists(toxiproxyAPIHost string, proxyName string, listenAddr string, upstreamAddr string) error {
	// Check if proxy already exists
	apiURL := fmt.Sprintf("http://%s/proxies/%s", toxiproxyAPIHost, proxyName)
	resp, err := http.Get(apiURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		log.Printf("[Setup] Proxy %s already exists", proxyName)

		return nil
	}

	// Create proxy
	createURL := fmt.Sprintf("http://%s/proxies", toxiproxyAPIHost)
	proxyConfig := map[string]interface{}{
		"name":     proxyName,
		"listen":   listenAddr,
		"upstream": upstreamAddr,
		"enabled":  true,
	}

	jsonData, err := json.Marshal(proxyConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal proxy config: %w", err)
	}

	req, err := http.NewRequest("POST", createURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create proxy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create proxy: status %d, body: %s", resp.StatusCode, string(body))
	}

	log.Printf("[Setup] Created proxy %s: %s -> %s", proxyName, listenAddr, upstreamAddr)

	return nil
}

// waitForProxyReady waits for the proxy to be ready to accept connections
// It checks both TCP connection and ToxiProxy API to ensure proxy is configured
func waitForProxyReady(proxyHost string, proxyPort int, toxiproxyAPIHost string, proxyName string, maxRetries int, retryInterval time.Duration) error {
	address := net.JoinHostPort(proxyHost, fmt.Sprintf("%d", proxyPort))

	for i := 0; i < maxRetries; i++ {
		// First check if proxy exists in ToxiProxy API
		if toxiproxyAPIHost != "" && proxyName != "" {
			apiURL := fmt.Sprintf("http://%s/proxies/%s", toxiproxyAPIHost, proxyName)
			resp, err := http.Get(apiURL)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()

					// Check if proxy is enabled
					var proxyInfo map[string]interface{}
					if json.Unmarshal(body, &proxyInfo) == nil {
						if enabled, ok := proxyInfo["enabled"].(bool); ok && enabled {
							// Proxy exists and is enabled, now check TCP connection
							conn, err := net.DialTimeout("tcp", address, 2*time.Second)
							if err == nil {
								_ = conn.Close()
								log.Printf("[Setup] Proxy %s is ready (verified via API and TCP)", address)

								return nil
							}
						}
					}
				}
			}
		} else {
			// Fallback to TCP-only check if API info not available
			conn, err := net.DialTimeout("tcp", address, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				log.Printf("[Setup] Proxy %s is ready", address)

				return nil
			}
		}

		if i < maxRetries-1 {
			log.Printf("[Setup] Waiting for proxy %s to be ready (attempt %d/%d)...", address, i+1, maxRetries)
			time.Sleep(retryInterval)
		}
	}

	return fmt.Errorf("proxy %s not ready after %d attempts", address, maxRetries)
}
