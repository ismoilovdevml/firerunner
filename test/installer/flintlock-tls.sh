#!/usr/bin/env bash
# The variables set here are read by install.sh's functions.
# shellcheck disable=SC2034
# Tests the installer's mutual TLS for flintlockd's API with install.sh's own
# functions: the certificates, flintlockd's config following firerunner, and
# the switch to TLS. A fake flintlockd (openssl s_server with TLS, busybox
# httpd without) restarts from the config the installer wrote; a fake
# firerunner's `vm list` gets an answer only when that server accepted its
# client certificate. It writes to /etc, so run it in a throwaway container:
#   docker run --rm -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/installer/flintlock-tls.sh
set -uo pipefail
apt-get update -qq >/dev/null && apt-get install -y -qq openssl busybox >/dev/null || exit 99
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
    "$@" >/dev/null; local got=$(( $? != 0 ))
    if [[ $got -eq $want ]]; then ok "$name"; else bad "$name"; fi
}

# shellcheck disable=SC1091
source "$WORK/install-lib.sh"
set +e; trap - ERR EXIT INT HUP TERM
CONF_DIR=$WORK/conf FLINTLOCK_TLS_DIR=$WORK/conf/flintlock-tls BIN_DIR=$WORK/bin
PENDING_FILE=$WORK/pending-restarts TMP_DIR=$WORK/tmp FLINTLOCK_TLS_WAIT=4
FLINTLOCK_TLS_MARKER=$WORK/flintlock-tls-switching
mkdir -p "$CONF_DIR" "$BIN_DIR" "$TMP_DIR" /etc/opt/flintlockd
echo token > "$CONF_DIR/flintlock.token"
FLCONF=/etc/opt/flintlockd/config.yaml
D=$FLINTLOCK_TLS_DIR

