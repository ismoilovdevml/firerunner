# Root filesystem for FireRunner job microVMs.
# Flintlock turns this image into the VM's root disk and boots /sbin/init (systemd).
ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG TARGETARCH=amd64
ARG GITLAB_RUNNER_VERSION=19.4.0

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv udev dbus kmod \
        cloud-init openssh-server sudo \
        iproute2 iputils-ping dnsutils ca-certificates \
        curl wget git git-lfs jq unzip zip xz-utils tar gzip make python3 \
        iptables gnupg \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Docker CE from Docker's own repository (current stable, with buildx and compose).
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && echo "deb [arch=${TARGETARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
        >/etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# gitlab-runner inside the guest is used by the custom executor for artifacts and cache.
RUN curl -fsSL -o /usr/local/bin/gitlab-runner \
        "https://gitlab-runner-downloads.s3.amazonaws.com/v${GITLAB_RUNNER_VERSION}/binaries/gitlab-runner-linux-${TARGETARCH}" \
    && curl -fsSL "https://gitlab-runner-downloads.s3.amazonaws.com/v${GITLAB_RUNNER_VERSION}/release.sha256" \
        | awk -v f="binaries/gitlab-runner-linux-${TARGETARCH}" '$2 == f {print $1 "  /usr/local/bin/gitlab-runner"}' | sha256sum -c - \
    && chmod 0755 /usr/local/bin/gitlab-runner

# /etc/resolv.conf is bind-mounted during the build, so link it at boot instead.
RUN printf 'L+ /etc/resolv.conf - - - - /run/systemd/resolve/stub-resolv.conf\n' >/etc/tmpfiles.d/resolv.conf \
    # Flintlock serves meta-data/user-data over MMDS (ds=nocloud-net on the kernel cmdline).
    && printf 'datasource_list: [ NoCloud, None ]\n' >/etc/cloud/cloud.cfg.d/90-firerunner.cfg \
    # Jobs are short-lived: skip timers and units that only slow boot down.
    && systemctl mask apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service \
        motd-news.timer e2scrub_all.timer fstrim.timer man-db.timer systemd-networkd-wait-online.service \
    # Docker starts on first use (socket activation) so it does not slow down boot.
    && systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket \
    # The guest kernel has legacy iptables NAT built in; nf_tables is only a module.
    && update-alternatives --set iptables /usr/sbin/iptables-legacy \
    && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy \
    # ...and no iptables raw table, which Docker 28 uses for direct-access filtering.
    && mkdir -p /etc/systemd/system/docker.service.d \
    && printf '[Service]\nEnvironment=DOCKER_INSECURE_NO_IPTABLES_RAW=1\n' >/etc/systemd/system/docker.service.d/firerunner.conf \
    && systemctl enable ssh \
    && sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && rm -f /etc/ssh/ssh_host_*_key* \
    && truncate -s 0 /etc/machine-id
