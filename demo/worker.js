// Demo worker script for testing the checker hypervisor

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL

console.log(
  `Worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
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
  return resp.json()
}

async function main() {
  console.log("Step 1: Starting work...")
  const step1Result = { step: 1, value: 2 + 2 }
  console.log("Step 1 complete:", step1Result)

  console.log("Checkpointing...")
  const checkpointResult = await checkpoint()
  console.log("Checkpoint complete:", JSON.stringify(checkpointResult))

  console.log("Step 2: Continuing after checkpoint...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Worker finished successfully")
  process.exit(0)
}

main().catch((err) => {
  console.error("Worker error:", err)
  process.exit(1)
})
