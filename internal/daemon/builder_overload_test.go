package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// Every builder caps its parallel build steps, from its memory unless set;
// setting the cap replaces builders, a new size alone does not.
func TestBuildkitdTOMLCapsParallelism(t *testing.T) {
	cfg := config.Default()
	if s := buildkitdTOML(cfg); !strings.Contains(s, "[worker.oci]\n  max-parallelism = 4\n") {
		t.Fatalf("8 GB builder without a step cap of 4:\n%s", s)
	}
	cfg.Builder.MemoryMB = 4096
	if s := buildkitdTOML(cfg); !strings.Contains(s, "max-parallelism = 2\n") {
		t.Fatalf("4 GB builder without a step cap of 2:\n%s", s)
	}
	cfg.VM.RegistryMirror = "http://10.200.0.1:5000"
	cfg.Builder.MaxParallelism = 3
	s := buildkitdTOML(cfg)
	if !strings.Contains(s, "max-parallelism = 3\n") || strings.Count(s, "[worker.oci]") != 1 {
		t.Fatalf("set cap of 3:\n%s", s)
	}
	// The worker table comes last: a key after it would land in it.
	if !strings.HasSuffix(s, "[worker.oci]\n  max-parallelism = 3\n") {
		t.Fatalf("[worker.oci] is not the last table:\n%s", s)
	}
	a := config.Default()
	b := a
	b.Builder.MaxParallelism = 2
	if builderSpec(a) == builderSpec(b) {
		t.Fatal("a new builder.max_parallelism must replace builders")
	}
}

// probeServer runs a TLS listener like buildkitd's (--tlscacert: client
// certificates required) with the server certificate of creds.
func probeServer(t *testing.T, creds *builderCreds, maxVersion uint16) int {
	t.Helper()
	cert, err := tls.X509KeyPair([]byte(creds.serverCert), []byte(creds.serverKey))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(creds.caPEM))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MaxVersion: maxVersion})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// The probe passes only when buildkitd's TLS answers with the builder's
// certificate. A frozen guest (its kernel accepts, buildkitd never runs), a
// closed port, something else on the port or another builder's certificate
// all fail, within the probe timeout.
func TestBuildkitdProbeNeedsBuildkitd(t *testing.T) {
	old := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })
	creds, err := newBuilderCreds()
	if err != nil {
		t.Fatal(err)
	}
	other, err := newBuilderCreds()
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]uint16{"TLS 1.3": tls.VersionTLS13, "TLS 1.2": tls.VersionTLS12} {
		if !probeBuildkitd("127.0.0.1", probeServer(t, creds, v), creds.caPEM) {
			t.Errorf("%s: answering buildkitd counted as silent", name)
		}
	}

	// Accepted by the kernel (the listen backlog) but never served.
	frozen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = frozen.Close() })
	frozenPort := frozen.Addr().(*net.TCPAddr).Port
	start := time.Now()
	if probeBuildkitd("127.0.0.1", frozenPort, creds.caPEM) {
		t.Error("frozen builder (TCP accepted, no TLS answer) counted as answering")
	}
	if took := time.Since(start); took > 3*probeTimeout {
		t.Errorf("probe of a frozen builder took %v", took)
	}

	garbage, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = garbage.Close() })
	go func() {
		for {
			c, err := garbage.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			_ = c.Close()
		}
	}()
	if probeBuildkitd("127.0.0.1", garbage.Addr().(*net.TCPAddr).Port, creds.caPEM) {
		t.Error("a non-TLS service counted as buildkitd")
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	if probeBuildkitd("127.0.0.1", closedPort, creds.caPEM) {
		t.Error("closed port counted as answering")
	}

	if probeBuildkitd("127.0.0.1", probeServer(t, other, 0), creds.caPEM) {
		t.Error("another builder's certificate counted as this builder answering")
	}

	// A record without a usable CA keeps the old check: the connect alone.
	if !probeBuildkitd("127.0.0.1", frozenPort, "") {
		t.Error("builder without a CA dropped although its port accepts")
	}
}

