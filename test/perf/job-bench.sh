#!/usr/bin/env bash
# Where does a job spend its time? Run this same job on a shell runner and on
# FireRunner and compare the lines: it times what differs between the two
# (CPU a job gets, small-file and sync disk writes, the image pull, a clone).
#
#   bench:
#     tags: [firecracker]          # and a copy with the shell runner's tag
#     variables:
#       BENCH_IMAGE: node:22       # an image your jobs use
#     script:
#       - bash test/perf/job-bench.sh
#
# Nothing here needs root; the image test needs docker. Output: one
# "bench <name> <seconds>" line per test, easy to diff between runners.
# The bash -c scripts below expand their own $1 and loops on purpose.
# shellcheck disable=SC2016
set -euo pipefail

BENCH_IMAGE="${BENCH_IMAGE:-node:22}"
BENCH_DIR="${BENCH_DIR:-${CI_PROJECT_DIR:-$PWD}/.bench}"
rm -rf "$BENCH_DIR" && mkdir -p "$BENCH_DIR"
trap 'rm -rf "$BENCH_DIR"' EXIT

# timed NAME CMD...: runs CMD and prints its wall time.
timed() {
    local name=$1 start end
    shift
    start=$(date +%s.%N)
    "$@" >/dev/null 2>&1 || echo "bench $name failed" >&2
    end=$(date +%s.%N)
    awk -v n="$name" -v s="$start" -v e="$end" 'BEGIN { printf "bench %-22s %8.2f\n", n, e - s }'
}

echo "host: $(hostname), $(nproc) CPUs, $(awk '/MemTotal/ {printf "%d MB", $2/1024}' /proc/meminfo), kernel $(uname -r)"
grep -m1 'model name' /proc/cpuinfo || true
echo "mitigations: $(grep -h . /sys/devices/system/cpu/vulnerabilities/* 2>/dev/null | grep -vc 'Not affected') active"
df -h "$BENCH_DIR" | tail -1

# CPU: one core, then every core the job has.
timed cpu-1core bash -c 'head -c 1500M /dev/zero | sha256sum'
timed cpu-all-cores bash -c 'for _ in $(seq "$(nproc)"); do head -c 1500M /dev/zero | sha256sum & done; wait'

# Disk: many small files (npm install, git checkout, layer extraction), then
# small synchronous writes (databases in services, package managers).
timed files-write-20k bash -c 'cd "$1" && mkdir f && for d in $(seq 200); do mkdir "f/$d"; for i in $(seq 100); do echo "$d $i" >"f/$d/$i"; done; done; sync' _ "$BENCH_DIR"
timed files-tar-untar bash -c 'cd "$1" && tar cf f.tar f && mkdir g && tar xf f.tar -C g && sync' _ "$BENCH_DIR"
timed files-delete bash -c 'rm -rf "$1/f" "$1/g" && sync' _ "$BENCH_DIR"
timed seq-write-1g dd if=/dev/zero of="$BENCH_DIR/big" bs=1M count=1024 conv=fsync
timed sync-writes-500 dd if=/dev/zero of="$BENCH_DIR/sync" bs=4k count=500 oflag=dsync

# Image: what the job's `image:` costs. On a shell runner the image is usually
# present already; "image-fresh" removes it first to show what a new VM pays
# (on a shared shell runner, the next job using it pulls it again).
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    timed image-present docker pull -q "$BENCH_IMAGE"
    timed image-run docker run --rm "$BENCH_IMAGE" true
    docker rmi -f "$BENCH_IMAGE" >/dev/null 2>&1 || true
    timed image-fresh docker pull -q "$BENCH_IMAGE"
fi

# Git: a full clone of this project, as a new VM does it (a shell runner
# usually fetches into the clone of an earlier job).
if [[ -n ${CI_REPOSITORY_URL:-} ]]; then
    timed git-clone-full git clone -q "$CI_REPOSITORY_URL" "$BENCH_DIR/clone"
    timed git-clone-depth20 git clone -q --depth 20 "$CI_REPOSITORY_URL" "$BENCH_DIR/clone20"
fi
