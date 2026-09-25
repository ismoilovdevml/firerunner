package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// TestMain points every host path the daemon touches at a temporary directory
// and replaces nft and the SSH and TCP probes of microVMs before any test runs.
// Tests run as root on runner hosts: one that forgets a stub must not remove
// the host's job state files (reconcile), pinned host keys and DHCP leases
// (vm.Forget), saved builder caches or firewall entries. Tests that stub one of
// these themselves restore it to the value set here.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "frd-host")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	jobStateGlob = filepath.Join(root, "jobs", "*.json")
	flintlockVMDir = filepath.Join(root, "flintlock-vm")
	builderCacheDir = filepath.Join(root, "builder-cache")
	vm.KnownHostsDir = filepath.Join(root, "known_hosts")
	vm.MuxDir = filepath.Join(root, "ssh-mux")
	vm.AdmissionFile = filepath.Join(root, "admission.json")
	poolBoot = func(context.Context, config.Config, *flintlock.Client, string, map[string]string) (*vm.Instance, error) {
		return nil, errors.New("no microVMs in tests")
	}
	nftRun = func(...string) error { return errors.New("nft is not run in tests") }
	alive = func(config.Config, *vm.Instance) bool { return false }
	warmDocker = func(config.Config, *vm.Instance) error { return nil }
	serviceActive = func(context.Context, string) bool { return false }
	thinPoolUsage = func(context.Context) (float64, float64, error) { return 0, 0, errors.New("no lvs in tests") }
	tcpOpen = func(string, int) bool { return false }
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestDecide(t *testing.T) {
	cfg := config.Default() // boot_timeout 3m, job_max_age 3h
	cases := []struct {
		name string
		f    vmFacts
		keep bool
	}{
		{"failed VM", vmFacts{State: "FAILED", Owned: true}, false},
		{"owned pool VM", vmFacts{State: "CREATED", Role: "pool", Owned: true, Age: time.Hour}, true},
		{"job older than max age", vmFacts{State: "CREATED", Role: "job", Job: "job-1", Owned: true, InJob: true, Age: 4 * time.Hour, JobAge: 4 * time.Hour}, false},
		{"job on a pool VM older than max age", vmFacts{State: "CREATED", Role: "pool", Owned: true, InJob: true, Age: time.Minute, JobAge: 4 * time.Hour}, false},
		{"running job", vmFacts{State: "CREATED", Role: "job", Job: "job-1", Owned: true, InJob: true, Age: 20 * time.Minute, JobAge: 20 * time.Minute}, true},
		{"young job on a pool VM that idled long", vmFacts{State: "CREATED", Role: "pool", Owned: true, InJob: true, Age: 4 * time.Hour, JobAge: time.Hour}, true},
		{"idle pool VM older than job max age", vmFacts{State: "CREATED", Role: "pool", Owned: true, Age: 4 * time.Hour}, true},
		{"pool VM from previous run", vmFacts{State: "CREATED", Role: "pool", Startup: true}, false},
		{"young orphan pool VM", vmFacts{State: "CREATED", Role: "pool", Age: time.Minute}, true},
		{"old orphan pool VM", vmFacts{State: "CREATED", Role: "pool", Age: 5 * time.Minute}, false},
		{"pool VM this daemon is still booting", vmFacts{State: "CREATED", Role: "pool", Age: 5 * time.Minute, Booting: true}, true},
		{"job VM still booting", vmFacts{State: "PENDING", Role: "job", Job: "job-2", Age: 2 * time.Minute}, true},
		{"job VM without job", vmFacts{State: "CREATED", Role: "job", Job: "job-3", Age: 7 * time.Minute}, false},
		{"fresh run VM", vmFacts{State: "CREATED", Role: "run", Age: time.Hour}, true},
		{"abandoned run VM", vmFacts{State: "CREATED", Role: "run", Age: 4 * time.Hour}, false},
		{"foreign VM without labels", vmFacts{State: "CREATED", Age: 10 * time.Hour}, true},
		{"owned builder", vmFacts{State: "CREATED", Role: "builder", Owned: true, Age: 2 * time.Hour}, true},
		{"owned builder older than job max age", vmFacts{State: "CREATED", Role: "builder", Owned: true, Age: 4 * time.Hour}, true},
		{"owned builder a week old", vmFacts{State: "CREATED", Role: "builder", Owned: true, Age: 7 * 24 * time.Hour}, true},
		{"builder from previous run", vmFacts{State: "CREATED", Role: "builder", Startup: true}, false},
		{"booting builder", vmFacts{State: "CREATED", Role: "builder", Age: 5 * time.Minute}, true},
		{"orphaned builder", vmFacts{State: "CREATED", Role: "builder", Age: 20 * time.Minute}, false},
		{"builder booting while the daemon starts", vmFacts{State: "CREATED", Role: "builder", Startup: true, Booting: true}, true},
		{"builder left by a previous run", vmFacts{State: "CREATED", Role: "builder", Startup: true}, false},
	}
	for _, c := range cases {
		if got := decide(c.f, cfg); (got == "") != c.keep {
			t.Errorf("%s: decide = %q, keep want %v", c.name, got, c.keep)
		}
	}
}

