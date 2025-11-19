package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rom8726/floxy"
	"github.com/rom8726/floxy/plugins/engine/metrics"

	"github.com/rom8726/chaoskit"
	"github.com/rom8726/chaoskit/injectors"
	"github.com/rom8726/chaoskit/validators"

	rolldepth "github.com/rom8726/floxy/plugins/engine/rollback-depth"
)

// failpoint.Inject("payment-handler-panic", func() { panic("gofail: payment handler panic") })

// FloxyStressTarget wraps Floxy engine as a ChaosKit target
type FloxyStressTarget struct {
	pool              *pgxpool.Pool
	engine            *floxy.Engine
	rollbackPlugin    *rolldepth.RollbackDepthPlugin
	metricsCollector  *FloxyMetricsCollector
	workflowInstances atomic.Int64
	successfulRuns    atomic.Int64
	failedRuns        atomic.Int64
	rollbackCount     atomic.Int64
	maxRollbackDepth  atomic.Int32
	mu                sync.RWMutex
	workers           []context.CancelFunc
}

func RegisterWorkflows(connString string) error {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return fmt.Errorf("failed to create pool: %w", err)
	}
	defer pool.Close()

	engine := floxy.NewEngine(pool,
		floxy.WithMissingHandlerCooldown(50*time.Millisecond),
		floxy.WithQueueAgingEnabled(true),
		floxy.WithQueueAgingRate(0.5),
	)

	target := &FloxyStressTarget{
		pool:   pool,
		engine: engine,
	}

	// Register workflows
	if err := target.registerWorkflows(ctx); err != nil {
		return fmt.Errorf("failed to register workflows: %w", err)
	}

	return nil
}

