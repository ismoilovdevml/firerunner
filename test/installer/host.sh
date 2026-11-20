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

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
