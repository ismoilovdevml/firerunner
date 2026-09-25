package vm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
)

// tempAdmission points AdmissionFile at a temporary directory for one test.
func tempAdmission(t *testing.T) string {
	t.Helper()
	old := AdmissionFile
	AdmissionFile = filepath.Join(t.TempDir(), "run", "admission.json")
	t.Cleanup(func() { AdmissionFile = old })
	return AdmissionFile
}

// admitCfg is a config whose microVMs need 1050 MB each (with overhead) on a
// host without reserve.
func admitCfg() config.Config {
	cfg := config.Default()
	cfg.VM.MemoryMB = 1000
	cfg.VM.HostReserveMB = 0
	return cfg
}

func listedVM(id, uid string, memMB int32) *types.MicroVM {
	return &types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid, MemoryInMb: memMB},
		Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}}
}

// A burst of prepares with room for one: each lists flintlock before the
// others' creates land. Exactly one may create a microVM.
func TestAdmitConcurrentOnlyOneFits(t *testing.T) {
	tempAdmission(t)
	pinMemTotal(t, 1050, nil)
	srv := flintlocktest.NewServer("uid-new")
	fl := dialFake(t, srv)
	cfg := admitCfg()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			n, _, err := Admit(context.Background(), cfg, fl, id)
			if err != nil {
				t.Errorf("Admit(%s) = %v", id, err)
				return
			}
			if n == 1 {
				// The fake does not list what it created: only the
				// reservation keeps the others out, as in the real race.
				if _, err := fl.Create(context.Background(), &types.MicroVMSpec{Id: id}); err != nil {
					t.Error(err)
				}
			}
		}(fmt.Sprintf("job-%d", i))
	}
	close(start)
	wg.Wait()
	if got := srv.Created(); len(got) != 1 {
		t.Fatalf("%d microVMs created with room for one", len(got))
	}
}

// Admit answers what Room answered (one listing, capped at what is wanted).
func TestAdmitRoom(t *testing.T) {
	cases := []struct {
		name          string
		totalMB, want int
		room          int
	}{
		{"nothing fits", 3000, 4, 0},
		{"exactly two fit", 4200, 4, 2},
		{"one MB short of two", 4199, 4, 1},
		{"capped at want", 20000, 3, 3},
		{"already over capacity", 1000, 2, 0},
		{"nothing wanted", 20000, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tempAdmission(t)
			pinMemTotal(t, c.totalMB, nil)
			srv := flintlocktest.NewServer("")
			srv.SetVMs(listedVM("pool-x", "ux", 2000)) // 2100 with overhead
			fl := dialFake(t, srv)
			ids := make([]string, c.want)
			for i := range ids {
				ids[i] = fmt.Sprintf("pool-%d", i)
			}
			n, _, err := Admit(context.Background(), admitCfg(), fl, ids...)
			if err != nil || n != c.room {
				t.Fatalf("Admit = %d, %v; want %d", n, err, c.room)
			}
			if lists := srv.ListCalls(); c.want > 0 && lists != 1 {
				t.Fatalf("Admit listed flintlock %d times, want 1", lists)
			}
		})
	}
}

