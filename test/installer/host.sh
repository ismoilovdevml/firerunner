#!/usr/bin/env bash
# The variables set here are read by install.sh's functions.
# shellcheck disable=SC2034
# Tests installer steps that act on the host's disks and files with
# install.sh's own functions, the system commands they call faked: the cache
# size cap, the choice of the thin pool disk and the install of the firerunner
# binary. It needs no root and changes nothing outside a temporary
# directory, so it also runs outside a container (bash 4.4 or later):
#   bash test/installer/host.sh
#   docker run --rm -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/installer/host.sh
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL_SH=${INSTALL_SH:-$HERE/../../install.sh}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
sed '$d' "$INSTALL_SH" > "$WORK/install-lib.sh"
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "PASS  $1"; }
bad() { fail=$((fail+1)); echo "FAIL  $1"; }
# expect NAME [!] COMMAND...: the command succeeds (or, after !, fails).
expect() {
    local name=$1 want=0; shift
    if [[ $1 == "!" ]]; then want=1; shift; fi
    "$@" >/dev/null 2>&1; local got=$(( $? != 0 ))
    if [[ $got -eq $want ]]; then ok "$name"; else bad "$name"; fi
}

# shellcheck disable=SC1091
source "$WORK/install-lib.sh"
set +e; trap - ERR INT HUP TERM; trap 'rm -rf "$WORK"' EXIT

