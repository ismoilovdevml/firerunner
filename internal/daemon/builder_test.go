package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// fakeNft emulates nft for the builders map: it records every call (the
// arguments, then the script, if any) and keeps the map's elements, port ->
// "ip . port". A script is applied all or nothing, like nft -f.
type fakeNft struct {
	mu      sync.Mutex
	calls   []string
	elems   map[int]string
	listErr error
}

func (f *fakeNft) run(script string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.Join(args, " ")
	if script != "" {
		call += "\n" + strings.TrimSpace(script)
	}
	f.calls = append(f.calls, call)
	if f.elems == nil {
		f.elems = map[int]string{}
	}
	switch strings.Join(args, " ") {
	case "list map inet firerunner builders":
		if f.listErr != nil {
			return "", f.listErr
		}
		var e []string
		for port, to := range f.elems {
			e = append(e, fmt.Sprintf("%d : %s", port, to))
		}
		out := "table inet firerunner {\n\tmap builders {\n\t\ttype inet_service : ipv4_addr . inet_service\n"
		if len(e) > 0 {
			sort.Strings(e)
			out += "\t\telements = { " + strings.Join(e, ",\n\t\t\t     ") + " }\n"
		}
		return out + "\t}\n}\n", nil
	case "-f -":
		next := maps.Clone(f.elems)
		for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
			if err := applyNft(next, line); err != nil {
				return "", err
			}
		}
		f.elems = next
		return "", nil
	}
	return "", applyNft(f.elems, strings.Join(args, " "))
}

// applyNft applies one add or delete of a builders map element like nft does:
// deleting a missing element or re-adding a port with another target fails.
func applyNft(elems map[int]string, cmd string) error {
	var port int
	var ip string
	if n, _ := fmt.Sscanf(cmd, "add element inet firerunner builders { %d : %s . 1234 }", &port, &ip); n == 2 {
		if have, ok := elems[port]; ok && have != ip+" . 1234" {
			return fmt.Errorf("nft: %s: Device or resource busy", cmd)
		}
		elems[port] = ip + " . 1234"
		return nil
	}
	if n, _ := fmt.Sscanf(cmd, "delete element inet firerunner builders { %d }", &port); n == 1 {
		if _, ok := elems[port]; !ok {
			return fmt.Errorf("nft: %s: No such file or directory", cmd)
		}
		delete(elems, port)
		return nil
	}
	return fmt.Errorf("fake nft does not know %q", cmd)
}

func (f *fakeNft) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

// writes are the calls that changed or tried to change the map.
func (f *fakeNft) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "list ") {
			out = append(out, c)
		}
	}
	return out
}

// stubBuilders emulates nft (see fakeNft) and answers builder probes without
// VMs. The returned function lists the nft calls so far, one per line.
func stubBuilders(t *testing.T, alive func(ip string) bool) func() string {
	t.Helper()
	nft := stubNft(t)
	oldTCP, oldBoot := tcpOpen, builderBoot
	tcpOpen = func(ip string, _ int) bool { return alive(ip) }
	builderBoot = func(*Daemon, context.Context, config.Config, string) {}
	t.Cleanup(func() { tcpOpen, builderBoot = oldTCP, oldBoot })
	// No builder VMs to copy caches from: saves fail fast unless a test stubs them.
	stubBuilderCache(t, func(context.Context, config.Config, *vm.Instance) (int64, error) {
		return 0, errors.New("stub: no VM")
	}, nil, nil)
	return nft.String
}

// stubNft replaces nft with a fakeNft for one test.
func stubNft(t *testing.T) *fakeNft {
	t.Helper()
	f := &fakeNft{}
	old := nftRun
	nftRun = f.run
	t.Cleanup(func() { nftRun = old })
	return f
}

// recordBuilders writes builders.json the way a daemon that has adopted its
// previous run's builders does.
func recordBuilders(d *Daemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buildersAdopted = true
	d.saveBuildersLocked()
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
	recordBuilders(d)
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
	recordBuilders(d)

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
	recordBuilders(d)
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
	nftRun = func(_ string, args ...string) (string, error) {
		entered <- strings.Join(args, " ")
		<-gate
		return "", nil
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); nftRun = old })
	return entered, release
}