# fake flintlockd on 127.0.0.1:9090, started from its config like the real one;
# REJECT=1: it does not accept firerunner's client certificate.
flintlockd_start() {
    if grep -qx 'insecure: false' "$FLCONF"; then
        local ca; ca=$(sed -n 's/^tls-client-ca: //p' "$FLCONF")
        [[ ${REJECT:-0} == 1 ]] && ca=$WORK/other.crt
        openssl s_server -quiet -www -accept 127.0.0.1:9090 -cert "$(sed -n 's/^tls-cert: //p' "$FLCONF")" \
            -key "$(sed -n 's/^tls-key: //p' "$FLCONF")" -CAfile "$ca" -Verify 1 -verify_return_error \
            -naccept 200 </dev/null >/dev/null 2>&1 &
    else
        busybox httpd -f -p 127.0.0.1:9090 -h "$WORK" &
    fi
    sleep 0.4
}
# Stopped by what it listens on: it may have been started in another subshell.
flintlockd_stop() {
    local i
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        pkill -f -- "-accept 127.0.0.1:9090" || true
        pkill -f -- "httpd -f -p 127.0.0.1:9090" || true
        timeout 1 bash -c '</dev/tcp/127.0.0.1/9090' 2>/dev/null || return 0
        [[ $i -gt 10 ]] && pkill -9 -f -- "127.0.0.1:9090"
        sleep 0.1
    done
}
# systemd: flintlockd (unless FL_DOWN=1) and firerunner are active; restarts
# are recorded, and flintlockd's restarts it from its config.
systemctl() {
    echo "$*" >> "$WORK/systemctl.log"
    case "$1 ${3:-${2:-}}" in
        "is-active flintlockd") [[ ${FL_DOWN:-0} != 1 ]]; return ;;
        "is-active firerunner") return 0 ;;
        "is-active "*) return 1 ;;
        "restart flintlockd") if [[ ${SLOW:-0} == 1 ]]; then sleep 1; fi; flintlockd_stop; flintlockd_start ;;
    esac
    return 0
}
JOBS=0; jobs_running() { [[ $JOBS == 1 ]]; }
# fake firerunner: `config get/set/path` of the flintlock TLS keys, kept like
# firerunner writes them (YAML under flintlock:) in the file FIRERUNNER_CONFIG
# names, by default $CONF_DIR/config.yaml; and `vm list`: a request over TLS
# with those keys, answered only when the server accepted the client
# certificate. OLD=1 does not know the keys; ONEPAIR=1 sets one key per
# `config set` (before SetAll); BROKEN=1 refuses its config.
cat > "$BIN_DIR/firerunner" <<'SH'
#!/usr/bin/env bash
f=${FIRERUNNER_CONFIG:-__CONF__/config.yaml}
[[ ${BROKEN:-0} == 1 ]] && { echo "config: invalid" >&2; exit 1; }
[[ ${OLD:-0} == 1 && ${3:-} == flintlock.tls* ]] && { echo "unknown key $3" >&2; exit 1; }
get() { sed -n "s|^    ${1#flintlock.}: ||p" "$f" 2>/dev/null; }
case "$1 ${2:-}" in
  "config get") get "$3"; exit 0 ;;
  "config path") echo "$f" ;;
  "config set")
    shift 2
    [[ ${ONEPAIR:-0} == 1 && $# -ne 2 ]] && { echo "usage: config set <key> <value>" >&2; exit 1; }
    grep -q '^flintlock:' "$f" 2>/dev/null || echo 'flintlock:' >> "$f"
    while [[ $# -ge 2 ]]; do sed -i "\|^    ${1#flintlock.}:|d" "$f"; echo "    ${1#flintlock.}: $2" >> "$f"; shift 2; done ;;
  "vm list")
    if [[ -z $(get flintlock.tls_ca_file) ]]; then
        # A plaintext client reaches only the plaintext flintlockd.
        pgrep -f -- "-accept 127.0.0.1:9090" >/dev/null && { echo "flintlock: TLS server" >&2; exit 1; }
        timeout 3 bash -c '</dev/tcp/127.0.0.1/9090' 2>/dev/null || { echo "flintlock: connection refused" >&2; exit 1; }
        exit 0
    fi
    printf 'GET / HTTP/1.0\r\n\r\n' | timeout 4 openssl s_client -quiet -connect 127.0.0.1:9090 -verify_return_error \
        -verify_ip 127.0.0.1 -CAfile "$(get flintlock.tls_ca_file)" -cert "$(get flintlock.tls_cert_file)" \
        -key "$(get flintlock.tls_key_file)" 2>/dev/null | grep -q '^HTTP/1.0 200' || { echo "flintlock: refused" >&2; exit 1; } ;;
esac
SH
sed -i "s|__CONF__|$CONF_DIR|" "$BIN_DIR/firerunner"; chmod +x "$BIN_DIR/firerunner"
FRCONF=$CONF_DIR/config.yaml
fr_tls() { printf 'flintlock:\n    tls_ca_file: %s\n    tls_cert_file: %s\n    tls_key_file: %s\n' "$D/ca.crt" "$D/client.crt" "$D/client.key" > "$FRCONF"; }
fr_plain() { printf 'flintlock:\n    endpoint: 127.0.0.1:9090\n' > "$FRCONF"; }
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=other \
    -keyout "$WORK/other.key" -out "$WORK/other.crt" 2>/dev/null

# ---- certificates
flintlock_tls_certs >/dev/null
expect "CA, flintlockd and client certificates made" test -s "$D/ca.crt" -a -s "$D/server.crt" -a -s "$D/client.crt"
expect "certificate directory is root-only"          test "$(stat -c %a "$D")" = 700
expect "private keys are 0600"                       bash -c "[[ \$(stat -c %a '$D/ca.key' '$D/server.key' '$D/client.key' | sort -u) == 600 ]]"
expect "server certificate chains to the CA"         openssl verify -CAfile "$D/ca.crt" "$D/server.crt"
expect "server certificate is for 127.0.0.1"         bash -c "openssl x509 -in '$D/server.crt' -noout -ext subjectAltName | grep -q 'IP Address:127.0.0.1'"
expect "client certificate is for clients only"      bash -c "openssl x509 -in '$D/client.crt' -noout -ext extendedKeyUsage | grep -q 'Client Authentication' && ! openssl x509 -in '$D/client.crt' -noout -ext extendedKeyUsage | grep -q Server"
expect "server key matches its certificate"          bash -c "[[ \$(openssl pkey -in '$D/server.key' -pubout) == \$(openssl x509 -in '$D/server.crt' -noout -pubkey) ]]"
sums() { sha256sum "$D"/*.crt "$D"/*.key; }
before=$(sums); CHANGED=" "; flintlock_tls_certs >/dev/null
expect "a second run changes nothing"                test "$before" = "$(sums)" -a "$CHANGED" = " "
# a client certificate that expires within 90 days is made again, the CA is kept
openssl x509 -req -in <(openssl req -new -key "$D/client.key" -subj /CN=firerunner 2>/dev/null) -CA "$D/ca.crt" -CAkey "$D/ca.key" \
    -days 30 -out "$D/client.crt" 2>/dev/null
ca_before=$(sha256sum "$D/ca.crt"); flintlock_tls_certs >/dev/null
expect "an expiring client certificate is renewed"   openssl x509 -checkend 7776000 -noout -in "$D/client.crt"
expect "...and the CA is kept"                       test "$ca_before" = "$(sha256sum "$D/ca.crt")"
# a server certificate from another CA is made again, and flintlockd must restart
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=other -keyout "$D/server.key" -out "$D/server.crt" 2>/dev/null
CHANGED=" "; flintlock_tls_certs >/dev/null
expect "a server certificate of another CA is reissued" openssl verify -CAfile "$D/ca.crt" "$D/server.crt"
expect "...and counts as changed (flintlockd restarts)" changed "$D/server.crt"

# a key that does not match its certificate (a run cut off between the two) is reissued
openssl genpkey -algorithm ec -pkeyopt ec_paramgen_curve:P-256 -out "$D/client.key" 2>/dev/null
flintlock_tls_certs >/dev/null
expect "a key not matching its certificate is reissued" \
    bash -c "[[ \$(openssl pkey -in '$D/client.key' -pubout) == \$(openssl x509 -in '$D/client.crt' -noout -pubkey) ]]"
# the CA is never replaced while it works, even without its key
mv "$D/ca.key" "$WORK/ca.key.away"; ca_before=$(sha256sum "$D/ca.crt")
flintlock_tls_certs >/dev/null 2>"$WORK/warn"
expect "CA without its key: kept, with a warning"    bash -c "[[ '$ca_before' == \"\$(sha256sum '$D/ca.crt')\" ]] && grep -q 'cannot be renewed' '$WORK/warn'"
mv "$WORK/ca.key.away" "$D/ca.key"

# ---- flintlockd's config follows what firerunner uses
fr_plain
flintlock_config_follow >/dev/null
expect "firerunner without TLS: flintlockd without TLS" grep -qx 'insecure: true' "$FLCONF"
fr_tls
flintlock_config_follow >/dev/null
expect "firerunner on TLS: flintlockd on TLS, client certificates required" \
    bash -c "grep -qx 'insecure: false' $FLCONF && grep -qx 'tls-client-validate: true' $FLCONF && grep -qx 'tls-client-ca: $D/ca.crt' $FLCONF"
expect "config stays root-only"                      test "$(stat -c %a "$FLCONF")" = 600
BROKEN=1 flintlock_config_follow >/dev/null
expect "a firerunner refusing its config does not turn TLS off" grep -qx 'insecure: false' "$FLCONF"
mv "$BIN_DIR/firerunner" "$WORK/fr.away"; flintlock_config_follow >/dev/null; mv "$WORK/fr.away" "$BIN_DIR/firerunner"
expect "...nor a missing binary (reinstall)"          grep -qx 'insecure: false' "$FLCONF"
# a missing CA while firerunner is on TLS: both restart at once
mv "$D/ca.crt" "$WORK/ca.crt.away"; FLINTLOCK_TLS_RESET=0
flintlock_tls_certs >/dev/null
expect "a new CA while on TLS restarts both now"     test "$FLINTLOCK_TLS_RESET" = 1
FLINTLOCK_TLS_RESET=0

# firerunner's config changes under a running flintlockd (a reinstall with an
# older config.yaml): the match after install_firerunner restarts it at once
fr_tls; flintlock_config 0 >/dev/null; flintlockd_stop; flintlockd_start; : > "$WORK/systemctl.log"
flintlock_tls_match >/dev/null
expect "firerunner now on TLS: flintlockd follows at once" \
    bash -c "grep -qx 'insecure: false' $FLCONF && grep -qx 'restart flintlockd' '$WORK/systemctl.log'"
expect "...and firerunner reaches it"                "$BIN_DIR/firerunner" vm list
: > "$WORK/systemctl.log"; flintlock_tls_match >/dev/null
expect "...a second match changes nothing"          bash -c "! grep -q restart '$WORK/systemctl.log'"
fr_plain; flintlock_config_follow >/dev/null; flintlockd_stop; flintlockd_start

# ---- the switch, run under set -e like the installer
switch() { : > "$WORK/systemctl.log"; rm -f "$WORK/later"; ( set -e; FLINTLOCK_TLS_LATER=""; flintlock_tls_switch; echo "$FLINTLOCK_TLS_LATER" > "$WORK/later" ) >/dev/null 2>"$WORK/warn"; }
later() { [[ $(cat "$WORK/later" 2>/dev/null) == "$1" ]]; }
no_restart() { ! grep -q restart "$WORK/systemctl.log"; }
plain() { grep -qx 'insecure: true' "$FLCONF"; }
listening() { timeout 2 bash -c '</dev/tcp/127.0.0.1/9090'; }
uses_tls() { [[ "$("$BIN_DIR/firerunner" config get flintlock.tls_cert_file)" == "$D/client.crt" ]]; }

OLD=1 switch
expect "firerunner without the keys: no switch, no abort" later "the installed firerunner does not know it"
expect "...nothing restarted, flintlockd without TLS" bash -c "! grep -q restart '$WORK/systemctl.log' && grep -qx 'insecure: true' $FLCONF"
ONEPAIR=1 switch
expect "firerunner setting one key at a time: no switch, no abort" later "the installed firerunner does not know it"
JOBS=1 switch
expect "jobs running: no switch"                     no_restart
expect "...and says why"                             later "jobs are running"
FL_DOWN=1 switch
expect "flintlockd not running: no switch"           later "flintlockd is not running"

REJECT=1 switch
expect "client certificate refused: firerunner keeps no keys" bash -c "! grep -q tls_cert_file '$FRCONF'"
expect "...flintlockd back to no TLS"                plain
expect "...and serving"                              listening
expect "...restarted twice, the daemon not"          bash -c "[[ \$(grep -cx 'restart flintlockd' '$WORK/systemctl.log') == 2 && \$(grep -cx 'restart firerunner' '$WORK/systemctl.log') == 0 ]]"
expect "...no marker or saved config left"          test ! -e "$FLINTLOCK_TLS_MARKER" -a ! -e "$FLCONF.plain"
expect "...and it says so"                           grep -q "could not reach flintlockd over TLS" "$WORK/warn"

echo "restart flintlockd" > "$PENDING_FILE"
switch
expect "switch: firerunner gets ca, cert and key"    bash -c "grep -qx '    tls_ca_file: $D/ca.crt' '$FRCONF' && grep -qx '    tls_cert_file: $D/client.crt' '$FRCONF' && grep -qx '    tls_key_file: $D/client.key' '$FRCONF'"
expect "...flintlockd on TLS, and firerunner reaches it" bash -c "grep -qx 'insecure: false' $FLCONF && '$BIN_DIR/firerunner' vm list"
expect "...flintlockd then the daemon restarted once" bash -c "[[ \$(grep -cx 'restart flintlockd' '$WORK/systemctl.log') == 1 && \$(grep -cx 'restart firerunner' '$WORK/systemctl.log') == 1 ]]"
expect "...the pending flintlockd restart is done"   bash -c "! grep -q flintlockd '$PENDING_FILE'"
expect "...no marker or saved config left"          test ! -e "$FLINTLOCK_TLS_MARKER" -a ! -e "$FLCONF.plain"
switch
expect "on TLS already: nothing restarts"            no_restart

# ---- a run killed inside the switch
fr_plain; flintlock_config_follow >/dev/null; flintlockd_stop; flintlockd_start
cp "$BIN_DIR/firerunner" "$WORK/fr.real"
# firerunner hanging on `vm list`, like a flintlockd that never answers
printf '#!/bin/sh\n[ "$1" = vm ] && exec sleep 30\nexec %s "$@"\n' "$WORK/fr.real" > "$BIN_DIR/firerunner"
kill_switch() { # kill_switch TERMS: TERM a switch that waits for firerunner, TERMS times, 0.3 s apart
    (
        eval "$(grep -x 'trap cleanup EXIT' "$INSTALL_SH")"; eval "$(grep -x "trap 'exit 130' INT HUP TERM" "$INSTALL_SH")"
        FLINTLOCK_TLS_WAIT=60 TMP_DIR=$WORK/tmp-kill; mkdir -p "$TMP_DIR"
        flintlock_tls_switch
    ) >/dev/null 2>&1 &
    local pid=$! i
    for _ in $(seq 1 50); do grep -qx 'insecure: false' "$FLCONF" && break; sleep 0.1; done; sleep 0.3
    for ((i = 0; i < $1; i++)); do kill -TERM "$pid" 2>/dev/null; sleep 0.3; done
    wait "$pid" 2>/dev/null
}
kill_switch 1
expect "SIGTERM mid-switch: flintlockd back to no TLS" plain
expect "...no marker or saved config left"           test ! -e "$FLINTLOCK_TLS_MARKER" -a ! -e "$FLCONF.plain"
SLOW=1 kill_switch 4           # more TERMs while the put-back restarts flintlockd (slowly)
expect "TERM again during the put-back: still put back" bash -c "grep -qx 'insecure: true' $FLCONF && test ! -e '$FLINTLOCK_TLS_MARKER'"
mv "$WORK/fr.real" "$BIN_DIR/firerunner"; flintlockd_stop; flintlockd_start

# a full disk during the put-back: the saved config is moved back, not written
cp -p "$FLCONF" "$FLCONF.plain"; flintlock_config 1 >/dev/null; : > "$FLINTLOCK_TLS_MARKER"
( set -e; put() { return 1; }; FLINTLOCK_TLS_SWITCHING=1; flintlock_tls_revert ) >/dev/null 2>&1
expect "a put-back that cannot write still restores no TLS" bash -c "grep -qx 'insecure: true' $FLCONF && test ! -e '$FLINTLOCK_TLS_MARKER'"

# killed outright (no trap): the marker is found by the next run
: > "$FLINTLOCK_TLS_MARKER"; FLINTLOCK_TLS_RESET=0 FLINTLOCK_TLS_DAEMON=0
flintlock_tls_leftover 2>/dev/null
expect "leftover marker, the two still talk: only the daemon restarts" \
    bash -c "[[ $FLINTLOCK_TLS_RESET == 0 && $FLINTLOCK_TLS_DAEMON == 1 ]] && test ! -e '$FLINTLOCK_TLS_MARKER'"
flintlock_config 1 >/dev/null; flintlockd_stop; flintlockd_start; flintlock_config 0 >/dev/null   # flintlockd on TLS, firerunner not
: > "$FLINTLOCK_TLS_MARKER"; FLINTLOCK_TLS_RESET=0 FLINTLOCK_TLS_DAEMON=0
flintlock_tls_leftover 2>/dev/null
expect "leftover marker, the two apart: both restart, jobs or not" test "$FLINTLOCK_TLS_RESET" = 1
: > "$WORK/systemctl.log"; CHANGED=" "
JOBS=1 start_services >/dev/null 2>&1
expect "...flintlockd restarts although nothing changed" grep -qx 'restart flintlockd' "$WORK/systemctl.log"
FLINTLOCK_TLS_RESET=0; : > "$WORK/systemctl.log"; CHANGED=" "
start_services >/dev/null 2>&1
expect "without it, an unchanged flintlockd is left alone" bash -c "! grep -qx 'restart flintlockd' '$WORK/systemctl.log'"
# its certificates count only while it serves TLS
: > "$WORK/systemctl.log"; CHANGED=" $D/server.crt "
start_services >/dev/null 2>&1
expect "a new server certificate, flintlockd without TLS: no restart" bash -c "! grep -qx 'restart flintlockd' '$WORK/systemctl.log'"
flintlock_config 1 >/dev/null; : > "$WORK/systemctl.log"; CHANGED=" $D/server.crt "
start_services >/dev/null 2>&1
expect "...with TLS: it restarts to load it"          grep -qx 'restart flintlockd' "$WORK/systemctl.log"
flintlockd_stop

# the installer ends on a signal through its EXIT trap, like the subshell above
expect "install.sh turns INT, HUP and TERM into an exit" grep -qx "trap 'exit 130' INT HUP TERM" "$INSTALL_SH"

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
