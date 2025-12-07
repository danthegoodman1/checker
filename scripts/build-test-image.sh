#!/bin/bash
# Build the Docker test image for running TestDockerWorker
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "Building checker-worker-test:latest..."
docker build -t checker-worker-test:latest "$PROJECT_ROOT/demo"
echo "Done!"
