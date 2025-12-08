# Docker Checkpoint/Restore on Linux

Docker checkpoint/restore uses CRIU (Checkpoint/Restore In Userspace) to save and restore the complete process state, including memory, file descriptors, and more. This enables true pause/resume of containerized workers.

## Prerequisites

### 1. Install CRIU

```bash
sudo apt-get update
sudo apt-get install -y criu

# Verify installation
criu --version

# Test CRIU functionality
sudo criu check
```

### 2. Enable Docker Experimental Mode

Docker checkpoint is an experimental feature that must be explicitly enabled:

```bash
# Create or edit daemon.json
sudo tee /etc/docker/daemon.json << 'EOF'
{
  "experimental": true
}
EOF

# Restart Docker
sudo systemctl restart docker

# Verify experimental mode is enabled
docker version --format '{{.Server.Experimental}}'
# Should output: true
```

### 3. Configure CRIU for TCP Connections

Workers maintain HTTP connections to the hypervisor API during checkpoint. CRIU needs the `tcp-established` option to checkpoint processes with active TCP connections:

```bash
# Create CRIU config directory
sudo mkdir -p /etc/criu

# Add tcp-established option for runc
echo "tcp-established" | sudo tee /etc/criu/runc.conf
```

**Note:** After restore, the TCP connection will be stale (the server has moved on). Workers should implement retry logic with idempotency tokens to handle this gracefully.

## Container Configuration

### Network Mode

**Important:** Use `network: "host"` for containers that will be checkpointed. This avoids network namespace issues where Docker cleans up the network namespace after checkpoint, causing restore to fail with:

```
bind-mount /proc/0/ns/net -> /var/run/docker/netns/xxx: no such file or directory
```

Example configuration:
```json
{
  "image": "your-worker-image:latest",
  "network": "host"
}
```

With host networking, the container shares the host's network stack, so network namespaces don't need to be saved/restored.

### Checkpoint Storage

Checkpoints are stored in Docker's default location:
```
/var/lib/docker/containers/<container-id>/checkpoints/<checkpoint-name>/
```

Custom checkpoint directories are **not supported** by Docker's containerd-based restore.

## Verify Setup

Test that checkpoint/restore works:

```bash
# Run a test container with host networking
docker run -d --name test-checkpoint --network host alpine sleep 1000

# Create a checkpoint (container will stop)
docker checkpoint create test-checkpoint cp1

# List checkpoints
docker checkpoint ls test-checkpoint

# Restore from checkpoint
docker start --checkpoint cp1 test-checkpoint

# Verify it's running
docker ps | grep test-checkpoint

# Cleanup
docker rm -f test-checkpoint
```
