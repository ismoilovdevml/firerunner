package vm

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

// muxEnv points the mux and known_hosts dirs at a temp dir and turns multiplexing on.
func muxEnv(t *testing.T) *Instance {
	t.Helper()
	// Short path: unix socket paths are limited to ~104 bytes.
	dir, err := os.MkdirTemp("/tmp", "mx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	oldMux, oldKH, oldOn := MuxDir, KnownHostsDir, Multiplex
	MuxDir, KnownHostsDir, Multiplex = filepath.Join(dir, "mux"), filepath.Join(dir, "kh"), true
	t.Cleanup(func() { MuxDir, KnownHostsDir, Multiplex = oldMux, oldKH, oldOn })
	if err := os.MkdirAll(MuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1:22 is closed or refuses our (missing) key: either way ssh exits 255 fast.
	return &Instance{ID: "job-1", IP: "127.0.0.1", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOnJ0MAjn3Q7Q9c0Q2lD9h8b8n0sJpJqk3X1n5mPq8zK"}
}

func TestSSHMultiplexArgs(t *testing.T) {
	inst := muxEnv(t)
	cfg := config.Default()
	args := strings.Join(SSH(cfg, inst, "true").Args, " ")
	if !strings.Contains(args, "ControlMaster=no") || !strings.Contains(args, "ControlPath="+filepath.Join(MuxDir, "job-1")) {
		t.Fatalf("multiplexed SSH must be a client of the job's master: %s", args)
	}
	Multiplex = false
	if args := strings.Join(SSH(cfg, inst, "true").Args, " "); strings.Contains(args, "Control") {
		t.Fatalf("multiplexing off, but: %s", args)
	}
	Multiplex = true
	unpinned := &Instance{ID: "job-2", IP: "127.0.0.1"}
	if args := strings.Join(SSH(cfg, unpinned, "true").Args, " "); strings.Contains(args, "Control") {
		t.Fatalf("an unpinned VM must not share a connection: %s", args)
	}
}

// Without a live master (never started, died, or a stale socket left behind)
// a stage must still connect on its own, promptly, instead of failing or hanging.
func TestSSHWithoutMasterConnectsDirectly(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	inst := muxEnv(t)
	cfg := config.Default()
	cfg.Network.SSHKey = filepath.Join(t.TempDir(), "missing_key")

	direct := func() (int, time.Duration) {
		start := time.Now()
		code, _ := ExitCode(SSH(cfg, inst, "true").Run())
		return code, time.Since(start)
	}
	want, _ := func() (int, time.Duration) { Multiplex = false; defer func() { Multiplex = true }(); return direct() }()

	// No socket at all.
	if code, took := direct(); code != want || took > 10*time.Second {
		t.Fatalf("no master: exit %d in %s, want %d like a direct connection", code, took, want)
	}
	// A stale socket from a master that died.
	l, err := net.Listen("unix", filepath.Join(MuxDir, "job-1"))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if code, took := direct(); code != want || took > 10*time.Second {
		t.Fatalf("stale socket: exit %d in %s, want %d like a direct connection", code, took, want)
	}
}

func TestStopMuxRemovesStaleSocket(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	inst := muxEnv(t)
	path := filepath.Join(MuxDir, "job-1")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	start := time.Now()
	StopMux(inst)
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("StopMux took %s on a dead master", took)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket left behind: %v", err)
	}
	StopMux(inst) // nothing left: must be a no-op
}

func TestStartMuxOffOrUnpinnedIsNoop(t *testing.T) {
	inst := muxEnv(t)
	Multiplex = false
	if err := StartMux(config.Default(), inst); err != nil {
		t.Fatalf("multiplexing off: %v", err)
	}
	Multiplex = true
	if err := StartMux(config.Default(), &Instance{ID: "job-2", IP: "127.0.0.1"}); err != nil {
		t.Fatalf("unpinned VM: %v", err)
	}
	if entries, _ := os.ReadDir(MuxDir); len(entries) != 0 {
		t.Fatalf("sockets created: %v", entries)
	}
}

func TestForgetRemovesMuxSocket(t *testing.T) {
	inst := muxEnv(t)
	path := filepath.Join(MuxDir, "job-1")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Network.LeasesFile = filepath.Join(t.TempDir(), "leases")
	Forget(cfg, inst.ID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left after Forget: %v", err)
	}
}
