package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
)

// overheadPct is Firecracker's per-VM memory overhead on top of the guest RAM.
const overheadPct = 5

// memTotalMB is a variable only so tests can pin host memory; production
// never reassigns it.
var memTotalMB = host.MemTotalMB

// Capacity is the host memory microVMs may commit: MemTotal minus the reserve.
func Capacity(cfg config.Config) (int, error) {
	total, err := memTotalMB()
	if err != nil {
		return 0, err
	}
	return total - cfg.VM.HostReserveMB, nil
}

// CommittedMB sums the guest memory promised to existing microVMs.
// Guest RAM is allocated lazily, so free memory right now says nothing about
// what running VMs will use later; committed memory does.
func CommittedMB(ctx context.Context, fl *flintlock.Client) (int, error) {
	vms, err := fl.List(ctx)
	if err != nil {
		return 0, err
	}
	return committedMB(vms), nil
}

func committedMB(vms []*types.MicroVM) int {
	total := 0
	for _, v := range vms {
		switch v.GetStatus().GetState() {
		case types.MicroVMStatus_FAILED, types.MicroVMStatus_DELETING:
			continue
		}
		total += WithOverhead(int(v.GetSpec().GetMemoryInMb()))
	}
	return total
}

// Fits reports whether one more microVM of the configured size fits, given
// extraMB already promised but not yet visible in flintlock.
func Fits(ctx context.Context, cfg config.Config, fl *flintlock.Client, extraMB int) (bool, string, error) {
	capacity, err := Capacity(cfg)
	if err != nil {
		return true, "", err // cannot tell: do not block
	}
	committed, err := CommittedMB(ctx, fl)
	if err != nil {
		return false, "", err
	}
	need := WithOverhead(cfg.VM.MemoryMB)
	msg := fmt.Sprintf("committed %d MB + new %d MB, capacity %d MB", committed+extraMB, need, capacity)
	return committed+extraMB+need <= capacity, msg, nil
}

// WithOverhead is mb of guest memory plus Firecracker's per-VM overhead.
func WithOverhead(mb int) int { return mb * (100 + overheadPct) / 100 }

// AdmissionFile records the memory admitted to microVMs that flintlock does
// not list yet, for every process on the host that creates microVMs: the
// daemon (pool VMs, builders) and `executor prepare` (cold boots), which
// gitlab-runner starts as separate processes, many at once. Without it each
// admission lists flintlock before the others' creates land, all of them see
// the same free memory, and the host over-commits; the guest RAM is allocated
// lazily, so the kernel OOM-kills a microVM later. A variable for tests.
//
// AdmissionFile + ".lock" serializes admissions. The lock is held for exactly
// one flintlock call, a single try, and a small file write: never while a
// caller waits for room or creates its microVM (from then on the reservation
// stands for it), and never while a failed listing is retried, which waits
// seconds exactly when flintlockd is busy deleting microVMs.
var AdmissionFile = "/run/firerunner/admission.json"

// admissionTimeout bounds one admission, the wait for the lock included, and
// so how long any process holds the lock: the budget of one flintlock listing
// everywhere else in FireRunner (a variable for tests).
var admissionTimeout = 30 * time.Second

// lockPoll is how often a waiting admission tries the lock again. flock is not
// interruptible, so waiting is polling: the wait must end with the context.
const lockPoll = 10 * time.Millisecond

// ErrAdmissionBusy is the error of an admission that waited for the host-wide
// admission lock until its deadline: other admissions held it all along, and
// flintlock was never asked. It deliberately does not wrap
// context.DeadlineExceeded, which callers read as a flintlock call cut off.
var ErrAdmissionBusy = errors.New("other microVM admissions held the admission lock")

// reservation is memory admitted to one microVM that flintlock may not list
// yet. It lasts until flintlock lists the VM, the process that admitted it
// says its boot returned (Unreserve) or exits, or Until passes.
type reservation struct {
	ID  string `json:"id"`
	MB  int    `json:"mb"` // guest memory with overhead
	PID int    `json:"pid"`
	// Until bounds a reservation whose process lives on but never created
	// the VM: vm.boot_timeout plus one flintlock call after the admission.
	Until time.Time `json:"until"`
	// Seen are the uids flintlock listed under ID at admission time: a
	// builder's previous VM (same id) may still be deleting, and it is not
	// the new one.
	Seen []string `json:"seen,omitempty"`
}

// FitsFunc has Fits's signature. AdmitFits calls it with the memory that other
// admissions reserved as extraMB and with a client that answers the listing
// the admission took (flintlock.Client.Listed), so the check does not list
// flintlock again while the admission lock is held.
type FitsFunc func(ctx context.Context, cfg config.Config, fl *flintlock.Client, extraMB int) (bool, string, error)

