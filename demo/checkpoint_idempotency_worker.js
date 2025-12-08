// Test worker for checkpoint idempotency
// Verifies that duplicate tokens don't cause duplicate checkpoints

const jobId = process.env.CHECKER_JOB_ID || "unknown"
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown"
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown"
const apiUrl = process.env.CHECKER_API_URL

console.log(
  `Checkpoint idempotency test worker starting... Job ID: ${jobId}, Definition: ${defName}@${defVersion}`
)

async function checkpointWithToken(token) {
  const resp = await fetch(`http://${apiUrl}/jobs/${jobId}/checkpoint`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  })
  if (!resp.ok) {
    throw new Error(`Checkpoint failed: ${resp.status} ${await resp.text()}`)
  }
  return await resp.json()
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

async function main() {
  const tokenA = "test-token-A-" + Date.now()
  const tokenB = "test-token-B-" + Date.now()

  // First checkpoint with token A
  console.log(`Checkpoint 1: token=${tokenA}`)
  const result1 = await checkpointWithToken(tokenA)
  const count1 = result1.job.CheckpointCount
  console.log(`  Result: CheckpointCount=${count1}`)

  // Second checkpoint with SAME token A - should be idempotent (count stays same)
  console.log(`Checkpoint 2: token=${tokenA} (duplicate)`)
  const result2 = await checkpointWithToken(tokenA)
  const count2 = result2.job.CheckpointCount
  console.log(`  Result: CheckpointCount=${count2}`)

  // Third checkpoint with DIFFERENT token B - should increment
  console.log(`Checkpoint 3: token=${tokenB}`)
  const result3 = await checkpointWithToken(tokenB)
  const count3 = result3.job.CheckpointCount
  console.log(`  Result: CheckpointCount=${count3}`)

  // Fourth checkpoint with token A again - still idempotent
  console.log(`Checkpoint 4: token=${tokenA} (duplicate again)`)
  const result4 = await checkpointWithToken(tokenA)
  const count4 = result4.job.CheckpointCount
  console.log(`  Result: CheckpointCount=${count4}`)

  // Verify results
  const success =
    count1 === 1 && // First checkpoint
    count2 === 1 && // Duplicate of A - no increment
    count3 === 2 && // New token B - increments
    count4 === 2    // Duplicate of A again - no increment

  console.log(`\nIdempotency test ${success ? "PASSED" : "FAILED"}`)
  console.log(`  Expected: [1, 1, 2, 2]`)
  console.log(`  Got:      [${count1}, ${count2}, ${count3}, ${count4}]`)

  await exit(success ? 0 : 1, {
    success,
    checkpoint_counts: [count1, count2, count3, count4],
    expected: [1, 1, 2, 2],
  })
}

main().catch((err) => {
  console.error("Worker error:", err)
  exit(1, { error: err.message })
})
