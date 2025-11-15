# GitLab Runner RootFS for Firecracker MicroVM
# This image runs inside Firecracker VMs and executes GitLab jobs

FROM ubuntu:22.04

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install base packages
RUN apt-get update && apt-get install -y \
    curl \
    wget \
    git \
    ca-certificates \
    openssh-client \
    apt-transport-https \
    gnupg \
    lsb-release \
    software-properties-common \
    systemd \
    systemd-sysv \
    cloud-init \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# Install Docker
RUN curl -fsSL https://get.docker.com -o get-docker.sh && \
    sh get-docker.sh && \
    rm get-docker.sh && \
    usermod -aG docker root

# Install GitLab Runner
RUN curl -L "https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh" | bash && \
    apt-get update && \
    apt-get install -y gitlab-runner && \
    rm -rf /var/lib/apt/lists/*

# Install common build tools
RUN apt-get update && apt-get install -y \
    build-essential \
    make \
    cmake \
    pkg-config \
    libssl-dev \
    python3 \
    python3-pip \
    nodejs \
    npm \
    && rm -rf /var/lib/apt/lists/*

# Create runner user
RUN useradd -m -s /bin/bash runner && \
    usermod -aG docker runner && \
    echo "runner ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Configure systemd
RUN systemctl enable docker && \
    systemctl enable gitlab-runner

# Cloud-init configuration for Firecracker metadata
COPY cloud-init.yaml /etc/cloud/cloud.cfg.d/99-firerunner.cfg

# Runner auto-registration script
COPY register-runner.sh /usr/local/bin/register-runner.sh
RUN chmod +x /usr/local/bin/register-runner.sh

# Startup script
COPY startup.sh /usr/local/bin/startup.sh
RUN chmod +x /usr/local/bin/startup.sh

# Cleanup
RUN apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Set systemd as the init process
CMD ["/lib/systemd/systemd"]
