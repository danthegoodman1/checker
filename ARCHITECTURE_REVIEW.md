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

### 1. ResultWaiters Pattern (Low Impact)

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

### 2. Recovery Code Paths (Documentation Issue)

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

### 3. Database Persistence Inconsistency

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
| 1. Simplify result waiters | ~20 | Low | Low |

---

## Top Recommendation

1. **Simplify result waiters** - Minor cleanup to reduce indirection
