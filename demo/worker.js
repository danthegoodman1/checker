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

  // Check for crash simulation
  if (params.crash === "before_checkpoint") {
    console.log("Simulating crash before checkpoint...")
    nonExistentFunction()
  }

  console.log("Step 1: Starting work...")
  const step1Result = { step: 1, value: 2 + 2 }
  console.log("Step 1 complete:", step1Result)

  console.log("Checkpointing...")
  const checkpointResult = await checkpoint()
  console.log("Checkpoint complete:", JSON.stringify(checkpointResult))

  if (params.crash === "after_checkpoint") {
    console.log("Simulating crash after checkpoint...")
    nonExistentFunction()
  }

  console.log("Step 2: Continuing after checkpoint...")
  const step2Result = { step: 2, value: step1Result.value * 2 }
  console.log("Step 2 complete:", step2Result)

  console.log("Worker finished successfully")
  await exit(0, { result: step2Result })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
