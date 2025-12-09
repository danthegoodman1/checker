// Test worker for checkpoint/restore functionality
// With CRIU on Linux, execution continues from where it left off after restore,
// so step 1 runs once, checkpoint suspends, then step 2 runs after restore.

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL

// Track how many times the pre-checkpoint code runs.
// With real CRIU (Linux), this stays at 1 because state is preserved.
let preCheckpointRuns = 0

console.log(
  `Checkpoint restore test worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)

async function checkpoint(suspendDuration, maxRetries = 5) {
  const token = crypto.randomUUID()
  const body = { token }
  if (suspendDuration) {
    body.suspend_duration = suspendDuration
  }

  // Retry loop - on restore, the connection will break and we retry with the same token
  for (let attempt = 0; attempt < maxRetries; attempt++) {
    try {
      // Use AbortController for fast timeout - after CRIU restore the old TCP
      // connection is dead and we want to fail fast and retry
      const controller = new AbortController()
      const timeoutId = setTimeout(() => controller.abort(), 500)

      const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/checkpoint`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal: controller.signal,
      })
      clearTimeout(timeoutId)
      if (!resp.ok) {
        throw new Error(
          `Checkpoint failed: ${resp.status} ${await resp.text()}`
        )
      }
      return await resp.json()
    } catch (err) {
      // Connection errors are expected on restore - retry with same token
      if (attempt < maxRetries - 1) {
        console.log(
          `Checkpoint request failed (attempt ${
            attempt + 1
          }/${maxRetries}), retrying: ${err.message}`
        )
        await new Promise((resolve) => setTimeout(resolve, 100))
        continue
      }
      throw err
    }
  }
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

async function main() {
  const params = await getParams()
  console.log("Received params:", JSON.stringify(params))

  const inputNumber = params.number ?? 0
  const suspendDuration = params.suspend_duration ?? "4s"
  const skipCheckpoint = params.skip_checkpoint ?? false
  const sleepMs = params.sleep_ms ?? 0
  // checkpoint_keep_running: checkpoint without suspending (keep running after checkpoint)
  const checkpointKeepRunning = params.checkpoint_keep_running ?? false
  // sleep_after_checkpoint_ms: sleep after checkpoint (useful for testing crash scenarios)
  const sleepAfterCheckpointMs = params.sleep_after_checkpoint_ms ?? 0

  // This code runs once before checkpoint
  preCheckpointRuns++
  console.log(`Pre-checkpoint code run count: ${preCheckpointRuns}`)

  console.log("Step 1: Adding 1 to input...")
  const step1Result = { step: 1, value: inputNumber + 1 }
  console.log("Step 1 complete:", step1Result)

  // Optional sleep before checkpoint (useful for testing container kill scenarios)
  if (sleepMs > 0) {
    console.log(`Sleeping for ${sleepMs}ms...`)
    await new Promise((resolve) => setTimeout(resolve, sleepMs))
  }

  // Checkpoint - with CRIU, execution continues from here after restore
  if (!skipCheckpoint) {
    if (checkpointKeepRunning) {
      console.log("Checkpointing (keep running)...")
      // Pass null/undefined for suspend_duration to keep running
      const checkpointResult = await checkpoint(null)
      console.log("Checkpoint complete (still running):", JSON.stringify(checkpointResult))
    } else {
      console.log("Checkpointing with suspend...")
      const checkpointResult = await checkpoint(suspendDuration)
      console.log("Checkpoint complete:", JSON.stringify(checkpointResult))
    }
  }

  // Optional sleep after checkpoint (for testing: crash while running after checkpoint)
  if (sleepAfterCheckpointMs > 0) {
    console.log(`Sleeping for ${sleepAfterCheckpointMs}ms after checkpoint...`)
    await new Promise((resolve) => setTimeout(resolve, sleepAfterCheckpointMs))
  }

  // This code runs after restore
  console.log("Step 2: Doubling the value...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Checkpoint restore test completed successfully")
  console.log(`Final pre-checkpoint run count: ${preCheckpointRuns}`)
  await exit(0, {
    result: step2Result,
    pre_checkpoint_runs: preCheckpointRuns,
  })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
