package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// drainCall sends one request to the daemon's API and decodes the drain
// status it answers; code is the HTTP status.
func drainCall(t *testing.T, d *Daemon, method string) (DrainStatus, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.apiHandler().ServeHTTP(rec, httptest.NewRequest(method, "/drain", nil))
	var st DrainStatus
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("%s /drain: %v: %q", method, err, rec.Body)
		}
	}
	return st, rec.Code
}

// countPoolBoots replaces the pool boot with one that only counts.
func countPoolBoots(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := poolBoot
	poolBoot = func(context.Context, config.Config, *flintlock.Client, string, map[string]string) (*vm.Instance, error) {
		n.Add(1)
		return nil, errors.New("stub: no microVMs")
	}
	t.Cleanup(func() { poolBoot = old })
	return &n
}

// While drained the pool is not refilled, so a host being emptied for
// maintenance does not boot VMs nobody will take; undrain refills at once.
func TestDrainStopsPoolRefill(t *testing.T) {
	boots := countPoolBoots(t)
	d, _ := newTestDaemon(t) // pool.size 2, nothing ready

	if st, code := drainCall(t, d, http.MethodPost); code != http.StatusOK || !st.Drained || st.Since.IsZero() {
		t.Fatalf("POST /drain = %+v, %d", st, code)
	}
	d.refill(context.Background())
	d.bg.Wait()
	if n := boots.Load(); n != 0 {
		t.Fatalf("drained daemon booted %d pool VMs", n)
	}

	if st, code := drainCall(t, d, http.MethodDelete); code != http.StatusOK || st.Drained {
		t.Fatalf("DELETE /drain = %+v, %d", st, code)
	}
	select {
	case <-d.refillNow:
	default:
		t.Fatal("undrain did not wake the refill")
	}
	d.refill(context.Background())
	d.bg.Wait()
	if n := boots.Load(); n != 2 {
		t.Fatalf("undrained daemon booted %d pool VMs, want 2", n)
	}
}

// A drained daemon still hands out the pool VMs it has: a job gitlab-runner
// took just before the drain should not cold-boot.
func TestDrainedDaemonStillHandsOutReadyVMs(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.ready = []*pooled{pooledVM("a", fingerprint(d.cfg), 0)}
	drainCall(t, d, http.MethodPost)
	if inst := d.Claim(); inst == nil || inst.UID != "a" {
		t.Fatalf("Claim while drained = %v, want a", inst)
	}
}

// While drained no builder boots; jobs build without one. A builder that is
// already there keeps serving its project's jobs.
func TestDrainStopsBuilderBoots(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var boots atomic.Int32
	builderBoot = func(*Daemon, context.Context, config.Config, string) { boots.Add(1) }
	d, _ := newTestDaemon(t)
	d.builders["7"] = readyBuilder("7", 20001, time.Hour, builderSpec(d.cfg))
	drainCall(t, d, http.MethodPost)

	if got := d.Builder("8", true).State; got != BuilderNone {
		t.Fatalf("drained, project without a builder: %s, want none", got)
	}
	if got := d.Builder("7", true); got.State != BuilderReady || got.Key == "" {
		t.Fatalf("drained, existing builder: %+v, want it ready", got)
	}
	d.bg.Wait()
	if n := boots.Load(); n != 0 {
		t.Fatalf("drained daemon booted %d builders", n)
	}

	drainCall(t, d, http.MethodDelete)
	if got := d.Builder("8", true).State; got != BuilderBooting {
		t.Fatalf("undrained, project without a builder: %s, want booting", got)
	}
}

// The drain outlives a daemon restart (firerunner upgrade restarts it while
// the host is drained), and undrain is not forgotten by one either.
func TestDrainSurvivesRestart(t *testing.T) {
	d, _ := newTestDaemon(t)
	first, _ := drainCall(t, d, http.MethodPost)

	restart := func() *Daemon {
		t.Helper()
		d2, err := New(d.cfgPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d2.fl.Close() })
		return d2
	}
	d2 := restart()
	st, _ := drainCall(t, d2, http.MethodGet)
	if !st.Drained || !st.Since.Equal(first.Since) {
		t.Fatalf("after a restart: %+v, want drained since %s", st, first.Since)
	}
	if got := metricValue(t, d2, "firerunner_drained", ""); got != 1 {
		t.Fatalf("firerunner_drained = %v after a restart while drained, want 1", got)
	}

	drainCall(t, d2, http.MethodDelete)
	if st, _ := drainCall(t, restart(), http.MethodGet); st.Drained {
		t.Fatalf("after undrain and a restart: %+v, want not drained", st)
	}
}

