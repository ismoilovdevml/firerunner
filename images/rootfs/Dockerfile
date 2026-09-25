# Root filesystem for FireRunner job microVMs.
# Flintlock turns this image into the VM's root disk and boots /sbin/init (systemd).
ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG TARGETARCH=amd64
ARG GITLAB_RUNNER_VERSION=19.4.0

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv systemd-resolved udev dbus kmod \
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

COPY firerunner-netfilter /usr/local/sbin/firerunner-netfilter

# /etc/resolv.conf is bind-mounted during the build, so link it at boot instead.
RUN printf 'L+ /etc/resolv.conf - - - - /run/systemd/resolve/stub-resolv.conf\n' >/etc/tmpfiles.d/resolv.conf \
    # Flintlock serves meta-data/user-data over MMDS (ds=nocloud-net on the kernel cmdline).
    && printf 'datasource_list: [ NoCloud, None ]\n' >/etc/cloud/cloud.cfg.d/90-firerunner.cfg \
    # eth0 carries only MMDS, at 169.254.0.1/16 (flintlock's network-config). networkd
    # probes IPv4 link-local addresses for conflicts (ACD) before using them, about 6 s
    # every time the link is configured, and cloud-init's first metadata request waits
    # for it. Nothing else is on that link: no probing.
    && mkdir -p /etc/systemd/network/10-netplan-eth0.network.d \
    && printf '[Address]\nAddress=169.254.0.1/16\nDuplicateAddressDetection=none\n' \
        >/etc/systemd/network/10-netplan-eth0.network.d/10-firerunner-no-acd.conf \
    # Jobs are short-lived: skip timers and units that only slow boot down.
    && systemctl mask apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service \
        motd-news.timer e2scrub_all.timer fstrim.timer man-db.timer systemd-networkd-wait-online.service \
    # Docker starts on first use (socket activation) so it does not slow down boot.
    && systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket \
    # iptables backend chosen at start from what the guest kernel supports.
    && mkdir -p /etc/systemd/system/docker.service.d \
    && printf '[Service]\nExecStartPre=/usr/local/sbin/firerunner-netfilter\nEnvironmentFile=-/run/firerunner-docker.env\n' \
        >/etc/systemd/system/docker.service.d/firerunner.conf \
    && systemctl enable ssh \
    && sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && rm -f /etc/ssh/ssh_host_*_key* \
    && truncate -s 0 /etc/machine-id
