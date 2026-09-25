#!/usr/bin/env bash
# The variables set here are read by install.sh's functions.
# shellcheck disable=SC2034
# Tests the installer's mutual TLS for flintlockd's API with install.sh's own
# functions: the certificates, flintlockd's config, and when firerunner gets
# the client certificate. `openssl s_server` stands in for flintlockd. It
# writes to /etc, so run it in a throwaway container:
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
set +e; trap - ERR
CONF_DIR=$WORK/conf FLINTLOCK_TLS_DIR=$WORK/conf/flintlock-tls BIN_DIR=$WORK/bin
PENDING_FILE=$WORK/pending-restarts TMP_DIR=$WORK/tmp FLINTLOCK_TLS_WAIT=3
mkdir -p "$CONF_DIR" "$BIN_DIR" "$TMP_DIR" /etc/opt/flintlockd
echo token > "$CONF_DIR/flintlock.token"
# systemd: flintlockd and firerunner run; restarts are recorded.
systemctl() { echo "$*" >> "$WORK/systemctl.log"; [[ $1 != is-active ]] || [[ $3 == flintlockd || $3 == firerunner ]]; }
JOBS=0; jobs_running() { [[ $JOBS == 1 ]]; }
# firerunner: `config get/set` of the flintlock TLS keys, kept in a file. OLD=1
# is a version that does not know them.
fake_firerunner() {
    cat > "$BIN_DIR/firerunner" <<SH
#!/usr/bin/env bash
[[ \${OLD:-0} == 1 && \$3 == flintlock.tls* ]] && { echo "unknown key \$3" >&2; exit 1; }
f=$WORK/fr-config
case "\$1 \$2" in
  "config get") sed -n "s|^\$3=||p" "\$f" 2>/dev/null; exit 0 ;;
  "config set") shift 2; while [[ \$# -ge 2 ]]; do sed -i "\\|^\$1=|d" "\$f" 2>/dev/null; echo "\$1=\$2" >> "\$f"; shift 2; done ;;
esac
SH
    chmod +x "$BIN_DIR/firerunner"
}
D=$FLINTLOCK_TLS_DIR

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

# ---- when flintlockd may serve TLS
tls_now() { FLINTLOCK_TLS_LATER=""; flintlock_tls_now; }
rm -f "$BIN_DIR/firerunner"
expect "fresh install: TLS at once"                  tls_now
fake_firerunner; rm -f "$WORK/fr-config"
OLD=1 expect "firerunner without the keys: later"  ! tls_now
expect "...and says why"                             test "$FLINTLOCK_TLS_LATER" = "the installed firerunner predates it"
JOBS=1 expect "jobs running: later"                ! tls_now
JOBS=0 expect "idle: TLS now"                        tls_now
echo "flintlock.tls_cert_file=$D/client.crt" > "$WORK/fr-config"
JOBS=1 expect "firerunner uses TLS already: TLS even while jobs run" tls_now
rm -f "$WORK/fr-config"

flintlock_config 1 >/dev/null
expect "TLS config: no insecure, client certificates required" \
    bash -c "grep -qx 'insecure: false' /etc/opt/flintlockd/config.yaml && grep -qx 'tls-client-validate: true' /etc/opt/flintlockd/config.yaml && grep -qx 'tls-client-ca: $D/ca.crt' /etc/opt/flintlockd/config.yaml"
expect "config stays root-only"                      test "$(stat -c %a /etc/opt/flintlockd/config.yaml)" = 600

# ---- restart deferred by jobs: flintlockd's config goes back to no TLS
echo "restart flintlockd" > "$PENDING_FILE"; FLINTLOCK_TLS_LATER=""
flintlock_tls_deferred
expect "deferred restart: config back to no TLS"     grep -qx 'insecure: true' /etc/opt/flintlockd/config.yaml
expect "...and the reason is kept for the summary"   test -n "$FLINTLOCK_TLS_LATER"
flintlock_config 1 >/dev/null; echo "flintlock.tls_cert_file=$D/client.crt" > "$WORK/fr-config"; flintlock_tls_deferred
expect "deferred restart, firerunner on TLS: config kept" grep -qx 'insecure: false' /etc/opt/flintlockd/config.yaml
rm -f "$PENDING_FILE" "$WORK/fr-config"

# ---- firerunner gets the client certificate only from a TLS flintlockd
serve() { # serve CERT KEY CLIENT_CA: a TLS server on flintlock's port that wants a client certificate
    openssl s_server -quiet -accept 127.0.0.1:9090 -cert "$1" -key "$2" -CAfile "$3" -Verify 1 -verify_return_error -naccept 50 </dev/null >/dev/null 2>&1 &
    SERVER=$!; sleep 0.5
}
stop() { kill "$SERVER" 2>/dev/null; wait "$SERVER" 2>/dev/null; }
uses_tls() { [[ "$("$BIN_DIR/firerunner" config get flintlock.tls_cert_file)" == "$D/client.crt" ]]; }

busybox httpd -p 127.0.0.1:9090 -h "$WORK"; sleep 0.3                 # flintlockd still without TLS
flintlock_tls_client 2>/dev/null
expect "plaintext flintlockd: firerunner keeps plaintext" ! uses_tls
pkill -f "busybox httpd"; sleep 0.3

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=impostor \
    -addext "subjectAltName=IP:127.0.0.1" -keyout "$WORK/imp.key" -out "$WORK/imp.crt" 2>/dev/null
serve "$WORK/imp.crt" "$WORK/imp.key" "$D/ca.crt"
flintlock_tls_client 2>/dev/null; stop
expect "a server with a certificate of another CA: not trusted" ! uses_tls

echo "restart flintlockd" > "$PENDING_FILE"
serve "$D/server.crt" "$D/server.key" "$D/ca.crt"
flintlock_tls_client 2>/dev/null
expect "flintlockd restart still pending: no change"  ! uses_tls
rm -f "$PENDING_FILE"; : > "$WORK/systemctl.log"
flintlock_tls_client >/dev/null 2>&1; stop
expect "TLS flintlockd: firerunner gets ca, cert and key" \
    bash -c "grep -qx 'flintlock.tls_ca_file=$D/ca.crt' '$WORK/fr-config' && grep -qx 'flintlock.tls_cert_file=$D/client.crt' '$WORK/fr-config' && grep -qx 'flintlock.tls_key_file=$D/client.key' '$WORK/fr-config'"
expect "...and the daemon restarts"                   grep -qx "restart firerunner" "$WORK/systemctl.log"

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
