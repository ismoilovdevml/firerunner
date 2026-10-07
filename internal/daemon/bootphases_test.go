package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// fakeClock only moves when a test step advances it, so phase times are exact.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// phaseSeries reads firerunner_builder_boot_phase_seconds by "phase/result":
// the number of observations and their sum. Asserting through the registry
// also checks the metric's name and label names.
func phaseSeries(t *testing.T, d *Daemon) map[string][2]float64 {
	t.Helper()
	mfs, err := d.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "firerunner_builder_boot_phase_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var names, vals []string
			for _, l := range m.GetLabel() {
				names = append(names, l.GetName())
				vals = append(vals, l.GetValue())
			}
			if strings.Join(names, ",") != "phase,result" {
				t.Fatalf("labels %v, want phase,result", names)
			}
			h := m.GetHistogram()
			out[strings.Join(vals, "/")] = [2]float64{float64(h.GetSampleCount()), h.GetSampleSum()}
		}
	}
	return out
}

// phaseCount is the number of observations of one phase and result; unlike
// phaseSeries it may run outside the test's goroutine.
func phaseCount(d *Daemon, phase, result string) uint64 {
	m := &dto.Metric{}
	if err := d.metrics.builderBootPhase.WithLabelValues(phase, result).(prometheus.Metric).Write(m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// Every phase and result is a series before the first boot, so dashboards show
// 0 instead of "No data".
func TestBuilderBootPhaseSeriesPreCreated(t *testing.T) {
	d, _ := newTestDaemon(t)
	var got []string
	for k := range phaseSeries(t, d) {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{
		"admit/failed", "admit/ok",
		"buildkitd/failed", "buildkitd/ok",
		"restore/failed", "restore/legacy", "restore/missing", "restore/ok", "restore/stale",
		"save_wait/failed", "save_wait/none", "save_wait/ok", "save_wait/timeout",
		"vm/failed", "vm/ok",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("series %v, want %v", got, want)
	}
}

// A builder boot records each phase it ran with its own time, including the
// one it failed in; the phases add up to the boot's total, which one log line
// shows with every phase. Each step of the boot moves a fake clock by a
// known amount.
func TestBuilderBootPhases(t *testing.T) {
	const (
		admitT   = 3 * time.Second
		vmT      = 7 * time.Second
		saveT    = 30 * time.Second
		restoreT = 20 * time.Second
		setupT   = 5 * time.Second
	)
	sshDrop := func(t *testing.T) error { return fmt.Errorf("stream: %w", exitErr(t, 255)) }
	refused := func(t *testing.T) error { return fmt.Errorf("tar: unexpected EOF: %w", exitErr(t, 1)) }
	for _, tc := range []struct {
		name        string
		cache       bool // a saved cache exists
		unreadable  bool // the saved cache cannot be opened
		save        bool // the previous builder is copying its cache out
		stopInSave  bool // the daemon stops while the save is pending
		stopInSetup bool // the daemon stops while buildkitd starts, which still succeeds
		noFit       bool // no host memory
		noEntry     bool // the builder was removed before its VM booted
		dropInLoad  bool // `builder rm` drops the cache while it loads
		bootErr     error
		loadErr     func(*testing.T) error
		wipeErr     error
		setupErr    error
		want        []string // phase/result/seconds, in order
		result      string
		total       time.Duration
	}{
		{name: "ready after a save and a restore", cache: true, save: true,
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/ok/30", "restore/ok/20", "buildkitd/ok/5"},
			result: "ready", total: admitT + vmT + saveT + restoreT + setupT},
		{name: "ready without a saved cache",
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/missing/0", "buildkitd/ok/5"},
			result: "ready", total: admitT + vmT + setupT},
		{name: "restore fails", cache: true, save: true, loadErr: sshDrop,
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/ok/30", "restore/failed/20"},
			result: "failed", total: admitT + vmT + saveT + restoreT},
		{name: "VM create fails", cache: true, bootErr: errors.New("flintlock: create failed"),
			want:   []string{"admit/ok/3", "vm/failed/7"},
			result: "failed", total: admitT + vmT},
		{name: "no host memory", noFit: true,
			want:   []string{"admit/failed/3"},
			result: "failed", total: admitT},
		{name: "buildkitd does not start", cache: true, setupErr: fmt.Errorf("starting buildkitd: %w", errBuildkitDown),
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/ok/20", "buildkitd/failed/5"},
			result: "failed", total: admitT + vmT + restoreT + setupT},
		{name: "builder removed while its VM booted", noEntry: true,
			want:   []string{"admit/ok/3", "vm/ok/7"},
			result: "abandoned", total: admitT + vmT},
		{name: "daemon stops while buildkitd starts", cache: true, stopInSetup: true,
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/ok/20", "buildkitd/ok/5"},
			result: "abandoned", total: admitT + vmT + restoreT + setupT},
		{name: "daemon stops while a save is pending", save: true, stopInSave: true,
			// buildkitd then fails at once on the ended context, like ssh does.
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/failed/30", "restore/missing/0", "buildkitd/failed/0"},
			result: "failed", total: admitT + vmT + saveT},
		{name: "refused copy, starts empty", cache: true, loadErr: refused,
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/failed/20", "buildkitd/ok/5"},
			result: "ready", total: admitT + vmT + restoreT + setupT},
		{name: "unreadable copy, starts empty", unreadable: true,
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/failed/0", "buildkitd/ok/5"},
			result: "ready", total: admitT + vmT + setupT},
		{name: "cache dropped while it loaded, wipe fails", cache: true, dropInLoad: true, wipeErr: errors.New("ssh: no route"),
			want:   []string{"admit/ok/3", "vm/ok/7", "save_wait/none/0", "restore/failed/20"},
			result: "failed", total: admitT + vmT + restoreT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBuilders(t, func(string) bool { return true })
			clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			ctx, stop := context.WithCancel(context.Background())
			t.Cleanup(stop)
			var dp *Daemon
			dir := stubBuilderCache(t, nil, nil, func(_ context.Context, _ config.Config, _ *vm.Instance, r io.Reader) error {
				clock.Advance(restoreT)
				if _, err := io.ReadAll(r); err != nil {
					return err
				}
				if tc.dropInLoad {
					dp.mu.Lock()
					dp.builders["7"].dropped = true
					dp.mu.Unlock()
				}
				if tc.loadErr != nil {
					return tc.loadErr(t)
				}
				return nil
			})
			if tc.wipeErr != nil {
				builderCacheWipe = func(context.Context, config.Config, *vm.Instance) error { return tc.wipeErr }
			}
			stubBoot(t, nil)
			var admitted sync.Once
			builderFits = func(context.Context, config.Config, *flintlock.Client, int) (bool, string, error) {
				admitted.Do(func() { clock.Advance(admitT) }) // admission asks more than once
				return !tc.noFit, "memory", nil
			}
			oldFitWait := builderFitWait
			builderFitWait = 0 // a boot without memory fails at the first check
			t.Cleanup(func() { builderFitWait = oldFitWait })
			builderVMBoot = func(_ context.Context, _ config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
				clock.Advance(vmT)
				if tc.bootErr != nil {
					return nil, tc.bootErr
				}
				return &vm.Instance{ID: id, UID: "uid-" + id, IP: "10.200.0.77"}, nil
			}
			builderSetup = func(ctx context.Context, _ config.Config, _ *vm.Instance, _ *builderCreds) error {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("starting buildkitd: %w", err)
				}
				clock.Advance(setupT)
				if tc.stopInSetup {
					stop()
				}
				return tc.setupErr
			}
			d, _ := newTestDaemon(t)
			dp = d
			d.now = clock.Now
			logs := &logBuffer{}
			d.log = slog.New(slog.NewJSONHandler(logs, nil))
			if tc.cache {
				writeFile(t, filepath.Join(dir, "7.tar"), saved("warm"), time.Minute)
			}
			if tc.unreadable {
				// A symlink to itself: open fails (ELOOP) but not with
				// ErrNotExist, even for root, unlike a file with mode 0.
				if err := os.Symlink("7.tar", filepath.Join(dir, "7.tar")); err != nil {
					t.Fatal(err)
				}
			}
			restoreResults := []string{"ok", "legacy", "missing", "stale", "failed"}
			countedBefore := map[string]float64{}
			for _, r := range restoreResults {
				countedBefore[r] = cacheCount(d, "restore", r)
			}
			if !tc.noEntry {
				d.Builder("7", true) // entry with credentials; builderBoot is stubbed
			}
			saveDone := make(chan struct{})
			if tc.save {
				// The copy finishes 30 s after the VM booted: once the vm
				// phase is observed, the next clock read ends save_wait.
				save := &cacheSave{done: make(chan struct{}), uid: "uid-old", start: time.Now()}
				d.saving["7"] = save
				go func() {
					defer close(saveDone)
					for deadline := time.Now().Add(5 * time.Second); phaseCount(d, "vm", "ok") == 0; time.Sleep(time.Millisecond) {
						if time.Now().After(deadline) {
							t.Error("vm phase never observed")
							break
						}
					}
					clock.Advance(saveT)
					if tc.stopInSave {
						stop()
						return
					}
					d.mu.Lock()
					delete(d.saving, "7")
					d.mu.Unlock()
					close(save.done)
				}()
			} else {
				close(saveDone)
			}
			d.bootBuilder(ctx, d.cfg, "7")
			<-saveDone
			d.bg.Wait()

			// The metric: one observation per phase that ran, none elsewhere.
			series := phaseSeries(t, d)
			var sum float64
			wantObs := map[string]float64{}
			for _, w := range tc.want {
				parts := strings.Split(w, "/")
				var secs float64
				if _, err := fmt.Sscan(parts[2], &secs); err != nil {
					t.Fatal(err)
				}
				wantObs[parts[0]+"/"+parts[1]] = secs
				sum += secs
			}
			for k, got := range series {
				want, ran := wantObs[k]
				switch {
				case ran && (got[0] != 1 || got[1] != want):
					t.Errorf("%s: %v observations, %v s; want 1, %v s", k, got[0], got[1], want)
				case !ran && got[0] != 0:
					t.Errorf("%s: %v observations of a phase that did not run or ended otherwise", k, got[0])
				}
			}
			if sum != tc.total.Seconds() {
				t.Errorf("phases add up to %v s, boot took %v", sum, tc.total)
			}

			// The log line: the same phases and the total.
			var line map[string]any
			for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var m map[string]any
				if err := json.Unmarshal([]byte(l), &m); err != nil {
					t.Fatalf("log line %q: %v", l, err)
				}
				if m["msg"] == "builder boot phases" {
					if line != nil {
						t.Fatal("more than one phase line for one boot")
					}
					line = m
				}
			}
			if line == nil {
				t.Fatalf("no phase line in:\n%s", logs.String())
			}
			if line["project"] != "7" || line["result"] != tc.result || line["total_s"] != tc.total.Seconds() {
				t.Errorf("phase line %v; want project 7, result %s, total_s %v", line, tc.result, tc.total.Seconds())
			}
			var logged float64
			for _, w := range tc.want {
				parts := strings.Split(w, "/")
				secs, ok := line[parts[0]+"_s"].(float64)
				if !ok {
					t.Errorf("phase line has no %s_s: %v", parts[0], line)
				}
				logged += secs
				if r, has := line[parts[0]]; parts[1] != "ok" && r != parts[1] || parts[1] == "ok" && has {
					t.Errorf("phase line %s = %v; want result %s (shown only when not ok)", parts[0], r, parts[1])
				}
			}
			if math.Abs(logged-tc.total.Seconds()) > 0.05*float64(len(tc.want)) {
				t.Errorf("logged phases add up to %v s, total_s %v", logged, line["total_s"])
			}
			for _, p := range builderPhaseResults {
				if _, has := line[p.phase+"_s"]; has && !slices.ContainsFunc(tc.want, func(w string) bool { return strings.HasPrefix(w, p.phase+"/") }) {
					t.Errorf("phase line has %s_s for a phase that did not run", p.phase)
				}
			}
			wantFailed := ""
			if tc.result == "failed" {
				wantFailed = strings.Split(tc.want[len(tc.want)-1], "/")[0]
			}
			if got, _ := line["failed_phase"].(string); got != wantFailed {
				t.Errorf("failed_phase = %q, want %q", got, wantFailed)
			}
			if tc.unreadable {
				// builder_cache_total does not count a copy it could not open.
				for _, r := range restoreResults {
					if got := cacheCount(d, "restore", r); got != countedBefore[r] {
						t.Errorf("builder_cache_total{restore,%s} = %v, was %v before an unreadable copy", r, got, countedBefore[r])
					}
				}
			}
			if tc.dropInLoad && cacheCount(d, "restore", "ok") != 1 {
				t.Errorf("restore ok = %v: the counter counts the load, the phase the failed wipe", cacheCount(d, "restore", "ok"))
			}
			if n := metricValue(t, d, "firerunner_vm_boot_failures_total", "builder"); (n == 1) != (tc.result == "failed") {
				t.Errorf("builder boot failures = %v for a %s boot", n, tc.result)
			}
		})
	}
}

