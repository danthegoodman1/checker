# Docker Checkpoint/Restore on Linux

Docker checkpoint/restore requires CRIU (Checkpoint/Restore In Userspace) and Docker experimental mode.

## Prerequisites

### 1. Install CRIU

```bash
sudo apt-get update
sudo apt-get install -y criu

# Verify installation
criu --version
```

### 2. Enable Docker Experimental Mode

```bash
# Create or edit daemon.json
sudo tee /etc/docker/daemon.json << 'EOF'
{
  "experimental": true
}
EOF

# Restart Docker
sudo systemctl restart docker
```

### 3. Configure CRIU for TCP Connections

Workers maintain HTTP connections to the hypervisor API. CRIU needs the `tcp-established` option to checkpoint processes with active TCP connections.

```bash
# Create CRIU config directory
sudo mkdir -p /etc/criu

# Add tcp-established option
echo "tcp-established" | sudo tee /etc/criu/runc.conf
```

## Verify Setup

Test that checkpoint/restore works:

```bash
# Run a test container
docker run -d --name test-checkpoint alpine sleep 1000

# Create a checkpoint
docker checkpoint create test-checkpoint cp1

# List checkpoints
docker checkpoint ls test-checkpoint

# Cleanup
docker rm -f test-checkpoint
```
