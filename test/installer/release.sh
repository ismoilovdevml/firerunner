#!/usr/bin/env bash
# Tests the installer's check of the release signature (verify_release) with
# install.sh's own function, against a local HTTP server and a key of its own.
# Run it in a throwaway container:
#   docker run --rm -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/installer/release.sh
set -uo pipefail
apt-get update -qq >/dev/null && apt-get install -y -qq curl openssl busybox >/dev/null || exit 99
INSTALL_SH=${INSTALL_SH:-/src/install.sh}
WORK=$(mktemp -d)
sed '$d' "$INSTALL_SH" > "$WORK/install-lib.sh"
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "PASS  $1"; }
bad() { fail=$((fail+1)); echo "FAIL  $1"; }
# expect NAME [!] COMMAND...: the command succeeds (or, after !, fails).
expect() {
    local name=$1 want=0; shift
    if [[ $1 == "!" ]]; then want=1; shift; fi
    "$@"; local got=$(( $? != 0 ))
    if [[ $got -eq $want ]]; then ok "$name"; else bad "$name"; fi
}

# shellcheck disable=SC1091
source "$WORK/install-lib.sh"
set +e; trap - ERR

# The release is signed with this test's key; another key stands for a forger.
openssl genpkey -algorithm ed25519 -out "$WORK/release.key" 2>/dev/null
openssl genpkey -algorithm ed25519 -out "$WORK/other.key" 2>/dev/null
RELEASE_KEY=$(openssl pkey -in "$WORK/release.key" -pubout)
sign() { openssl pkeyutl -sign -inkey "$WORK/$1" -rawin -in "$2" -out "$2.sig"; }

SRV=$WORK/srv
for r in signed unsigned forged tampered locked; do
    mkdir -p "$SRV/$r"
    echo "0123abcd  firerunner-linux-amd64" > "$SRV/$r/checksums.txt"
done
sign release.key "$SRV/signed/checksums.txt"
sign other.key "$SRV/forged/checksums.txt"
sign release.key "$SRV/tampered/checksums.txt"
echo "4567ef01  firerunner-linux-amd64" > "$SRV/tampered/checksums.txt"   # changed after signing
sign release.key "$SRV/locked/checksums.txt"
echo "/locked:user:secret" > "$WORK/httpd.conf"                          # 401 for this release
busybox httpd -p 127.0.0.1:18080 -h "$SRV" -c "$WORK/httpd.conf"
sleep 0.5
URL=http://127.0.0.1:18080

# release NAME [FR_ALLOW_UNSIGNED]: what install_firerunner does with a release's checksums.
release() {
    local dir=$WORK/dl/$1; mkdir -p "$dir"
    curl -fsS -o "$dir/fr.sums" "$URL/$1/checksums.txt" 2>/dev/null || cp "$SRV/$1/checksums.txt" "$dir/fr.sums"
    ( FR_VERSION=v9.9.9 FR_ALLOW_UNSIGNED=${2:-} verify_release "$dir/fr.sums" "$URL/$1/checksums.txt.sig" ) 2>"$dir/err"
}
expect "signed release accepted"                               release signed
expect "unsigned release refused"                            ! release unsigned
grep -q "FR_ALLOW_UNSIGNED=1 installs it anyway" "$WORK/dl/unsigned/err" && ok "refusal names FR_ALLOW_UNSIGNED" || bad "refusal names FR_ALLOW_UNSIGNED"
expect "unsigned release with FR_ALLOW_UNSIGNED=1"             release unsigned 1
grep -q "WARNING: firerunner v9.9.9 is not signed" "$WORK/dl/unsigned/err" && ok "unsigned install warns" || bad "unsigned install warns"
expect "release signed with another key refused"             ! release forged
expect "...also with FR_ALLOW_UNSIGNED=1"                    ! release forged 1
expect "checksums changed after signing refused"             ! release tampered
expect "signature download error (401) is not 'unsigned'"    ! release locked 1
grep -q "HTTP 401" "$WORK/dl/locked/err" && ok "download error named" || bad "download error named"

# install_firerunner checks the signature before it trusts a checksum.
signature_first() { declare -f install_firerunner | grep -A1 'verify_release ' | grep -q 'verify "'; }
expect "install_firerunner checks the signature before the checksum" signature_first
# The key in install.sh is the one release.yml signs with; its Go copy is
# compared by internal/upgrade's tests.
( source "$WORK/install-lib.sh"; printf '%s\n' "$RELEASE_KEY" > "$WORK/shipped.pub" )
expect "install.sh's key is an Ed25519 public key"             bash -c "openssl pkey -pubin -in '$WORK/shipped.pub' -noout -text 2>/dev/null | grep -q ED25519"

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
