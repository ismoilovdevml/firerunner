package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

func TestImageStatsTop(t *testing.T) {
	var s imageStats
	now := time.Now()
	for _, img := range []string{"node:22", "node:22", "node:22", "golang:1.26", "golang:1.26", "alpine:3", "once:1"} {
		s.note(img)
	}
	// Job variables set the image: nothing a shell or docker reads specially.
	for _, bad := range []string{"-v /:/host", "a b", "$(reboot)", "x;y", "img'q", ""} {
		s.note(bad)
		s.note(bad)
	}
	if got := s.top(5, nil, now); !slices.Equal(got, []string{"node:22", "golang:1.26"}) {
		t.Fatalf("top = %v, want the images used twice or more, most used first", got)
	}
	if got := s.top(1, nil, now); !slices.Equal(got, []string{"node:22"}) {
		t.Fatalf("top(1) = %v", got)
	}
	if got := s.top(5, []string{"node:22"}, now); !slices.Equal(got, []string{"golang:1.26"}) {
		t.Fatalf("top skipping pool.preload_images = %v", got)
	}
	if got := s.top(0, nil, now); got != nil {
		t.Fatalf("top(0) = %v, want none", got)
	}

	s.pullFailed("node:22", now)
	if got := s.top(5, nil, now); !slices.Equal(got, []string{"golang:1.26"}) {
		t.Fatalf("top after a failed pull = %v", got)
	}
	if got := s.top(5, nil, now.Add(failedImageFor+time.Minute)); !slices.Equal(got, []string{"node:22", "golang:1.26"}) {
		t.Fatalf("a failed image is tried again after a day: %v", got)
	}
}

// Only the last recentImages jobs count.
func TestImageStatsForgetsOldJobs(t *testing.T) {
	var s imageStats
	s.note("old:1")
	s.note("old:1")
	for range recentImages {
		s.note("new:1")
	}
	if got := s.top(5, nil, time.Now()); !slices.Equal(got, []string{"new:1"}) {
		t.Fatalf("top = %v, want only the recent jobs' image", got)
	}
}

func TestPreloadScript(t *testing.T) {
	s := preloadScript([]string{"sdk:1"}, []string{"node:22"})
	for _, want := range []string{
		"set -e; docker pull -q 'sdk:1'",
		"; set +e; docker pull -q 'node:22' >/dev/null 2>&1 || echo 'firerunner-preload-failed node:22'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if s := preloadScript([]string{"sdk:1"}, nil); strings.Contains(s, "set +e") {
		t.Fatalf("no images by use, yet:\n%s", s)
	}
	out := "sha256:abc\nfirerunner-preload-failed private.corp/app:1\n"
	if got := failedPulls(out); !slices.Equal(got, []string{"private.corp/app:1"}) {
		t.Fatalf("failedPulls = %v", got)
	}
}

// A job's prepare event feeds the image statistics; one that failed does not.
func TestPrepareEventNotesImage(t *testing.T) {
	d, _ := newTestDaemon(t)
	for range 2 {
		d.record(Event{Kind: "prepare", Source: "pool", OK: true, Image: "node:22"})
		d.record(Event{Kind: "prepare", Source: "cold", Image: "broken:1"})
	}
	if got := d.images.top(5, nil, time.Now()); !slices.Equal(got, []string{"node:22"}) {
		t.Fatalf("top = %v", got)
	}
}

