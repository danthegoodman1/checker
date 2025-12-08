#!/bin/bash
# Build the Docker test image for running TestDockerCheckpointRestore
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "Building checker-checkpoint-restore-test:latest..."
docker build -t checker-checkpoint-restore-test:latest -f "$PROJECT_ROOT/demo/Dockerfile.checkpoint_restore" "$PROJECT_ROOT/demo"
echo "Done!"
