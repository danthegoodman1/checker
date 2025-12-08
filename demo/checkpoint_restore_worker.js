// Test worker for checkpoint/restore functionality
// Uses time-based logic to checkpoint with suspend on first run,
// then skip checkpoint after restore (to avoid infinite loops on macOS).

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL
const spawnedAt = process.env.CHECKER_JOB_SPAWNED_AT
  ? parseInt(process.env.CHECKER_JOB_SPAWNED_AT, 10)
  : null

console.log(
  `Checkpoint restore test worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)

async function checkpoint(suspendDuration) {
  const body = suspendDuration ? { suspend_duration: suspendDuration } : {}
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/checkpoint`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!resp.ok) {
    throw new Error(`Checkpoint failed: ${resp.status} ${await resp.text()}`)
  }
  const result = await resp.json()

  // If the server specifies a grace period, wait for it before returning.
  // This ensures the worker is idle (not making progress) when the container is stopped.
  if (result.grace_period_ms && result.grace_period_ms > 0) {
    console.log(
      `Waiting ${result.grace_period_ms}ms grace period before checkpoint completes...`
    )
    await new Promise((resolve) => setTimeout(resolve, result.grace_period_ms))
  }

  return result
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
  await exit(0, {
    result: step2Result,
    checkpoint_skipped: checkpointSkipped,
  })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
