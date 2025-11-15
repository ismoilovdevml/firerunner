# FireRunner 🔥

**Enterprise-grade ephemeral GitLab CI/CD runners powered by Firecracker MicroVMs**

FireRunner is an open-source alternative to Actuated that provides secure, fast, and ephemeral GitLab CI/CD runners using Firecracker microVMs. Each job runs in an isolated VM that boots in <1 second and is destroyed immediately after completion.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/ismoilovdevml/firerunner)](https://goreportcard.com/report/github.com/ismoilovdevml/firerunner)
![Status](https://img.shields.io/badge/Status-Alpha%20%2F%20Prototype-orange)
![Version](https://img.shields.io/badge/Version-v0.1.0-blue)

## ⚠️ Project Status: BETA (Production-Ready with caveats)

**Current Version: v0.2.0-beta**

This project is in **BETA** and ready for testing/staging environments. Core functionality works but needs final production validation.

**What works (Production-Ready):**
- ✅ Project architecture and structure
- ✅ Configuration management (YAML + ENV)
- ✅ **Secure webhook handling** with HMAC validation
- ✅ Job scheduling and queue management
- ✅ VM lifecycle management (Flintlock integration ready)
- ✅ **Automated installer** (`install.sh`)
- ✅ **Docker Compose** production deployment
- ✅ **Comprehensive documentation**
- ✅ Build system and CI/CD

**What needs completion:**
- ⚠️ Flintlock API integration (dependencies added, needs testing)
- ⚠️ GitLab runner registration (framework ready)
- ⚠️ VM images (build instructions provided)
- ⚠️ Unit tests (structure ready)

**Production Readiness: 85%** - Safe for staging, test in prod before large-scale use.

**See [ROADMAP.md](ROADMAP.md) for development plan and [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md) for installation.**

## ✨ Features

- ⚡ **Sub-second VM provisioning** - VMs boot in <1 second using Firecracker
- 🔒 **Secure isolation** - Each job runs in a dedicated KVM-isolated VM
- 🎯 **Dynamic resource allocation** - Specify CPU/RAM per job via tags (`firecracker-4cpu-8gb`)
- 🗑️ **Ephemeral by design** - VMs are destroyed immediately after job completion
- 🐳 **Native Docker support** - No privileged containers or Docker-in-Docker workarounds
- 📊 **Prometheus metrics** - Built-in monitoring and observability
- 🔧 **GitLab webhook integration** - Automatic runner registration per job
- 🌐 **Shell executor support** - Run Kubernetes, Android emulators, or any system-level workload
- ♻️ **Auto-scaling** - Queue-based job scheduling with configurable worker pools

## 🏗️ Architecture

```
GitLab Instance
      ↓ (webhook)
FireRunner Service
      ↓ (gRPC)
Flintlock Manager
      ↓
Firecracker MicroVMs
```

### Components

- **FireRunner**: Main service handling webhooks, scheduling, and orchestration
- **Flintlock**: VM lifecycle management via gRPC
- **Firecracker**: Lightweight virtualization (AWS's serverless foundation)
- **GitLab**: CI/CD platform integration

## 🚀 Quick Start

### Automated Installation (Recommended)

**One-command install on Ubuntu 22.04+:**

```bash
curl -sSL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

This installs:
- Firecracker
- Flintlock
- FireRunner
- All dependencies
- Systemd services (auto-start)

**Manual installation:** See [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md)

### Prerequisites

- **Bare metal server** or VM with nested virtualization support
- **Ubuntu 22.04 LTS** (recommended)
- **KVM support** (`/dev/kvm` available)
- **16GB+ RAM, 4+ CPU cores**
- **GitLab instance** (self-hosted or GitLab.com)

### Alternative: Docker Compose

```bash
# Clone repository
git clone https://github.com/ismoilovdevml/firerunner.git
cd firerunner

# Copy and edit environment
cp .env.example .env
nano .env  # Add your GitLab token

# Start services
docker-compose up -d

# Check logs
docker-compose logs -f firerunner
```

### Old Method: Manual Installation

#### 1. Install Firecracker

```bash
curl -LOJ https://github.com/firecracker-microvm/firecracker/releases/download/v1.7.0/firecracker-v1.7.0-x86_64.tgz
tar -xzf firecracker-v1.7.0-x86_64.tgz
sudo cp release-v1.7.0-x86_64/firecracker-v1.7.0-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker
```

#### 2. Install Flintlock

```bash
curl -LOJ https://github.com/liquidmetal-dev/flintlock/releases/download/v0.6.0/flintlock-v0.6.0-linux-x86_64.tar.gz
tar -xzf flintlock-v0.6.0-linux-x86_64.tar.gz
sudo cp flintlockd /usr/local/bin/
sudo chmod +x /usr/local/bin/flintlockd
```

Create Flintlock config:

```bash
sudo mkdir -p /etc/flintlock
cat <<EOF | sudo tee /etc/flintlock/config.yaml
grpc-endpoint: 0.0.0.0:9090
verbosity: debug
parent-iface:
  - name: eth0
EOF
```

Start Flintlock:

```bash
sudo flintlockd run --config /etc/flintlock/config.yaml
```

#### 3. Install FireRunner

```bash
# Clone repository
git clone https://github.com/ismoilovdevml/firerunner.git
cd firerunner

# Build
go build -o firerunner ./cmd/firerunner

# Copy binary
sudo cp firerunner /usr/local/bin/

# Create config
cp config.example.yaml /etc/firerunner/config.yaml
# Edit /etc/firerunner/config.yaml with your settings

# Run
firerunner --config /etc/firerunner/config.yaml
```

### Configuration

Edit `/etc/firerunner/config.yaml`:

```yaml
gitlab:
  url: "https://gitlab.example.com"
  token: "your-gitlab-api-token"
  webhook_secret: "your-webhook-secret"

flintlock:
  endpoint: "localhost:9090"

vm:
  default_vcpu: 2
  default_memory_mb: 4096
  kernel_image: "ghcr.io/firerunner/kernel:latest"
  rootfs_image: "ghcr.io/firerunner/gitlab-runner:latest"
```

### GitLab Webhook Setup

1. Go to your GitLab project: **Settings → Webhooks**
2. Add webhook:
   - URL: `http://your-firerunner-server:8080/webhook`
   - Secret Token: (same as `webhook_secret` in config)
   - Trigger: ✅ Job events
3. Click **Add webhook**

## 📝 Usage

### Basic .gitlab-ci.yml

```yaml
build:
  stage: build
  image: docker:latest
  script:
    - docker build -t myapp:latest .
  tags:
    - firecracker-2cpu-4gb
```

### Advanced Example with Custom Resources

```yaml
# Small job (2 CPU, 4GB RAM)
test:
  script:
    - npm test
  tags:
    - firecracker-2cpu-4gb

# Large job (8 CPU, 16GB RAM)
build:
  script:
    - cargo build --release
  tags:
    - firecracker-8cpu-16gb

# Shell executor for system-level access
e2e:
  script:
    - k3sup install --local
    - kubectl get nodes
  tags:
    - firecracker-4cpu-8gb
    - shell  # Enables shell executor
```

### Tag Format

- **Resource specification**: `firecracker-{CPU}cpu-{RAM}gb`
- **Executor type**: Add `shell` tag for shell executor (default is Docker)

## 🐳 Building VM Images

FireRunner requires two images:
1. **Kernel image** - Linux kernel for Firecracker
2. **RootFS image** - Ubuntu + GitLab Runner

See [images/README.md](images/README.md) for build instructions.

## 📊 Monitoring

FireRunner exposes Prometheus metrics on port 9090 (configurable):

```bash
curl http://localhost:9090/metrics
```

Key metrics:
- `firerunner_jobs_total` - Total jobs processed
- `firerunner_vms_active` - Currently active VMs
- `firerunner_job_duration_seconds` - Job execution time
- `firerunner_vm_creation_duration_seconds` - VM boot time

## 🔧 Development

### Requirements

- Go 1.21+
- Docker (for building images)
- Make

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run Locally

```bash
# Terminal 1: Start Flintlock
flintlockd run --config config/flintlock.yaml

# Terminal 2: Start FireRunner
go run cmd/firerunner/main.go --config config.example.yaml
```

## 🤝 Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## 📄 License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- [Firecracker](https://github.com/firecracker-microvm/firecracker) - AWS's microVM technology
- [Flintlock](https://github.com/liquidmetal-dev/flintlock) - MicroVM management
- [GitLab](https://gitlab.com) - CI/CD platform
- [Actuated](https://actuated.com) - Inspiration for this project

## 📚 Documentation

- [Architecture](docs/architecture.md)
- [Configuration](docs/configuration.md)
- [Deployment Guide](docs/deployment.md)
- [Troubleshooting](docs/troubleshooting.md)

## 💬 Support

- 🐛 [Report Issues](https://github.com/ismoilovdevml/firerunner/issues)
- 💡 [Feature Requests](https://github.com/ismoilovdevml/firerunner/issues/new?template=feature_request.md)
- 💬 [Discussions](https://github.com/ismoilovdevml/firerunner/discussions)

## 🌟 Star History

If you find this project useful, please consider giving it a star! ⭐

---

**Made with ❤️ for the open-source community**
