package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"strings"
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

// A builder a job uses survives a few missed probes (a heavy build), but one
// silent for builderBusyStrikes reconciles is frozen: it is dropped, so its
// jobs fail now instead of when their connections time out.
func TestCheckBuildersDropsFrozenBusyBuilder(t *testing.T) {
	stubBuilders(t, func(string) bool { return false })
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "job-9.json"), &vm.JobState{BuilderProject: "1"}); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDaemon(t)
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, 0, builderSpec(d.cfg))}
	for i := 1; i < builderBusyStrikes; i++ {
		d.checkBuilders(map[string]bool{"uid-1": true}, time.Now())
		if d.builders["1"] == nil {
			t.Fatalf("busy builder dropped after %d missed probes", i)
		}
	}
	d.checkBuilders(map[string]bool{"uid-1": true}, time.Now())
	if d.builders["1"] != nil {
		t.Fatalf("builder silent for %d probes kept for its jobs", builderBusyStrikes)
	}
}
