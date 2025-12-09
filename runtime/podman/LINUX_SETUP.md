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

### 3. Configure Podman for Checkpoint/Restore

Podman needs to be configured to use `runc` as the OCI runtime and `cgroupfs` as the cgroup manager. The default `systemd` cgroup manager has issues with checkpoint/restore where the systemd transient scope gets cleaned up immediately after restore, causing containers to exit.

```bash
sudo mkdir -p /etc/containers
sudo tee /etc/containers/containers.conf << 'EOF'
[engine]
runtime = "runc"
cgroup_manager = "cgroupfs"
EOF

# Verify the settings
podman info --format 'Runtime: {{.Host.OCIRuntime.Path}}, CgroupManager: {{.Host.CgroupManager}}'
```

### 4. Verify Checkpoint/Restore Works

```bash
# Start a test container
podman run -d --name criu-test --network=host alpine sleep 1000

# Checkpoint and export
podman container checkpoint criu-test --export /tmp/criu-test.tar.gz

# Remove original container
podman rm -f criu-test

# Restore from checkpoint (name is preserved from the checkpoint)
podman container restore --import /tmp/criu-test.tar.gz

# Verify container is running (should show "Up X seconds")
podman ps --filter name=criu-test

# Cleanup
podman rm -f criu-test
rm /tmp/criu-test.tar.gz
```

## Troubleshooting

### Container exits immediately after restore

If containers exit with code 0 immediately after restore, check the cgroup manager:

```bash
podman info --format '{{.Host.CgroupManager}}'
```

If it shows `systemd`, update `/etc/containers/containers.conf` to use `cgroupfs` as shown above.

### CRIU check fails

Run `sudo criu check` to see what's missing. Common issues:
- Missing kernel configuration options
- Need root or specific capabilities (CAP_SYS_PTRACE, etc.)
- SELinux/AppArmor blocking CRIU operations
