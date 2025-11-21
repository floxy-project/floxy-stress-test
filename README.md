# Floxy Stress Test with ChaosKit

Comprehensive stress testing for Floxy workflow engine using [ChaosKit framework](https://github.com/rom8726/chaoskit).

## Overview

This example demonstrates how to use [ChaosKit](https://github.com/rom8726/chaoskit) to perform reliability testing on the Floxy saga-based workflow engine.
It tests various workflow patterns under chaos conditions to validate:

- Rollback recursion depth limits
- Goroutine leak prevention
- Infinite loop detection
- Compensation handler reliability
- SavePoint functionality
- Fork/Join parallel execution

## Features

### Workflow Patterns Tested

1. **Simple Order Workflow**
   - Linear execution with compensation handlers
   - Tests basic rollback mechanism

2. **Complex Order Workflow**
   - Multiple SavePoints
   - Rollback to specific checkpoints
   - Multi-level compensation

3. **Parallel Processing Workflow**
   - Fork/Join patterns
   - Concurrent step execution
   - Parallel compensation

4. **Nested Workflow**
   - Nested Fork/Join structures
   - Complex dependency graphs
   - Deep rollback chains

### Chaos Injection Methods

This example demonstrates chaos injection techniques:

1. **Context Injections** (Always Active)
   - Random delay injection (20-100ms, 15% probability)
   - Panic injection (15% probability)
   - Error injection (15% probability)
   - No build flags required

2. **Failpoint-based injection** (Optional)
   - Panic injection via failpoints
   - Requires build with `-tags failpoint`
   - Requires instrumenting code with failpoint.Inject
   - 3% probability per failpoint

3. **ToxiProxy** (Optional)
   - Network chaos for database connections
   - Latency injection (100ms ± 20ms)
   - Bandwidth limiting (500 KB/s)
   - Connection timeouts (2 seconds)
   - Enable with `USE_TOXIPROXY=true` in docker-compose.yml

### Validation

- **Recursion Depth**: Ensures rollback depth stays below 10
- **Goroutine Leak**: Monitors goroutine count (limit: 100)
- **Infinite Loop**: Detects stuck workflows (timeout: 15s)

### Metrics Collection

Integrated Floxy plugins:
- **Rollback Depth Plugin**: Tracks maximum rollback depth per instance
- **Metrics Plugin**: Collects workflow and step statistics

## Prerequisites

- Docker and Docker Compose
- Make (for convenience commands)

## Quick Start

### Using Make Commands

```bash
# Build the stress test application
make docker-build

# Start all services (PostgreSQL, ToxiProxy, Stress Tester, UI)
make docker-up

# Stop all services
make docker-down

# Clean up everything including volumes
make docker-clean

# Restart services
make docker-restart
```

### Manual Docker Compose

```bash
# Build
docker compose build floxy-stress-tester

# Start all services
docker compose up

# Stop services
docker compose down
```

## Architecture

The test environment consists of:

1. **floxy-stress-pg**: PostgreSQL 17 database
   - Port: 5432
   - Database: `floxy`
   - User: `floxy` / Password: `password`

2. **floxy-stress-toxiproxy**: ToxiProxy server for network chaos
   - API Port: 8474
   - Proxy Port: 6432
   - Automatically creates proxy from `toxiproxy.json` config

3. **floxy-stress-tester**: Stress test application
   - Runs workflows continuously
   - Injects chaos conditions
   - Collects metrics and statistics

4. **floxy-stress-ui**: Floxy UI (optional)
   - Port: 3001
   - Web interface for monitoring workflows

## Configuration

### Environment Variables

Configure the stress test via environment variables in `docker-compose.yml`:

```yaml
environment:
  DB_HOST: floxy-stress-pg          # PostgreSQL host
  DB_PORT: 5432                      # PostgreSQL port
  DB_NAME: floxy                     # Database name
  DB_USER: floxy                     # Database user
  DB_PASSWORD: password              # Database password
  USE_TOXIPROXY: false               # Enable ToxiProxy (true/false)
  TOXIPROXY_HOST: floxy-stress-toxiproxy:8474  # ToxiProxy API address
  REPEAT: 100                        # Number of workflow executions
```

### Test Parameters

Modify test parameters in `main.go`:

```go
// Test duration
RunFor(30 * time.Second)

// Worker count
workerCount := 5

// Validation limits
Assert("recursion-depth", validators.RecursionDepthLimit(10))
Assert("goroutine-leak", validators.GoroutineLimit(100))
Assert("no-slow-iteration", validators.NoSlowIteration(15*time.Second))
```

### Context Injections

```go
// Random delay (20-100ms, 15% probability)
Inject("context-delay", injectors.RandomDelayWithProbability(
    time.Millisecond*20, 
    time.Millisecond*100, 
    0.15))

// Panic injection (15% probability)
Inject("context-panic", injectors.PanicProbability(0.15))

// Error injection (15% probability)
Inject("context-error", injectors.ErrorWithProbability("chaos error", 0.15))
```

### ToxiProxy Configuration

Edit `toxiproxy.json` to configure proxy:

```json
{
  "proxies": {
    "pg": {
      "listen": "0.0.0.0:6432",
      "upstream": "floxy-stress-pg:5432",
      "enabled": true
    }
  }
}
```

To enable ToxiProxy, set `USE_TOXIPROXY: true` in docker-compose.yml.

## Output

### Real-time Statistics

Every 10 seconds, you'll see statistics like:

```
[Stats] map[
  failed_runs:12
  max_rollback_depth:4
  metrics:map[
    avg_step_duration_ms:15
    avg_workflow_duration_ms:120
    steps_completed:450
    steps_failed:35
    steps_started:485
    workflows_completed:88
    workflows_failed:12
    workflows_started:100
  ]
  rollback_count:12
  successful_runs:88
  total_workflows:100
]
```

### Final Report

At the end, you'll see:

```
=== Final Chaos Test Report ===
ChaosKit Execution Report
========================
Total Executions: 150
Success: 135
Failed: 15
Success Rate: 90.00%
Average Duration: 250ms

=== Floxy Statistics ===
Total workflows: 150
Successful runs: 135
Failed runs: 15
Rollback count: 15
Max rollback depth: 6
Metrics: map[...]
```

## What It Tests

### Rollback Mechanism

- Verifies compensation handlers execute in correct order
- Ensures rollback doesn't exceed maximum depth
- Tests SavePoint rollback functionality
- Validates state consistency after rollback

### Concurrency

- 5 concurrent workers processing workflows
- Multiple workflow instances running simultaneously
- Fork/Join parallel execution
- Race condition detection

### Error Handling

- Random failures trigger compensation
- Retry mechanisms with max retry limits
- Graceful degradation under load
- Proper error propagation

### Resource Management

- Goroutine leak detection
- Database connection pooling
- Worker lifecycle management
- Graceful shutdown

## Integration with ChaosKit

This example demonstrates ChaosKit's capabilities:

1. **Target Interface**: Floxy engine wrapped as ChaosKit target
2. **Context Injectors**: Delay, panic, and error injection via context
3. **Failpoint Injectors**: Failpoint-based chaos injection (compile-time, optional)
4. **ToxiProxy Injectors**: Network-level chaos for database connections (optional)
5. **Validators**: Recursion depth, goroutine leak, infinite loop detection
6. **Failure Policy**: ContinueOnFailure for continuous testing
7. **Metrics**: Automatic collection and reporting

### Injection Methods Comparison

| Method | Type | Requires Build Flags | Code Changes | Status |
|--------|------|---------------------|--------------|--------|
| Context Injections | Runtime | No | No | ✅ Active |
| Failpoint | Compile-time | `-tags failpoint` | Yes (failpoint.Inject) | ⚠️ Optional |
| ToxiProxy | Network | No | No (proxy setup) | ⚠️ Optional |
| Monkey Patching | Runtime | `-gcflags=all=-l` | No | ❌ Disabled |

## Troubleshooting

### Database connection errors

Check if PostgreSQL is running:

```bash
docker compose ps floxy-stress-pg
docker compose logs floxy-stress-pg
```

### ToxiProxy not working

1. Check if ToxiProxy is running:
   ```bash
   docker compose ps floxy-stress-toxiproxy
   docker compose logs floxy-stress-toxiproxy
   ```

2. Verify proxy is created:
   ```bash
   curl http://localhost:8474/proxies
   ```

3. Ensure `USE_TOXIPROXY: true` in docker-compose.yml

### High failure rate

This is expected with 15% random failure injection. Adjust the probability in `main.go`:

```go
Inject("context-panic", injectors.PanicProbability(0.05))  // 5% instead of 15%
```

### Goroutine limit exceeded

Increase the limit or reduce worker count:

```go
Assert("goroutine-leak", validators.GoroutineLimit(200))
// or
workerCount := 3
```

### Viewing logs

```bash
# All services
docker compose logs -f

# Specific service
docker compose logs -f floxy-stress-tester

# Last 100 lines
docker compose logs --tail=100 floxy-stress-tester
```

## Expected Results

A healthy Floxy engine should show:

- Success rate: 80-90% (with 15% random failures)
- Max rollback depth: < 10
- No goroutine leaks
- No infinite loops
- Proper compensation execution
- Consistent state after rollback

## Development

### Building locally

```bash
# Build Go application
go build -o floxy-stress-test .

# Run locally (requires PostgreSQL running)
DB_HOST=localhost DB_PORT=5432 DB_NAME=floxy DB_USER=floxy DB_PASSWORD=password ./floxy-stress-test
```

### Running tests with Failpoint

1. Uncomment failpoint calls in handlers (`main.go`)
2. Build with failpoint tag:
   ```bash
   docker compose build --build-arg BUILD_TAGS=failpoint floxy-stress-tester
   ```

### Enabling ToxiProxy

1. Set `USE_TOXIPROXY: true` in docker-compose.yml
2. Restart services:
   ```bash
   make docker-restart
   ```