// A builder a job uses survives missed probes (a heavy build) however often
// reconcile runs, but one silent for builderBusySilence is frozen and is
// dropped, so the project's next builds get a new builder. An answer in
// between starts the silence over.
func TestCheckBuildersDropsFrozenBusyBuilder(t *testing.T) {
	var answering atomic.Bool
	stubBuilders(t, func(string) bool { return answering.Load() })
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "job-9.json"), &vm.JobState{BuilderProject: "1"}); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDaemon(t)
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, 0, builderSpec(d.cfg))}
	check := func() { d.checkBuilders(map[string]bool{"uid-1": true}, time.Now()) }
	// daemon.reconcile_interval 10s: 30 misses within five minutes.
	for i := 0; i < 30; i++ {
		check()
		if d.builders["1"] == nil {
			t.Fatalf("busy builder dropped after %d quick missed probes", i+1)
		}
	}
	d.builders["1"].silentSince = time.Now().Add(-builderBusySilence + time.Minute)
	check()
	if d.builders["1"] == nil {
		t.Fatal("busy builder dropped before builderBusySilence")
	}
	d.builders["1"].silentSince = time.Now().Add(-builderBusySilence)
	answering.Store(true)
	check()
	if b := d.builders["1"]; b == nil || b.strikes != 0 || !b.silentSince.IsZero() {
		t.Fatalf("an answer did not reset the silence: %+v", b)
	}
	answering.Store(false)
	check()
	if d.builders["1"] == nil {
		t.Fatal("silence counted from before the builder answered")
	}
	d.builders["1"].silentSince = time.Now().Add(-builderBusySilence)
	check()
	if d.builders["1"] != nil {
		t.Fatalf("busy builder silent for %v kept", builderBusySilence)
	}
}

// Both probe paths hand the builder's own CA to the probe: without it the
// probe falls back to a TCP connect, which a frozen guest still accepts.
func TestBuilderProbesUseBuilderCA(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var mu sync.Mutex
	got := map[string][]string{}
	buildkitdAnswers = func(ip string, _ int, ca string) bool {
		mu.Lock()
		got[ip] = append(got[ip], ca)
		mu.Unlock()
		return true
	}
	d, _ := newTestDaemon(t)
	b := readyBuilder("1", 20001, 0, builderSpec(d.cfg))
	b.CA = "builder-1-ca"
	d.builders = map[string]*builder{"1": b}
	recordBuilders(d)
	d.checkBuilders(map[string]bool{"uid-1": true}, time.Now())
	if cas := got[b.Instance.IP]; len(cas) == 0 || cas[0] != "builder-1-ca" {
		t.Fatalf("reconcile probe got CA %q, want the builder's", cas)
	}
	got = map[string][]string{}
	d.builders = map[string]*builder{}
	d.buildersAdopted = false
	d.adoptBuilders(map[string]bool{"uid-1": true})
	if cas := got[b.Instance.IP]; len(cas) == 0 || cas[0] != "builder-1-ca" {
		t.Fatalf("adoption probe got CA %q, want the builder's", cas)
	}
}

// Startup probes the previous run's builders all at once: silent ones cost
// one builder's probes, not one per builder.
func TestAdoptBuildersProbesInParallel(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	const slow = 200 * time.Millisecond
	buildkitdAnswers = func(string, int, string) bool { time.Sleep(slow); return false }
	d, _ := newTestDaemon(t)
	live := map[string]bool{}
	for _, p := range []string{"1", "2", "3", "4"} {
		d.builders[p] = readyBuilder(p, 20000+int(p[0]-'0'), 0, builderSpec(d.cfg))
		live["uid-"+p] = true
	}
	recordBuilders(d)
	d.builders = map[string]*builder{}
	start := time.Now()
	d.adoptBuilders(live)
	if took := time.Since(start); took > 3*slow {
		t.Fatalf("adopting 4 silent builders took %v: probed one after another", took)
	}
	if len(d.builders) != 0 {
		t.Fatalf("silent builders adopted: %v", d.builders)
	}
}