// A pool VM pulls the most used images; one it cannot pull (private) is
// skipped from then on and the VM still joins the pool.
func TestPoolVMPreloadsByUse(t *testing.T) {
	d, _ := newTestDaemon(t)
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >> " + filepath.Join(bin, "ssh.log") + "\necho 'firerunner-preload-failed private.corp/app:1'\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldBoot := poolBoot
	t.Cleanup(func() { poolBoot = oldBoot })
	poolBoot = func(_ context.Context, _ config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
		return &vm.Instance{ID: id, UID: "u-" + id, IP: "10.200.0.5", HostKey: "ssh-ed25519 AAAA"}, nil
	}
	for range 3 {
		d.images.note("node:22")
		d.images.note("private.corp/app:1")
	}
	cfg := d.cfgSnapshot()
	cfg.Pool.PreloadTop = 2
	d.mu.Lock()
	d.booting++
	d.mu.Unlock()
	d.bootOne(context.Background(), cfg, "pool-x")

	log, _ := os.ReadFile(filepath.Join(bin, "ssh.log"))
	want := preloadCommand(nil, []string{"node:22", "private.corp/app:1"})
	if !strings.Contains(string(log), want) {
		t.Fatalf("pool VM did not pull the most used images:\n%s", log)
	}
	if got := d.images.top(5, nil, time.Now()); !slices.Equal(got, []string{"node:22"}) {
		t.Fatalf("after the failed pull, top = %v, want the private image skipped", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ready) != 1 {
		t.Fatalf("pool after the preload: %d ready, want 1", len(d.ready))
	}
}

// The preload runs in a unit of its own, so that it can be stopped whole.
func TestPreloadCommand(t *testing.T) {
	c := preloadCommand([]string{"sdk:1"}, nil)
	if !strings.HasPrefix(c, "systemd-run --unit="+preloadUnit+" ") || !strings.Contains(c, " --pipe --wait ") {
		t.Fatalf("preload not in a transient unit: %s", c)
	}
	if !strings.HasSuffix(c, " sh -c "+shellQuote(preloadScript([]string{"sdk:1"}, nil))) {
		t.Fatalf("unit does not run the script: %s", c)
	}
}

// A job claiming the VM mid-preload stops the pulls in the VM, not only ssh:
// they would go on taking the job's network.
func TestPreloadCancelStopsPulls(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stopped := 0
	old := stopPreload
	t.Cleanup(func() { stopPreload = old })
	stopPreload = func(config.Config, *vm.Instance) error { stopped++; return nil }

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	inst := &vm.Instance{ID: "pool-x", UID: "u", IP: "10.200.0.5", HostKey: "ssh-ed25519 AAAA"}
	start := time.Now()
	if _, err := preload(ctx, config.Config{}, inst, []string{"sdk:1"}, nil); err == nil {
		t.Fatal("cancelled preload reported success")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel did not end the preload")
	}
	if stopped != 1 {
		t.Fatalf("stopPreload called %d times, want 1", stopped)
	}

	// A preload that finishes on its own leaves nothing to stop.
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := preload(context.Background(), config.Config{}, inst, []string{"sdk:1"}, nil); err != nil || stopped != 1 {
		t.Fatalf("finished preload: err %v, stopPreload called %d times", err, stopped)
	}
}

// A pool refresh (POST /pool/refresh, after a new rootfs image under the same
// tag) replaces the whole pool, not only the ready VMs: a VM booted from the
// old image and still preloading, or still booting, does not join the pool
// once its boot ends, and no job is handed one meanwhile.
func TestPoolRefreshReplacesVMsStillBooting(t *testing.T) {
	stubPoolVM := func(t *testing.T, during func()) {
		t.Helper()
		oldBoot, oldStop := poolBoot, stopPreload
		t.Cleanup(func() { poolBoot, stopPreload = oldBoot, oldStop })
		stopPreload = func(config.Config, *vm.Instance) error { return nil }
		poolBoot = func(_ context.Context, _ config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
			if during != nil {
				during()
			}
			return &vm.Instance{ID: id, UID: "u-" + id, IP: "10.200.0.5", HostKey: "ssh-ed25519 AAAA"}, nil
		}
	}
	slowSSH := func(t *testing.T) {
		t.Helper()
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	boot := func(d *Daemon, cfg config.Config) chan struct{} {
		d.mu.Lock()
		d.booting++
		d.mu.Unlock()
		done := make(chan struct{})
		go func() { defer close(done); d.bootOne(context.Background(), cfg, "pool-x") }()
		return done
	}
	waitPreloading := func(t *testing.T, d *Daemon) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			d.mu.Lock()
			n := len(d.preloading)
			d.mu.Unlock()
			if n == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the pool VM never started its preload")
			}
		}
	}
	check := func(t *testing.T, d *Daemon, done chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the refresh did not stop the preload")
		}
		d.mu.Lock()
		ready, booting := len(d.ready), d.booting
		d.mu.Unlock()
		if ready != 0 || booting != 0 {
			t.Fatalf("after the refresh: %d ready, %d booting; want the old-image VM in neither", ready, booting)
		}
		if got := metricValue(t, d, "firerunner_vm_boot_failures_total", "preload"); got != 0 {
			t.Fatalf("a refresh counted as %v preload failures", got)
		}
	}

	t.Run("ready VMs and the gauge", func(t *testing.T) {
		d, srv := newTestDaemon(t)
		fp := fingerprint(d.cfg)
		d.ready = []*pooled{pooledVM("a", fp, 0), pooledVM("b", fp, 0)}
		d.metrics.poolReady.Set(2)
		d.drain(context.Background(), "refresh")
		srv.WaitDeleted(t, "a", 2*time.Second)
		srv.WaitDeleted(t, "b", 2*time.Second)
		if got := metricValue(t, d, "firerunner_pool_ready", ""); got != 0 {
			t.Fatalf("firerunner_pool_ready = %v after the refresh, want 0", got)
		}
	})
	t.Run("a preloading VM is not claimed after the refresh", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		cancelled := false
		d.preloading["warm"] = &preloadingVM{inst: &vm.Instance{ID: "pool-warm", UID: "warm"}, specID: fingerprint(d.cfg),
			cancel: func() { cancelled = true }}
		d.drain(context.Background(), "refresh")
		if !cancelled {
			t.Fatal("the refresh did not stop the old-image VM's preload")
		}
		if inst := d.Claim(); inst != nil {
			t.Fatalf("Claim = %s after the refresh, want no old-image VM", inst.ID)
		}
	})
	t.Run("a VM preloading during the refresh", func(t *testing.T) {
		slowSSH(t)
		stubPoolVM(t, nil)
		d, srv := newTestDaemon(t)
		cfg := d.cfgSnapshot()
		cfg.Pool.PreloadImages = []string{"sdk:1"}
		done := boot(d, cfg)
		waitPreloading(t, d)
		d.drain(context.Background(), "refresh")
		check(t, d, done)
		srv.WaitDeleted(t, "u-pool-x", 2*time.Second)
	})
	t.Run("a VM booting during the refresh", func(t *testing.T) {
		d, srv := newTestDaemon(t)
		stubPoolVM(t, func() { d.drain(context.Background(), "refresh") })
		done := boot(d, d.cfgSnapshot())
		check(t, d, done)
		srv.WaitDeleted(t, "u-pool-x", 2*time.Second)
	})
}
