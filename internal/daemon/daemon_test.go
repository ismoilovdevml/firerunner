package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
)

func TestDecide(t *testing.T) {
	cfg := config.Default() // boot_timeout 3m, job_max_age 3h
	cases := []struct {
		name string
		f    vmFacts
		keep bool
	}{
		{"failed VM", vmFacts{State: "FAILED", Owned: true}, false},
		{"owned pool VM", vmFacts{State: "CREATED", Role: "pool", Owned: true, Age: time.Hour}, true},
		{"owned job VM older than max age", vmFacts{State: "CREATED", Role: "job", Owned: true, Age: 4 * time.Hour}, false},
		{"running job", vmFacts{State: "CREATED", Role: "job", Job: "job-1", Owned: true, Age: 20 * time.Minute}, true},
		{"pool VM from previous run", vmFacts{State: "CREATED", Role: "pool", Startup: true}, false},
		{"young orphan pool VM", vmFacts{State: "CREATED", Role: "pool", Age: time.Minute}, true},
		{"old orphan pool VM", vmFacts{State: "CREATED", Role: "pool", Age: 5 * time.Minute}, false},
		{"orphan pool VM while booting", vmFacts{State: "CREATED", Role: "pool", Age: 5 * time.Minute, Booting: 1}, true},
		{"job VM still booting", vmFacts{State: "PENDING", Role: "job", Job: "job-2", Age: 2 * time.Minute}, true},
		{"job VM without job", vmFacts{State: "CREATED", Role: "job", Job: "job-3", Age: 7 * time.Minute}, false},
		{"fresh run VM", vmFacts{State: "CREATED", Role: "run", Age: time.Hour}, true},
		{"abandoned run VM", vmFacts{State: "CREATED", Role: "run", Age: 4 * time.Hour}, false},
		{"foreign VM without labels", vmFacts{State: "CREATED", Age: 10 * time.Hour}, true},
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
		"smoke1": {"", ""}, "mvm-1": {"", ""},
	} {
		if r, j := RoleOf(id); r != want[0] || j != want[1] {
			t.Errorf("RoleOf(%q) = %q,%q want %v", id, r, j, want)
		}
	}
}
