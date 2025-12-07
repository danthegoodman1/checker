// Demo worker script for testing the checker hypervisor

console.log("Worker starting...");

// Read job metadata from environment
const jobId = process.env.CHECKER_JOB_ID || "unknown";
const defName = process.env.CHECKER_JOB_DEFINITION_NAME || "unknown";
const defVersion = process.env.CHECKER_JOB_DEFINITION_VERSION || "unknown";

console.log(`Job ID: ${jobId}`);
console.log(`Definition: ${defName}@${defVersion}`);
console.log(`API URL: ${process.env.CHECKER_API_URL}`);

// Simulate some work
const result = {
  message: "Hello from worker!",
  timestamp: new Date().toISOString(),
  computed: 2 + 2,
};

console.log("Work completed:", JSON.stringify(result));
console.log("Worker exiting with code 0");

// Exit successfully
process.exit(0);