// A reservation stands for its microVM until flintlock lists it, its boot
// returned (Unreserve) or its process is gone; never twice.
func TestAdmitReservationLifetime(t *testing.T) {
	cfg := admitCfg()
	admit := func(t *testing.T, fl *flintlock.Client, ids ...string) int {
		t.Helper()
		n, why, err := Admit(context.Background(), cfg, fl, ids...)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("admitted %d of %v (%s)", n, ids, why)
		return n
	}

	t.Run("counted until listed, then only by the listing", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 3150, nil) // three VMs
		srv := flintlocktest.NewServer("")
		fl := dialFake(t, srv)
		if n := admit(t, fl, "job-a", "job-b"); n != 2 {
			t.Fatalf("admitted %d, want 2", n)
		}
		if n := admit(t, fl, "job-c", "job-d"); n != 1 {
			t.Fatalf("admitted %d with two reserved, want 1", n)
		}
		// All three are listed now: counted once each, nothing left.
		srv.SetVMs(listedVM("job-a", "ua", 1000), listedVM("job-b", "ub", 1000), listedVM("job-c", "uc", 1000))
		if n := admit(t, fl, "job-e"); n != 0 {
			t.Fatalf("admitted %d on a full host", n)
		}
		// job-a is deleted: its memory is free again.
		srv.SetVMs(listedVM("job-b", "ub", 1000), listedVM("job-c", "uc", 1000))
		if n := admit(t, fl, "job-f"); n != 1 {
			t.Fatalf("admitted %d after a delete, want 1", n)
		}
	})

	t.Run("released when the boot returned", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 1050, nil)
		fl := dialFake(t, flintlocktest.NewServer(""))
		if n := admit(t, fl, "job-a"); n != 1 {
			t.Fatal("first admission refused")
		}
		if n := admit(t, fl, "job-b"); n != 0 {
			t.Fatal("admitted while job-a was reserved")
		}
		if err := Unreserve("job-a"); err != nil {
			t.Fatal(err)
		}
		if n := admit(t, fl, "job-b"); n != 1 {
			t.Fatal("job-a's memory still reserved after its boot failed")
		}
		if err := Unreserve("job-unknown"); err != nil {
			t.Fatalf("Unreserve of nothing = %v", err)
		}
	})

	t.Run("an old VM with the same id does not stand for the new one", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 2100, nil) // the old builder and one more
		srv := flintlocktest.NewServer("")
		srv.SetVMs(listedVM("bld-7", "old", 1000)) // still copying its cache out
		fl := dialFake(t, srv)
		if n := admit(t, fl, "bld-7"); n != 1 {
			t.Fatal("new builder refused")
		}
		if n := admit(t, fl, "job-a"); n != 0 {
			t.Fatal("the new bld-7 was taken for created because the old one is listed")
		}
		srv.SetVMs(listedVM("bld-7", "old", 1000), listedVM("bld-7", "new", 1000))
		if n := admit(t, fl, "job-a"); n != 0 {
			t.Fatal("admitted on a full host")
		}
	})

	t.Run("expires after the longest boot", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 1050, nil)
		old := admissionTimeout
		admissionTimeout = time.Second
		t.Cleanup(func() { admissionTimeout = old })
		short := cfg
		short.VM.BootTimeout = 0 // expires admissionTimeout after the admission
		fl := dialFake(t, flintlocktest.NewServer(""))
		if n, _, err := Admit(context.Background(), short, fl, "job-a"); n != 1 || err != nil {
			t.Fatalf("Admit = %d, %v", n, err)
		}
		if n := admit(t, fl, "job-b"); n != 0 {
			t.Fatal("admitted while job-a was reserved")
		}
		time.Sleep(1100 * time.Millisecond)
		if n := admit(t, fl, "job-b"); n != 1 {
			t.Fatal("an expired reservation still holds memory")
		}
	})
}

// Admissions exclude each other across processes: gitlab-runner starts every
// prepare as its own `firerunner executor` process, and the daemon is another.
func TestAdmitAcrossProcesses(t *testing.T) {
	file := tempAdmission(t)
	srv := flintlocktest.NewServer("")
	flcfg := flintlocktest.StartUnix(t, srv)
	const procs = 6
	var cmds []*exec.Cmd
	var outs []*bufio.Reader
	for i := 0; i < procs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAdmitHelperProcess$")
		cmd.Env = append(os.Environ(), "FR_ADMIT_HELPER=job-"+strconv.Itoa(i), "FR_ADMIT_FILE="+file,
			"FR_ADMIT_ENDPOINT="+flcfg.Endpoint, "FR_ADMIT_TOKEN="+flcfg.TokenFile, "FR_ADMIT_MEM=3150")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := cmd.StdinPipe() // the helper holds its reservation until this closes
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
		cmds = append(cmds, cmd)
		outs = append(outs, bufio.NewReader(stdout))
	}
	admitted := 0
	for i, out := range outs {
		line, err := out.ReadString('\n')
		if err != nil {
			t.Fatalf("helper %d: %v", i, err)
		}
		switch strings.TrimSpace(line) {
		case "admitted":
			admitted++
		case "refused":
		default:
			t.Fatalf("helper %d: %q", i, line)
		}
	}
	if admitted != 3 {
		t.Fatalf("%d of %d processes admitted with room for 3", admitted, procs)
	}
	// The helpers exit without Unreserve (a prepare killed during its boot):
	// once they are gone, their memory is free again.
	for _, c := range cmds {
		_ = c.Process.Kill()
		_ = c.Wait()
	}
	pinMemTotal(t, 3150, nil)
	fl := dialFake(t, srv)
	if n, _, err := Admit(context.Background(), admitCfg(), fl, "job-a", "job-b", "job-c"); n != 3 || err != nil {
		t.Fatalf("Admit after the helpers died = %d, %v; want 3", n, err)
	}
}