// The total is the sum of the phases: time after the last one (the lock,
// builders.json, deleting a failed VM) is not added, and the first finish
// fixes the result.
func TestBootPhasesTotalIsTheSumOfPhases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phases []time.Duration
		result string
	}{
		{"ready", []time.Duration{3 * time.Second, 7 * time.Second, 5 * time.Second}, "ready"},
		{"failed in the first phase", []time.Duration{4 * time.Second}, "failed"},
		{"no phase ended", nil, "abandoned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			p := newBootPhases(clock.Now, NewMetrics().builderBootPhase)
			var sum time.Duration
			for i, d := range tc.phases {
				clock.Advance(d)
				p.end(builderPhaseResults[i].phase, "ok")
				sum += d
			}
			clock.Advance(time.Minute) // bookkeeping after the last phase
			p.finish(tc.result)
			clock.Advance(time.Minute)
			p.finish("abandoned")
			if p.result != tc.result || p.total != sum {
				t.Fatalf("result %s, total %v; want %s, %v", p.result, p.total, tc.result, sum)
			}
		})
	}
}

// A previous builder's copy that outlasts builderSaveWait ends save_wait as a
// timeout, and the boot goes on.
func TestBuilderBootSaveWaitTimeout(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	stubBuilderCache(t, nil, nil, nil)
	stubBoot(t, nil)
	old := builderSaveWait
	builderSaveWait = 0
	t.Cleanup(func() { builderSaveWait = old })
	d, _ := newTestDaemon(t)
	d.Builder("7", true)
	d.saving["7"] = &cacheSave{done: make(chan struct{}), uid: "uid-old", start: time.Now()}
	d.bootBuilder(context.Background(), d.cfg, "7")
	series := phaseSeries(t, d)
	if series["save_wait/timeout"][0] != 1 || series["save_wait/ok"][0] != 0 || series["buildkitd/ok"][0] != 1 {
		t.Fatalf("save_wait timeout %v, ok %v, buildkitd ok %v; want 1, 0, 1",
			series["save_wait/timeout"][0], series["save_wait/ok"][0], series["buildkitd/ok"][0])
	}
}
