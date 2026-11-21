#!/usr/bin/env bash
# The variables set here are read by install.sh's functions.
# shellcheck disable=SC2034
# Tests installer steps that act on the host's disks and files with
# install.sh's own functions, the system commands they call faked: the cache
# size cap, the choice of the thin pool disk, the install of the firerunner
# binary, the registry mirrors' units, their move from the single service of
# earlier versions and their upkeep (eviction, garbage collection), and what a
# drained host's gitlab-runner is spared. It needs no root and changes nothing outside a temporary
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

# ---- registry mirrors: a service each, their data on a file system of its own
# Paths under $R; systemctl answers from files ($R/active/UNIT) and logs what
# it is asked; chown (own_tree) and the jobs check are faked.
R=$WORK/reg; mkdir -p "$R/active"
UNIT_DIR=$R/units CONF_DIR=$R/conf REGISTRY_CONF_DIR=$R/conf/registries.d LIB_DIR=$R/lib BIN_DIR=$R/bin
REGISTRY_DIR=$R/registries REGISTRY_IMG=$R/registries.img REGISTRY_STATE_DIR=$R/maint PENDING_FILE=$R/pending-restarts
LEGACY_REGISTRIES_SH=$R/lib/registries.sh LEGACY_REGISTRY_CONF=$R/conf/registry.yml LEGACY_MIRROR_CONF_DIR=$R/conf/registry-mirrors.d
LEGACY_REGISTRY_DATA=$R/data/registry LEGACY_MIRROR_DATA_DIR=$R/data/registry-mirrors DRAIN_FILE=$R/drain.json
REGISTRY_MAINT=$R/bin/firerunner-registry-maint
FR_SUBNET=10.200.0
declare -A MIRROR_PORT=([ghcr.io]=5001 [quay.io]=5002)
mkdir -p "$UNIT_DIR" "$CONF_DIR" "$LIB_DIR" "$REGISTRY_DIR" "$R/bin"
# SELinux is on while $R/selinux exists.
selinux_on() { [[ -f $R/selinux ]]; }
SC_LOG=$R/systemctl.log; : > "$SC_LOG"
systemctl() {
    echo "$*" >> "$SC_LOG"
    case $1 in
        is-active)  [[ -f $R/active/${*: -1} ]] ;;
        is-enabled) [[ -f $R/enabled/${*: -1} ]] ;;
        *)          return 0 ;;
    esac
}
own_tree() { echo "$1 $2" >> "$R/own.log"; }
jobs_running() { [[ -f $R/jobs-running ]]; }
u=$UNIT_DIR y=$REGISTRY_CONF_DIR

check_registry_gb() { ( FR_REGISTRY_GB=$1; die() { exit 1; }; preflight_settings ) >/dev/null 2>&1; }
expect "FR_REGISTRY_GB=auto allowed"    check_registry_gb auto
expect "FR_REGISTRY_GB=50 allowed"      check_registry_gb 50
expect "FR_REGISTRY_GB=4 refused (below ${REGISTRY_MIN_GB})" ! check_registry_gb 4
expect "FR_REGISTRY_GB=10G refused"   ! check_registry_gb 10G
# gb_for SETTING ROOT_KB: the size a new image gets on a root file system of ROOT_KB.
gb_for() { ( FR_REGISTRY_GB=$1 ROOT_KB=$2; df() { printf 'F 1024-blocks Used Available Capacity Mounted\n/dev/x %s 0 %s 0%% /\n' "$ROOT_KB" "$ROOT_KB"; }; registry_gb ); }
expect "auto: a fifth of a 100 GiB root file system" test "$(gb_for auto $((100 * 1024 * 1024)))" = 20
expect "auto: at least ${REGISTRY_MIN_GB} GB on a 10 GiB one" test "$(gb_for auto $((10 * 1024 * 1024)))" = "$REGISTRY_MIN_GB"
expect "FR_REGISTRY_GB=30: 30" test "$(gb_for 30 $((100 * 1024 * 1024)))" = 30