// TestAdmitHelperProcess is one process of TestAdmitAcrossProcesses; run
// directly it does nothing.
func TestAdmitHelperProcess(t *testing.T) {
	id := os.Getenv("FR_ADMIT_HELPER")
	if id == "" {
		return
	}
	AdmissionFile = os.Getenv("FR_ADMIT_FILE")
	mem, _ := strconv.Atoi(os.Getenv("FR_ADMIT_MEM"))
	memTotalMB = func() (int, error) { return mem, nil }
	fl, err := flintlock.Dial(config.Flintlock{Endpoint: os.Getenv("FR_ADMIT_ENDPOINT"),
		TokenFile: os.Getenv("FR_ADMIT_TOKEN"), Namespace: "test"})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	n, _, err := Admit(context.Background(), admitCfg(), fl, id)
	switch {
	case err != nil:
		fmt.Println("error:", err)
	case n == 1:
		fmt.Println("admitted")
	default:
		fmt.Println("refused")
	}
	// Stay alive (holding the reservation) until the test is done with us.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

func TestAdmitFailurePaths(t *testing.T) {
	cfg := admitCfg()

	t.Run("flintlock down admits nothing and reserves nothing", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 1050, nil)
		srv := flintlocktest.NewServer("")
		srv.FailList(status.Error(codes.Unavailable, "down"))
		fl := dialFake(t, srv)
		if n, _, err := Admit(context.Background(), cfg, fl, "job-a"); n != 0 || status.Code(err) != codes.Unavailable {
			t.Fatalf("Admit = %d, %v; want 0 with Unavailable", n, err)
		}
		if n, _, err := Admit(context.Background(), cfg, fl, "job-b"); n != 1 || err != nil {
			t.Fatalf("Admit after flintlock came back = %d, %v; want 1", n, err)
		}
	})

	t.Run("unknown host memory does not block", func(t *testing.T) {
		tempAdmission(t)
		pinMemTotal(t, 0, errors.New("no /proc/meminfo"))
		fl := dialFake(t, flintlocktest.NewServer(""))
		if n, _, err := Admit(context.Background(), cfg, fl, "job-a", "job-b"); n != 2 || err != nil {
			t.Fatalf("Admit = %d, %v; want 2", n, err)
		}
	})

	t.Run("a held lock is waited for, not forever", func(t *testing.T) {
		file := tempAdmission(t)
		pinMemTotal(t, 1050, nil)
		fl := dialFake(t, flintlocktest.NewServer(""))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		lock, err := os.OpenFile(file+".lock", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		if n, _, err := Admit(ctx, cfg, fl, "job-a"); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Admit under a held lock = %d, %v; want 0 with a deadline error", n, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("Admit waited %s for the lock", took)
		}
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		if n, _, err := Admit(context.Background(), cfg, fl, "job-a"); n != 1 || err != nil {
			t.Fatalf("Admit after the lock was released = %d, %v", n, err)
		}
	})

	t.Run("a reservation that cannot be recorded is not admitted", func(t *testing.T) {
		file := tempAdmission(t)
		pinMemTotal(t, 1050, nil)
		fl := dialFake(t, flintlocktest.NewServer(""))
		if err := os.MkdirAll(file+".tmp", 0o700); err != nil { // the temp file cannot be written
			t.Fatal(err)
		}
		if n, _, err := Admit(context.Background(), cfg, fl, "job-a"); n != 0 || err == nil {
			t.Fatalf("Admit = %d, %v; want 0 with the write error", n, err)
		}
	})
}

// errTransient is what flintlockd answers a listing while it deletes a microVM.
var errTransient = status.Error(codes.Unknown, "getting microvm spec: failed reading from content store")

// admitOne admits the microVM "vm-1" through Admit or through AdmitFits with
// the builders' production check, Fits.
type admitOne func(context.Context, config.Config, *flintlock.Client) (bool, error)

var admitOnes = []struct {
	name  string
	admit admitOne
}{
	{"Admit", func(ctx context.Context, cfg config.Config, fl *flintlock.Client) (bool, error) {
		n, _, err := Admit(ctx, cfg, fl, "vm-1")
		return n == 1, err
	}},
	{"AdmitFits with Fits", func(ctx context.Context, cfg config.Config, fl *flintlock.Client) (bool, error) {
		ok, _, err := AdmitFits(ctx, cfg, fl, "vm-1", Fits)
		return ok, err
	}},
}

