package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if *got != *want {
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
