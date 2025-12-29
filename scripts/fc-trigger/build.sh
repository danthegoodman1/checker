#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

docker run --rm --platform linux/amd64 -v "$SCRIPT_DIR":/src -w /src rust:alpine sh -c '
    apk add --no-cache musl-dev &&
    cargo build --release
'

echo "Binary built at: target/release/fc-trigger"
ls -lh target/release/fc-trigger
file target/release/fc-trigger