FR_REGISTRY_MIRRORS=ghcr.io REGISTRY_FRONT=1 CHANGED=" "
: > "$LIB_DIR/registry-maint.sh"   # where an earlier build of this layout put the upkeep script
registry_files >/dev/null 2>&1
expect "docker.io: the registry on 127.0.0.1:15000, behind its front" grep -qx '  addr: 127.0.0.1:15000' "$y/docker.io.yml"
expect "...pulling from Docker Hub" grep -qx '  remoteurl: https://registry-1.docker.io' "$y/docker.io.yml"
expect "...into a directory of its own" grep -qx "    rootdirectory: $REGISTRY_DIR/docker.io" "$y/docker.io.yml"
expect "...that the ttl may delete from" grep -qx '    enabled: true' "$y/docker.io.yml"
expect "ghcr.io: 127.0.0.1:15001, pulling from ghcr.io" grep -qx '  addr: 127.0.0.1:15001' "$y/ghcr.io.yml"
expect "...from ghcr.io" grep -qx '  remoteurl: https://ghcr.io' "$y/ghcr.io.yml"
expect "ghcr.io's front on the bridge port microVMs use (5001)" grep -qx 'FR_FRONT_LISTEN=10.200.0.1:5001' "$y/ghcr.io.env"
expect "...in front of its registry" grep -qx 'FR_FRONT_UPSTREAM=http://127.0.0.1:15001' "$y/ghcr.io.env"
expect "Docker Hub's front on 5000, as before" grep -qx 'FR_FRONT_LISTEN=10.200.0.1:5000' "$y/docker.io.env"
expect "registry@: not root" grep -qx 'User=firerunner-registry' "$u/firerunner-registry@.service"
expect "registry@: writes only its own mirror's data" grep -qx "ReadWritePaths=$REGISTRY_DIR/%i" "$u/firerunner-registry@.service"
expect "registry@: never without its file system" grep -qx "RequiresMountsFor=$REGISTRY_DIR" "$u/firerunner-registry@.service"
expect "registry@: no capabilities" grep -qx 'CapabilityBoundingSet=' "$u/firerunner-registry@.service"
expect "registry@: stopped and restarted with firerunner-registry" grep -qx 'PartOf=firerunner-registry.service' "$u/firerunner-registry@.service"
# shellcheck disable=SC2016
expect "front@: its addresses come from the .env file (systemd expands them)" grep -qF 'registry-front --listen ${FR_FRONT_LISTEN} --upstream ${FR_FRONT_UPSTREAM}' "$u/firerunner-registry-front@.service"
expect "front@: a dynamic user" grep -qx 'DynamicUser=yes' "$u/firerunner-registry-front@.service"
expect "front@: its 60 s for running pulls fit in the stop timeout" grep -qx 'TimeoutStopSec=75s' "$u/firerunner-registry-front@.service"
expect "front@: no capabilities" grep -qx 'CapabilityBoundingSet=' "$u/firerunner-registry-front@.service"
expect "firerunner-registry: only starts the others" grep -qx 'Type=oneshot' "$u/firerunner-registry.service"
expect "eviction: as the registry user" grep -qx 'User=firerunner-registry' "$u/firerunner-registry-evict.service"
expect "...from under 10 % or 1536 MiB free to 20 % and 3072 MiB free" grep -qx "ExecStart=$REGISTRY_MAINT evict 10 1536 20 3072" "$u/firerunner-registry-evict.service"
expect "...checked every 2 minutes" grep -qx 'OnUnitActiveSec=2min' "$u/firerunner-registry-evict.timer"
expect "garbage collection: root only to stop and start a mirror" grep -qx 'CapabilityBoundingSet=CAP_SETUID CAP_SETGID' "$u/firerunner-registry-gc.service"
expect "...the same script" grep -qx "ExecStart=$REGISTRY_MAINT gc" "$u/firerunner-registry-gc.service"
expect "the upkeep script is in BIN_DIR (bin_t under SELinux: its units run unconfined)" test -x "$REGISTRY_MAINT"
expect "...not in LIB_DIR (lib_t: init_t may not delete the mirrors' files)" test ! -e "$LIB_DIR/registry-maint.sh"
expect "mount options without SELinux: no context" test "$(registry_mount_options)" = loop,discard,nodev,nosuid,noexec,nofail
touch "$R/selinux"
expect "with SELinux: every file var_lib_t (a new ext4 has no labels)" test "$(registry_mount_options)" = loop,discard,nodev,nosuid,noexec,nofail,context=system_u:object_r:var_lib_t:s0
rm -f "$R/selinux"
CHANGED=" "; REGISTRY_FRONT=0
registry_files >/dev/null 2>&1
expect "without registry-front: each registry on its bridge port, as before" grep -qx '  addr: 10.200.0.1:5000' "$y/docker.io.yml"
expect "...and its config changed (it restarts)" changed "$y/docker.io.yml"
FR_REGISTRY_MIRRORS=none REGISTRY_FRONT=1
registry_files >/dev/null 2>&1
expect "a dropped mirror's config and front settings are removed" test ! -e "$y/ghcr.io.yml" -a ! -e "$y/ghcr.io.env"
expect "...Docker Hub's stay" test -f "$y/docker.io.yml" -a -f "$y/docker.io.env"