// claimWithin fails the test if POST /claim's Claim waits for the daemon lock
// while what (slow work) runs.
func claimWithin(t *testing.T, d *Daemon, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { d.Claim(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Claim blocked during %s: it runs under the daemon lock", what)
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
	t.Run("a builder becoming ready", func(t *testing.T) {
		stubBoot(t, nil)
		entered, release := blockNft(t)
		d.builders = map[string]*builder{}
		d.Builder("3", true)
		done := make(chan struct{})
		go func() { d.bootBuilder(context.Background(), d.cfg, "3"); close(done) }()
		<-entered
		claimWithin(t, d, "a new builder's port mapping")
		release()
		<-done
		d.mu.Lock()
		b := d.builders["3"]
		d.mu.Unlock()
		if b == nil || !b.ready {
			t.Fatalf("builder not ready: %+v", b)
		}
	})
}

// A new builder's port is mapped before jobs are told it is ready, and not
// under the daemon lock.
func TestBuilderPortMappedBeforeReady(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	stubBoot(t, nil)
	d, _ := newTestDaemon(t)
	mapped, locked, readyAtMap := false, false, false
	old := nftRun
	nftRun = func(script string, args ...string) (string, error) {
		if script != "" && !mapped {
			mapped = true
			if !d.mu.TryLock() { // nothing else runs: held by bootBuilder
				locked = true
			} else {
				readyAtMap = d.builders["3"] != nil && d.builders["3"].ready
				d.mu.Unlock()
			}
		}
		return old(script, args...)
	}
	d.Builder("3", true)
	d.bootBuilder(context.Background(), d.cfg, "3")
	if !mapped || locked || readyAtMap {
		t.Fatalf("port mapped %v, under the lock %v, after the builder was ready %v", mapped, locked, readyAtMap)
	}
}

func TestBuilderPersistsLastUsedAtMostOncePerMinute(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.buildersAdopted = true
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
		`level=ERROR msg="image preload failed, VM kept without it" vm=pool-a`,
		`level=INFO msg="image preload stopped: daemon shutting down" vm=pool-b`,
		`level=ERROR msg="delete failed" vm=pool-c`,
		`level=INFO msg="delete not finished before shutdown" vm=pool-d`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// Measured on the trial host: a job asked for a builder seconds after the
// daemon restarted, before the old builders were adopted. The new builder
// took an adopted builder's port, and two projects shared one DNAT port.
func TestAdoptAfterNewBuildersStarted(t *testing.T) {
	calls := stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	base := d.cfg.Builder.PortBase
	d.builders["1"] = readyBuilder("1", base+1, 0, builderSpec(d.cfg))
	d.builders["2"] = readyBuilder("2", base+2, 0, builderSpec(d.cfg))
	recordBuilders(d)

	d2, _ := newTestDaemon(t)
	d2.cfg.Daemon.Socket = d.cfg.Daemon.Socket
	d2.Builder("9", true) // booting, took the first port
	d2.Builder("2", true) // project 2 got a new builder before adoption
	if d2.builders["9"].Port != base+1 {
		t.Fatalf("new builder port %d", d2.builders["9"].Port)
	}
	newTwo := d2.builders["2"]
	d2.adoptBuilders(map[string]bool{"uid-1": true, "uid-2": true})

	if d2.builders["2"] != newTwo {
		t.Fatal("adoption replaced the project's new builder")
	}
	one := d2.builders["1"]
	if one == nil || !one.ready || one.Port == base+1 {
		t.Fatalf("adopted builder 1 = %+v, want it on a free port", one)
	}
	ports := map[int]string{}
	for p, b := range d2.builders {
		if other, dup := ports[b.Port]; dup {
			t.Fatalf("projects %s and %s share port %d", other, p, b.Port)
		}
		ports[b.Port] = p
	}
	want := fmt.Sprintf("add element inet firerunner builders { %d : 10.200.0.1 . 1234 }", one.Port)
	if !strings.Contains(calls(), want) {
		t.Fatalf("adopted builder not mapped on its new port: %v", calls())
	}
}

// Builders pull through the proxy, trust vm.ca_file and reach insecure
// registries the way the setting says (TLS without a check, or plain HTTP).
func TestBuildkitScriptCorporateNetwork(t *testing.T) {
	creds := &builderCreds{caPEM: "CA", serverCert: "CERT", serverKey: "KEY"}
	plain := buildkitScript(config.Default(), creds)
	if strings.Contains(plain, "HTTPS_PROXY") || strings.Contains(plain, "/etc/ssl/certs") || strings.Contains(plain, "insecure") {
		t.Fatalf("defaults already set corporate options:\n%s", plain)
	}

	cfg := config.Default()
	cfg.VM.RegistryMirror = "http://10.200.0.1:5000"
	cfg.Proxy.Enabled = true
	cfg.VM.CAFile = "/etc/firerunner/ca.pem"
	cfg.VM.InsecureRegistries = []string{"harbor.corp:443", "http://10.0.0.5:5000", "10.200.0.1:5000"}
	s := buildkitScript(cfg, creds)
	for _, want := range []string{
		"-e 'HTTPS_PROXY=http://10.200.0.1:3128'",
		"-e 'NO_PROXY=localhost,127.0.0.1,::1,10.200.0.1,",
		"-v /etc/ssl/certs:/etc/ssl/certs:ro",
		"[registry.\"docker.io\"]\n  mirrors = [\"10.200.0.1:5000\"]\n",
		"[registry.\"10.200.0.1:5000\"]\n  http = true\n",
		"[registry.\"harbor.corp:443\"]\n  insecure = true\n",
		"[registry.\"10.0.0.5:5000\"]\n  http = true\n",
		"exit 3",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The mirror listed again as insecure must not produce a second TOML table.
	if n := strings.Count(s, `[registry."10.200.0.1:5000"]`); n != 1 {
		t.Fatalf("mirror table %d times: buildkitd refuses duplicate TOML tables", n)
	}
}

// An https:// mirror keeps its certificate checked: only a plain-HTTP mirror
// and vm.insecure_registries get tables.
func TestBuildkitdTOMLHTTPSMirror(t *testing.T) {
	cfg := config.Default()
	cfg.VM.RegistryMirror = "https://mirror.corp:5000"
	if s := buildkitdTOML(cfg); strings.Contains(s, "insecure") || strings.Contains(s, "http = true") || !strings.Contains(s, `mirrors = ["mirror.corp:5000"]`) {
		t.Fatalf("https mirror:\n%s", s)
	}
	cfg.VM.InsecureRegistries = []string{"mirror.corp:5000"}
	if s := buildkitdTOML(cfg); !strings.Contains(s, "[registry.\"mirror.corp:5000\"]\n  insecure = true") {
		t.Fatalf("https mirror listed as insecure:\n%s", s)
	}
}

// A CA rotated in place changes the fingerprint, so pool VMs and builders
// with the old CA are replaced.
func TestFingerprintFollowsCAContent(t *testing.T) {
	cfg := config.Default()
	cfg.VM.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cfg.VM.CAFile, []byte("old CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, bspec := fingerprint(cfg), builderSpec(cfg)
	if fingerprint(cfg) != before {
		t.Fatal("fingerprint not stable")
	}
	if err := os.WriteFile(cfg.VM.CAFile, []byte("new CA, other size"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fingerprint(cfg) == before || builderSpec(cfg) == bspec {
		t.Fatal("a new CA at the same path does not replace VMs or builders")
	}
	// No CA, no proxy, no insecure registries: the v0.1.0 fingerprint.
	plain := config.Default()
	if strings.Contains(fingerprint(plain), "|") || strings.Contains(builderSpec(plain), "|ca:") {
		t.Fatalf("defaults changed the fingerprint: %s / %s", fingerprint(plain), builderSpec(plain))
	}
}

// Every builder boot gets its own VM id, so its MAC, lease and pinned key are
// its own: the project's previous builder may still exist (saving its cache, a
// failed delete, not adopted after a restart), and a MAC derived from the
// public project id alone could be taken by any guest.
func TestBuilderVMIDIsUniquePerBoot(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	stubBuilderCache(t, nil, nil, nil)
	boots := stubBoot(t, nil)
	var unreserved []string // booted VMs without a host memory reservation under their id
	stubbed := builderVMBoot
	builderVMBoot = func(ctx context.Context, cfg config.Config, fl *flintlock.Client, id string, labels map[string]string) (*vm.Instance, error) {
		if data, _ := os.ReadFile(vm.AdmissionFile); !strings.Contains(string(data), `"id":"`+id+`"`) {
			unreserved = append(unreserved, id)
		}
		return stubbed(ctx, cfg, fl, id, labels)
	}
	d, _ := newTestDaemon(t)
	var ids []string
	for i := 0; i < 2; i++ {
		d.Builder("7", true)
		d.bootBuilder(context.Background(), d.cfg, "7")
		d.mu.Lock()
		b := d.builders["7"]
		delete(d.builders, "7") // gone, as after its removal
		d.mu.Unlock()
		if b == nil || !b.ready || b.Instance.ID != boots.last() {
			t.Fatalf("boot %d: builder %+v, booted %q", i, b, boots.last())
		}
		ids = append(ids, b.Instance.ID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two boots of project 7 used the same VM id %q", ids[0])
	}
	if len(unreserved) > 0 {
		t.Fatalf("booted %v without host memory reserved under that id", unreserved)
	}
	for _, id := range ids {
		suffix, ok := strings.CutPrefix(id, "bld-7-")
		if !ok || len(suffix) != 8 || strings.Trim(suffix, "0123456789abcdef") != "" {
			t.Fatalf("id %q, want bld-7-<8 hex digits>", id)
		}
		if role, _ := RoleOf(id); role != "builder" {
			t.Fatalf("RoleOf(%q) = %q: reconcile would not know it as a builder", id, role)
		}
		if vm.MAC(id) == vm.MAC("bld-7") {
			t.Fatalf("MAC of %q is the one derived from the project id", id)
		}
	}
}

// builders.json written before per-boot ids (bld-<project>) and after
// (bld-<project>-<hex>) is adopted; the project comes from the record.
func TestAdoptBuildersOfBothIDForms(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	old := readyBuilder("7", 20001, 0, builderSpec(d.cfg)) // id bld-7
	cur := readyBuilder("8", 20002, 0, builderSpec(d.cfg))
	cur.Instance.ID = "bld-8-0a1b2c3d"
	d.builders = map[string]*builder{"7": old, "8": cur}
	recordBuilders(d)

	d2, _ := newTestDaemon(t)
	d2.cfg.Daemon.Socket = d.cfg.Daemon.Socket
	d2.adoptBuilders(map[string]bool{"uid-7": true, "uid-8": true})
	if b := d2.builders["7"]; b == nil || b.Instance.ID != "bld-7" || !b.ready {
		t.Fatalf("old-form builder not adopted: %+v", b)
	}
	if b := d2.builders["8"]; b == nil || b.Instance.ID != "bld-8-0a1b2c3d" || !b.ready {
		t.Fatalf("per-boot id builder not adopted: %+v", b)
	}
}

// Every reconcile checks the port mappings. One already in place is left
// alone, a missing one is added, and one pointing elsewhere is replaced in a
// single nft transaction: between a separate delete and add, connections to
// the port would be dropped.
func TestBuilderPortsRemappedOnlyWhenNeeded(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	nft := stubNft(t)
	d, _ := newTestDaemon(t)
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, 0, spec), "2": readyBuilder("2", 20002, 0, spec)}
	present := map[string]bool{"uid-1": true, "uid-2": true}
	nft.elems = map[int]string{20002: "10.200.0.2 . 1234"}

	d.checkBuilders(present, time.Now()) // 20001 missing
	want := []string{"-f -\nadd element inet firerunner builders { 20001 : 10.200.0.1 . 1234 }"}
	if got := nft.writes(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("missing mapping: nft writes %q, want %q", got, want)
	}

	nft.calls = nil
	d.checkBuilders(present, time.Now()) // all in place
	if got := nft.writes(); len(got) != 0 {
		t.Fatalf("mappings in place, but nft ran %q", got)
	}

	nft.calls = nil
	nft.elems[20001] = "10.200.0.99 . 1234" // firerunner-net reloaded with an old address
	d.checkBuilders(present, time.Now())
	want = []string{"-f -\ndelete element inet firerunner builders { 20001 }\nadd element inet firerunner builders { 20001 : 10.200.0.1 . 1234 }"}
	if got := nft.writes(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrong mapping: nft writes %q, want one transaction %q", got, want)
	}
	if nft.elems[20001] != "10.200.0.1 . 1234" || nft.elems[20002] != "10.200.0.2 . 1234" {
		t.Fatalf("map after remap: %v", nft.elems)
	}

	// nft cannot list the map (firerunner-net stopped): nothing is written blind.
	nft.calls = nil
	nft.listErr = errors.New("nft: No such file or directory")
	delete(nft.elems, 20001)
	d.checkBuilders(present, time.Now())
	if got := nft.writes(); len(got) != 0 {
		t.Fatalf("listing failed, but nft ran %q", got)
	}
}

// parseBuilderMap reads nft's listing of the builders map, one or many lines.
func TestParseBuilderMap(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      map[int]string
	}{
		{"empty map", "table inet firerunner {\n\tmap builders {\n\t\ttype inet_service : ipv4_addr . inet_service\n\t}\n}\n", map[int]string{}},
		{"one element", "table inet firerunner {\n\tmap builders {\n\t\ttype inet_service : ipv4_addr . inet_service\n\t\telements = { 20001 : 10.200.0.13 . 1234 }\n\t}\n}\n",
			map[int]string{20001: "10.200.0.13 . 1234"}},
		{"wrapped elements", "table inet firerunner {\n\tmap builders {\n\t\ttype inet_service : ipv4_addr . inet_service\n\t\telements = { 20001 : 10.200.0.13 . 1234,\n\t\t\t     20002 : 10.200.0.14 . 1234 }\n\t}\n}\n",
			map[int]string{20001: "10.200.0.13 . 1234", 20002: "10.200.0.14 . 1234"}},
		{"unexpected text", "garbage", map[int]string{}},
	} {
		got := parseBuilderMap(tc.out)
		if !maps.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A builder whose boot ends with the daemon stopping is deleted, and the port
// mapping made for it does not stay behind.
func TestBuilderBootCutByShutdownUnmapsPort(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	nft := stubNft(t)
	boots := stubBoot(t, nil)
	ctx, stop := context.WithCancel(context.Background())
	builderSetup = func(context.Context, config.Config, *vm.Instance, *builderCreds) error { stop(); return nil }
	d, srv := newTestDaemon(t)
	d.Builder("3", true)
	port := d.builders["3"].Port
	d.bootBuilder(ctx, d.cfg, "3")
	srv.WaitDeleted(t, "uid-"+boots.last(), 2*time.Second)
	d.bg.Wait()
	if !strings.Contains(nft.String(), fmt.Sprintf("{ %d : 10.200.0.77 . 1234 }", port)) {
		t.Fatalf("port never mapped: %s", nft.String())
	}
	if to, ok := nft.elems[port]; ok {
		t.Fatalf("port %d still maps to %s after the builder was dropped", port, to)
	}
	if d.builders["3"].ready {
		t.Fatal("builder marked ready while shutting down")
	}
}

// When the startup listing fails, builders.json is not read then. Until it is,
// the file keeps the previous run's builders (a new builder does not overwrite
// it), and the first listing that works adopts them: otherwise reconcile would
// delete them as orphans, without saving their caches.
func TestBuildersFileKeptUntilAdopted(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	stubBoot(t, nil)
	d, _ := newTestDaemon(t)
	d.builders["1"] = readyBuilder("1", 20001, 0, builderSpec(d.cfg))
	recordBuilders(d)

	d2, _ := newTestDaemon(t) // its startup listing failed: no adoption yet
	d2.cfg.Daemon.Socket = d.cfg.Daemon.Socket
	d2.Builder("9", true)
	d2.bootBuilder(context.Background(), d2.cfg, "9") // ready: would write builders.json
	projects := func() []string {
		var list []*builder
		data, _ := os.ReadFile(d2.buildersFile())
		_ = json.Unmarshal(data, &list)
		var out []string
		for _, b := range list {
			out = append(out, b.Project)
		}
		sort.Strings(out)
		return out
	}
	if got := projects(); strings.Join(got, ",") != "1" {
		t.Fatalf("builders.json before adoption = %v, want the previous run's [1]", got)
	}
	d2.checkBuilders(map[string]bool{"uid-1": true, "uid-" + d2.builders["9"].Instance.ID: true}, time.Now())
	if b := d2.builders["1"]; b == nil || !b.ready {
		t.Fatalf("previous run's builder not adopted at the first listing: %+v", b)
	}
	if got := projects(); strings.Join(got, ",") != "1,9" {
		t.Fatalf("builders.json after adoption = %v, want [1 9]", got)
	}
}

// The same through Run: flintlockd does not answer the startup listing but
// answers reconcile's; the previous run's builder is adopted, not deleted.
func TestRunAdoptsBuildersAfterFailedStartupListing(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	oldFor, oldRetry := startupListFor, startupRetry
	startupListFor, startupRetry = 0, 10*time.Millisecond
	t.Cleanup(func() { startupListFor, startupRetry = oldFor, oldRetry })
	d, srv := newTestDaemon(t)
	d.builders["7"] = readyBuilder("7", 20001, 0, builderSpec(d.cfg))
	recordBuilders(d)
	d.builders, d.buildersAdopted = map[string]*builder{}, false
	uid := "uid-7"
	srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: "bld-7", Uid: &uid},
		Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}})
	srv.FailList(status.Error(codes.Unavailable, "down")) // only the startup listing fails

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		b := d.builders["7"]
		d.mu.Unlock()
		if b != nil && b.ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("builder of the previous run never adopted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := srv.Deleted(); len(got) != 0 {
		t.Fatalf("deleted %v", got)
	}
}
