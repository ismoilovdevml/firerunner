#!/usr/bin/env bash
# The variables set here are read by install.sh's functions.
# shellcheck disable=SC2034
# Tests installer steps that act on the host's disks and files with
# install.sh's own functions, the system commands they call faked: the cache
# size cap. It needs no root and changes nothing outside a temporary
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

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