# Earlier versions: their single service, or their data of a mirror still in use.
FR_REGISTRY_MIRRORS=ghcr.io
expect "after this version's units: nothing of earlier versions" ! registry_legacy
printf '[Service]\nExecStart=%s\n' "$LEGACY_REGISTRIES_SH" > "$u/firerunner-registry.service"
expect "the single service of earlier versions is found" registry_legacy
rm -f "$u/firerunner-registry.service"; mkdir -p "$LEGACY_MIRROR_DATA_DIR/ghcr.io"
expect "earlier versions' data of a mirror in use is found" registry_legacy

# Migration. df: the mirrors' file system ($RSIZE KiB, $RUSED used), the root one ($ROOTAVAIL free).
RSIZE=$((10 * 1024 * 1024)) RUSED=0 ROOTAVAIL=$((50 * 1024 * 1024))
df() {
    if [[ ${*: -1} == "$REGISTRY_DIR" ]]; then
        printf 'F 1024-blocks Used Available Capacity Mounted\n/dev/loop0 %s %s 0 0%% %s\n' "$RSIZE" "$RUSED" "$REGISTRY_DIR"
    else
        printf 'F 1024-blocks Used Available Capacity Mounted\n/dev/root 1 0 %s 0%% /\n' "$ROOTAVAIL"
    fi
}
legacy_fixture() {
    rm -rf "$R/data" "$REGISTRY_DIR" "$u/firerunner-registry.service.d"; mkdir -p "$REGISTRY_DIR"
    mkdir -p "$LEGACY_REGISTRY_DATA/docker/registry/v2/blobs" "$LEGACY_MIRROR_DATA_DIR/ghcr.io/docker" "$LEGACY_MIRROR_DATA_DIR/old.example"
    echo hub > "$LEGACY_REGISTRY_DATA/docker/registry/v2/blobs/b1"
    echo gh > "$LEGACY_MIRROR_DATA_DIR/ghcr.io/docker/b2"
    : > "$LEGACY_REGISTRIES_SH"; : > "$LEGACY_REGISTRY_CONF"
    mkdir -p "$LEGACY_MIRROR_CONF_DIR" "$u/firerunner-registry.service.d"
    : > "$LEGACY_MIRROR_CONF_DIR/ghcr.io.yml"; : > "$u/firerunner-registry.service.d/firerunner-proxy.conf"
    printf '[Service]\nExecStart=%s\n' "$LEGACY_REGISTRIES_SH" > "$u/firerunner-registry.service"
    touch "$R/active/firerunner-registry"; : > "$SC_LOG"; : > "$R/own.log"
}
legacy_fixture
registry_migrate >/dev/null 2>&1
expect "migration: the single service is stopped first" grep -qx 'stop firerunner-registry' "$SC_LOG"
expect "...Docker Hub's data copied in" grep -qx hub "$REGISTRY_DIR/docker.io/docker/registry/v2/blobs/b1"
expect "...ghcr.io's data copied in" grep -qx gh "$REGISTRY_DIR/ghcr.io/docker/b2"
expect "...given to the registry user before it is in place" grep -qx "$REGISTRY_DIR/.docker.io.migrating firerunner-registry" "$R/own.log"
expect "...no copy left half done" test -z "$(compgen -G "$REGISTRY_DIR/.*.migrating")"
expect "...the old data kept, renamed" grep -qx hub "$LEGACY_REGISTRY_DATA.migrated/docker/registry/v2/blobs/b1"
expect "...ghcr.io's too" grep -qx gh "$LEGACY_MIRROR_DATA_DIR/ghcr.io.migrated/docker/b2"
expect "...a dropped mirror's old data left alone" test -d "$LEGACY_MIRROR_DATA_DIR/old.example"
expect "...the old launcher, configs and drop-in removed" test ! -e "$LEGACY_REGISTRIES_SH" -a ! -e "$LEGACY_REGISTRY_CONF" -a ! -e "$LEGACY_MIRROR_CONF_DIR" -a ! -e "$u/firerunner-registry.service.d"
cp -a "$REGISTRY_DIR" "$R/before"; : > "$R/own.log"
registry_migrate >/dev/null 2>&1
expect "migration again: nothing copied" test ! -s "$R/own.log"
expect "...the mirrors' data unchanged" diff -r "$R/before" "$REGISTRY_DIR"
expect "...nothing renamed twice" test -z "$(compgen -G "$LEGACY_REGISTRY_DATA.migrated-*")"
legacy_fixture; mkdir -p "$REGISTRY_DIR/.docker.io.migrating"; echo junk > "$REGISTRY_DIR/.docker.io.migrating/half"
registry_migrate >/dev/null 2>&1
expect "a copy cut short is made again from the start" test ! -e "$REGISTRY_DIR/docker.io/half" -a -f "$REGISTRY_DIR/docker.io/docker/registry/v2/blobs/b1"
legacy_fixture; mkdir -p "$REGISTRY_DIR/docker.io"; echo copied > "$REGISTRY_DIR/docker.io/kept"
registry_migrate >/dev/null 2>&1
expect "cut short after the copy: the copy is kept, not made again" test -f "$REGISTRY_DIR/docker.io/kept" -a ! -e "$REGISTRY_DIR/docker.io/docker"
expect "...and the old data renamed, kept" test -f "$LEGACY_REGISTRY_DATA.migrated/docker/registry/v2/blobs/b1"
legacy_fixture; RUSED=$((RSIZE * 80 / 100))
registry_migrate >/dev/null 2>&1
expect "data that would fill the file system past 80 % is not copied" test ! -e "$REGISTRY_DIR/docker.io"
expect "...but kept" test -f "$LEGACY_REGISTRY_DATA.migrated/docker/registry/v2/blobs/b1"
legacy_fixture; RUSED=0 RSIZE=$((3 * 1024 * 1024))
registry_migrate >/dev/null 2>&1
expect "data that would leave under 3072 MiB free is not copied (a 3 GiB file system)" test ! -e "$REGISTRY_DIR/docker.io"
RSIZE=$((10 * 1024 * 1024))
legacy_fixture; RUSED=0 ROOTAVAIL=100
registry_migrate >/dev/null 2>&1
expect "no room on the root disk for a copy: not copied, kept" test ! -e "$REGISTRY_DIR/docker.io" -a -f "$LEGACY_REGISTRY_DATA.migrated/docker/registry/v2/blobs/b1"
ROOTAVAIL=$((50 * 1024 * 1024))

