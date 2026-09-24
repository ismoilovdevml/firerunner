package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// stubBuilders records nft calls and answers builder probes without VMs. The
// returned function lists the calls so far, one per line.
func stubBuilders(t *testing.T, alive func(ip string) bool) func() string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	oldNft, oldTCP, oldBoot := nftRun, tcpOpen, builderBoot
	nftRun = func(args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	tcpOpen = func(ip string, _ int) bool { return alive(ip) }
	builderBoot = func(*Daemon, context.Context, config.Config, string) {}
	t.Cleanup(func() { nftRun, tcpOpen, builderBoot = oldNft, oldTCP, oldBoot })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(calls, "\n")
	}
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
		if got := d.Builder(bad, true).State; got != BuilderDisabled {
			t.Errorf("Builder(%q) = %s, want disabled", bad, got)
		}
	}
	// First request starts a builder on the first port and already hands out its
	// port and client credentials; later ones see it booting with the same ones.
	first := d.Builder("7", true)
	if first.State != BuilderBooting || first.Port != d.cfg.Builder.PortBase+1 || first.Key == "" || first.CA == "" {
		t.Fatalf("first request = %+v", first)
	}
	if b := d.builders["7"]; b == nil || b.Port != d.cfg.Builder.PortBase+1 || b.ready {
		t.Fatalf("builder entry = %+v", b)
	}
	if got := d.Builder("7", true); got.State != BuilderBooting || got.Key != first.Key {
		t.Fatalf("second request = %+v", got)
	}
	// Once ready the job gets the port and the client credentials.
	d.builders["7"] = readyBuilder("7", d.cfg.Builder.PortBase+1, 0, builderSpec(d.cfg))
	if got := d.Builder("7", true); got.State != BuilderReady || got.Port != d.cfg.Builder.PortBase+1 || got.Key != "key" {
		t.Fatalf("ready request = %+v", got)
	}

	// Slots full: a busy builder is never evicted, an idle one is.
	d.builders["8"] = readyBuilder("8", d.cfg.Builder.PortBase+2, time.Minute, builderSpec(d.cfg))
	if got := d.Builder("9", true).State; got != BuilderBusy {
		t.Fatalf("with all builders recently used: %s, want busy", got)
	}
	d.builders["8"].LastUsed = time.Now().Add(-time.Hour)
	if got := d.Builder("9", true).State; got != BuilderBooting {
		t.Fatalf("with an idle builder: %s, want booting", got)
	}
	if _, ok := d.builders["8"]; ok {
		t.Fatal("least recently used builder was not evicted")
	}
	if d.builders["9"].Port != d.cfg.Builder.PortBase+2 {
		t.Fatalf("freed port not reused: %d", d.builders["9"].Port)
	}

	d.mu.Lock() // the delete spawned by the eviction above reads d.cfg
	d.cfg.Builder.Enabled = false
	d.mu.Unlock()
	if got := d.Builder("7", true).State; got != BuilderDisabled {
		t.Fatalf("disabled: %s", got)
	}
}

