# Hypervisor Architecture Review

## Overview

The hypervisor is a well-designed system for managing checkpointable/snapshottable job instances. The core components are:

1. **Hypervisor** - Central orchestrator managing job lifecycle
2. **JobRunner** - Per-job state machine using command-loop actor pattern
3. **Runtime Interface** - Abstractions for Podman/Firecracker
4. **Dual HTTP APIs** - Caller API (external) + Runtime API (container callbacks)
5. **Resume Poller** - Background polling for suspended/pending_retry jobs
6. **PostgreSQL** - Durable state for crash recovery

---

## Complexity Analysis & Recommendations

### 1. Command Loop Boilerplate (Medium Impact)

**Current State:**

The JobRunner uses an actor pattern with 11 command types. Each public method is ~15 lines of channel send/receive boilerplate:

```go
func (r *JobRunner) Start() error {
    resultChan := make(chan commandResult, 1)
    select {
    case r.cmdChan <- command{typ: cmdStart, ctx: r.ctx, resultChan: resultChan}:
    case <-r.doneChan:
        return fmt.Errorf("job runner stopped")
    }

    select {
    case result := <-resultChan:
        return result.err
    case <-r.doneChan:
        return fmt.Errorf("job runner stopped")
    }
}
```

This pattern is duplicated 9 times. While correct, it's verbose.

**Recommendation:**

Extract a helper method:

```go
func (r *JobRunner) sendCommand(cmd command) commandResult {
    select {
    case r.cmdChan <- cmd:
    case <-r.doneChan:
        return commandResult{err: fmt.Errorf("job runner stopped")}
    }
    select {
    case result := <-cmd.resultChan:
        return result
    case <-r.doneChan:
        return commandResult{err: fmt.Errorf("job runner stopped")}
    }
}
```

This could reduce the public API methods by ~100 lines.

---

### 2. Checkpoint Request Collapsing (Low Impact)

**Current State:**

When checkpoint is in progress, subsequent requests are collapsed:

```go
if r.job.CurrentOperation == JobOpCheckpointing {
    r.logger.Debug().Str("token", cmd.token).Msg("checkpoint already in progress, collapsing request")
    r.pendingCheckpointRequests = append(r.pendingCheckpointRequests, cmd.resultChan)
    return
}
```

**Question:** Does this happen in practice? Workers typically don't issue concurrent checkpoint requests.

**Recommendation:**

If this is defensive code that never triggers, consider removing it. The idempotency token already handles retries after restore.

---

### 3. ResultWaiters Pattern (Low Impact)

**Current State:**

```go
// Result waiters - channels to notify when job completes
resultWaiters []chan struct{}
```

This is a slice that accumulates waiters. The `doneChan` already serves a similar purpose.

**Recommendation:**

Consider using `doneChan` directly for `WaitForResult()`. The current implementation has the waiter wait on a per-call channel that then waits on `doneChan`:

```go
go func() {
    select {
    case <-resultChan:
    case <-r.doneChan:
    }
    close(waitChan)
}()
```

This indirection could be simplified.

---

### 4. Recovery Code Paths (Documentation Issue)

**Current State:**

The recovery logic has multiple entry points:

- `recoverRunningJob` - crashed mid-execution
- `restartPendingJob` - never started
- Resume poller - suspended/pending_retry

**Recommendation:**

Consider unifying into a single recovery path or at minimum add a comment block explaining when each is used:

```go
// Recovery scenarios:
// 1. Job was pending when we crashed → restartPendingJob (start fresh)
// 2. Job was running when we crashed:
//    a. Has checkpoint → wakeJob (restore from checkpoint)  
//    b. No checkpoint → retry if policy allows, else fail
// 3. Job was suspended/pending_retry → resume poller handles it
```

---

### 5. Database Persistence Inconsistency

**Current State:**

Some persist methods use `ReliableExecInTx` and others use `ReliableExec`:

```go
// Sometimes:
if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {

// Other times:
if dbErr := query.ReliableExec(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
```

**Recommendation:**

Standardize on one pattern (likely `ReliableExecInTx` for consistency) unless there's a specific reason for the difference.

---

## Summary: Priority Ranking

| Change | Lines Reduced | Risk | Effort |
|--------|--------------|------|--------|
| 1. Unify wake mechanism (poller only) | ~40 | Low | Low |
| 2. Extract command helper | ~100 | Low | Low |
| 3. Remove checkpoint collapsing | ~30 | Low | Low |
| 4. Simplify result waiters | ~20 | Low | Low |

---

## Top 2 Recommendations

1. **Unify wake mechanism** - Simplest change, biggest clarity improvement
2. **Extract command helper** - Pure refactor, no behavior change
