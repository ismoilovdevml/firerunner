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

# The tarball is checked against the release signature (.tar.sign, made over
# the uncompressed tar) with the stable maintainers' keys, not only against a
# checksum served from the same CDN. kernel-signing-keys.asc holds the keys of
# Greg Kroah-Hartman (647F28654894E3BD457199BE38DBBDC86092693E) and Sasha Levin
# (E27E5D8A3403A2EF66873BBCDEA66FF797772CDC), fetched from the kernel.org WKD;
# the fingerprints are the ones published on https://www.kernel.org/signature.html.
# gpgv trusts only the keys in that keyring.
WORKDIR /src
COPY kernel-signing-keys.asc /tmp/kernel-signing-keys.asc
RUN major="${KERNEL_VERSION%%.*}" \
    && curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${KERNEL_VERSION}.tar.xz" \
    && curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${KERNEL_VERSION}.tar.sign" \
    && gpg --batch --dearmor < /tmp/kernel-signing-keys.asc > /tmp/kernel-signing-keys.gpg \
    && xz -cd "linux-${KERNEL_VERSION}.tar.xz" \
       | gpgv --keyring /tmp/kernel-signing-keys.gpg "linux-${KERNEL_VERSION}.tar.sign" - \
    && tar -xf "linux-${KERNEL_VERSION}.tar.xz" && rm "linux-${KERNEL_VERSION}.tar.xz" "linux-${KERNEL_VERSION}.tar.sign"

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