func TestExpireBuilders(t *testing.T) {
	calls := stubBuilders(t, func(string) bool { return true })
	d, srv := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{
		"1": readyBuilder("1", 20001, time.Minute, spec),                            // in use
		"2": readyBuilder("2", 20002, d.cfg.Builder.IdleTTL+time.Hour, spec),        // idle past ttl
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
	d.bg.Wait()
	if !strings.Contains(calls(), "delete element inet firerunner builders { 20002 }") {
		t.Errorf("port mapping not removed: %v", calls())
	}
}

func TestBuilderSpecIgnoresSize(t *testing.T) {
	a := config.Default()
	b := a
	b.Builder.VCPU, b.Builder.MemoryMB = 16, 16384
	if builderSpec(a) != builderSpec(b) {
		t.Fatal("resizing builders must not replace existing ones (and their caches)")
	}
	c := a
	c.Builder.Image = "moby/buildkit:v9"
	if builderSpec(a) == builderSpec(c) {
		t.Fatal("a new BuildKit image must replace builders")
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
	present := map[string]bool{"uid-1": true, "uid-2": true}
	d.checkBuilders(present, time.Now())
	// A gone VM goes at once; one missed probe is only a strike.
	if len(d.builders) != 2 || d.builders["3"] != nil || d.builders["2"].strikes != 1 {
		t.Fatalf("after one check: %v", d.builders)
	}
	d.checkBuilders(present, time.Now())
	if len(d.builders) != 1 || d.builders["1"] == nil || d.builders["1"].strikes != 0 {
		t.Fatalf("after two checks: %v", d.builders)
	}
	if !strings.Contains(calls(), "add element inet firerunner builders { 20001 : 10.200.0.1 . 1234 }") {
		t.Errorf("live builder not remapped: %v", calls())
	}
}

// A builder a job builds on is never dropped for missed probes (only when its
// VM is gone), and one that became ready after the listing is not "gone".
func TestCheckBuildersKeepsBusyAndNewBuilders(t *testing.T) {
	stubBuilders(t, func(string) bool { return false })
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "job-9.json"), &vm.JobState{BuilderProject: "1"}); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	listedAt := time.Now()
	fresh := readyBuilder("2", 20002, 0, spec)
	fresh.BornAt = listedAt.Add(time.Second) // ready after the listing
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, 0, spec), "2": fresh}
	for i := 0; i < 3; i++ {
		d.checkBuilders(map[string]bool{"uid-1": true}, listedAt)
	}
	if d.builders["1"] == nil || d.builders["1"].strikes != 3 {
		t.Fatalf("busy builder removed or not counted: %+v", d.builders["1"])
	}
	if d.builders["2"] == nil {
		t.Fatal("builder that became ready after the listing was removed as gone")
	}
}

