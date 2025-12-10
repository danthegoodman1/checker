#!/bin/bash
# Test Firecracker snapshot/restore functionality
# Usage:
#   ./fc-snapshot-test.sh create <dir_with_dockerfile> <kernel>  - Build image, boot VM, snapshot when ready
#   ./fc-snapshot-test.sh restore <kernel>                       - Restore from snapshot and run
#
# Dependencies: buildah, skopeo, umoci, e2fsprogs, firecracker, jq

set -euo pipefail

die() { echo "error: $1" >&2; exit 1; }

CMD="${1:-}"

# Persistent snapshot directory
SNAP_DIR="$HOME/.fc-snapshots"
mkdir -p "$SNAP_DIR"

SNAPSHOT_FILE="$SNAP_DIR/snapshot"
MEM_FILE="$SNAP_DIR/mem"
ROOTFS="$SNAP_DIR/rootfs.ext4"

WORK=$(mktemp -d)
trap "rm -rf $WORK; buildah rm \$(buildah containers -q) &>/dev/null || true" EXIT
SOCKET="$WORK/fc.sock"

api() { 
    curl -s --unix-socket "$SOCKET" -X PUT "http://localhost/$1" \
        -H "Content-Type: application/json" -d "$2"
}

api_patch() {
    curl -s --unix-socket "$SOCKET" -X PATCH "http://localhost/$1" \
        -H "Content-Type: application/json" -d "$2"
}

wait_socket() {
    for _ in {1..100}; do [[ -S "$SOCKET" ]] && return 0; sleep 0.01; done
    die "socket timeout"
}

build_rootfs() {
    local DIR="$1"
    local IMG="fc-snapshot-$$"
    
    echo "Building image from $DIR..."
    buildah bud -t "$IMG" "$DIR" >/dev/null 2>&1
    
    # Export to OCI layout and extract config
    skopeo copy "containers-storage:localhost/$IMG" "oci:$WORK/oci:latest" >/dev/null
    CONFIG=$(skopeo inspect --config "oci:$WORK/oci:latest")
    WORKDIR=$(echo "$CONFIG" | jq -r '.config.WorkingDir // "/"')
    ENTRYPOINT=$(echo "$CONFIG" | jq -r '(.config.Entrypoint // []) | map(@sh) | join(" ")')
    CMD_ARGS=$(echo "$CONFIG" | jq -r '(.config.Cmd // []) | map(@sh) | join(" ")')
    ENV_VARS=$(echo "$CONFIG" | jq -r '(.config.Env // []) | .[] | split("=") | "export \(.[0])=\"\(.[1:] | join("="))\"" ')
    
    # Unpack to rootfs
    umoci unpack --image "$WORK/oci:latest" "$WORK/bundle" >/dev/null
    FS="$WORK/bundle/rootfs"
    
    # Prepare for Firecracker
    mkdir -p "$FS"/{dev,proc,sys,run,tmp}
    [[ ! -s "$FS/etc/resolv.conf" ]] && printf "nameserver 8.8.8.8\n" > "$FS/etc/resolv.conf"
    
    # Create marker file with the command to run (read after restore)
    cat > "$FS/.fc_cmd" <<EOF
$ENV_VARS
cd $WORKDIR
$ENTRYPOINT $CMD_ARGS
EOF
    
    # Generate init that supports snapshot workflow
    cat > "$FS/init" <<'INIT_EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null || true
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Check if this is a snapshot restore (marker file exists)
if [ -f /.fc_restored ]; then
    # Restored from snapshot - run the command
    . /.fc_cmd
    EXIT_CODE=$?
    echo "--- exit: $EXIT_CODE ---"
    reboot -f
fi

# First boot - signal ready and wait for snapshot
echo "===SNAPSHOT_READY==="

# Busy-wait checking for restore marker
# (The snapshot will be taken while we're in this loop)
while true; do
    if [ -f /.fc_restored ]; then
        . /.fc_cmd
        EXIT_CODE=$?
        echo "--- exit: $EXIT_CODE ---"
        reboot -f
    fi
    sleep 0.001
done
INIT_EOF
    chmod +x "$FS/init"
    
    # Cleanup image
    buildah rmi "$IMG" &>/dev/null || true
    
    # Create ext4
    SIZE_MB=$(( $(du -sm "$FS" | cut -f1) * 2 ))
    [[ $SIZE_MB -lt 64 ]] && SIZE_MB=64
    rm -f "$ROOTFS"
    truncate -s "${SIZE_MB}M" "$ROOTFS"
    mkfs.ext4 -F -L rootfs -O ^metadata_csum,^64bit -d "$FS" "$ROOTFS" >/dev/null
    e2fsck -fy "$ROOTFS" &>/dev/null || true
    resize2fs -M "$ROOTFS" &>/dev/null || true
    
    echo "Rootfs created: $ROOTFS"
}