// Admit decides, host-wide, how many of the microVMs ids (of cfg's size) may be
// created now, with a single flintlock listing, and reserves memory for them:
// the first n of ids may be created. Call Unreserve(id) for each once its boot
// returned. Errors admit nothing: flintlock down, the lock busy for
// admissionTimeout (ErrAdmissionBusy), and a transient listing error, which
// is not retried under the lock (the caller asks again, as when there is no
// room). Unknown host memory admits everything, like Fits.
func Admit(ctx context.Context, cfg config.Config, fl *flintlock.Client, ids ...string) (int, string, error) {
	return admit(ctx, cfg, fl, ids, nil)
}

// AdmitFits admits one microVM, id, when fits says it fits next to the memory
// other admissions reserved. Builders use it: their check is a test hook.
func AdmitFits(ctx context.Context, cfg config.Config, fl *flintlock.Client, id string, fits FitsFunc) (bool, string, error) {
	n, why, err := admit(ctx, cfg, fl, []string{id}, fits)
	return n == 1, why, err
}

func admit(ctx context.Context, cfg config.Config, fl *flintlock.Client, ids []string, fits FitsFunc) (int, string, error) {
	if len(ids) == 0 {
		return 0, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()
	unlock, err := lockAdmission(ctx)
	if err != nil {
		return 0, "", err
	}
	defer unlock()
	vms, err := fl.ListOnce(ctx)
	if err != nil {
		return 0, "", err
	}
	loaded := loadReservations()
	pending := stillPending(loaded, vms, time.Now())
	reserved := 0
	for _, r := range pending {
		reserved += r.MB
	}
	need := WithOverhead(cfg.VM.MemoryMB)
	n, why := 0, ""
	switch capacity, cerr := Capacity(cfg); {
	case fits != nil:
		ok, w, err := fits(ctx, cfg, fl.Listed(vms), reserved)
		if err != nil {
			return 0, w, err
		}
		if ok {
			n = 1
		}
		why = w
	case cerr != nil:
		n, why = len(ids), fmt.Sprintf("host memory unknown (%v), not limited", cerr)
	default:
		committed := committedMB(vms)
		n = max(0, min(len(ids), (capacity-committed-reserved)/need))
		why = fmt.Sprintf("committed %d MB, admitted and not created yet %d MB, %d MB each, capacity %d MB",
			committed, reserved, need, capacity)
	}
	if n == 0 && len(pending) == len(loaded) {
		return 0, why, nil
	}
	seen := uidsByID(vms)
	until := time.Now().Add(cfg.VM.BootTimeout + admissionTimeout)
	for _, id := range ids[:n] {
		pending = append(pending, reservation{ID: id, MB: need, PID: os.Getpid(), Until: until, Seen: seen[id]})
	}
	if err := saveReservations(pending); err != nil {
		return 0, why, fmt.Errorf("recording admitted memory: %w", err)
	}
	return n, why, nil
}

// Unreserve drops the reservation this process holds for id. Call it when the
// boot of id returned, whether or not the microVM was created: a created one
// is listed by flintlock from then on, a failed one never will be.
func Unreserve(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), admissionTimeout)
	defer cancel()
	unlock, err := lockAdmission(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	all := loadReservations()
	keep := slices.DeleteFunc(slices.Clone(all), func(r reservation) bool { return r.ID == id && r.PID == os.Getpid() })
	if len(keep) == len(all) {
		return nil
	}
	return saveReservations(keep)
}

// lockAdmission takes the host-wide admission lock, waiting at most until ctx
// ends: ErrAdmissionBusy at its deadline, the cancellation if the caller gave
// up. The returned function releases it.
func lockAdmission(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(AdmissionFile), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(AdmissionFile+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Closing the file releases the lock.
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w (%s)", ErrAdmissionBusy, f.Name())
			}
			return nil, fmt.Errorf("waiting for another microVM admission (%s): %w", f.Name(), ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// loadReservations reads AdmissionFile. It is only ever replaced by a rename,
// so an unreadable file can only be missing: nothing is reserved.
func loadReservations() []reservation {
	data, err := os.ReadFile(AdmissionFile)
	if err != nil {
		return nil
	}
	var out []reservation
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// saveReservations replaces AdmissionFile (a tmpfs: no fsync).
func saveReservations(rs []reservation) error {
	data, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	tmp := AdmissionFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, AdmissionFile)
}

// stillPending drops reservations whose microVM flintlock lists (it counts
// in committed memory now), whose process is gone or that expired.
func stillPending(rs []reservation, vms []*types.MicroVM, now time.Time) []reservation {
	listed := uidsByID(vms)
	var out []reservation
	for _, r := range rs {
		created := slices.ContainsFunc(listed[r.ID], func(uid string) bool { return !slices.Contains(r.Seen, uid) })
		if created || now.After(r.Until) || !processAlive(r.PID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func uidsByID(vms []*types.MicroVM) map[string][]string {
	out := map[string][]string{}
	for _, v := range vms {
		out[v.GetSpec().GetId()] = append(out[v.GetSpec().GetId()], v.GetSpec().GetUid())
	}
	return out
}

// processAlive reports whether pid runs; signal 0 only checks.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
