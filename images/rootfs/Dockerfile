# Root filesystem for FireRunner job microVMs.
# Flintlock turns this image into the VM's root disk and boots /sbin/init (systemd).
ARG UBUNTU_VERSION=22.04
FROM ubuntu:${UBUNTU_VERSION}

ARG TARGETARCH=amd64
ARG GITLAB_RUNNER_VERSION=19.0.1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv udev dbus kmod \
        cloud-init openssh-server sudo \
        iproute2 iputils-ping dnsutils ca-certificates \
        curl wget git git-lfs jq unzip zip xz-utils tar gzip make python3 \
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
    && systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer \
        e2scrub_all.timer fstrim.timer man-db.timer systemd-networkd-wait-online.service \
    && systemctl enable ssh \
    && sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && rm -f /etc/ssh/ssh_host_*_key* \
    && truncate -s 0 /etc/machine-id