func TestFingerprintChangesWithBootSettings(t *testing.T) {
	a := config.Default()
	b := a
	b.Pool.PreloadImages = []string{"alpine"}
	c := a
	c.VM.MemoryMB = 4096
	if fingerprint(a) == fingerprint(b) || fingerprint(a) == fingerprint(c) {
		t.Fatal("fingerprint must change when images or VM size change")
	}
	d := a
	d.Pool.Size = 9 // does not affect how a VM boots
	if fingerprint(a) != fingerprint(d) {
		t.Fatal("pool size must not invalidate pooled VMs")
	}
}

func TestTransientListError(t *testing.T) {
	err := errors.New("rpc error: code = Unknown desc = getting microvm spec: reading content sha256:ab: failed reading from content store")
	if !flintlock.IsTransient(err) || flintlock.IsTransient(errors.New("connection refused")) || flintlock.IsTransient(nil) {
		t.Fatal("IsTransient misclassifies")
	}
}

func TestRoleOf(t *testing.T) {
	for id, want := range map[string][2]string{
		"pool-ab12cd": {"pool", ""}, "job-74196": {"job", "job-74196"}, "run-1a2b3c": {"run", ""},
		"smoke1": {"", ""}, "mvm-1": {"", ""}, "bld-111": {"builder", ""},
	} {
		if r, j := RoleOf(id); r != want[0] || j != want[1] {
			t.Errorf("RoleOf(%q) = %q,%q want %v", id, r, j, want)
		}
	}
}

func TestTidyVMDirs(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, age time.Time, withFile bool) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if withFile {
			if err := os.MkdirAll(filepath.Join(p, "01UID"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(p, age, age); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := mk("job-1", old, false)         // deleted VM, empty, old: removed
	fresh := mk("job-2", time.Now(), false) // being created: kept
	liveDir := mk("pool-a", old, false)     // listed: kept
	withState := mk("job-3", old, true)     // not empty: kept
	if err := tidyVMDirs(root, map[string]bool{"pool-a": true}); err != nil {
		t.Fatalf("a non-empty dir is not an error: %v", err)
	}
	for p, want := range map[string]bool{gone: false, fresh: true, liveDir: true, withState: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	if err := tidyVMDirs(filepath.Join(root, "missing"), nil); err != nil { // no namespace dir yet
		t.Fatal(err)
	}
	// A read-only state dir (e.g. a hardened unit without write access) is reported.
	ro := mk("ro", old, false)
	mk("ro/job-9", old, false)
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if os.Geteuid() != 0 {
		if err := tidyVMDirs(ro, nil); err == nil {
			t.Fatal("a failed removal must be reported")
		}
	}
}

// Every host path a daemon test can reach is a temporary one (see TestMain):
// `go test` as root on a runner host must not delete its job state files,
// pinned host keys, DHCP leases or saved caches.
func TestHostPathsAreIsolated(t *testing.T) {
	d, _ := newTestDaemon(t)
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"jobStateGlob":        jobStateGlob,
		"flintlockVMDir":      flintlockVMDir,
		"builderCacheDir":     builderCacheDir,
		"vm.KnownHostsDir":    vm.KnownHostsDir,
		"vm.MuxDir":           vm.MuxDir,
		"vm.AdmissionFile":    vm.AdmissionFile,
		"network.leases_file": d.cfgSnapshot().Network.LeasesFile,
		"daemon.socket":       d.cfgSnapshot().Daemon.Socket,
	} {
		dir := filepath.Dir(p)
		for {
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
				break
			}
			dir = filepath.Dir(dir)
		}
		if !strings.HasPrefix(dir+string(filepath.Separator), tmp+string(filepath.Separator)) {
			t.Errorf("%s = %s: a host path outside %s", name, p, tmp)
		}
	}
	if err := nftRun("list", "ruleset"); err == nil {
		t.Error("nft ran: tests must never touch the host firewall")
	}
}
