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

### 3. Convert Podman to runc

```bash
sudo mkdir -p /etc/containers

# Edit (or create) /etc/containers/containers.conf
sudo nano /etc/containers/containers.conf
```

Add the following configuration:

```toml
[engine]
runtime = "runc"
```

Verify the runtime is set correctly:

```bash
podman info --format '{{.Host.OCIRuntime.Path}}'
```
