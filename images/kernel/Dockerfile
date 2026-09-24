# Guest kernel for FireRunner job VMs: a longterm kernel built from
# Firecracker's microvm config plus firerunner.config. Flintlock reads
# /boot/vmlinux from this image.
FROM ubuntu:24.04 AS build

ARG KERNEL_VERSION=6.18.53
# Firecracker commit the base config is taken from.
ARG FIRECRACKER_REF=e67ab399d73721e7fc19f58da1ff960e28f1bac1

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      bc bison build-essential ca-certificates curl dwarves flex gnupg \
      libelf-dev libssl-dev python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN major="${KERNEL_VERSION%%.*}" \
    && curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${KERNEL_VERSION}.tar.xz" \
    && curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/sha256sums.asc" \
       | grep " linux-${KERNEL_VERSION}.tar.xz\$" | sha256sum -c - \
    && tar -xf "linux-${KERNEL_VERSION}.tar.xz" && rm "linux-${KERNEL_VERSION}.tar.xz"

WORKDIR /src/linux-${KERNEL_VERSION}
COPY firerunner.config /tmp/firerunner.config
RUN series="$(echo "${KERNEL_VERSION}" | cut -d. -f1,2)" \
    && curl -fsSLo .config "https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FIRECRACKER_REF}/resources/guest_configs/microvm-kernel-ci-x86_64-${series}.config" \
    && scripts/kconfig/merge_config.sh -m .config /tmp/firerunner.config \
    && make olddefconfig \
    && for opt in $(grep -oE '^CONFIG_[A-Z0-9_]+' /tmp/firerunner.config); do \
         grep -qx "${opt}=y" .config || { echo "missing after olddefconfig: ${opt}"; exit 1; }; \
       done \
    && make -j"$(nproc)" vmlinux \
    && mkdir -p /out/boot && cp vmlinux /out/boot/vmlinux && cp .config "/out/boot/config-${KERNEL_VERSION}"

FROM scratch
COPY --from=build /out/boot /boot
