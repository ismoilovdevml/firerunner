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
	if !strings.Contains(string(log), "docker pull -q 'node:22'") || !strings.Contains(string(log), "docker pull -q 'private.corp/app:1'") {
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