func TestAdoptBuildersProbesTwice(t *testing.T) {
	var probes atomic.Int32
	stubBuilders(t, func(string) bool { return probes.Add(1) > 1 }) // first probe misses
	d, _ := newTestDaemon(t)
	d.builders["1"] = readyBuilder("1", 20001, 0, builderSpec(d.cfg))
	d.mu.Lock()
	d.saveBuildersLocked()
	d.mu.Unlock()
	d.builders = map[string]*builder{}
	d.adoptBuilders(map[string]bool{"uid-1": true})
	if d.builders["1"] == nil {
		t.Fatal("builder dropped at startup after one missed probe")
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

func TestAdoptMigratesLegacySpec(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.builders["1"] = readyBuilder("1", 20001, 0, legacyBuilderSpec(d.cfg))
	d.mu.Lock()
	d.saveBuildersLocked()
	d.mu.Unlock()
	d.builders = map[string]*builder{}
	d.adoptBuilders(map[string]bool{"uid-1": true})
	if d.builders["1"] == nil || d.builders["1"].SpecID != builderSpec(d.cfg) {
		t.Fatalf("legacy spec not migrated: %+v", d.builders["1"])
	}
	d.expireBuilders()
	if d.builders["1"] == nil {
		t.Fatal("upgraded daemon replaced a builder recorded by the old one")
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

// A builder a running job uses is never expired or evicted, however long the build takes.
func TestBusyBuilderIsKept(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "75226.json"), &vm.JobState{BuilderProject: "2"}); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.cfg.Builder.Max = 2
	d.builders = map[string]*builder{
		"1": readyBuilder("1", 20001, 30*time.Minute, spec),
		"2": readyBuilder("2", 20002, d.cfg.Builder.IdleTTL+time.Hour, spec), // long build
	}
	d.expireBuilders()
	if b := d.builders["2"]; b == nil || time.Since(b.LastUsed) > time.Minute {
		t.Fatalf("busy builder expired or not refreshed: %+v", b)
	}
	d.builders["2"].LastUsed = time.Now().Add(-time.Hour)
	if got := d.Builder("3", true).State; got != BuilderBooting {
		t.Fatalf("new project with an idle builder to evict: %s", got)
	}
	if d.builders["2"] == nil || d.builders["1"] != nil {
		t.Fatalf("evicted the busy builder instead of the idle one: %v", d.builders)
	}
}

// After a failed boot the project gets "busy" for a while instead of a new
// boot on every poll of its waiting jobs.
func TestFailedBuilderBootCoolsDown(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var boots atomic.Int32
	done := make(chan struct{}, 4)
	builderBoot = func(d *Daemon, _ context.Context, _ config.Config, project string) {
		boots.Add(1)
		d.mu.Lock()
		delete(d.builders, project)
		d.builderFailed[project] = time.Now()
		d.mu.Unlock()
		done <- struct{}{}
	}
	d, _ := newTestDaemon(t)
	d.Builder("7", true)
	<-done
	for i := 0; i < 4; i++ {
		if got := d.Builder("7", true).State; got != BuilderBusy {
			t.Fatalf("during cooldown: %s, want busy", got)
		}
	}
	if n := boots.Load(); n != 1 {
		t.Fatalf("%d boots for one failing project, want 1", n)
	}
	d.mu.Lock()
	d.builderFailed["7"] = time.Now().Add(-builderRetryAfter - time.Second)
	d.mu.Unlock()
	d.Builder("7", true)
	<-done
	if n := boots.Load(); n != 2 {
		t.Fatalf("no retry after the cooldown (boots=%d)", n)
	}
}

// Without start, a project with no builder gets "none" and nothing boots:
// projects that never build do not occupy a builder slot.
func TestBuilderWithoutStartNeverBoots(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var boots atomic.Int32
	builderBoot = func(*Daemon, context.Context, config.Config, string) { boots.Add(1) }
	d, _ := newTestDaemon(t)
	if got := d.Builder("7", false).State; got != BuilderNone {
		t.Fatalf("no builder, start=false: %s, want none", got)
	}
	if boots.Load() != 0 || len(d.builders) != 0 {
		t.Fatalf("start=false booted a builder (boots=%d, builders=%v)", boots.Load(), d.builders)
	}
	d.builders["7"] = readyBuilder("7", 20001, time.Hour, builderSpec(d.cfg))
	if got := d.Builder("7", false); got.State != BuilderReady || got.Key == "" {
		t.Fatalf("existing builder, start=false: %+v", got)
	}
	if time.Since(d.builders["7"].LastUsed) > time.Minute {
		t.Fatal("using an existing builder must refresh LastUsed")
	}
}

// blockNft makes every nft call wait until the test releases it, and reports
// each call on entered.
func blockNft(t *testing.T) (entered chan string, release func()) {
	t.Helper()
	entered, gate := make(chan string, 64), make(chan struct{})
	old := nftRun
	nftRun = func(args ...string) error {
		entered <- strings.Join(args, " ")
		<-gate
		return nil
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); nftRun = old })
	return entered, release
}

