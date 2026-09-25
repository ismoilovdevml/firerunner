package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
)

func TestLeaseIPMalformedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "leases")
	body := "\n" +
		"garbage\n" +
		"1790224115 aa:fc:00:00:00:01\n" + // no ip field
		"   \t  \n" +
		"1790224116 aa:fc:00:00:00:01 10.200.0.20 * 01:aa\n" +
		"only-two fields\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if ip, err := LeaseIP(p, "aa:fc:00:00:00:01"); err != nil || ip != "10.200.0.20" {
		t.Fatalf("LeaseIP = %q, %v; want the one well-formed lease", ip, err)
	}
}

func TestLeaseIPUnreadable(t *testing.T) {
	// A directory opens but cannot be scanned: the error must surface, not read as "no lease".
	if _, err := LeaseIP(t.TempDir(), "aa:fc:00:00:00:01"); err == nil {
		t.Fatal("want an error reading a directory as the leases file")
	}
}

func TestExitCode(t *testing.T) {
	notRun := errors.New("ssh: executable not found")
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantErr  error
	}{
		{"success", nil, 0, nil},
		{"non-zero exit", exec.Command("sh", "-c", "exit 3").Run(), 3, nil},
		{"could not run", notRun, -1, notRun},
	}
	for _, c := range cases {
		code, err := ExitCode(c.err)
		if code != c.wantCode || !errors.Is(err, c.wantErr) || (c.wantErr == nil) != (err == nil) {
			t.Errorf("%s: ExitCode = %d, %v; want %d, %v", c.name, code, err, c.wantCode, c.wantErr)
		}
	}
}

func TestJobStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs", "42.json")
	want := &JobState{
		Instance:  Instance{ID: "job-42", UID: "u-42", IP: "10.200.0.9"},
		Source:    "pool",
		StartedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Failed:    true,
	}
	if err := SaveJobState(path, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
	got, err := LoadJobState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
}

func TestLoadJobStateErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadJobState(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJobState(bad); err == nil {
		t.Fatal("want an error for corrupt state")
	}
}

// pinMemTotal fixes host memory for one test; tests using it must not run in parallel.
func pinMemTotal(t *testing.T, mb int, err error) {
	t.Helper()
	orig := memTotalMB
	memTotalMB = func() (int, error) { return mb, err }
	t.Cleanup(func() { memTotalMB = orig })
}

func vmWith(state types.MicroVMStatus_MicroVMState, memMB int32) *types.MicroVM {
	return &types.MicroVM{
		Spec:   &types.MicroVMSpec{Id: "pool-x", MemoryInMb: memMB},
		Status: &types.MicroVMStatus{State: state},
	}
}

func dialFake(t *testing.T, srv *flintlocktest.Server) *flintlock.Client {
	t.Helper()
	fl, err := flintlock.Dial(flintlocktest.StartUnix(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.Close() })
	return fl
}

func TestFits(t *testing.T) {
	srv := flintlocktest.NewServer("")
	srv.SetVMs(
		vmWith(types.MicroVMStatus_CREATED, 2000),  // 2100 with overhead
		vmWith(types.MicroVMStatus_PENDING, 1000),  // 1050
		vmWith(types.MicroVMStatus_FAILED, 8000),   // ignored
		vmWith(types.MicroVMStatus_DELETING, 8000), // ignored
	)
	fl := dialFake(t, srv)
	cfg := config.Default()
	cfg.VM.MemoryMB = 1000     // needs 1050 with overhead
	cfg.VM.HostReserveMB = 500 // capacity = total - 500
	// committed 3150 + new 1050 = 4200 -> exact fit at total 4700
	cases := []struct {
		name    string
		totalMB int
		extraMB int
		fits    bool
	}{
		{"exact boundary fits", 4700, 0, true},
		{"one MB short", 4699, 0, false},
		{"extra promised memory counts", 4700, 1, false},
		{"extra promised memory fits with room", 4800, 100, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinMemTotal(t, c.totalMB, nil)
			ok, msg, err := Fits(context.Background(), cfg, fl, c.extraMB)
			if err != nil || ok != c.fits {
				t.Fatalf("Fits = %v, %q, %v; want %v", ok, msg, err, c.fits)
			}
			if !strings.Contains(msg, "new 1050 MB") {
				t.Fatalf("message lacks the 5%% overhead: %q", msg)
			}
		})
	}
}

func TestFitsFailurePaths(t *testing.T) {
	cfg := config.Default()

	t.Run("unknown host memory does not block", func(t *testing.T) {
		srv := flintlocktest.NewServer("")
		fl := dialFake(t, srv)
		pinMemTotal(t, 0, errors.New("no /proc/meminfo"))
		ok, _, err := Fits(context.Background(), cfg, fl, 0)
		if !ok || err == nil {
			t.Fatalf("Fits = %v, %v; want true with the error", ok, err)
		}
		if srv.ListCalls() != 0 {
			t.Fatal("flintlock must not be asked when capacity is unknown")
		}
	})
	t.Run("flintlock down blocks", func(t *testing.T) {
		srv := flintlocktest.NewServer("")
		srv.FailList(status.Error(codes.Unavailable, "down"))
		fl := dialFake(t, srv)
		pinMemTotal(t, 1<<20, nil)
		ok, _, err := Fits(context.Background(), cfg, fl, 0)
		if ok || status.Code(err) != codes.Unavailable {
			t.Fatalf("Fits = %v, %v; want false with Unavailable", ok, err)
		}
	})
}

func TestCreateJobStateIsExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jobs", "job-1.json")
	if err := CreateJobState(p, &JobState{Instance: Instance{IP: "10.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := CreateJobState(p, &JobState{Instance: Instance{IP: "10.0.0.2"}}); err == nil {
		t.Fatal("second create must fail")
	}
	st, err := LoadJobState(p)
	if err != nil || st.IP != "10.0.0.1" {
		t.Fatalf("state replaced: %+v %v", st, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// bootEnv prepares Boot against a fake flintlockd: an SSH key, a leases file
// with leases, pinned keys in a temp dir, a short boot timeout, and recorded
// dhcp_release calls (released).
func bootEnv(t *testing.T, leases string) (cfg config.Config, srv *flintlocktest.Server, fl *flintlock.Client, released *[][]string) {
	t.Helper()
	dir := t.TempDir()
	cfg = config.Default()
	cfg.Network.SSHKey = filepath.Join(dir, "id_ed25519")
	cfg.Network.LeasesFile = filepath.Join(dir, "leases")
	cfg.VM.BootTimeout = 300 * time.Millisecond
	for path, data := range map[string]string{cfg.Network.SSHKey + ".pub": "ssh-ed25519 AAAA test\n", cfg.Network.LeasesFile: leases} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldKH, oldPoll, oldRelease, oldDev := KnownHostsDir, bootPoll, releaseLease, routeDev
	KnownHostsDir, bootPoll = filepath.Join(dir, "kh"), 20*time.Millisecond
	released = &[][]string{}
	var mu sync.Mutex
	releaseLease = func(args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		*released = append(*released, args)
		return nil
	}
	routeDev = func(string) string { return "br-fc" }
	t.Cleanup(func() { KnownHostsDir, bootPoll, releaseLease, routeDev = oldKH, oldPoll, oldRelease, oldDev })
	srv = flintlocktest.NewServer("uid-new")
	return cfg, srv, dialFake(t, srv), released
}

// A lease dnsmasq already held for the new VM's MAC before the create belongs
// to another VM (an id used again, or a guest that took the MAC). Boot never
// takes that address, and a failed boot never releases that VM's lease.
func TestBootIgnoresLeasesFromBeforeCreate(t *testing.T) {
	const id = "bld-7"
	mac := MAC(id)
	exp := time.Now().Add(time.Hour).Unix()
	old := fmt.Sprintf("%d %s 10.200.0.50 %s 01:%s\n", exp, mac, id, mac)
	for _, tc := range []struct {
		name    string
		fresh   string // lease line dnsmasq writes once the VM exists
		wantErr string
		release string // address released when the boot fails
	}{
		{"only the old VM's lease", "", "got no DHCP lease", ""},
		{"a new lease after the create", fmt.Sprintf("%d %s 127.0.0.1 %s 01:%s\n", exp+60, mac, id, mac), "at 127.0.0.1", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, srv, fl, released := bootEnv(t, old)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for tc.fresh != "" && len(srv.Created()) == 0 {
					time.Sleep(5 * time.Millisecond)
				}
				if tc.fresh != "" {
					f, err := os.OpenFile(cfg.Network.LeasesFile, os.O_APPEND|os.O_WRONLY, 0)
					if err == nil {
						_, _ = f.WriteString(tc.fresh)
						_ = f.Close()
					}
				}
			}()
			inst, err := Boot(context.Background(), cfg, fl, id, nil)
			<-done
			if inst != nil || err == nil || !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Boot = %v, %v; want an ErrNotReady mentioning %q", inst, err, tc.wantErr)
			}
			if got := srv.Deleted(); len(got) != 1 || got[0] != "uid-new" {
				t.Fatalf("deleted %v, want the new VM", got)
			}
			var ips []string
			for _, args := range *released {
				ips = append(ips, args[1])
			}
			if strings.Join(ips, ",") != tc.release {
				t.Fatalf("released %v, want %q (never the old VM's 10.200.0.50)", ips, tc.release)
			}
		})
	}
}

// A VM whose address is known releases the lease of that address only, even
// when another VM holds a newer lease for the same MAC.
func TestForgetInstanceReleasesOnlyItsLease(t *testing.T) {
	cfg, _, _, released := bootEnv(t, "")
	mac := MAC("bld-7")
	leases := fmt.Sprintf("1790286904 %s 10.200.0.50 * 01:a\n1790286905 %s 10.200.0.77 * 01:b\n", mac, mac)
	if err := os.WriteFile(cfg.Network.LeasesFile, []byte(leases), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ ip, want string }{
		{"10.200.0.50", "br-fc 10.200.0.50 " + mac + " 01:a"},
		{"10.200.0.77", "br-fc 10.200.0.77 " + mac + " 01:b"},
		{"10.200.0.99", ""},                        // its lease is gone: nothing to release
		{"", "br-fc 10.200.0.77 " + mac + " 01:b"}, // address unknown: the newest lease
	} {
		*released = nil
		ForgetInstance(cfg, &Instance{ID: "bld-7", IP: tc.ip})
		got := ""
		if len(*released) > 0 {
			got = strings.Join((*released)[0], " ")
		}
		if got != tc.want || len(*released) > 1 {
			t.Errorf("ForgetInstance(ip %q) released %v, want %q", tc.ip, *released, tc.want)
		}
	}
}
