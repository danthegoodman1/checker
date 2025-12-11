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

### 2. ~~Recovery Code Paths (Documentation Issue)~~ ✅ RESOLVED

Added comment block to `recoverJobs` explaining all recovery scenarios.

---

### 3. ~~Database Persistence Inconsistency~~ ✅ RESOLVED

All database operations now consistently use `ReliableExecInTx` for transaction safety.

---

## Summary: Priority Ranking

| Change | Lines Reduced | Risk | Effort |
|--------|--------------|------|--------|
| 1. Simplify result waiters | ~20 | Low | Low |

---

## Top Recommendation

1. **Simplify result waiters** - Minor cleanup to reduce indirection
