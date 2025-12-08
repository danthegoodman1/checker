// Demo worker script for testing the checker hypervisor

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL

const arch = process.env.CHECKER_ARCH || "unknown"
const os = process.env.CHECKER_OS || "unknown"

// Node.js detected values
const nodeArch = process.arch // e.g., 'x64', 'arm64'
const nodePlatform = process.platform // e.g., 'linux', 'darwin', 'win32'

console.log(
  `Worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)
console.log(`  Hypervisor: ${os}/${arch}, Worker: ${nodePlatform}/${nodeArch}`)

async function checkpoint(suspendDuration, maxRetries = 5) {
  const token = crypto.randomUUID()
  const body = { token }
  if (suspendDuration) {
    body.suspend_duration = suspendDuration
  }

  // Retry loop - on restore, the connection will break and we retry with the same token
  for (let attempt = 0; attempt < maxRetries; attempt++) {
    try {
      const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/checkpoint`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
      if (!resp.ok) {
        throw new Error(`Checkpoint failed: ${resp.status} ${await resp.text()}`)
      }
      return await resp.json()
    } catch (err) {
      // Connection errors are expected on restore - retry with same token
      if (attempt < maxRetries - 1) {
        console.log(`Checkpoint request failed (attempt ${attempt + 1}/${maxRetries}), retrying: ${err.message}`)
        // Small delay before retry to let things settle after restore
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

  console.log("Checkpointing...")
  const checkpointResult = await checkpoint()
  console.log("Checkpoint complete:", JSON.stringify(checkpointResult))

  if (params.crash === "after_checkpoint" && metadata.retry_count === 0) {
    console.log("Simulating crash after checkpoint (retry_count=0)...")
    nonExistentFunction()
  }

  console.log("Step 2: Doubling the value...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Worker finished successfully")
  await exit(0, { result: step2Result })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
