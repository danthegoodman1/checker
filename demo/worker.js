// Demo worker script for testing the checker hypervisor

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL
const spawnedAt = process.env.CHECKER_JOB_SPAWNED_AT
  ? parseInt(process.env.CHECKER_JOB_SPAWNED_AT, 10)
  : null

const arch = process.env.CHECKER_ARCH || "unknown"
const os = process.env.CHECKER_OS || "unknown"

// Node.js detected values
const nodeArch = process.arch // e.g., 'x64', 'arm64'
const nodePlatform = process.platform // e.g., 'linux', 'darwin', 'win32'

console.log(
  `Worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)
console.log(`  Hypervisor: ${os}/${arch}, Worker: ${nodePlatform}/${nodeArch}`)

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

async function getMetadata() {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/metadata`)
  if (!resp.ok) {
    throw new Error(`Get metadata failed: ${resp.status} ${await resp.text()}`)
  }
  return resp.json()
}

async function main() {
  const params = await getParams()
  const metadata = await getMetadata()
  console.log("Received params:", JSON.stringify(params))
  console.log("Received metadata:", JSON.stringify(metadata))

  // Check for crash simulation - only crash if retry_count is 0
  if (params.crash === "before_checkpoint" && metadata.retry_count === 0) {
    console.log("Simulating crash before checkpoint (retry_count=0)...")
    nonExistentFunction()
  }

  // Always crash mode - crashes on every attempt (for testing retry exhaustion)
  if (params.crash === "always") {
    console.log(
      `Simulating crash (always mode, retry_count=${metadata.retry_count})...`
    )
    nonExistentFunction()
  }

  const inputNumber = params.number ?? 0
  console.log("Step 1: Adding 1 to input...")
  const step1Result = { step: 1, value: inputNumber + 1 }
  console.log("Step 1 complete:", step1Result)

  // Handle time-based checkpoint logic for checkpoint/restore testing
  // This prevents infinite loops on macOS where checkpoint just stops/starts the container
  // If checkpoint_suspend_within_secs is set, only checkpoint if within that many seconds of spawn
  let checkpointSkipped = false
  if (params.checkpoint_suspend_within_secs && spawnedAt !== null) {
    const nowSeconds = Math.floor(Date.now() / 1000)
    const cutoffTime = spawnedAt + params.checkpoint_suspend_within_secs
    if (nowSeconds <= cutoffTime) {
      console.log(
        `Time-based checkpoint: now=${nowSeconds}, spawnedAt=${spawnedAt}, cutoff=${cutoffTime}, calling checkpoint with suspend`
      )
      const checkpointResult = await checkpoint(
        params.checkpoint_suspend_duration || "2s"
      )
      console.log("Checkpoint complete:", JSON.stringify(checkpointResult))
    } else {
      console.log(
        `Time-based checkpoint: now=${nowSeconds}, spawnedAt=${spawnedAt}, cutoff=${cutoffTime}, skipping (restored after cutoff)`
      )
      checkpointSkipped = true
    }
  } else {
    console.log("Checkpointing...")
    const checkpointResult = await checkpoint()
    console.log("Checkpoint complete:", JSON.stringify(checkpointResult))
  }

  if (params.crash === "after_checkpoint" && metadata.retry_count === 0) {
    console.log("Simulating crash after checkpoint (retry_count=0)...")
    nonExistentFunction()
  }

  console.log("Step 2: Doubling the value...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Worker finished successfully")
  await exit(0, { result: step2Result, checkpoint_skipped: checkpointSkipped })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