# Starting: dropped mirrors stop, a registry before its front, restarts wait for jobs.
FR_REGISTRY_MIRRORS=ghcr.io REGISTRY_FRONT=1 PENDING="" CHANGED=" "
rm -rf "$R/active"; mkdir -p "$R/active"; rm -f "$PENDING_FILE" "$R/jobs-running"
mkdir -p "$u/firerunner-registry.service.wants"
ln -sfn ../firerunner-registry@.service "$u/firerunner-registry.service.wants/firerunner-registry@quay.io.service"
: > "$SC_LOG"; registry_start >/dev/null 2>&1
line_of() { grep -n -m1 -x "$1" "$SC_LOG" | cut -d: -f1; }
expect "start: a dropped mirror is stopped and disabled" grep -qx 'disable --now -q firerunner-registry@quay.io.service' "$SC_LOG"
expect "...a mirror in use is not" ! grep -q 'disable --now -q firerunner-registry@ghcr.io' "$SC_LOG"
expect "start: every registry and front started" bash -c "grep -qx 'restart firerunner-registry@docker.io' '$SC_LOG' && grep -qx 'restart firerunner-registry-front@ghcr.io' '$SC_LOG'"
expect "...a registry before its front" test "$(line_of 'restart firerunner-registry@docker.io')" -lt "$(line_of 'restart firerunner-registry-front@docker.io')"
expect "...then firerunner-registry started, not restarted" bash -c "grep -qx 'start firerunner-registry' '$SC_LOG' && ! grep -qx 'restart firerunner-registry' '$SC_LOG'"
# Jobs run; docker.io's registry runs with a changed config (moving off the bridge port), its front is new.
touch "$R/active/firerunner-registry@docker.io" "$R/jobs-running"
CHANGED=" $y/docker.io.yml " PENDING=""; rm -f "$PENDING_FILE"; : > "$SC_LOG"
registry_start >/dev/null 2>&1
expect "jobs running: a changed registry is not restarted" ! grep -qx 'restart firerunner-registry@docker.io' "$SC_LOG"
expect "...nor its new front started (the registry may hold its port)" ! grep -qx 'restart firerunner-registry-front@docker.io' "$SC_LOG"
expect "...nor even enabled (firerunner-registry would start it)" ! grep -qx 'enable -q firerunner-registry-front@docker.io' "$SC_LOG"
expect "...both wait, the registry first" test "$(grep 'docker.io' "$PENDING_FILE" | tr '\n' ' ')" = "restart firerunner-registry@docker.io restart firerunner-registry-front@docker.io "
expect "...a mirror that did not run starts now" grep -qx 'restart firerunner-registry-front@ghcr.io' "$SC_LOG"
# The next run, when no job runs, with nothing changed since: the restart left
# for the registry comes first, then its front is enabled and started.
rm -f "$R/jobs-running"; touch "$R/active/firerunner-registry@ghcr.io" "$R/active/firerunner-registry-front@ghcr.io"
CHANGED=" " PENDING=""; : > "$SC_LOG"
registry_start >/dev/null 2>&1
expect "idle again: the registry's left restart is done" grep -qx 'restart firerunner-registry@docker.io' "$SC_LOG"
expect "...before its front is enabled" test "$(line_of 'restart firerunner-registry@docker.io')" -lt "$(line_of 'enable -q firerunner-registry-front@docker.io')"
expect "...and started" test "$(line_of 'enable -q firerunner-registry-front@docker.io')" -lt "$(line_of 'restart firerunner-registry-front@docker.io')"
expect "...nothing left pending for docker.io" ! grep -q 'docker.io' "$PENDING_FILE"
expect "...a running mirror with nothing changed is left alone" ! grep -q 'restart firerunner-registry.*@ghcr.io' "$SC_LOG"

