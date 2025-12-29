#!/bin/bash
# Full Firecracker snapshot test: build trigger, create snapshot, restore
# Usage: ./fc-test-all.sh <kernel> [dockerfile_dir]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

KERNEL="${1:-}"
DOCKERFILE_DIR="${2:-$SCRIPT_DIR/fc-test}"

[[ -z "$KERNEL" ]] && { echo "usage: $0 <kernel_path> [dockerfile_dir]"; exit 1; }
[[ ! -f "$KERNEL" ]] && { echo "error: kernel not found: $KERNEL"; exit 1; }
[[ ! -f "$DOCKERFILE_DIR/Dockerfile" ]] && { echo "error: no Dockerfile in $DOCKERFILE_DIR"; exit 1; }

echo "=== Building fc-trigger ==="
"$SCRIPT_DIR/fc-trigger/build.sh"

echo ""
echo "=== Creating Snapshot ==="
"$SCRIPT_DIR/fc-snapshot-test-rust.sh" create "$DOCKERFILE_DIR" "$KERNEL"

echo ""
echo "=== Restoring Snapshot ==="
"$SCRIPT_DIR/fc-snapshot-test-rust.sh" restore "$KERNEL"

