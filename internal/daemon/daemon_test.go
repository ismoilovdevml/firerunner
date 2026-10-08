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

	"github.com/liquidmetal-dev/flintlock/api/types"

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
	vm.ThinPoolFile = filepath.Join(root, "thinpool.json")
	poolBoot = func(context.Context, config.Config, *flintlock.Client, string, map[string]string) (*vm.Instance, error) {
		return nil, errors.New("no microVMs in tests")
	}
	nftRun = func(string, ...string) (string, error) { return "", errors.New("nft is not run in tests") }
	reconcileBindings = func(context.Context, map[string]vm.Binding) ([]string, []string, error) { return nil, nil, nil }
	releaseLease = func(config.Config, string, string) {}
	alive = func(config.Config, *vm.Instance) bool { return false }
	warmDocker = func(config.Config, *vm.Instance) error { return nil }
	serviceActive = func(context.Context, string) bool { return false }
	thinPoolUsage = func(context.Context) (float64, float64, error) { return 0, 0, errors.New("no lvs in tests") }
	buildkitdAnswers = func(string, int, string) bool { return false }
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
		"vm.ThinPoolFile":     vm.ThinPoolFile,
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
	if _, err := nftRun("", "list", "ruleset"); err == nil {
		t.Error("nft ran: tests must never touch the host firewall")
	}
}

// A guest that floods its console (or a builder living for days) cannot fill
// the host disk: firecracker's appended files over the bound are emptied with
// a line saying so. Nothing else in the state dir is touched, and a symlink
// is never followed.
func TestCapVMFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "job-1", "u1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	size := func(name string, n int64) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(n); err != nil { // sparse: no real disk needed
			t.Fatal(err)
		}
		_ = f.Close()
		return p
	}
	const limit = 1 << 20
	console := size("firecracker.stdout", limit+1)
	metrics := size("firecracker.metrics", limit)
	metadata := size("metadata.json", limit+1)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, make([]byte, limit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "firecracker.log")); err != nil {
		t.Fatal(err)
	}

	if got := capVMFiles(root, limit); len(got) != 1 || got[0] != console {
		t.Fatalf("emptied %v, want only the console", got)
	}
	if data, _ := os.ReadFile(console); !strings.Contains(string(data), "everything before this line was dropped") || len(data) > 200 {
		t.Fatalf("console after the bound: %d bytes %q", len(data), data)
	}
	for _, p := range []string{metrics, metadata, outside} {
		if fi, err := os.Stat(p); err != nil || fi.Size() < limit {
			t.Errorf("%s changed: %v %v", p, fi.Size(), err)
		}
	}
	// Firecracker keeps appending after the file was emptied (O_APPEND).
	f, err := os.OpenFile(console, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("boot ok\n")
	_ = f.Close()
	if data, _ := os.ReadFile(console); !strings.HasSuffix(string(data), "dropped]\nboot ok\n") {
		t.Fatalf("console after more output: %q", data)
	}
	if got := capVMFiles(filepath.Join(root, "missing"), limit); len(got) != 0 {
		t.Fatalf("missing root: %v", got)
	}
}

// Reconcile binds every listed microVM whose tap and address are known: VMs
// of an older binary, a binding that failed at boot, sets a firewall reload
// emptied. VMs without a reported tap or a lease are left unchecked.
func TestBindAddressesFromTheListing(t *testing.T) {
	d, _ := newTestDaemon(t)
	var got map[string]vm.Binding
	old := reconcileBindings
	reconcileBindings = func(_ context.Context, want map[string]vm.Binding) ([]string, []string, error) {
		got = want
		return nil, nil, nil
	}
	t.Cleanup(func() { reconcileBindings = old })
	listed := func(id, tap string) *types.MicroVM {
		mac := vm.MAC(id)
		dev := "eth1"
		v := &types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Interfaces: []*types.NetworkInterface{{DeviceId: dev, GuestMac: &mac}}},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}}
		if tap != "" {
			v.Status.NetworkInterfaces = map[string]*types.NetworkInterfaceStatus{dev: {HostDeviceName: tap}}
		}
		return v
	}
	exp := time.Now().Add(15 * time.Minute).Unix()
	leases := fmt.Sprintf("%d %s 10.200.0.21 job-1 *\n%d %s 10.200.0.22 job-2 *\n", exp, vm.MAC("job-1"), exp, vm.MAC("job-2"))
	leases += fmt.Sprintf("%d %s 10.200.0.24 job-4 *\n", exp, vm.MAC("job-4"))
	if err := os.WriteFile(d.cfg.Network.LeasesFile, []byte(leases), 0o600); err != nil {
		t.Fatal(err)
	}
	d.bindAddresses(context.Background(), d.cfg, []*types.MicroVM{
		listed("job-1", "fltap1"),  // bound
		listed("job-2", ""),        // flintlock reports no tap: unchecked
		listed("job-3", "fltap3"),  // no lease yet: unchecked
		listed("job-4", "fltap4a"), // a retried prepare: two VMs, one MAC,
		listed("job-4", "fltap4b"), // one lease; each binds at its own boot
	})
	if len(got) != 1 || got["fltap1"] != (vm.Binding{Tap: "fltap1", MAC: vm.MAC("job-1"), IP: "10.200.0.21"}) {
		t.Fatalf("bindings wanted: %+v", got)
	}
}

// Reconcile releases the lease of a MAC flintlock does not list once it has
// been seen so for leaseGrace; never a listed VM's, nor one whose VM shows up
// in a later listing (created after an earlier one), nor an expired one.
func TestReleaseStaleLeases(t *testing.T) {
	d, _ := newTestDaemon(t)
	var got []string
	old := releaseLease
	releaseLease = func(_ config.Config, mac, ip string) { got = append(got, mac+" "+ip) }
	t.Cleanup(func() { releaseLease = old })
	t0 := time.Now()
	future := t0.Add(15 * time.Minute)
	leases := []vm.Lease{
		{Expires: future, MAC: "aa:fc:00:00:00:01", IP: "10.200.0.1"},               // gone VM
		{Expires: future, MAC: "aa:fc:00:00:00:02", IP: "10.200.0.2"},               // listed VM
		{Expires: future, MAC: "aa:fc:00:00:00:03", IP: "10.200.0.3"},               // listed from the second pass on
		{Expires: t0.Add(-time.Minute), MAC: "aa:fc:00:00:00:04", IP: "10.200.0.4"}, // expired
	}
	listed := map[string]bool{"aa:fc:00:00:00:02": true}
	d.releaseStaleLeases(d.cfg, listed, leases, t0)
	listed["aa:fc:00:00:00:03"] = true
	d.releaseStaleLeases(d.cfg, listed, leases, t0.Add(time.Minute))
	if len(got) != 0 {
		t.Fatalf("released %v before the grace", got)
	}
	d.releaseStaleLeases(d.cfg, listed, leases, t0.Add(leaseGrace))
	if strings.Join(got, ",") != "aa:fc:00:00:00:01 10.200.0.1" {
		t.Fatalf("released %v, want only the gone VM's lease", got)
	}
	// dnsmasq drops a released lease from its file; one still there (a
	// release that failed) starts its grace again instead of a release on
	// every pass.
	d.releaseStaleLeases(d.cfg, listed, leases, t0.Add(leaseGrace+time.Minute))
	if len(got) != 1 {
		t.Fatalf("released %v, want no second release right away", got)
	}
}