// A second drain keeps the time the first one started: drain is run again
// to keep waiting, and the time says how long the host has taken no jobs.
func TestDrainAgainKeepsItsStart(t *testing.T) {
	d, _ := newTestDaemon(t)
	first, _ := drainCall(t, d, http.MethodPost)
	time.Sleep(10 * time.Millisecond)
	again, _ := drainCall(t, d, http.MethodPost)
	if !again.Since.Equal(first.Since) {
		t.Fatalf("second drain since %s, want the first's %s", again.Since, first.Since)
	}
}

func TestDrainedMetric(t *testing.T) {
	d, _ := newTestDaemon(t)
	for _, c := range []struct {
		method string
		want   float64
	}{{http.MethodGet, 0}, {http.MethodPost, 1}, {http.MethodGet, 1}, {http.MethodDelete, 0}} {
		drainCall(t, d, c.method)
		if got := metricValue(t, d, "firerunner_drained", ""); got != c.want {
			t.Fatalf("after %s /drain: firerunner_drained = %v, want %v", c.method, got, c.want)
		}
	}
}

// The drain status says what the host still runs: jobs (their state files,
// as firerunner_jobs_running), pool VMs and builders still booting.
func TestDrainStatusCountsWhatStillRuns(t *testing.T) {
	dir := tempJobStates(t)
	for _, f := range []string{"job-1.json", "job-2.json"} {
		if err := vm.SaveJobState(filepath.Join(dir, f), &vm.JobState{}); err != nil {
			t.Fatal(err)
		}
	}
	stubBuilders(t, func(string) bool { return true })
	d, _ := newTestDaemon(t)
	d.booting = 1
	d.builders["7"] = readyBuilder("7", 20001, 0, builderSpec(d.cfg))
	d.builders["8"] = &builder{Project: "8", Port: 20002} // still booting

	st, code := drainCall(t, d, http.MethodGet)
	if code != http.StatusOK || st.Drained || st.JobsRunning != 2 || st.PoolBooting != 1 || st.BuildersBooting != 1 {
		t.Fatalf("GET /drain = %+v, %d; want not drained, 2 jobs, 1 pool VM and 1 builder booting", st, code)
	}
}

// A drain that cannot be recorded is refused: a daemon restart would
// otherwise end it silently. An undrain that cannot remove the record is
// refused too: a restart would bring the drain back.
func TestDrainRecordFailures(t *testing.T) {
	block := func(t *testing.T, d *Daemon) {
		t.Helper()
		// A directory with something in it: neither replaced nor removed,
		// also not by root.
		if err := os.MkdirAll(filepath.Join(d.drainFile(), "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("drain", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		block(t, d)
		if _, code := drainCall(t, d, http.MethodPost); code != http.StatusInternalServerError {
			t.Fatalf("POST /drain with an unwritable record: %d, want 500", code)
		}
		if st, _ := drainCall(t, d, http.MethodGet); st.Drained {
			t.Fatal("drained without a record")
		}
	})
	t.Run("undrain", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		drainCall(t, d, http.MethodPost)
		if err := os.Remove(d.drainFile()); err != nil {
			t.Fatal(err)
		}
		block(t, d)
		if _, code := drainCall(t, d, http.MethodDelete); code != http.StatusInternalServerError {
			t.Fatalf("DELETE /drain with an unremovable record: %d, want 500", code)
		}
		if st, _ := drainCall(t, d, http.MethodGet); !st.Drained {
			t.Fatal("undrained though the record stays")
		}
	})
}

// A record that cannot be read still means drained: it was written by a
// drain, and starting jobs' VMs on a host being serviced is the worse
// mistake. Its time is then the file's.
func TestUnreadableDrainRecordIsADrain(t *testing.T) {
	d, _ := newTestDaemon(t)
	if err := os.WriteFile(d.drainFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d2, err := New(d.cfgPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.fl.Close() })
	fi, err := os.Stat(d.drainFile())
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := drainCall(t, d2, http.MethodGet); !st.Drained || !st.Since.Equal(fi.ModTime()) {
		t.Fatalf("unreadable record: %+v, want drained since %s", st, fi.ModTime())
	}
}

// The client speaks the same API.
func TestClientDrain(t *testing.T) {
	d, _ := newTestDaemon(t)
	c := serveAPI(t, d)
	ctx := context.Background()

	if st, err := c.SetDrain(ctx, true); err != nil || !st.Drained {
		t.Fatalf("SetDrain(true) = %+v, %v", st, err)
	}
	if st, err := c.DrainStatus(ctx); err != nil || !st.Drained {
		t.Fatalf("DrainStatus = %+v, %v", st, err)
	}
	if st, err := c.SetDrain(ctx, false); err != nil || st.Drained {
		t.Fatalf("SetDrain(false) = %+v, %v", st, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.DrainStatus(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("DrainStatus with a cancelled context: %v, want context.Canceled", err)
	}
}