# The forwarder stays while a mirror's restart (through it) is pending.
printf 'restart firerunner-registry@ghcr.io\n' > "$PENDING_FILE"; : > "$SC_LOG"; PENDING=""
stop_forwarder >/dev/null 2>&1
expect "forwarder kept while a mirror's restart is pending" ! grep -q 'disable --now -q firerunner-proxy' "$SC_LOG"

# ---- the forwarder's unit (configure_proxy, with a firerunner that accepts every config set)
mkdir -p "$BIN_DIR"; printf '#!/bin/sh\nexit 0\n' > "$BIN_DIR/firerunner"; chmod 0755 "$BIN_DIR/firerunner"
PROXY_FILE=$R/proxy-upstream CA_FILE=$R/ca.pem EGRESS_FILE=$R/egress-deny FR_CA_FILE='' FR_NO_PROXY='' FR_INSECURE_REGISTRIES=''
echo http://proxy.corp:3128 > "$PROXY_FILE"
configure_proxy >/dev/null 2>&1
P=$u/firerunner-proxy.service
expect "proxy unit: stopped no sooner than proxy.shutdown_timeout (30 s) + 15 s" grep -qx 'TimeoutStopSec=45' "$P"
expect "...a runtime directory for its metrics socket" grep -qx 'RuntimeDirectory=firerunner-proxy' "$P"
expect "...root only" grep -qx 'RuntimeDirectoryMode=0700' "$P"
expect "...and writable under ProtectSystem=strict" grep -qx 'ReadWritePaths=/run/firerunner-proxy' "$P"
expect "...the address families stay" grep -qx 'RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX' "$P"

