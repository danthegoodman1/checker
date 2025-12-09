# Podman Checkpoint/Restore on Linux

Podman provides native support for checkpoint/restore using CRIU, with portable checkpoint exports that can be moved between nodes.

## Prerequisites

### 1. Install Podman

```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y podman

# Fedora/RHEL
sudo dnf install -y podman

# Verify installation
podman --version
```

### 2. Install CRIU

```bash
# Ubuntu/Debian
sudo apt-get install -y criu

# Fedora/RHEL
sudo dnf install -y criu

# Verify installation
criu --version

# Test CRIU functionality
sudo criu check
```

### 3. Enable Podman Socket

The Podman runtime uses the Podman API socket for communication.

**For rootless (recommended for development):**
```bash
# Enable and start the user socket
systemctl --user enable podman.socket
systemctl --user start podman.socket

# Verify it's running
systemctl --user status podman.socket

# The socket will be at /run/user/$UID/podman/podman.sock
```

**For root (production):**
```bash
# Enable and start the system socket
sudo systemctl enable podman.socket
sudo systemctl start podman.socket

# Verify it's running
sudo systemctl status podman.socket

# The socket will be at /run/podman/podman.sock
```

## Configuration

### Environment Variables

- `PODMAN_SOCKET`: Override the default socket path (e.g., `unix:///custom/path/podman.sock`)

### Network Mode

For checkpoint/restore compatibility, use **host networking**:

```json
{
  "image": "your-worker-image:latest",
  "network": "host"
}
```

Host networking avoids network namespace issues during checkpoint/restore.

### Checkpoint Storage

Checkpoints are exported to:
```
/tmp/checker/podman-checkpoints/<execution-id>/checkpoint-<execution-id>.tar.gz
```

These are portable tar files that can be moved to other nodes for restore.

## Verify Setup

Test that checkpoint/restore works:

```bash
# Run a test container
podman run -d --name test-checkpoint --network host alpine sleep 1000

# Create a checkpoint and export it
podman container checkpoint --export=/tmp/checkpoint.tar.gz test-checkpoint

# Verify checkpoint file exists
ls -la /tmp/checkpoint.tar.gz

# Remove the original container
podman rm test-checkpoint

# Restore from the checkpoint (creates a new container)
podman container restore --import=/tmp/checkpoint.tar.gz

# Verify it's running
podman ps

# Cleanup
podman rm -f $(podman ps -q)
rm /tmp/checkpoint.tar.gz
```
