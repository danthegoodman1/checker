// Test worker for checkpoint/restore functionality
// Uses time-based logic to checkpoint with suspend on first run,
// then skip checkpoint after restore (to avoid infinite loops on macOS).
//
// On Linux with real CRIU: in-memory state is preserved across checkpoint/restore,
// so preCheckpointRuns will be 1 (code before checkpoint runs only once).
// On macOS (workaround): container restarts from scratch, so preCheckpointRuns
// will be 2 (code before checkpoint runs twice - once per container start).

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL
const spawnedAt = process.env.CHECKER_JOB_SPAWNED_AT
  ? parseInt(process.env.CHECKER_JOB_SPAWNED_AT, 10)
  : null

// Track how many times the pre-checkpoint code runs.
// With real CRIU (Linux), this stays at 1 because state is preserved.
// With macOS workaround, this becomes 2 because container restarts from scratch.
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
        throw new Error(`Checkpoint failed: ${resp.status} ${await resp.text()}`)
      }
      return await resp.json()
    } catch (err) {
      // Connection errors are expected on restore - retry with same token
      if (attempt < maxRetries - 1) {
        console.log(`Checkpoint request failed (attempt ${attempt + 1}/${maxRetries}), retrying: ${err.message}`)
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
  const checkpointWithinSecs = params.checkpoint_within_secs ?? 3
  const suspendDuration = params.suspend_duration ?? "4s"

  // Increment pre-checkpoint run counter BEFORE any checkpoint logic.
  // With real CRIU (Linux), this only runs once because state is restored.
  // With macOS workaround, this runs twice because container restarts from scratch.
  preCheckpointRuns++
  console.log(`Pre-checkpoint code run count: ${preCheckpointRuns}`)

  console.log("Step 1: Adding 1 to input...")
  const step1Result = { step: 1, value: inputNumber + 1 }
  console.log("Step 1 complete:", step1Result)

  // Time-based checkpoint logic to prevent infinite loops on macOS
  // (where checkpoint just stops/starts the container without preserving state)
  // Only checkpoint if within the time window since spawn
  let checkpointSkipped = false
  const nowSeconds = Math.floor(Date.now() / 1000)

  if (spawnedAt === null) {
    console.log("Warning: CHECKER_JOB_SPAWNED_AT not set, skipping checkpoint")
    checkpointSkipped = true
  } else {
    const cutoffTime = spawnedAt + checkpointWithinSecs
    if (nowSeconds <= cutoffTime) {
      console.log(
        `Time check: now=${nowSeconds}, spawnedAt=${spawnedAt}, cutoff=${cutoffTime} -> checkpointing with suspend`
      )
      const checkpointResult = await checkpoint(suspendDuration)
      console.log("Checkpoint complete:", JSON.stringify(checkpointResult))
    } else {
      console.log(
        `Time check: now=${nowSeconds}, spawnedAt=${spawnedAt}, cutoff=${cutoffTime} -> skipping (restored after cutoff)`
      )
      checkpointSkipped = true
    }
  }

  console.log("Step 2: Doubling the value...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Checkpoint restore test completed successfully")
  console.log(`Final pre-checkpoint run count: ${preCheckpointRuns}`)
  await exit(0, {
    result: step2Result,
    checkpoint_skipped: checkpointSkipped,
    pre_checkpoint_runs: preCheckpointRuns,
  })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