# ---- drained hosts: gitlab-runner stays stopped (firerunner drain / undrain)
touch "$DRAIN_FILE" "$R/active/gitlab-runner"; : > "$SC_LOG"; PENDING=""
restart_when_idle gitlab-runner >/dev/null 2>&1
expect "drained: gitlab-runner is not restarted" ! grep -q '^restart gitlab-runner' "$SC_LOG"
printf 'restart gitlab-runner\nrestart containerd-flintlock\n' > "$PENDING_FILE"; : > "$SC_LOG"
apply_pending >/dev/null 2>&1
expect "drained: a pending gitlab-runner restart is not done" ! grep -q 'gitlab-runner' "$SC_LOG"
expect "...the other pending restarts are" grep -qx 'restart containerd-flintlock' "$SC_LOG"
rm -f "$DRAIN_FILE"; : > "$SC_LOG"
restart_when_idle gitlab-runner >/dev/null 2>&1
expect "not drained: gitlab-runner restarts as before" grep -qx 'restart gitlab-runner' "$SC_LOG"
# install_firerunner's upgrade of a drained host's gitlab-runner installs it without a start.
upgrade_keeps_drain() {
    declare -f install_firerunner | grep -A4 'if drained; then' | grep -q 'install -m 0755' &&
        ! declare -f install_firerunner | grep -A4 'if drained; then' | grep -q 'systemctl start'
}
expect "drained: an upgraded gitlab-runner is installed, not started" upgrade_keeps_drain