// The lock is held for exactly one flintlock listing: the builders' check
// (Fits) decides on the listing the admission took, it does not list again,
// and it still counts the microVMs that listing holds.
func TestAdmitListsFlintlockOnce(t *testing.T) {
	for _, a := range admitOnes {
		for _, c := range []struct {
			name    string
			totalMB int
			want    bool
		}{
			{"room next to the listed microVM", 2100, true},
			{"the listed microVM leaves no room", 2099, false},
		} {
			t.Run(a.name+"/"+c.name, func(t *testing.T) {
				tempAdmission(t)
				pinMemTotal(t, c.totalMB, nil)
				srv := flintlocktest.NewServer("")
				srv.SetVMs(listedVM("pool-x", "ux", 1000)) // 1050 with overhead
				fl := dialFake(t, srv)
				ok, err := a.admit(context.Background(), admitCfg(), fl)
				if ok != c.want || err != nil {
					t.Fatalf("admitted %v, %v; want %v", ok, err, c.want)
				}
				if n := srv.ListCalls(); n != 1 {
					t.Fatalf("one admission listed flintlock %d times, want 1", n)
				}
			})
		}
	}
}

// A transient listing error (flintlockd deleting a microVM) is not retried
// while the lock is held, where every other admission of the host waits: the
// admission returns at once with nothing admitted and nothing reserved, the
// lock is free, and the caller's own wait loop asks again.
func TestAdmitTransientListingErrorReleasesTheLock(t *testing.T) {
	for _, a := range admitOnes {
		t.Run(a.name, func(t *testing.T) {
			file := tempAdmission(t)
			pinMemTotal(t, 1050, nil) // room for one
			srv := flintlocktest.NewServer("")
			srv.FailList(errTransient)
			fl := dialFake(t, srv)
			start := time.Now()
			ok, err := a.admit(context.Background(), admitCfg(), fl)
			took := time.Since(start)
			if ok || !flintlock.IsTransient(err) {
				t.Fatalf("admitted %v, %v; want nothing with the transient error", ok, err)
			}
			// flintlock.List would wait 300 ms before its first retry.
			if n := srv.ListCalls(); n != 1 || took > 250*time.Millisecond {
				t.Fatalf("%d listings in %s under the lock; want 1, no retry wait", n, took)
			}
			lock, err := os.OpenFile(file+".lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("the admission lock is still held after the error: %v", err)
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			if ok, err := a.admit(context.Background(), admitCfg(), fl); !ok || err != nil {
				t.Fatalf("next admission = %v, %v; want admitted (nothing reserved by the failed one)", ok, err)
			}
		})
	}
}

// AdmitFits lets a caller decide with its own check (builders), given the
// memory other admissions reserved.
func TestAdmitFits(t *testing.T) {
	tempAdmission(t)
	pinMemTotal(t, 100000, nil)
	fl := dialFake(t, flintlocktest.NewServer(""))
	cfg := admitCfg()
	if n, _, err := Admit(context.Background(), cfg, fl, "pool-a"); n != 1 || err != nil {
		t.Fatalf("Admit = %d, %v", n, err)
	}
	var sawExtra int
	fits := func(_ context.Context, _ config.Config, _ *flintlock.Client, extraMB int) (bool, string, error) {
		sawExtra = extraMB
		return extraMB < 2000, "test", nil
	}
	bcfg := cfg
	bcfg.VM.MemoryMB = 8192
	if ok, _, err := AdmitFits(context.Background(), bcfg, fl, "bld-1", fits); !ok || err != nil || sawExtra != 1050 {
		t.Fatalf("AdmitFits = %v, %v, saw %d MB reserved; want true with pool-a's 1050", ok, err, sawExtra)
	}
	if ok, _, _ := AdmitFits(context.Background(), bcfg, fl, "bld-2", fits); ok || sawExtra != 1050+WithOverhead(8192) {
		t.Fatalf("AdmitFits = %v, saw %d MB; want refused with both reserved", ok, sawExtra)
	}
	down := errors.New("flintlock down")
	failing := func(context.Context, config.Config, *flintlock.Client, int) (bool, string, error) {
		return true, "", down
	}
	if ok, _, err := AdmitFits(context.Background(), bcfg, fl, "bld-3", failing); ok || !errors.Is(err, down) {
		t.Fatalf("AdmitFits with a failing check = %v, %v; want refused with its error", ok, err)
	}
}
