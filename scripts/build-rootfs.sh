#!/bin/bash
# Convert OCI image to Firecracker ext4 rootfs
# Usage: build-rootfs.sh <image_ref> [output_path]
# Example: build-rootfs.sh docker://node:20-alpine ./rootfs.ext4
#
# Dependencies:
#   - skopeo (install: apt/dnf install skopeo)
#   - umoci (install: apt/dnf install umoci)
#   - e2fsprogs (usually pre-installed on Linux)

set -euo pipefail

die() { echo "error: $1" >&2; exit 1; }

IMAGE_REF="${1:-}"
OUTPUT_PATH="${2:-./rootfs.ext4}"

[[ -z "$IMAGE_REF" ]] && die "usage: $0 <image_ref> [output_path]"
command -v skopeo &>/dev/null || die "skopeo not found"
command -v umoci &>/dev/null || die "umoci not found"
command -v mkfs.ext4 &>/dev/null || die "mkfs.ext4 not found"

TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Pull image to OCI layout
skopeo copy "$IMAGE_REF" "oci:$TEMP_DIR/image:latest" >/dev/null

# Unpack to rootfs
umoci unpack --image "$TEMP_DIR/image:latest" "$TEMP_DIR/bundle" >/dev/null
ROOTFS="$TEMP_DIR/bundle/rootfs"

# Prepare for Firecracker
mkdir -p "$ROOTFS"/{dev,proc,sys,run,tmp}
[[ ! -s "$ROOTFS/etc/resolv.conf" ]] && printf "nameserver 8.8.8.8\n" > "$ROOTFS/etc/resolv.conf"

# Create ext4 (start 2x content size, then shrink to minimum)
SIZE_MB=$(( $(du -sm "$ROOTFS" | cut -f1) * 2 ))
[[ $SIZE_MB -lt 64 ]] && SIZE_MB=64

truncate -s "${SIZE_MB}M" "$OUTPUT_PATH"
mkfs.ext4 -F -L rootfs -O ^metadata_csum,^64bit -d "$ROOTFS" "$OUTPUT_PATH" >/dev/null
e2fsck -fy "$OUTPUT_PATH" &>/dev/null || true
resize2fs -M "$OUTPUT_PATH" &>/dev/null || true

echo "$OUTPUT_PATH"