cmd_create() {
    local DIR="${2:-}"
    local KERNEL="${3:-}"
    
    [[ -z "$DIR" || -z "$KERNEL" ]] && die "usage: $0 create <dir_with_dockerfile> <kernel_path>"
    [[ ! -f "$DIR/Dockerfile" ]] && die "no Dockerfile in $DIR"
    [[ ! -f "$KERNEL" ]] && die "kernel not found: $KERNEL"
    
    echo "=== Creating Snapshot ==="
    
    # Build rootfs from Dockerfile
    build_rootfs "$DIR"
    
    # Start Firecracker
    firecracker --api-sock "$SOCKET" --level Error &
    FC_PID=$!
    wait_socket
    
    # Configure VM
    api "boot-source" "{
        \"kernel_image_path\": \"$KERNEL\",
        \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off init=/init nomodule audit=0 tsc=reliable no_timer_check noreplace-smp 8250.nr_uarts=1\"
    }"
    api "drives/rootfs" "{
        \"drive_id\": \"rootfs\",
        \"path_on_host\": \"$ROOTFS\",
        \"is_root_device\": true,
        \"is_read_only\": false
    }"
    api "machine-config" '{"vcpu_count":1,"mem_size_mib":512}'
    
    echo "Booting VM..."
    api "actions" '{"action_type":"InstanceStart"}' >/dev/null
    
    # Wait for SNAPSHOT_READY signal
    echo "Waiting for VM to be ready..."
    sleep 2  # Give time for boot + init to reach ready state
    
    # Pause VM
    echo "Pausing VM..."
    api_patch "vm" '{"state":"Paused"}'
    
    # Create snapshot
    echo "Creating snapshot..."
    rm -f "$SNAPSHOT_FILE" "$MEM_FILE"
    api "snapshot/create" "{
        \"snapshot_type\": \"Full\",
        \"snapshot_path\": \"$SNAPSHOT_FILE\",
        \"mem_file_path\": \"$MEM_FILE\"
    }"
    
    # Cleanup
    kill $FC_PID 2>/dev/null || true
    wait $FC_PID 2>/dev/null || true
    
    echo ""
    echo "=== Snapshot Created ==="
    echo "Snapshot: $SNAPSHOT_FILE ($(du -h "$SNAPSHOT_FILE" | cut -f1))"
    echo "Memory:   $MEM_FILE ($(du -h "$MEM_FILE" | cut -f1))"
    echo "Rootfs:   $ROOTFS ($(du -h "$ROOTFS" | cut -f1))"
    echo ""
    echo "Test restore with: ./fc-snapshot-test.sh restore $KERNEL"
}

cmd_restore() {
    local KERNEL="${2:-}"
    
    [[ -z "$KERNEL" ]] && die "usage: $0 restore <kernel_path>"
    [[ ! -f "$KERNEL" ]] && die "kernel not found: $KERNEL"
    [[ ! -f "$SNAPSHOT_FILE" ]] && die "No snapshot found. Run 'create' first."
    [[ ! -f "$MEM_FILE" ]] && die "No memory file found. Run 'create' first."
    [[ ! -f "$ROOTFS" ]] && die "No rootfs found. Run 'create' first."
    
    echo "=== Restoring from Snapshot ==="
    
    # Create a fresh copy of rootfs with restore marker
    RESTORE_ROOTFS="$WORK/rootfs.ext4"
    cp "$ROOTFS" "$RESTORE_ROOTFS"
    
    # Add restore marker file using debugfs
    echo "restored" > "$WORK/marker"
    debugfs -w -R "write $WORK/marker /.fc_restored" "$RESTORE_ROOTFS" 2>/dev/null
    
    # Start Firecracker
    firecracker --api-sock "$SOCKET" --level Error &
    FC_PID=$!
    wait_socket
    
    # Time the restore
    START_TIME=$(python3 -c 'import time; print(time.time())')
    
    # Restore from snapshot
    api "snapshot/load" "{
        \"snapshot_path\": \"$SNAPSHOT_FILE\",
        \"mem_backend\": {
            \"backend_type\": \"File\",
            \"backend_path\": \"$MEM_FILE\"
        },
        \"enable_diff_snapshots\": false
    }"
    
    # Update the drive to use our modified rootfs
    api_patch "drives/rootfs" "{
        \"drive_id\": \"rootfs\",
        \"path_on_host\": \"$RESTORE_ROOTFS\"
    }"
    
    # Resume
    api_patch "vm" '{"state":"Resumed"}'
    
    END_TIME=$(python3 -c 'import time; print(time.time())')
    RESTORE_MS=$(python3 -c "print(f'{($END_TIME - $START_TIME) * 1000:.1f}')")
    
    echo "Restore + resume time: ${RESTORE_MS}ms"
    echo ""
    echo "--- VM Output ---"
    
    # Wait for completion
    wait $FC_PID 2>/dev/null || true
    
    echo ""
    echo "=== Restore Complete ==="
}

case "${CMD:-}" in
    create)  cmd_create "$@" ;;
    restore) cmd_restore "$@" ;;
    *)       die "usage: $0 <create|restore> ..." ;;
esac