# ---- registry upkeep: eviction and garbage collection (firerunner-registry-maint)
registry_maint_script > "$WORK/registry-maint.sh"
S=$WORK/store
# blob STORE DIGEST ATIME: a 64 KiB blob last read at ATIME.
blob() {
    local d=$S/$1/docker/registry/v2/blobs/sha256/${2:0:2}/$2
    mkdir -p "$d"; head -c 65536 /dev/zero > "$d/data"; touch -a -d "@$3" "$d/data"
}
# The manifest aa01 is read longest ago; then the layers bb02, cc03 (ghcr.io), dd04.
store_fixture() {
    rm -rf "$S"; mkdir -p "$S"
    blob docker.io aa01 500; blob docker.io bb02 1000; blob ghcr.io cc03 2000; blob docker.io dd04 3000
    mkdir -p "$S/docker.io/docker/registry/v2/repositories/library/x/_manifests/revisions/sha256/aa01"
    echo sha256:aa01 > "$S/docker.io/docker/registry/v2/repositories/library/x/_manifests/revisions/sha256/aa01/link"
}
has_blob() { test -f "$S/$1/docker/registry/v2/blobs/sha256/${2:0:2}/$2/data"; }
store_fixture
K=$(du -k "$S/docker.io/docker/registry/v2/blobs/sha256/bb/bb02/data" | cut -f1)
# maint ARGS: the script with df reporting $MSIZE KiB, $MUSED used, the rest
# free; rm fails for a path containing $RM_FAILS, if set.
maint() {
    (
        df() { printf 'F 1024-blocks Used Available Capacity Mounted\n/dev/loop0 %s %s %s 0%% /x\n' "$MSIZE" "$MUSED" "$((MSIZE - MUSED))"; }
        mountpoint() { return 0; }
        rm() { if [[ -n ${RM_FAILS:-} && $* == *"$RM_FAILS"* ]]; then return 1; fi; command rm "$@"; }
        export -f df mountpoint rm
        MSIZE=$MSIZE MUSED=$MUSED RM_FAILS=${RM_FAILS:-} FR_REGISTRY_DIR=$S bash "$WORK/registry-maint.sh" "$@"
    )
}
MSIZE=$((10 * K)) MUSED=$((8 * K))
expect "evict: 20 % free, not under 10 %" maint evict 10 0 20 0
expect "...nothing deleted" bash -c "test \$(find '$S' -name data | wc -l) = 4"
MSIZE=$((10 * K)) MUSED=$((10 * K))
expect "evict: full, up to 20 % free" maint evict 10 0 20 0
expect "...the two layers read longest ago deleted" bash -c "! test -e '$S/docker.io/docker/registry/v2/blobs/sha256/bb/bb02' && ! test -e '$S/ghcr.io/docker/registry/v2/blobs/sha256/cc/cc03'"
expect "...across mirrors, the newest layer kept" has_blob docker.io dd04
expect "...a manifest never evicted, however old" has_blob docker.io aa01
# Absolute headroom: half the file system free is still too little under START_MIB.
store_fixture; MSIZE=$((100 * K)) MUSED=$((50 * K)) MIB=$((50 * K / 1024 + 1))
expect "evict: 50 % free but under START_MIB" maint evict 10 "$MIB" 20 "$MIB"
expect "...every layer it may delete is deleted, the manifest kept" bash -c "test \$(find '$S' -name data | wc -l) = 1 && test -f '$S/docker.io/docker/registry/v2/blobs/sha256/aa/aa01/data'"
# A layer it cannot delete (EACCES under SELinux, say) is skipped, not the end of the run.
store_fixture; MSIZE=$((10 * K)) MUSED=$((10 * K))
RM_FAILS=bb02
expect "evict: a layer it cannot delete fails the run" ! maint evict 10 0 20 0
RM_FAILS=""
expect "...after deleting the next ones in its place" bash -c "test -f '$S/docker.io/docker/registry/v2/blobs/sha256/bb/bb02/data' && ! test -e '$S/ghcr.io/docker/registry/v2/blobs/sha256/cc/cc03' && ! test -e '$S/docker.io/docker/registry/v2/blobs/sha256/dd/dd04'"
expect "evict: not mounted, nothing done" bash -c "store=\$(mktemp -d); mountpoint() { return 1; }; export -f mountpoint; FR_REGISTRY_DIR=\$store bash '$WORK/registry-maint.sh' evict 10 1536 20 3072 | grep -q 'not mounted'"

