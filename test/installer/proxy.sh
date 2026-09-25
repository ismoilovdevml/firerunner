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
CONF_DIR=$WORK/conf PROXY_FILE=$WORK/conf/proxy-upstream CA_FILE=$WORK/conf/ca.pem EGRESS_FILE=$WORK/conf/egress-deny
PENDING_FILE=$WORK/pending-restarts LIB_DIR=$WORK/lib
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
expect "FR_PROXY with # in the password refused"   ! check_preflight "http://bob:p#ss@proxy:3128"
expect "FR_PROXY with a bare % refused"            ! check_preflight "http://bob:50%off@proxy:3128"
expect "FR_PROXY at the forwarder itself refused"  ! check_preflight "http://127.0.0.1:3128"
expect "FR_PROXY at localhost:3128 refused"        ! check_preflight "http://localhost:3128"
expect "FR_PROXY at the bridge address refused"    ! check_preflight "http://10.200.0.1:3128"
expect "FR_PROXY at a local CNTLM on 3129 allowed"   check_preflight "http://127.0.0.1:3129"

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

# ---- FR_EGRESS_DENY is remembered; FR_EGRESS_DENY=none clears it
FR_PROXY='' FR_CA_FILE='' FR_EGRESS_DENY=192.168.0.0/16,10.9.0.0/16
setup_proxy >/dev/null 2>&1
FR_EGRESS_DENY=''
expect "egress deny remembered for a re-run" test "$(egress_deny)" = "192.168.0.0/16,10.9.0.0/16"
expect "firewall rule from the remembered list" test "$(egress_deny_rules)" = '    iifname "br-fc" ip daddr { 192.168.0.0/16, 10.9.0.0/16 } drop'
FR_EGRESS_DENY=none
setup_proxy >/dev/null 2>&1
expect "FR_EGRESS_DENY=none clears it" test -z "$(egress_deny)"
FR_EGRESS_DENY=''
expect "cleared list stays cleared" test -z "$(egress_deny)"

# ---- discard_blocks follows the pool's zeroing
lvs() { [[ $1 == --noheadings ]] && echo "  $LVS_ZERO"; return 0; }
LVS_ZERO=zero; expect "zeroing pool: discard off" test "$(discard_blocks)" = false
LVS_ZERO="";   expect "pool without zeroing: discard on" test "$(discard_blocks)" = true
unset -f lvs

# ---- restarts wait while jobs run
systemctl() { echo "$*" >> $WORK/systemctl.log; [[ $1 == is-active ]] && return 0; return 0; }
mkdir -p /run/firerunner/jobs; : > /run/firerunner/jobs/job-1.json
: > $WORK/systemctl.log; PENDING=""
restart_when_idle containerd-flintlock >/dev/null 2>&1
expect "no restart while a job runs" ! grep -q "^restart containerd-flintlock" $WORK/systemctl.log
expect "the restart is reported as pending" test "$PENDING" = "containerd-flintlock "
expect "the pending restart is remembered for a later run" grep -qx "restart containerd-flintlock" $PENDING_FILE

# ---- the forwarder stops only after the services that use it
STOP_FORWARDER=1; : > $WORK/systemctl.log
stop_forwarder >/dev/null 2>&1
expect "forwarder kept while a service restart is pending" ! grep -q "disable --now -q firerunner-proxy" $WORK/systemctl.log
expect "its stop is remembered after the restart" test "$(tail -1 $PENDING_FILE)" = "stop firerunner-proxy"

# ---- a later run while jobs still run applies nothing; an idle one applies all, in order
: > $WORK/systemctl.log
apply_pending >/dev/null 2>&1
expect "nothing applied while jobs run" test ! -s $WORK/systemctl.log
rm -f /run/firerunner/jobs/job-1.json
apply_pending >/dev/null 2>&1
expect "idle run: pending restart applied" grep -qx "restart containerd-flintlock" $WORK/systemctl.log
expect "idle run: then the forwarder stopped" test "$(tail -1 $WORK/systemctl.log)" = "disable --now -q firerunner-proxy"
expect "pending list cleared" test ! -f $PENDING_FILE

# ---- a restart done now drops the same pending entry
echo "restart firerunner-registry" > $PENDING_FILE; : > $WORK/systemctl.log
restart_when_idle firerunner-registry >/dev/null 2>&1
expect "restart when no job runs" grep -q "^restart firerunner-registry" $WORK/systemctl.log
expect "its pending entry is dropped" ! grep -q firerunner-registry $PENDING_FILE

# ---- FR_EGRESS_DENY given to a run from before it was remembered is kept
rm -f $EGRESS_FILE; mkdir -p $LIB_DIR
printf '    iifname "br-fc" ip daddr { 192.168.0.0/16, 172.16.0.0/12 } drop\n    iifname "br-fc" ip daddr 169.254.0.0/16 drop\n' > $LIB_DIR/net-up.sh
FR_EGRESS_DENY='' FR_PROXY='' FR_CA_FILE=''
setup_proxy >/dev/null 2>&1
expect "old FR_EGRESS_DENY recovered from net-up.sh" test "$(egress_deny)" = "192.168.0.0/16,172.16.0.0/12"

# ---- FR_CA_FILE=none drops the CA
expect "FR_CA_FILE=none passes preflight" check_preflight "" "" none
printf -- '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n' > $CA_FILE
FR_CA_FILE=none
setup_proxy >/dev/null 2>&1
expect "FR_CA_FILE=none removes the CA" test ! -f $CA_FILE
FR_CA_FILE=''

# ---- the drop-ins are applied once per run (a second call restarts again)
expect "proxy_dropins_apply is called once in main" test "$(grep -c '^    proxy_dropins_apply$' "$INSTALL_SH")" = 1

# ---- `sudo firerunner` works where sudo's secure_path lacks /usr/local/bin
mkdir -p $WORK/bin /usr/bin; : > $WORK/bin/firerunner
BIN_DIR=$WORK/bin link_on_sudo_path
expect "firerunner linked into /usr/bin" test "$(readlink /usr/bin/firerunner)" = "$WORK/bin/firerunner"
rm -f /usr/bin/firerunner; echo "not ours" > /usr/bin/firerunner
BIN_DIR=$WORK/bin link_on_sudo_path
expect "an existing /usr/bin/firerunner is not replaced" grep -q "not ours" /usr/bin/firerunner
rm -f /usr/bin/firerunner

echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