# ---- cache: the oldest archives go above the size cap
cache_clean_script > "$WORK/cache-clean.sh"
C=$WORK/cache
# cache_fixture: three 1 GiB archives (as find reports them), oldest first:
# "a/old file", b/mid, a/new.
cache_fixture() {
    mkdir -p "$C/a" "$C/b"
    for f in "a/old file" b/mid a/new; do echo x > "$C/$f"; done
    printf '%s\0' "1000.5 1048576 $C/a/old file" "3000.1 1048576 $C/a/new" "2000.0 1048576 $C/b/mid" > "$WORK/listing"
}
# The script's find -printf answers from the fixture (the age pass and the
# empty-directory pass find nothing); df reports a 10 GiB file system.
cache_clean() {
    (
        find() { if [[ " $* " == *" -printf "* ]]; then cat "$LISTING"; fi; }
        df() { printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 10485760 0 0 0%% /\n'; }
        export -f find df
        LISTING=$WORK/listing FR_CACHE_DIR=$C bash "$WORK/cache-clean.sh" "$@"
    )
}
cache_fixture
expect "cache: 3 GiB under a 5 GB cap: nothing deleted" cache_clean 14 5
expect "...all archives kept" test -f "$C/a/old file" -a -f "$C/b/mid" -a -f "$C/a/new"
expect "cache: 3 GiB above a 2 GB cap" cache_clean 14 2
expect "...the oldest archive (a path with a space) deleted" test ! -e "$C/a/old file"
expect "...the newer ones kept" test -f "$C/b/mid" -a -f "$C/a/new"
cache_fixture
expect "cache: auto cap, a fifth of 10 GiB" cache_clean 14 auto
expect "...the oldest archive deleted, the newer ones kept" test ! -e "$C/a/old file" -a -f "$C/b/mid" -a -f "$C/a/new"
cache_fixture
expect "cache: a 1 GB cap leaves only the newest" cache_clean 14 1
expect "...only the newest left" test ! -e "$C/a/old file" -a ! -e "$C/b/mid" -a -f "$C/a/new"
check_cache_settings() { ( FR_CACHE_DAYS=$1 FR_CACHE_MAX_GB=$2; die() { exit 1; }; preflight_settings ) >/dev/null 2>&1; }
expect "FR_CACHE_MAX_GB=50 allowed"          check_cache_settings 14 50
expect "FR_CACHE_MAX_GB=auto allowed"        check_cache_settings 14 auto
expect "FR_CACHE_MAX_GB=0 refused"         ! check_cache_settings 14 0
expect "FR_CACHE_MAX_GB=10G refused"       ! check_cache_settings 14 10G
expect "FR_CACHE_DAYS with a space refused" ! check_cache_settings "14 -delete" auto

# ---- thin pool disk: only a disk nothing uses is wiped
# Fake disks, described by files under $D: disks (lsblk -d), tree/NAME (the
# disk and what is on it, with mount points), sig/NAME (blkid), data/NAME
# (a first MiB that is not zeros), holders/NAME, mpath, pvs and swaps.
D=$WORK/disks
G=$((1024 * 1024 * 1024))
disks_reset() {
    rm -rf "$D"; mkdir -p "$D/tree" "$D/sig" "$D/data" "$D/holders"
    : > "$D/disks"; : > "$D/pvs"; : > "$D/mpath"; printf 'Filename Type Size Used Priority\n' > "$D/swaps"
}
# disk NAME SIZE_GB [RM RO]: a whole disk with nothing on it.
disk() {
    echo "$1 disk ${3:-0} ${4:-0} $(($2 * G))" >> "$D/disks"
    echo "/dev/$1 " > "$D/tree/$1"
}
lsblk() {
    local last=${*: -1}
    case "$*" in
        "-dnbro NAME,TYPE,RM,RO,SIZE") cat "$D/disks" ;;
        "-nrpo NAME,MOUNTPOINT "*)     cat "$D/tree/${last#/dev/}" ;;
        "-nrpo NAME "*)                cut -d' ' -f1 "$D/tree/${last#/dev/}" ;;
        "-dno SIZE,MODEL "*)           echo "100G FakeDisk" ;;
    esac
}
blkid() { cat "$D/sig/${2#/dev/}" 2>/dev/null; }
pvs() { cat "$D/pvs"; }
multipath() { grep -qx "$2" "$D/mpath"; }
udevadm() { :; }
disk_head_zero() { [[ ! -e $D/data/${1#/dev/} ]]; }
disk_has_holders() { [[ -e $D/holders/${1#/dev/} ]]; }
SWAPS_FILE=$D/swaps
# picks: what thinpool_disk does without FR_DISK (it stops at -b: the disks are fake).
picks() { ( FR_DISK=""; die() { echo "DIE $*"; exit 1; }; log() { echo "LOG $*"; }; thinpool_disk ) 2>&1; }
in_use() { disk_in_use "/dev/$1"; }

disks_reset
disk frt-sda 50; echo "/dev/frt-sda1 /" >> "$D/tree/frt-sda"      # the system disk
disk frt-sdb 100
expect "one blank disk next to the system disk is taken" grep -q "LOG using blank disk /dev/frt-sdb " <<<"$(picks)"
disk frt-sdc 100
expect "two blank disks: none is taken" grep -q "DIE found 2 blank disks (/dev/frt-sdb /dev/frt-sdc): choose the one to wipe" <<<"$(picks)"
expect "...and nothing is picked" ! grep -q "LOG using" <<<"$(picks)"
disks_reset; disk frt-sdb 100; touch "$D/data/frt-sdb"
expect "a disk with data but no signature (raw, Ceph, detached LUKS) is not blank" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100; echo '/dev/frt-sdb: TYPE="crypto_LUKS"' > "$D/sig/frt-sdb"
expect "a disk with a signature is not blank" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 8
expect "a disk under ${DISK_MIN_GB} GB is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100 1
expect "a removable disk is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100 0 1
expect "a read-only disk is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100; echo /dev/frt-sdb > "$D/mpath"
expect "a multipath path is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100; touch "$D/holders/frt-sdb"
expect "a disk with a holder is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"
disks_reset; disk frt-sdb 100; echo "/dev/frt-sdb  " >> "$D/pvs"
expect "an LVM physical volume is not taken" grep -q "DIE no blank disk found" <<<"$(picks)"

# FR_DISK set: the disk and everything on it is checked.
disks_reset; disk frt-sdb 100
expect "FR_DISK: an unused disk may be wiped" ! in_use frt-sdb
echo "/dev/frt-sdb1 " >> "$D/tree/frt-sdb"
expect "FR_DISK: a disk with an unmounted partition is refused" in_use frt-sdb
expect "...naming the partition" grep -q "partitions or devices on it: /dev/frt-sdb1" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo "/dev/frt-sdb1 /data" >> "$D/tree/frt-sdb"
expect "FR_DISK: a disk with a mounted partition is refused" grep -q "/dev/frt-sdb1 is mounted on /data" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo "/dev/frt-sdb2 [SWAP]" >> "$D/tree/frt-sdb"
expect "FR_DISK: a disk with swap on a partition is refused" grep -q "/dev/frt-sdb2 is swap" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo "/dev/frt-sdb partition 4G 0 -2" >> "$D/swaps"
expect "FR_DISK: swap listed only in /proc/swaps is refused" grep -q "/dev/frt-sdb is swap" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo "  /dev/frt-sdb" > "$D/pvs"
expect "FR_DISK: an LVM physical volume is refused" grep -q "LVM physical volume" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo "/dev/mapper/vg-data /srv" >> "$D/tree/frt-sdb"
expect "FR_DISK: a disk under a mounted LV is refused" grep -q "/dev/mapper/vg-data is mounted on /srv" <<<"$(in_use frt-sdb)"
disks_reset; disk frt-sdb 100; echo /dev/frt-sdb > "$D/mpath"
expect "FR_DISK: a multipath path is refused" grep -q "multipath" <<<"$(in_use frt-sdb)"

# ---- firerunner binary: checked, then renamed into place
B=$WORK/bin; mkdir -p "$B"
# fake_release VERSION: a "firerunner" that says it is VERSION.
fake_release() { printf '#!/bin/sh\necho "firerunner %s"\n' "$1" > "$WORK/release-$1"; echo "$WORK/release-$1"; }
inode() { ls -i "$1" | awk '{print $1}'; }
echo old > "$B/firerunner"; chmod 0755 "$B/firerunner"; before=$(inode "$B/firerunner")
CHANGED=" "
# installs VERSION WANT: install_binary of a release saying VERSION with FR_VERSION=WANT,
# ended by the installer's own EXIT trap (cleanup) as a real run is.
installs() { ( trap cleanup EXIT; FR_VERSION=$2; install_binary "$(fake_release "$1")" "$B/firerunner" release_runs ) >/dev/null 2>&1; }
expect "a release that is FR_VERSION is installed" installs v0.3.0 v0.3.0
expect "...in place of the old one" grep -q v0.3.0 "$B/firerunner"
expect "...by a rename (a new inode, never a file half written)" test "$(inode "$B/firerunner")" != "$before"
expect "...executable" test -x "$B/firerunner"
expect "...and no copy is left next to it" test -z "$(compgen -G "$B/.firerunner-new.*")"
cp "$B/firerunner" "$WORK/installed"
expect "an older release in place of FR_VERSION is refused" ! installs v0.2.0 v0.4.0
expect "...the installed binary is untouched" cmp -s "$WORK/installed" "$B/firerunner"
expect "...and its copy is removed" test -z "$(compgen -G "$B/.firerunner-new.*")"
same_inode=$(inode "$B/firerunner"); CHANGED=" "
FR_VERSION=v0.3.0 install_binary "$(fake_release v0.3.0)" "$B/firerunner" release_runs
expect "the same binary again is not replaced" test "$(inode "$B/firerunner")" = "$same_inode"
expect "...nor recorded as changed" ! changed "$B/firerunner"
FR_VERSION=v0.4.0 install_binary "$(fake_release v0.4.0)" "$B/firerunner" release_runs
expect "a new binary is recorded as changed (the daemon restarts)" changed "$B/firerunner"
# FR_VERSION=latest: never an older release than the installed one.
fake_release v0.5.0 >/dev/null; cp "$WORK/release-v0.5.0" "$B/firerunner"; cp "$B/firerunner" "$WORK/installed"
latest_installs() { ( trap cleanup EXIT; BIN_DIR=$B FR_VERSION=latest; install_binary "$(fake_release "$1")" "$B/firerunner" release_runs ) >/dev/null 2>&1; }
expect "latest: an older release (a backport marked latest) is refused" ! latest_installs v0.4.1
expect "...the installed binary is untouched" cmp -s "$WORK/installed" "$B/firerunner"
expect "...and its copy is removed" test -z "$(compgen -G "$B/.firerunner-new.*")"
expect "latest: a newer release is installed" latest_installs v0.6.0
expect "...in place of the old one" grep -q v0.6.0 "$B/firerunner"

# ---- ufw: rules for the bridge ports follow the ports in use
U=$WORK/ufw; mkdir -p "$U"
CONF_DIR=$U UFW_PORTS_FILE=$U/ufw-bridge-ports PROXY_FILE=$U/proxy-upstream
FR_BRIDGE=br-fc FR_SUBNET=10.200.0
ufw() { echo "$*" >> "$U/ufw.log"; }
# ufw_run MIRRORS [proxy]: one installer run's ufw rules, with these mirrors (host=port,...).
ufw_run() {
    MIRROR_PORT=(); local m; FR_REGISTRY_MIRRORS=""
    for m in ${1//,/ }; do MIRROR_PORT[${m%=*}]=${m#*=}; FR_REGISTRY_MIRRORS+="${FR_REGISTRY_MIRRORS:+,}${m%=*}"; done
    [[ -n $FR_REGISTRY_MIRRORS ]] || FR_REGISTRY_MIRRORS=none
    if [[ ${2:-} == proxy ]]; then echo http://proxy.corp:3128 > "$PROXY_FILE"; else rm -f "$PROXY_FILE"; fi
    : > "$U/ufw.log"
    ufw_bridge_ports >/dev/null
}
deleted() { grep -qx "delete allow in on br-fc to 10.200.0.1 port $1 proto tcp" "$U/ufw.log"; }
allowed() { grep -qx "allow in on br-fc to 10.200.0.1 port $1 proto tcp" "$U/ufw.log"; }
ufw_run ghcr.io=5001,quay.io=5002 proxy
expect "ufw: mirror, cache and forwarder ports allowed" bash -c "grep -c '^allow in on br-fc to 10.200.0.1 port' '$U/ufw.log' | grep -qx 5"
expect "...and nothing deleted on a first run" ! grep -q '^delete' "$U/ufw.log"
ufw_run quay.io=5002
expect "ufw: a dropped mirror's port is deleted" deleted 5001
expect "ufw: the forwarder's port is deleted once the proxy is off" deleted 3128
kept() { ! grep -Eq "^delete .* port ($1) " "$U/ufw.log"; }
expect "...the ports still used are kept" kept "5000|5002|9000"
expect "...and allowed again" allowed 5002
ufw_run quay.io=5002
expect "ufw: a run with nothing changed deletes nothing" ! grep -q '^delete' "$U/ufw.log"
echo "br-fc 10.200.0.1 5001" > "$UFW_PORTS_FILE"; ufw_run quay.io=5002
expect "ufw: only rules in its own list are deleted" bash -c "test \"\$(grep -c '^delete' '$U/ufw.log')\" = 1"

# ---- downloads: the default versions have a pinned checksum
# The keys are built as the install steps build them: a default version
# raised without its pin would fall back to the download host's checksum file.
pin_ok() { [[ $(pinned "$1" 2>/dev/null) =~ ^[0-9a-f]{64}$ ]]; }
expect "containerd ${CONTAINERD_VERSION} pinned"       pin_ok "v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-amd64.tar.gz"
expect "firecracker ${FIRECRACKER_VERSION} pinned"     pin_ok "v${FIRECRACKER_VERSION}/firecracker-v${FIRECRACKER_VERSION}-x86_64.tgz"
expect "flintlockd ${FLINTLOCK_VERSION} pinned"        pin_ok "v${FLINTLOCK_VERSION}/flintlockd_amd64"
expect "registry ${REGISTRY_VERSION} pinned"           pin_ok "v${REGISTRY_VERSION}/registry_${REGISTRY_VERSION}_linux_amd64.tar.gz"
expect "versitygw ${VERSITYGW_VERSION} pinned"         pin_ok "v${VERSITYGW_VERSION}/versitygw_v${VERSITYGW_VERSION}_Linux_x86_64.tar.gz"
expect "gitlab-runner ${GITLAB_RUNNER_VERSION} pinned" pin_ok "v${GITLAB_RUNNER_VERSION}/binaries/gitlab-runner-linux-amd64"
expect "another version has no pin" ! pin_ok "v9.9.9/flintlockd_amd64"
expect "...and says so" grep -q "no pinned checksum for v9.9.9/flintlockd_amd64" <<<"$(pinned v9.9.9/flintlockd_amd64 2>&1)"

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