// claimWithin fails the test if POST /claim's Claim waits for the daemon lock.
func claimWithin(t *testing.T, d *Daemon, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { d.Claim(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Claim blocked while nft ran for %s: nft runs under the daemon lock", what)
	}
}

func TestNftNeverBlocksClaims(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	spec := builderSpec(d.cfg)

	t.Run("remapping live builders", func(t *testing.T) {
		entered, release := blockNft(t)
		d.builders = map[string]*builder{"1": readyBuilder("1", 20001, 0, spec)}
		done := make(chan struct{})
		go func() { d.checkBuilders(map[string]bool{"uid-1": true}, time.Now()); close(done) }()
		<-entered
		claimWithin(t, d, "a port mapping")
		release()
		<-done
	})
	t.Run("removing an expired builder", func(t *testing.T) {
		entered, release := blockNft(t)
		d.builders = map[string]*builder{"2": readyBuilder("2", 20002, d.cfg.Builder.IdleTTL+time.Hour, spec)}
		done := make(chan struct{})
		go func() { d.expireBuilders(); close(done) }()
		<-entered
		claimWithin(t, d, "a builder removal")
		release()
		<-done
		d.bg.Wait()
	})
}

func TestBuilderPersistsLastUsedAtMostOncePerMinute(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.builders["1"] = readyBuilder("1", d.cfg.Builder.PortBase+1, 0, builderSpec(d.cfg))
	_ = os.Remove(d.buildersFile())
	if d.Builder("1", false).State != BuilderReady {
		t.Fatal("builder not ready")
	}
	if _, err := os.Stat(d.buildersFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("builders.json written for a builder used just now: %v", err)
	}
	d.builders["1"].LastUsed = time.Now().Add(-2 * time.Minute)
	d.Builder("1", false)
	if _, err := os.Stat(d.buildersFile()); err != nil {
		t.Fatalf("LastUsed older than a minute not persisted: %v", err)
	}
}

// serveAPI serves the daemon's socket API and returns a client for it.
func serveAPI(t *testing.T, d *Daemon) *Client {
	t.Helper()
	l, err := d.listenSocket()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(d.cfg.Daemon.Socket)
}

// An operator removal never kills a running build unless forced (#37).
func TestRemoveBuildersKeepsBusyUnlessForced(t *testing.T) {
	calls := stubBuilders(t, func(string) bool { return true })
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "job-9.json"), &vm.JobState{BuilderProject: "101"}); err != nil {
		t.Fatal(err)
	}
	d, srv := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{
		"101": readyBuilder("101", 20001, 0, spec),               // a job builds on it
		"8":   readyBuilder("8", 20002, time.Hour, spec),         // idle
		"9":   {Project: "9", Port: 20003, LastUsed: time.Now()}, // still booting
	}
	dc := serveAPI(t, d)

	res, err := dc.RemoveBuilder("all", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "8" || strings.Join(res.Skipped, ",") != "101" {
		t.Fatalf("rm --all = %+v, want removed [8] skipped [101]", res)
	}
	if d.builders["101"] == nil || d.builders["9"] == nil || d.builders["8"] != nil {
		t.Fatalf("builders after rm --all: %v", d.builders)
	}
	srv.WaitDeleted(t, "uid-8", 2*time.Second)

	if res, err = dc.RemoveBuilder("777", false); err != nil || len(res.Removed)+len(res.Skipped) != 0 {
		t.Fatalf("rm of a project without builder = %+v, %v", res, err)
	}

	res, err = dc.RemoveBuilder("101", true)
	if err != nil || strings.Join(res.Removed, ",") != "101" || len(res.Skipped) != 0 {
		t.Fatalf("rm 101 --force = %+v, %v", res, err)
	}
	srv.WaitDeleted(t, "uid-101", 2*time.Second)
	d.bg.Wait()
	if !strings.Contains(calls(), "delete element inet firerunner builders { 20001 }") {
		t.Errorf("forced removal left the port mapping: %v", calls())
	}
}

// Stopping the daemon mid-preload or mid-delete is expected: INFO, not ERROR,
// and not a boot failure (#37).
func TestShutdownMessagesAreInfo(t *testing.T) {
	d, srv := newTestDaemon(t)
	var logs bytes.Buffer
	d.log = slog.New(slog.NewTextHandler(&logs, nil))
	down := status.Error(codes.Unavailable, "grpc: the client connection is closing")
	stopped, stop := context.WithCancel(context.Background())
	stop()

	d.preloadFailed(context.Background(), "pool-a", errors.New("pull failed"))
	d.preloadFailed(stopped, "pool-b", context.Canceled)
	if got := metricValue(t, d, "firerunner_vm_boot_failures_total", "preload"); got != 1 {
		t.Fatalf("preload failures = %v, want 1 (the shutdown one is not a failure)", got)
	}

	srv.FailDelete(down)
	d.delete(context.Background(), &vm.Instance{ID: "pool-c", UID: "c"}, "expired")
	d.mu.Lock()
	d.runCtx = stopped
	d.mu.Unlock()
	srv.FailDelete(down)
	d.delete(context.Background(), &vm.Instance{ID: "pool-d", UID: "d"}, "shutdown")

	for _, want := range []string{
		`level=ERROR msg="image preload failed, VM kept without it" id=pool-a`,
		`level=INFO msg="image preload stopped: daemon shutting down" id=pool-b`,
		`level=ERROR msg="delete failed" id=pool-c`,
		`level=INFO msg="delete not finished before shutdown" id=pool-d`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}
