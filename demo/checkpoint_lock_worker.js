// Test worker for checkpoint lock functionality
// Takes a lock, schedules release after delay, then tries to checkpoint.
// The checkpoint should block until the lock is released.

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL

console.log(
  `Checkpoint lock test worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)

async function checkpoint() {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/checkpoint`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  })
  if (!resp.ok) {
    throw new Error(`Checkpoint failed: ${resp.status} ${await resp.text()}`)
  }
  return resp.json()
}

async function exit(exitCode, output) {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/exit`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ exit_code: exitCode, output }),
  })
  if (!resp.ok) {
    throw new Error(`Exit failed: ${resp.status} ${await resp.text()}`)
  }
  process.exit(exitCode)
}

async function getParams() {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/params`)
  if (!resp.ok) {
    throw new Error(`Get params failed: ${resp.status} ${await resp.text()}`)
  }
  return resp.json()
}

async function takeLock() {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/lock`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  })
  if (!resp.ok) {
    throw new Error(`Take lock failed: ${resp.status} ${await resp.text()}`)
  }
  const result = await resp.json()
  return result.lock_id
}

async function releaseLock(lockId) {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/lock/${lockId}`, {
    method: "DELETE",
  })
  if (!resp.ok) {
    throw new Error(`Release lock failed: ${resp.status} ${await resp.text()}`)
  }
}

async function main() {
  const params = await getParams()
  console.log("Received params:", JSON.stringify(params))

  const lockHoldMs = params.lock_hold_ms || 2000
  console.log(`Will hold checkpoint lock for ${lockHoldMs}ms`)

  // Take the lock
  const lockId = await takeLock()
  console.log(`Acquired lock: ${lockId}`)

  // Schedule lock release after delay
  setTimeout(async () => {
    console.log(`Releasing lock ${lockId} after ${lockHoldMs}ms hold`)
    try {
      await releaseLock(lockId)
      console.log(`Lock ${lockId} released`)
    } catch (err) {
      console.error(`Failed to release lock: ${err.message}`)
    }
  }, lockHoldMs)

  // Immediately try to checkpoint - this should block until lock is released
  const checkpointStartTime = Date.now()
  console.log(`Attempting checkpoint while lock is held...`)
  const checkpointResult = await checkpoint()
  const checkpointEndTime = Date.now()
  const checkpointDuration = checkpointEndTime - checkpointStartTime

  console.log(
    `Checkpoint completed in ${checkpointDuration}ms, result:`,
    JSON.stringify(checkpointResult)
  )

  // The checkpoint should have been blocked for at least the lock hold duration
  const wasBlocked = checkpointDuration >= lockHoldMs
  console.log(
    `Checkpoint was blocked: ${wasBlocked} (duration: ${checkpointDuration}ms, expected >= ${lockHoldMs}ms)`
  )

  console.log("Checkpoint lock test completed successfully")
  await exit(0, {
    lock_hold_ms: lockHoldMs,
    checkpoint_duration_ms: checkpointDuration,
    was_blocked: wasBlocked,
  })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
