package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// stubBuilders records nft calls and answers builder probes without VMs.
func stubBuilders(t *testing.T, alive func(ip string) bool) *[]string {
	t.Helper()
	var mu sync.Mutex
	calls := &[]string{}
	oldNft, oldTCP, oldBoot := nftRun, tcpOpen, builderBoot
	nftRun = func(args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, strings.Join(args, " "))
		return nil
	}
	tcpOpen = func(ip string, _ int) bool { return alive(ip) }
	builderBoot = func(*Daemon, context.Context, config.Config, string) {}
	t.Cleanup(func() { nftRun, tcpOpen, builderBoot = oldNft, oldTCP, oldBoot })
	return calls
}

func readyBuilder(project string, port int, lastUsed time.Duration, spec string) *builder {
	return &builder{Project: project, Port: port, ready: true, SpecID: spec, BornAt: time.Now().Add(-time.Hour),
		LastUsed: time.Now().Add(-lastUsed), CA: "ca", Cert: "cert", Key: "key",
		Instance: vm.Instance{ID: "bld-" + project, UID: "uid-" + project, IP: "10.200.0." + project}}
}

func TestBuilderRequest(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.cfg.Builder.Max = 2

	for _, bad := range []string{"", "abc", "1;rm -rf /", "../1"} {
		if got := d.Builder(bad).State; got != BuilderDisabled {
			t.Errorf("Builder(%q) = %s, want disabled", bad, got)
		}
	}
	// First request starts a builder on the first port; later ones see it booting.
	if got := d.Builder("7"); got.State != BuilderBooting {
		t.Fatalf("first request = %+v", got)
	}
	if b := d.builders["7"]; b == nil || b.Port != d.cfg.Builder.PortBase+1 || b.ready {
		t.Fatalf("builder entry = %+v", b)
	}
	if got := d.Builder("7"); got.State != BuilderBooting {
		t.Fatalf("second request = %+v", got)
	}
	// Once ready the job gets the port and the client credentials.
	d.builders["7"] = readyBuilder("7", d.cfg.Builder.PortBase+1, 0, builderSpec(d.cfg))
	if got := d.Builder("7"); got.State != BuilderReady || got.Port != d.cfg.Builder.PortBase+1 || got.Key != "key" {
		t.Fatalf("ready request = %+v", got)
	}

	// Slots full: a busy builder is never evicted, an idle one is.
	d.builders["8"] = readyBuilder("8", d.cfg.Builder.PortBase+2, time.Minute, builderSpec(d.cfg))
	if got := d.Builder("9").State; got != BuilderBusy {
		t.Fatalf("with all builders recently used: %s, want busy", got)
	}
	d.builders["8"].LastUsed = time.Now().Add(-time.Hour)
	if got := d.Builder("9").State; got != BuilderBooting {
		t.Fatalf("with an idle builder: %s, want booting", got)
	}
	if _, ok := d.builders["8"]; ok {
		t.Fatal("least recently used builder was not evicted")
	}
	if d.builders["9"].Port != d.cfg.Builder.PortBase+2 {
		t.Fatalf("freed port not reused: %d", d.builders["9"].Port)
	}

	d.cfg.Builder.Enabled = false
	if got := d.Builder("7").State; got != BuilderDisabled {
		t.Fatalf("disabled: %s", got)
	}
}

func TestExpireBuilders(t *testing.T) {
	calls := stubBuilders(t, func(string) bool { return true })
	d, srv := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{
		"1": readyBuilder("1", 20001, time.Minute, spec),                            // in use
		"2": readyBuilder("2", 20002, 13*time.Hour, spec),                           // idle past ttl
		"3": readyBuilder("3", 20003, 20*time.Minute, "old"),                        // old config, idle
		"4": readyBuilder("4", 20004, time.Minute, "old"),                           // old config, busy
		"5": {Project: "5", Port: 20005, LastUsed: time.Now().Add(-24 * time.Hour)}, // still booting
	}
	d.expireBuilders()
	for _, p := range []string{"1", "4", "5"} {
		if _, ok := d.builders[p]; !ok {
			t.Errorf("builder %s removed", p)
		}
	}
	for _, p := range []string{"2", "3"} {
		if _, ok := d.builders[p]; ok {
			t.Errorf("builder %s kept", p)
		}
		srv.WaitDeleted(t, "uid-"+p, 2*time.Second)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "delete element inet firerunner builders { 20002 }") {
		t.Errorf("port mapping not removed: %v", *calls)
	}
}

func TestCheckBuildersDropsDeadAndRemapsLive(t *testing.T) {
	calls := stubBuilders(t, func(ip string) bool { return ip != "10.200.0.2" })
	d, _ := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{
		"1": readyBuilder("1", 20001, 0, spec),
		"2": readyBuilder("2", 20002, 0, spec), // buildkitd not answering
		"3": readyBuilder("3", 20003, 0, spec), // VM gone
	}
	d.checkBuilders(map[string]bool{"uid-1": true, "uid-2": true})
	if len(d.builders) != 1 || d.builders["1"] == nil {
		t.Fatalf("builders after check: %v", d.builders)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "add element inet firerunner builders { 20001 : 10.200.0.1 . 1234 }") {
		t.Errorf("live builder not remapped: %v", *calls)
	}
}

func TestBuildersSurviveRestart(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.builders["1"] = readyBuilder("1", 20001, 0, builderSpec(d.cfg))
	d.builders["2"] = readyBuilder("2", 20002, 0, builderSpec(d.cfg))
	d.builders["3"] = &builder{Project: "3", Port: 20003} // booting: not recorded
	d.mu.Lock()
	d.saveBuildersLocked()
	d.mu.Unlock()

	d2, _ := newTestDaemon(t)
	d2.cfg.Daemon.Socket = d.cfg.Daemon.Socket
	d2.adoptBuilders(map[string]bool{"uid-1": true}) // builder 2's VM is gone
	if len(d2.builders) != 1 || d2.builders["1"] == nil || !d2.builders["1"].ready || d2.builders["1"].Key != "key" {
		t.Fatalf("adopted: %v", d2.builders)
	}
}

// Each builder has its own CA: its client certificate works against its own
// server and is rejected by another builder's server.
func TestBuilderCredsIsolateProjects(t *testing.T) {
	a, err := newBuilderCreds()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBuilderCreds()
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(a, a); err != nil {
		t.Fatalf("own builder rejected its client: %v", err)
	}
	if err := handshake(a, b); err == nil {
		t.Fatal("builder accepted a client certificate from another project's CA")
	}
}

// handshake runs a TLS server with server's credentials and connects with client's.
func handshake(server, client *builderCreds) error {
	serverCert, err := tls.X509KeyPair([]byte(server.serverCert), []byte(server.serverKey))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(server.caPEM))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	defer ln.Close()
	errc := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer c.Close()
		errc <- c.(*tls.Conn).Handshake()
	}()
	clientCert, err := tls.X509KeyPair([]byte(client.clientCert), []byte(client.clientKey))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(client.caPEM))
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(),
		&tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: roots, ServerName: BuilderServerName, MinVersion: tls.VersionTLS12})
	if err == nil {
		err = conn.Handshake()
		conn.Close()
	}
	if serr := <-errc; err == nil {
		err = serr
	}
	return err
}
