# Building Production-Ready VM Images

## Overview

FireRunner needs two images for each MicroVM:
1. **Kernel Image** - Linux kernel for Firecracker
2. **RootFS Image** - Ubuntu + GitLab Runner + Docker

## Prerequisites

- Docker installed
- 10GB free disk space
- Root/sudo access
- Internet connection

## Step 1: Build RootFS Image

### 1.1 Build Docker Image

```bash
cd images/rootfs

# Build the image
docker build -t firerunner/gitlab-runner:latest .

# Test locally
docker run -it --rm firerunner/gitlab-runner:latest /bin/bash
```

### 1.2 Convert to Firecracker-compatible Format

Firecracker needs a raw ext4 filesystem image, not a Docker image.

```bash
# Export Docker image to tar
docker export $(docker create firerunner/gitlab-runner:latest) > rootfs.tar

# Create ext4 filesystem image (2GB)
dd if=/dev/zero of=rootfs.ext4 bs=1M count=2048

# Format as ext4
mkfs.ext4 -F rootfs.ext4

# Mount and extract
mkdir -p /mnt/rootfs
sudo mount rootfs.ext4 /mnt/rootfs
sudo tar -xf rootfs.tar -C /mnt/rootfs
sudo umount /mnt/rootfs

# Cleanup
rm rootfs.tar

# Upload to container registry (as OCI image)
# Flintlock expects OCI image format
docker build -t ghcr.io/ismoilovdevml/firerunner-rootfs:latest -f - . <<EOF
FROM scratch
ADD rootfs.ext4 /disk.img
EOF

docker push ghcr.io/ismoilovdevml/firerunner-rootfs:latest
```

**Easier Method (Recommended):**

Use Flintlock's `builder` tool:

```bash
# Install flintlock-builder
go install github.com/liquidmetal-dev/flintlock/tools/builder@latest

# Build rootfs from Dockerfile
builder build \
  --dockerfile images/rootfs/Dockerfile \
  --output-image ghcr.io/ismoilovdevml/firerunner-rootfs:latest \
  --size 2G
```

## Step 2: Build Kernel Image

### 2.1 Download Pre-built Kernel

**Option A: Use Flintlock's Pre-built Kernel (Easiest)**

```bash
# Flintlock provides ready-to-use kernels
# No build needed!
KERNEL_IMAGE="ghcr.io/liquidmetal-dev/flintlock-kernel:5.10"
```

Update `config.yaml`:
```yaml
vm:
  kernel_image: "ghcr.io/liquidmetal-dev/flintlock-kernel:5.10"
  rootfs_image: "ghcr.io/ismoilovdevml/firerunner-rootfs:latest"
```

**Option B: Build Custom Kernel (Advanced)**

```bash
cd images/kernel

# Download kernel source
wget https://cdn.kernel.org/pub/linux/kernel/v5.x/linux-5.10.186.tar.xz
tar -xf linux-5.10.186.tar.xz
cd linux-5.10.186

# Use Firecracker minimal config
curl -o .config https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-x86_64-5.10.config

# Build kernel
make -j$(nproc) vmlinux

# Copy kernel
cp vmlinux ../../vmlinux-5.10

# Package as OCI image
cd ../..
docker build -t ghcr.io/ismoilovdevml/firerunner-kernel:5.10 -f - . <<EOF
FROM scratch
ADD vmlinux-5.10 /vmlinux
EOF

docker push ghcr.io/ismoilovdevml/firerunner-kernel:5.10
```

### 2.2 Kernel Configuration for Firerunner

Key kernel features needed:
- `CONFIG_VIRTIO=y` - Virtio drivers
- `CONFIG_VIRTIO_BLK=y` - Block device
- `CONFIG_VIRTIO_NET=y` - Network device
- `CONFIG_OVERLAY_FS=y` - Docker overlay
- `CONFIG_NAMESPACES=y` - Containers
- `CONFIG_CGROUPS=y` - Resource limits

## Step 3: Test Images Locally

### 3.1 Test with Flintlock

```bash
# Start Flintlock
sudo flintlockd run --config /etc/flintlock/config.yaml

# Create test VM via gRPC
grpcurl -plaintext \
  -d '{
    "microvm": {
      "id": "test-vm",
      "namespace": "test",
      "vcpu": 2,
      "memory_in_mb": 2048,
      "kernel": {
        "image": "ghcr.io/liquidmetal-dev/flintlock-kernel:5.10",
        "filename": "vmlinux"
      },
      "root_volume": {
        "id": "root",
        "source": {
          "container_source": "ghcr.io/ismoilovdevml/firerunner-rootfs:latest"
        }
      }
    }
  }' \
  localhost:9090 \
  microvm.services.api.v1alpha1.MicroVMService/CreateMicroVM
```

### 3.2 Verify VM Boots

```bash
# Check VM status
grpcurl -plaintext \
  -d '{"id": "test-vm", "namespace": "test"}' \
  localhost:9090 \
  microvm.services.api.v1alpha1.MicroVMService/GetMicroVM

# Should show state: CREATED
```

## Step 4: Production Image Pipeline

### 4.1 Automated Build (GitHub Actions)

Create `.github/workflows/build-images.yml`:

```yaml
name: Build VM Images

on:
  push:
    paths:
      - 'images/**'
    branches: [main]

jobs:
  build-rootfs:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Build RootFS
        run: |
          cd images/rootfs
          docker build -t ghcr.io/${{ github.repository }}/rootfs:${{ github.sha }} .
          docker tag ghcr.io/${{ github.repository }}/rootfs:${{ github.sha }} \
                     ghcr.io/${{ github.repository }}/rootfs:latest

      - name: Login to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Push Images
        run: |
          docker push ghcr.io/${{ github.repository }}/rootfs:${{ github.sha }}
          docker push ghcr.io/${{ github.repository }}/rootfs:latest
```

## Step 5: Image Versioning

Use semantic versioning:

```
ghcr.io/ismoilovdevml/firerunner-rootfs:v1.0.0
ghcr.io/ismoilovdevml/firerunner-rootfs:latest
ghcr.io/ismoilovdevml/firerunner-kernel:5.10
```

Update on changes:
- **Major**: Breaking changes (new kernel version)
- **Minor**: New features (pre-installed packages)
- **Patch**: Bug fixes, security updates

## Common Issues

### Issue: Image too large

**Solution**: Multi-stage build
```dockerfile
FROM ubuntu:22.04 AS builder
RUN apt-get update && apt-get install -y build-essential
# ... build steps

FROM ubuntu:22.04
COPY --from=builder /app/binary /usr/local/bin/
# Only copy what's needed
```

### Issue: Slow boot time

**Solution**:
- Remove unnecessary packages
- Disable unused systemd services
- Pre-pull Docker images

### Issue: Docker-in-Docker not working

**Solution**: Ensure these in rootfs:
```dockerfile
RUN systemctl enable docker
RUN usermod -aG docker gitlab-runner
```

## Production Checklist

- [ ] RootFS image < 2GB
- [ ] Kernel image tested with Firecracker
- [ ] Images pushed to registry
- [ ] Images tagged with version
- [ ] Boot time < 3 seconds
- [ ] Docker works inside VM
- [ ] GitLab runner can register
- [ ] Automated build pipeline
- [ ] Security scanning (Trivy)
- [ ] Image signing (cosign)

## Next Steps

After images are ready:
1. Update `config.yaml` with image URLs
2. Test with FireRunner
3. Run production workload
4. Monitor performance
5. Iterate and improve