# The gc step: systemctl, setpriv logged to $GC/log. systemctl is-active says
# what $GC/state-of-unit holds (else inactive); setpriv fails with "boom" while
# $GC/fail exists, with distribution's "Path not found" while $GC/notfound does.
store_fixture
GC=$WORK/gc; mkdir -p "$GC/conf" "$GC/jobs"; : > "$GC/conf/docker.io.yml"; : > "$GC/conf/ghcr.io.yml"
gc_run() {
    (
        systemctl() {
            echo "systemctl $*" >> "$GC/log"
            if [[ $1 == is-active ]]; then
                local s=inactive
                [[ ! -f $GC/state-of-unit ]] || s=$(cat "$GC/state-of-unit")
                echo "$s"; [[ $s == active ]]
            fi
        }
        setpriv() {
            echo "setpriv $*" >> "$GC/log"
            if [[ -f $GC/fail ]]; then echo boom; return 1; fi
            if [[ -f $GC/notfound ]]; then echo 'Path not found: /docker/registry/v2/repositories/x/_layers'; return 1; fi
        }
        mountpoint() { return 0; }
        export -f systemctl setpriv mountpoint
        export GC
        FR_REGISTRY_DIR=$S FR_REGISTRY_CONF_DIR=$GC/conf FR_REGISTRY_STATE_DIR=$GC/state FR_JOBS_DIR=$GC/jobs \
            bash "$WORK/registry-maint.sh" gc
    ) >/dev/null 2>&1
}
gc_line() { grep -n -m1 -- "$1" "$GC/log" | cut -d: -f1; }
gc_fresh() { rm -rf "$GC/state" "$GC/fail" "$GC/notfound"; : > "$GC/log"; }
gc_fresh; echo active > "$GC/state-of-unit"; touch "$GC/jobs/job-1.json"
gc_run
expect "gc: nothing while a job runs" ! grep -q 'mask\|stop\|setpriv' "$GC/log"
rm -f "$GC/jobs/job-1.json"; : > "$GC/log"
expect "gc: idle, a mirror is garbage-collected" gc_run
expect "...masked first: nothing may start it meanwhile" test "$(gc_line 'systemctl mask --runtime -q firerunner-registry@docker.io')" -lt "$(gc_line 'systemctl stop firerunner-registry@docker.io')"
expect "...then stopped" test "$(gc_line 'systemctl stop firerunner-registry@docker.io')" -lt "$(gc_line 'garbage-collect')"
expect "...as the registry user, with its config" grep -q "setpriv --reuid=firerunner-registry --regid=firerunner-registry --clear-groups -- /usr/local/bin/registry garbage-collect $GC/conf/docker.io.yml" "$GC/log"
expect "...then unmasked" test "$(gc_line 'garbage-collect')" -lt "$(gc_line 'systemctl unmask --runtime -q firerunner-registry@docker.io')"
expect "...and started again" test "$(gc_line 'systemctl unmask --runtime -q firerunner-registry@docker.io')" -lt "$(gc_line 'systemctl start firerunner-registry@docker.io')"
expect "...a mirror that never stored an image is left alone (garbage-collect fails on it)" ! grep -q 'firerunner-registry@ghcr.io\|ghcr.io.yml' "$GC/log"
: > "$GC/log"; gc_run
expect "gc: not again the same day" ! grep -q 'setpriv' "$GC/log"
gc_fresh; touch "$GC/fail"
expect "gc: a failed garbage-collect fails the run" ! gc_run
expect "...and the mirror is unmasked and started again all the same" bash -c "grep -q 'systemctl unmask --runtime -q firerunner-registry@docker.io' '$GC/log' && grep -q 'systemctl start firerunner-registry@docker.io' '$GC/log'"
gc_fresh; touch "$GC/notfound"
expect "gc: distribution's 'Path not found' (manifests without layers) is no failure" gc_run
expect "...the mirror started again" grep -q 'systemctl start firerunner-registry@docker.io' "$GC/log"
gc_fresh; echo activating > "$GC/state-of-unit"
gc_run
expect "gc: a mirror starting or restarting after a crash is skipped" ! grep -q 'mask\|setpriv' "$GC/log"
gc_fresh; echo inactive > "$GC/state-of-unit"
gc_run
expect "gc: a mirror that is not running is collected while masked" bash -c "grep -q setpriv '$GC/log' && grep -q 'systemctl mask --runtime' '$GC/log'"
expect "...and not started" ! grep -q 'systemctl start' "$GC/log"

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
