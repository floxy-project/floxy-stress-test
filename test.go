package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rom8726/chaoskit"
	"github.com/rom8726/floxy"
	"github.com/rom8726/floxy/plugins/engine/metrics"
	rolldepth "github.com/rom8726/floxy/plugins/engine/rollback-depth"
)

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
	_ = t.engine.Shutdown()

	// Close pool
	t.pool.Close()

	// Print final stats
	stats := t.GetStats()
	log.Printf("\n\n\n[Floxy] Final stats: %+v", stats)

	return nil
}

func (t *FloxyStressTarget) worker(ctx context.Context, workerID string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			empty, err := t.engine.ExecuteNext(ctx, workerID)
			if err != nil {
				log.Printf("[Floxy] Worker %s error: %v", workerID, err)

				return fmt.Errorf("worker %s error: %w", workerID, err)
			}
			if empty {
				return nil
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

	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()

	ctxMonitor, cancel := context.WithCancel(ctx)
	defer cancel()

	go func(ctx context.Context) {
		defer wg.Done()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			status, err := t.engine.GetStatus(context.Background(), instanceID)
			if err != nil {
				log.Printf("failed to get status: %s", err)

				continue
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

				return
			}

			if status == floxy.StatusFailed {
				t.failedRuns.Add(1)
				if depth > 0 {
					t.rollbackCount.Add(1)
				}
				t.rollbackPlugin.ResetMaxDepth(instanceID)

				return
			}

			if status == floxy.StatusAborted || status == floxy.StatusCancelled {
				t.failedRuns.Add(1)
				t.rollbackPlugin.ResetMaxDepth(instanceID)

				return
			}

			time.Sleep(100 * time.Millisecond)
		}
	}(ctxMonitor)

	return t.worker(ctx, uuid.NewString())
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

// PaymentHandler processes payment
var processPaymentHandler = func(ctx context.Context, order map[string]any) error {
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

	// Call a processable function
	if err := processPaymentHandler(ctx, order); err != nil {
		return nil, err
	}

	return json.Marshal(order)
}

// InventoryHandler processes inventory reservation
// To enable failpoint, uncomment failpoint calls and build with: go build -tags failpoint -gcflags=all=-l
var processInventoryHandler = func(ctx context.Context, order map[string]any) error {
	// failpoint requires build with -tags failpoint

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
// To enable failpoint, uncomment failpoint calls and build with: go build -tags failpoint -gcflags=all=-l
var processShippingHandler = func(ctx context.Context, order map[string]any) error {
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

func RunFloxyWorkflow(ctx context.Context, target chaoskit.Target) error {
	floxyTarget, ok := target.(*FloxyStressTarget)
	if !ok {
		return fmt.Errorf("target is not FloxyStressTarget")
	}

	return floxyTarget.ExecuteRandomWorkflow(ctx)
}
