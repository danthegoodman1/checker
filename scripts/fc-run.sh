#!/bin/bash
# Build Dockerfile and run in Firecracker, capturing output.
# Usage: fc-run.sh <dir_with_dockerfile> <kernel_path>
#
# Dependencies: buildah, skopeo, umoci, e2fsprogs, firecracker, jq

set -euo pipefail

die() { echo "error: $1" >&2; exit 1; }

DIR="${1:-}"
KERNEL="${2:-}"

[[ -z "$DIR" || -z "$KERNEL" ]] && die "usage: $0 <dir_with_dockerfile> <kernel_path>"
[[ ! -f "$DIR/Dockerfile" ]] && die "no Dockerfile in $DIR"
[[ ! -f "$KERNEL" ]] && die "kernel not found: $KERNEL"
command -v jq &>/dev/null || die "jq not found"

WORK=$(mktemp -d)
trap "rm -rf $WORK; buildah rm \$(buildah containers -q) &>/dev/null || true" EXIT

ROOTFS="$WORK/rootfs.ext4"
SOCKET="$WORK/fc.sock"
IMG="fc-run-$$"

# Build image
buildah bud -t "$IMG" "$DIR" >/dev/null 2>&1

# Export to OCI layout and extract config
skopeo copy "containers-storage:localhost/$IMG" "oci:$WORK/oci:latest" >/dev/null
CONFIG=$(skopeo inspect --config "oci:$WORK/oci:latest")
WORKDIR=$(echo "$CONFIG" | jq -r '.config.WorkingDir // "/"')
ENTRYPOINT=$(echo "$CONFIG" | jq -r '(.config.Entrypoint // []) | map(@sh) | join(" ")')
CMD=$(echo "$CONFIG" | jq -r '(.config.Cmd // []) | map(@sh) | join(" ")')
ENV_VARS=$(echo "$CONFIG" | jq -r '(.config.Env // []) | .[] | split("=") | "export \(.[0])=\"\(.[1:] | join("="))\"" ')

# Unpack to rootfs
umoci unpack --image "$WORK/oci:latest" "$WORK/bundle" >/dev/null
FS="$WORK/bundle/rootfs"

# Prepare for Firecracker
mkdir -p "$FS"/{dev,proc,sys,run,tmp}
[[ ! -s "$FS/etc/resolv.conf" ]] && printf "nameserver 8.8.8.8\n" > "$FS/etc/resolv.conf"

# Generate init from image's ENTRYPOINT/CMD
FULL_CMD="$ENTRYPOINT $CMD"
cat > "$FS/init" << INIT
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null || true
$ENV_VARS
cd $WORKDIR
echo "running: $FULL_CMD" > /dev/console
$FULL_CMD > /dev/console 2>&1
EXIT_CODE=\$?
echo "--- exited with code \$EXIT_CODE ---" > /dev/console
reboot -f
INIT
chmod +x "$FS/init"

# Create ext4
SIZE_MB=$(( $(du -sm "$FS" | cut -f1) * 2 ))
[[ $SIZE_MB -lt 64 ]] && SIZE_MB=64
truncate -s "${SIZE_MB}M" "$ROOTFS"
mkfs.ext4 -F -L rootfs -O ^metadata_csum,^64bit -d "$FS" "$ROOTFS" >/dev/null
e2fsck -fy "$ROOTFS" &>/dev/null || true
resize2fs -M "$ROOTFS" &>/dev/null || true

# Cleanup image
buildah rmi "$IMG" &>/dev/null || true

# Run Firecracker (stdout/stderr = serial console output)
firecracker --api-sock "$SOCKET" &
FC_PID=$!
sleep 0.3

api() { curl -s --unix-socket "$SOCKET" -X PUT "http://localhost/$1" -H "Content-Type: application/json" -d "$2"; }

api "boot-source" "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off init=/init\"}"
api "drives/rootfs" "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$ROOTFS\",\"is_root_device\":true,\"is_read_only\":false}"
api "machine-config" "{\"vcpu_count\":1,\"mem_size_mib\":512}"
api "actions" '{"action_type":"InstanceStart"}' >/dev/null

wait $FC_PID 2>/dev/null
echo "exit_code: $?"
