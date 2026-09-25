#!/usr/bin/env bash
# The variables set here are read by install.sh's functions, which also set the proxy ones.
# shellcheck disable=SC2034,SC2154
# Tests the installer's corporate-network steps (proxy, CA, drop-ins) with
# install.sh's own functions. It writes to /etc, so run it in a throwaway
# container:
#   docker run --rm -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/installer/proxy.sh
set -uo pipefail
INSTALL_SH=${INSTALL_SH:-/src/install.sh}
WORK=$(mktemp -d)
sed '$d' "$INSTALL_SH" > $WORK/install-lib.sh
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

# shellcheck disable=SC1090,SC1091
source $WORK/install-lib.sh
set +e; trap - ERR
CONF_DIR=$WORK/conf PROXY_FILE=$WORK/conf/proxy-upstream CA_FILE=$WORK/conf/ca.pem
FR_SUBNET=10.200.0 BIN_DIR=$WORK/bin
mkdir -p /etc/systemd/system /usr/local/share/ca-certificates
update-ca-certificates() { echo updated > $WORK/ca-updated; }

# ---- preflight refuses malformed settings
check_preflight() { ( FR_PROXY=$1 FR_NO_PROXY=${2:-} FR_CA_FILE=${3:-} FR_INSECURE_REGISTRIES=${4:-}
    die() { exit 1; }; log() { :; }; EUID=0 preflight_settings ) >/dev/null 2>&1; }
expect "FR_PROXY https:// refused"         ! check_preflight "https://proxy:3128"
expect "FR_PROXY without port refused"     ! check_preflight "http://proxy.corp"
expect "FR_PROXY with a path refused"      ! check_preflight "http://proxy:3128/x"
expect "FR_PROXY with credentials allowed"   check_preflight "http://CORP%5Cbob:p%40ss@proxy.corp:3128"
expect "FR_NO_PROXY with a space refused"  ! check_preflight "" "a.corp, b.corp"
expect "FR_NO_PROXY with \$() refused"     ! check_preflight "" 'a$(reboot)'
echo "not a cert" > $WORK/bad.pem
expect "FR_CA_FILE without a certificate refused" ! check_preflight "" "" $WORK/bad.pem
expect "FR_INSECURE_REGISTRIES with a path refused" ! check_preflight "" "" "" "harbor.corp/v2"
expect "FR_INSECURE_REGISTRIES http:// and host:port allowed" check_preflight "" "" "" "harbor.corp:443,http://10.0.0.5:5000"

# ---- setup_proxy: password file root-only, CA trusted, downloads use the proxy
printf -- '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n' > $WORK/corp.pem
FR_PROXY='http://bob:p%40ss@proxy.corp:3128/' FR_NO_PROXY='gitlab.corp' FR_CA_FILE=$WORK/corp.pem
setup_proxy >/dev/null 2>&1
expect "proxy file written without the trailing /" test "$(cat $PROXY_FILE)" = 'http://bob:p%40ss@proxy.corp:3128'
expect "proxy file is 0600" test "$(stat -c %a $PROXY_FILE)" = 600
expect "CA copied to the config dir" cmp -s $WORK/corp.pem $CA_FILE
expect "CA added to the host trust store" test -f $WORK/ca-updated
expect "installer downloads use the proxy" test "$https_proxy" = 'http://bob:p%40ss@proxy.corp:3128'
expect "installer no_proxy has the bridge and FR_NO_PROXY" test "$no_proxy" = 'localhost,127.0.0.1,10.200.0.1,gitlab.corp'

# ---- drop-ins: the local forwarder, never the password
proxy_dropin containerd-flintlock >/dev/null
D=/etc/systemd/system/containerd-flintlock.service.d/firerunner-proxy.conf
expect "drop-in written" test -f $D
expect "drop-in points at the local forwarder" grep -q '"HTTPS_PROXY=http://127.0.0.1:3128"' $D
expect "drop-in keeps the bridge and FR_NO_PROXY out of the proxy" grep -q 'NO_PROXY=localhost,127.0.0.1,::1,10.200.0.1,10.200.0.0/24,gitlab.corp"' $D
expect "drop-in holds no password" ! grep -q 'p%40ss\|bob' $D
expect "firewall lets microVMs reach the forwarder" test "$(proxy_input_rules)" = '    iifname "br-fc" ip daddr 10.200.0.1 tcp dport 3128 accept'

# ---- proxy removed: drop-ins and the firewall rule go away
rm -f $PROXY_FILE
proxy_dropin containerd-flintlock >/dev/null
expect "drop-in removed without a proxy" test ! -f $D
expect "no firewall rule without a proxy" test -z "$(proxy_input_rules)"

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