// createProxyIfNotExists creates a proxy via ToxiProxy API if it doesn't exist
func createProxyIfNotExists(toxiproxyAPIHost string, proxyName string, listenAddr string, upstreamAddr string) error {
	// Check if proxy already exists
	apiURL := fmt.Sprintf("http://%s/proxies/%s", toxiproxyAPIHost, proxyName)
	resp, err := http.Get(apiURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
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
	defer resp.Body.Close()

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
					resp.Body.Close()

					// Check if proxy is enabled
					var proxyInfo map[string]interface{}
					if json.Unmarshal(body, &proxyInfo) == nil {
						if enabled, ok := proxyInfo["enabled"].(bool); ok && enabled {
							// Proxy exists and is enabled, now check TCP connection
							conn, err := net.DialTimeout("tcp", address, 2*time.Second)
							if err == nil {
								conn.Close()
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
				conn.Close()
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

func NewFloxyStressTarget(connString, proxyConnString string) (*FloxyStressTarget, error) {
	ctx := context.Background()

	poolMigrations, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("failed to create pool: %w", err)
	}

	if err := floxy.RunMigrations(ctx, poolMigrations); err != nil {
		poolMigrations.Close()

		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}
	poolMigrations.Close()

	// Wait for proxy to be ready before connecting
	// This will be called from main() with toxiproxy API info

	pool, err := pgxpool.New(ctx, proxyConnString)
	if err != nil {
		return nil, fmt.Errorf("failed to create pool: %w", err)
	}

	engine := floxy.NewEngine(pool,
		floxy.WithMissingHandlerCooldown(50*time.Millisecond),
		floxy.WithQueueAgingEnabled(true),
		floxy.WithQueueAgingRate(0.5),
	)

	// Register plugins
	rollbackPlugin := rolldepth.New()
	engine.RegisterPlugin(rollbackPlugin)

	metricsCollector := NewFloxyMetricsCollector()
	metricsPlugin := metrics.New(metricsCollector)
	engine.RegisterPlugin(metricsPlugin)

	target := &FloxyStressTarget{
		pool:             pool,
		engine:           engine,
		rollbackPlugin:   rollbackPlugin,
		metricsCollector: metricsCollector,
	}

	// Register handlers
	target.registerHandlers()

	return target, nil
}

func (t *FloxyStressTarget) Name() string {
	return "floxy-stress-test"
}

func (t *FloxyStressTarget) Setup(ctx context.Context) error {
	log.Println("[Floxy] Setting up stress test target...")

	// Start worker pool
	workerCount := 5
	t.mu.Lock()
	t.workers = make([]context.CancelFunc, workerCount)
	for i := 0; i < workerCount; i++ {
		workerCtx, cancel := context.WithCancel(ctx)
		t.workers[i] = cancel

		go t.worker(workerCtx, fmt.Sprintf("worker-%d", i))
	}
	t.mu.Unlock()

	log.Printf("[Floxy] Started %d workers", workerCount)

	return nil
}

func (t *FloxyStressTarget) Teardown(ctx context.Context) error {
	log.Println("[Floxy] Tearing down stress test target...")

	// Stop workers
	t.mu.Lock()
	for _, cancel := range t.workers {
		cancel()
	}
	t.mu.Unlock()

	// Wait a bit for workers to finish
	time.Sleep(500 * time.Millisecond)

	// Shutdown engine
	t.engine.Shutdown()

	// Close pool
	t.pool.Close()

	// Print final stats
	stats := t.GetStats()
	log.Printf("[Floxy] Final stats: %+v", stats)

	return nil
}

func (t *FloxyStressTarget) worker(ctx context.Context, workerID string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			empty, err := t.engine.ExecuteNext(ctx, workerID)
			if err != nil {
				log.Printf("[Floxy] Worker %s error: %v", workerID, err)
				time.Sleep(100 * time.Millisecond)

				continue
			}
			if empty {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

func (t *FloxyStressTarget) registerHandlers() {
	handlers := []floxy.StepHandler{
		&PaymentHandler{target: t},
		&InventoryHandler{target: t},
		&ShippingHandler{target: t},
		&NotificationHandler{target: t},
		&CompensationHandler{target: t},
		&ValidationHandler{target: t},
	}

	for _, h := range handlers {
		t.engine.RegisterHandler(h)
	}
}

func (t *FloxyStressTarget) registerWorkflows(ctx context.Context) error {
	workflows := []struct {
		name    string
		builder func() (*floxy.WorkflowDefinition, error)
	}{
		{"simple-order", t.buildSimpleOrderWorkflow},
		{"complex-order", t.buildComplexOrderWorkflow},
		{"parallel-processing", t.buildParallelProcessingWorkflow},
		{"nested-workflow", t.buildNestedWorkflow},
	}

	for _, wf := range workflows {
		workflow, err := wf.builder()
		if err != nil {
			return fmt.Errorf("failed to build %s: %w", wf.name, err)
		}
		if err := t.engine.RegisterWorkflow(ctx, workflow); err != nil {
			return fmt.Errorf("failed to register %s: %w", wf.name, err)
		}
	}

	return nil
}

func (t *FloxyStressTarget) buildSimpleOrderWorkflow() (*floxy.WorkflowDefinition, error) {
	return floxy.NewBuilder("simple-order", 1).
		Step("validate", "validation", floxy.WithStepMaxRetries(2)).
		Then("process-payment", "payment", floxy.WithStepMaxRetries(3)).
		OnFailure("refund-payment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "refund"})).
		Then("reserve-inventory", "inventory", floxy.WithStepMaxRetries(2)).
		OnFailure("release-inventory", "compensation", floxy.WithStepMetadata(map[string]any{"action": "release"})).
		Then("ship", "shipping").
		OnFailure("cancel-shipment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "cancel"})).
		Then("notify", "notification").
		Build()
}

func (t *FloxyStressTarget) buildComplexOrderWorkflow() (*floxy.WorkflowDefinition, error) {
	return floxy.NewBuilder("complex-order", 1).
		Step("validate", "validation", floxy.WithStepMaxRetries(2)).
		SavePoint("validation-checkpoint").
		Then("process-payment", "payment", floxy.WithStepMaxRetries(3)).
		OnFailure("refund-payment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "refund"})).
		SavePoint("payment-checkpoint").
		Then("reserve-inventory", "inventory", floxy.WithStepMaxRetries(2)).
		OnFailure("release-inventory", "compensation", floxy.WithStepMetadata(map[string]any{"action": "release"})).
		Then("ship", "shipping").
		OnFailure("cancel-shipment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "cancel"})).
		Then("notify", "notification").
		Build()
}

func (t *FloxyStressTarget) buildParallelProcessingWorkflow() (*floxy.WorkflowDefinition, error) {
	return floxy.NewBuilder("parallel-processing", 1).
		Step("validate", "validation").
		Fork("parallel-tasks",
			func(b *floxy.Builder) {
				b.Step("process-payment", "payment", floxy.WithStepMaxRetries(2)).
					OnFailure("refund-payment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "refund"}))
			},
			func(b *floxy.Builder) {
				b.Step("reserve-inventory", "inventory", floxy.WithStepMaxRetries(2)).
					OnFailure("release-inventory", "compensation", floxy.WithStepMetadata(map[string]any{"action": "release"}))
			},
		).
		Join("join", floxy.JoinStrategyAll).
		Then("ship", "shipping").
		OnFailure("cancel-shipment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "cancel"})).
		Build()
}

func (t *FloxyStressTarget) buildNestedWorkflow() (*floxy.WorkflowDefinition, error) {
	return floxy.NewBuilder("nested-workflow", 1).
		Step("validate", "validation").
		Then("process-payment", "payment", floxy.WithStepMaxRetries(3)).
		OnFailure("refund-payment", "compensation", floxy.WithStepMetadata(map[string]any{"action": "refund"})).
		SavePoint("payment-checkpoint").
		Fork("nested-parallel",
			func(b *floxy.Builder) {
				b.Step("reserve-inventory-1", "inventory").
					OnFailure("release-inventory-1", "compensation", floxy.WithStepMetadata(map[string]any{"action": "release"})).
					Then("ship-1", "shipping").
					OnFailure("cancel-shipment-1", "compensation", floxy.WithStepMetadata(map[string]any{"action": "cancel"}))
			},
			func(b *floxy.Builder) {
				b.Step("reserve-inventory-2", "inventory").
					OnFailure("release-inventory-2", "compensation", floxy.WithStepMetadata(map[string]any{"action": "release"})).
					Then("ship-2", "shipping").
					OnFailure("cancel-shipment-2", "compensation", floxy.WithStepMetadata(map[string]any{"action": "cancel"}))
			},
		).
		Join("join", floxy.JoinStrategyAll).
		Then("notify", "notification").
		Build()
}

func (t *FloxyStressTarget) ExecuteRandomWorkflow(ctx context.Context) error {
	workflows := []string{
		"simple-order-v1",
		"complex-order-v1",
		"parallel-processing-v1",
		"nested-workflow-v1",
	}

	workflowID := workflows[rand.Intn(len(workflows))]

	// Create random order data
	order := map[string]any{
		"order_id": fmt.Sprintf("ORD-%d", time.Now().UnixNano()),
		"user_id":  fmt.Sprintf("user-%d", rand.Intn(1000)),
		"amount":   float64(rand.Intn(500) + 50),
		"items":    rand.Intn(10) + 1,
		// Random failure injection
		"should_fail": rand.Float64() < 0.15, // 15% chance of failure
	}

	input, _ := json.Marshal(order)

	instanceID, err := t.engine.Start(ctx, workflowID, input)
	if err != nil {
		return fmt.Errorf("failed to start workflow: %w", err)
	}

	t.workflowInstances.Add(1)

	// Track rollback depth for this instance
	chaoskit.RecordRecursionDepth(ctx, 0)

	// Wait for completion with timeout
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := t.engine.GetStatus(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		// Check rollback depth
		depth := t.rollbackPlugin.GetMaxDepth(instanceID)
		if depth > 0 {
			chaoskit.RecordRecursionDepth(ctx, depth)
			currentMax := t.maxRollbackDepth.Load()
			if int32(depth) > currentMax {
				t.maxRollbackDepth.Store(int32(depth))
			}
		}

		if status == floxy.StatusCompleted {
			t.successfulRuns.Add(1)
			t.rollbackPlugin.ResetMaxDepth(instanceID)

			return nil
		}

		if status == floxy.StatusFailed {
			t.failedRuns.Add(1)
			if depth > 0 {
				t.rollbackCount.Add(1)
			}
			t.rollbackPlugin.ResetMaxDepth(instanceID)

			return fmt.Errorf("workflow failed")
		}

		if status == floxy.StatusAborted || status == floxy.StatusCancelled {
			t.failedRuns.Add(1)
			t.rollbackPlugin.ResetMaxDepth(instanceID)

			return fmt.Errorf("workflow %s", status)
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("workflow timeout")
}

func (t *FloxyStressTarget) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"total_workflows":    t.workflowInstances.Load(),
		"successful_runs":    t.successfulRuns.Load(),
		"failed_runs":        t.failedRuns.Load(),
		"rollback_count":     t.rollbackCount.Load(),
		"max_rollback_depth": t.maxRollbackDepth.Load(),
		"metrics":            t.metricsCollector.GetStats(),
	}
}

// Step handlers implementation
// These handlers are targets for monkey patching and gofail injection

// PaymentHandler processes payment
// Can be patched with monkey patching injectors
// To enable gofail, uncomment failpoint calls and build with: go build -tags failpoint -gcflags=all=-l
var processPaymentHandler = func(ctx context.Context, order map[string]any) error {
	// Gofail failpoint (requires build with -tags failpoint)
	// Uncomment to enable:
	// import "github.com/pingcap/failpoint"
	// failpoint.Inject("payment-handler-panic", func() { panic("gofail: payment handler panic") })

	// Simulate processing time
	time.Sleep(time.Millisecond * time.Duration(10+rand.Intn(20)))

	// Check for forced failure
	if shouldFail, ok := order["should_fail"].(bool); ok && shouldFail && rand.Float64() < 0.3 {
		return fmt.Errorf("payment processing failed")
	}

	order["payment_status"] = "paid"

	return nil
}

type PaymentHandler struct{ target *FloxyStressTarget }

func (h *PaymentHandler) Name() string { return "payment" }
func (h *PaymentHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	var order map[string]any
	if err := json.Unmarshal(input, &order); err != nil {
		return nil, err
	}

	// Call a processable function (can be monkey patched)
	if err := processPaymentHandler(ctx, order); err != nil {
		return nil, err
	}

	return json.Marshal(order)
}

// InventoryHandler processes inventory reservation
// To enable gofail, uncomment failpoint calls and build with: go build -tags failpoint -gcflags=all=-l
var processInventoryHandler = func(ctx context.Context, order map[string]any) error {
	// Gofail failpoint (requires build with -tags failpoint)
	// Uncomment to enable:
	// import "github.com/pingcap/failpoint"
	// failpoint.Inject("inventory-handler-panic", func() { panic("gofail: inventory handler panic") })

	time.Sleep(time.Millisecond * time.Duration(5+rand.Intn(15)))

	if shouldFail, ok := order["should_fail"].(bool); ok && shouldFail && rand.Float64() < 0.2 {
		return fmt.Errorf("inventory reservation failed")
	}

	order["inventory_status"] = "reserved"

	return nil
}

type InventoryHandler struct{ target *FloxyStressTarget }

func (h *InventoryHandler) Name() string { return "inventory" }
func (h *InventoryHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	var order map[string]any
	if err := json.Unmarshal(input, &order); err != nil {
		return nil, err
	}

	if err := processInventoryHandler(ctx, order); err != nil {
		return nil, err
	}

	return json.Marshal(order)
}

// ShippingHandler processes shipping
// To enable gofail, uncomment failpoint calls and build with: go build -tags failpoint -gcflags=all=-l
var processShippingHandler = func(ctx context.Context, order map[string]any) error {
	// Gofail failpoint (requires build with -tags failpoint)
	// Uncomment to enable:
	// import "github.com/pingcap/failpoint"
	// failpoint.Inject("shipping-handler-panic", func() { panic("gofail: shipping handler panic") })

	time.Sleep(time.Millisecond * time.Duration(10+rand.Intn(20)))

	if shouldFail, ok := order["should_fail"].(bool); ok && shouldFail && rand.Float64() < 0.1 {
		return fmt.Errorf("shipping failed")
	}

	order["shipping_status"] = "shipped"

	return nil
}

type ShippingHandler struct{ target *FloxyStressTarget }

func (h *ShippingHandler) Name() string { return "shipping" }
func (h *ShippingHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	var order map[string]any
	if err := json.Unmarshal(input, &order); err != nil {
		return nil, err
	}

	if err := processShippingHandler(ctx, order); err != nil {
		return nil, err
	}

	return json.Marshal(order)
}

type NotificationHandler struct{ target *FloxyStressTarget }

func (h *NotificationHandler) Name() string { return "notification" }
func (h *NotificationHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	time.Sleep(time.Millisecond * time.Duration(5+rand.Intn(10)))

	return input, nil
}

type ValidationHandler struct{ target *FloxyStressTarget }

func (h *ValidationHandler) Name() string { return "validation" }
func (h *ValidationHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	time.Sleep(time.Millisecond * time.Duration(5+rand.Intn(10)))

	return input, nil
}

type CompensationHandler struct{ target *FloxyStressTarget }

func (h *CompensationHandler) Name() string { return "compensation" }
func (h *CompensationHandler) Execute(
	ctx context.Context,
	stepCtx floxy.StepContext,
	input json.RawMessage,
) (json.RawMessage, error) {
	action, _ := stepCtx.GetVariableAsString("action")
	time.Sleep(time.Millisecond * time.Duration(5+rand.Intn(10)))

	return json.Marshal(map[string]any{"compensated": action})
}

// Floxy Metrics Collector
type FloxyMetricsCollector struct {
	workflowsStarted      atomic.Int64
	workflowsCompleted    atomic.Int64
	workflowsFailed       atomic.Int64
	stepsStarted          atomic.Int64
	stepsCompleted        atomic.Int64
	stepsFailed           atomic.Int64
	totalStepDuration     atomic.Int64
	totalWorkflowDuration atomic.Int64
}

func NewFloxyMetricsCollector() *FloxyMetricsCollector {
	return &FloxyMetricsCollector{}
}

func (c *FloxyMetricsCollector) RecordWorkflowStarted(instanceID int64, workflowID string) {
	c.workflowsStarted.Add(1)
}

func (c *FloxyMetricsCollector) RecordWorkflowCompleted(
	instanceID int64,
	workflowID string,
	duration time.Duration,
	status floxy.WorkflowStatus,
) {
	c.workflowsCompleted.Add(1)
	c.totalWorkflowDuration.Add(duration.Milliseconds())
}

func (c *FloxyMetricsCollector) RecordWorkflowFailed(instanceID int64, workflowID string, duration time.Duration) {
	c.workflowsFailed.Add(1)
	c.totalWorkflowDuration.Add(duration.Milliseconds())
}

func (c *FloxyMetricsCollector) RecordWorkflowStatus(instanceID int64, workflowID string, status floxy.WorkflowStatus) {
}

func (c *FloxyMetricsCollector) RecordStepStarted(
	instanceID int64,
	workflowID,
	stepName string,
	stepType floxy.StepType,
) {
	c.stepsStarted.Add(1)
}

func (c *FloxyMetricsCollector) RecordStepCompleted(
	instanceID int64,
	workflowID, stepName string,
	stepType floxy.StepType,
	duration time.Duration,
) {
	c.stepsCompleted.Add(1)
	c.totalStepDuration.Add(duration.Milliseconds())
}

func (c *FloxyMetricsCollector) RecordStepFailed(
	instanceID int64,
	workflowID, stepName string,
	stepType floxy.StepType,
	duration time.Duration,
) {
	c.stepsFailed.Add(1)
	c.totalStepDuration.Add(duration.Milliseconds())
}

func (c *FloxyMetricsCollector) RecordStepStatus(
	instanceID int64,
	workflowID, stepName string,
	status floxy.StepStatus,
) {
}

func (c *FloxyMetricsCollector) GetStats() map[string]interface{} {
	avgStepDuration := int64(0)
	if c.stepsCompleted.Load() > 0 {
		avgStepDuration = c.totalStepDuration.Load() / (c.stepsCompleted.Load() + c.stepsFailed.Load())
	}

	avgWorkflowDuration := int64(0)
	if c.workflowsCompleted.Load() > 0 {
		avgWorkflowDuration = c.totalWorkflowDuration.Load() / (c.workflowsCompleted.Load() + c.workflowsFailed.Load())
	}

	return map[string]interface{}{
		"workflows_started":        c.workflowsStarted.Load(),
		"workflows_completed":      c.workflowsCompleted.Load(),
		"workflows_failed":         c.workflowsFailed.Load(),
		"steps_started":            c.stepsStarted.Load(),
		"steps_completed":          c.stepsCompleted.Load(),
		"steps_failed":             c.stepsFailed.Load(),
		"avg_step_duration_ms":     avgStepDuration,
		"avg_workflow_duration_ms": avgWorkflowDuration,
	}
}

// ChaosKit integration
func RunFloxyWorkflow(ctx context.Context, target chaoskit.Target) error {
	floxyTarget, ok := target.(*FloxyStressTarget)
	if !ok {
		return fmt.Errorf("target is not FloxyStressTarget")
	}

	return floxyTarget.ExecuteRandomWorkflow(ctx)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("=== Floxy Stress Test with ChaosKit ===")
	log.Println("Testing Floxy workflow engine with advanced chaos injection")
	log.Println()
	log.Println("Chaos injection methods:")
	log.Println("  1. Monkey patching - runtime function patching")
	log.Println("  2. Failpoint - failpoint-based injection")
	log.Println("  3. ToxiProxy - network chaos for database")
	log.Println()

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

	// Wait for proxy to be ready if using ToxiProxy
	if useToxiProxy && connString != directConnString {
		// Extract host and port from proxy connection string
		parsedURL, err := url.Parse(connString)
		if err == nil && parsedURL.Host != "" {
			proxyHost := parsedURL.Hostname()
			proxyPortStr := parsedURL.Port()
			if proxyPortStr == "" {
				proxyPortStr = "6432"
			}
			proxyPort, err := strconv.Atoi(proxyPortStr)
			if err == nil && proxyHost != "" {
				log.Println("[Setup] Ensuring ToxiProxy proxy is created...")

				// Try to create proxy if it doesn't exist
				// Listen address: 0.0.0.0:6432 (inside container, listens on all interfaces)
				// Upstream: PostgreSQL address (e.g., floxy-stress-pg:5432)
				listenAddr := fmt.Sprintf("0.0.0.0:%d", proxyPort)
				upstreamAddr := fmt.Sprintf("%s:%s", dbHost, dbPort)

				// Wait for ToxiProxy API to be ready first
				apiReady := false
				apiURL := fmt.Sprintf("http://%s/version", toxiproxyHost)
				log.Printf("[Setup] Waiting for ToxiProxy API at %s to be ready...", apiURL)
				for i := 0; i < 30; i++ {
					client := &http.Client{Timeout: 2 * time.Second}
					resp, err := client.Get(apiURL)
					if err == nil && resp.StatusCode == http.StatusOK {
						resp.Body.Close()
						apiReady = true
						log.Printf("[Setup] ToxiProxy API is ready")
						break
					}
					if i%5 == 0 && i > 0 {
						log.Printf("[Setup] Still waiting for ToxiProxy API... (attempt %d/30)", i+1)
					}
					time.Sleep(1 * time.Second)
				}

				if !apiReady {
					log.Printf("[Setup] Warning: ToxiProxy API at %s not ready after 60 seconds, continuing anyway...", apiURL)
				}

				// Create proxy if it doesn't exist
				if err := createProxyIfNotExists(toxiproxyHost, "pg", listenAddr, upstreamAddr); err != nil {
					log.Printf("[Setup] Warning: Failed to create proxy via API (may already exist): %v", err)
				}

				log.Println("[Setup] Waiting for ToxiProxy proxy to be ready...")
				// Proxy name from toxiproxy.json is "pg"
				if err := waitForProxyReady(proxyHost, proxyPort, toxiproxyHost, "pg", 30, 2*time.Second); err != nil {
					log.Fatalf("Failed to wait for proxy: %v", err)
				}
			}
		}
	}

	// Create a Floxy target
	log.Println("[Setup] Creating Floxy target...")
	if err := RegisterWorkflows(directConnString); err != nil {
		log.Fatalf("Failed to register workflows: %v", err)
	}

	floxyTarget, err := NewFloxyStressTarget(directConnString, connString)
	if err != nil {
		log.Fatalf("Failed to create Floxy target: %v", err)
	}

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Statistics reporter
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats := floxyTarget.GetStats()
				log.Printf("[Stats] %+v", stats)
			}
		}
	}()

	// Build injectors
	log.Println("[Setup] Creating chaos injectors...")

	// 1. Monkey patching injectors for handlers
	// Note: Must pass pointer to function for monkey patching
	monkeyPanicTargets := []injectors.PatchTarget{
		{
			Func:        &processPaymentHandler,
			Probability: 0.05, // 5% chance of panic
			FuncName:    "processPaymentHandler",
		},
		{
			Func:        &processInventoryHandler,
			Probability: 0.05,
			FuncName:    "processInventoryHandler",
		},
		{
			Func:        &processShippingHandler,
			Probability: 0.05,
			FuncName:    "processShippingHandler",
		},
	}

	monkeyPanicInjector := injectors.MonkeyPatchPanic(monkeyPanicTargets)

	monkeyDelayTargets := []injectors.DelayPatchTarget{
		{
			Func:        &processPaymentHandler,
			MinDelay:    50 * time.Millisecond,
			MaxDelay:    200 * time.Millisecond,
			Probability: 0.2, // 20% chance of delay
			DelayBefore: true,
			FuncName:    "processPaymentHandler",
		},
		{
			Func:        &processInventoryHandler,
			MinDelay:    30 * time.Millisecond,
			MaxDelay:    150 * time.Millisecond,
			Probability: 0.15,
			DelayBefore: true,
			FuncName:    "processInventoryHandler",
		},
	}

	monkeyDelayInjector := injectors.MonkeyPatchDelay(monkeyDelayTargets)

	// 2. Gofail injector (requires build with -tags failpoint)
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
		// Monkey patching injectors (always active)
		Inject("monkey-panic", monkeyPanicInjector).
		Inject("monkey-delay", monkeyDelayInjector)

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
		Assert("no-slow-iteration", validators.NoSlowIteration(15*time.Second)).
		RunFor(30 * time.Second). // Run for 30 seconds
		Build()

	// Create executor with ContinueOnFailure policy
	executor := chaoskit.NewExecutor(
		chaoskit.WithFailurePolicy(chaoskit.ContinueOnFailure),
	)

	// Run in background
	log.Println("[Main] Starting chaos scenario...")
	go func() {
		if err := executor.Run(ctx, scenario); err != nil {
			log.Printf("Scenario error: %v", err)
		}
	}()

	// Wait for signal
	<-sigCh
	log.Println("\n[Main] Shutting down...")
	cancel()

	// Stop injectors
	log.Println("[Cleanup] Stopping injectors...")
	// Note: ToxiProxy proxy is managed via toxiproxy.json configuration file,
	// so we don't need to delete it programmatically

	// Wait a bit for cleanup
	time.Sleep(2 * time.Second)

	// Get verdict and generate report
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

	// Exit with verdict code
	//os.Exit(report.Verdict.ExitCode())
}
