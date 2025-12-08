#!/bin/bash
# Build the Docker test image for running TestDockerCheckpointLock
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "Building checker-checkpoint-lock-test:latest..."
docker build -t checker-checkpoint-lock-test:latest -f "$PROJECT_ROOT/demo/Dockerfile.checkpoint_lock" "$PROJECT_ROOT/demo"
echo "Done!"
