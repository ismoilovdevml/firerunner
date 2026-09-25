package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

func TestFinishEventsCountFireRunnerFailuresApart(t *testing.T) {
	d, _ := newTestDaemon(t)
	for _, e := range []Event{
		{Kind: "finish", Result: ResultSuccess},
		{Kind: "finish", Result: "failed"}, // an executor older than the result field
		{Kind: "finish", Result: ResultScriptFailure},
		{Kind: "finish", Result: ResultSystemFailure, Reason: "ssh_lost"},
		{Kind: "finish", Result: ResultSystemFailure, Reason: "ssh_lost"},
		{Kind: "finish", Result: ResultSystemFailure, Reason: "made-up"},
		{Kind: "finish", Result: "bogus"},
	} {
		d.record(e)
	}
	for _, c := range []struct {
		result, reason string
		want           float64
	}{
		{ResultSuccess, "none", 1},
		{ResultScriptFailure, "none", 2},
		{ResultSystemFailure, "ssh_lost", 2},
		{ResultSystemFailure, "other", 2}, // unknown reasons and results never become new series
	} {
		if got := value(t, d.metrics.jobs.WithLabelValues(c.result, c.reason)); got != c.want {
			t.Errorf("jobs{%s,%s} = %v, want %v", c.result, c.reason, got, c.want)
		}
	}
	if n := series(t, d.metrics.jobs); n != 2+len(FailureReasons) {
		t.Errorf("jobs has %d series, want %d (pre-created only)", n, 2+len(FailureReasons))
	}
}

func TestPrepareEvents(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.record(Event{Kind: "prepare", Source: "cold", OK: true, Seconds: 16, WaitSeconds: 3, BootSeconds: 12.5})
	d.record(Event{Kind: "prepare", Source: "cold", OK: true, Seconds: 14}) // older executor
	d.record(Event{Kind: "prepare", Source: "pool", OK: true, Seconds: 0.2})
	d.record(Event{Kind: "prepare", Source: "cold", Reason: "canceled"})
	d.record(Event{Kind: "prepare", Source: "cold", Reason: "admission_timeout", WaitSeconds: 180})
	d.record(Event{Kind: "prepare", Source: "cold", Reason: "vm_boot"})
	d.record(Event{Kind: "prepare", Source: "cold"})                                       // older executor: failure without reason
	d.record(Event{Kind: "prepare", Source: "pool", OK: true, Seconds: 5, WaitSeconds: 4}) // took a pool VM while waiting
	d.record(Event{Kind: "pool_vm_dead", VM: "pool-abc"})

	boot := sampleSum(t, d, "firerunner_vm_boot_seconds", "cold")
	if boot.count != 2 || boot.sum != 26.5 {
		t.Errorf("cold boot seconds count=%v sum=%v, want 2 and 26.5 (boot time, not the whole prepare)", boot.count, boot.sum)
	}
	if got := value(t, d.metrics.bootFailures.WithLabelValues("cold")); got != 2 {
		t.Errorf("cold boot failures = %v, want 2 (cancelled jobs and memory timeouts are not boot failures)", got)
	}
	if got := value(t, d.metrics.bootFailures.WithLabelValues("pool_dead")); got != 1 {
		t.Errorf("pool_dead = %v, want 1", got)
	}
	// Only jobs without a ready pool VM waited for memory: a pool hit is not a 0 s wait.
	wait := sampleSum(t, d, "firerunner_job_admission_wait_seconds", "")
	if wait.count != 7 || wait.sum != 187 {
		t.Errorf("admission wait count=%v sum=%v, want 7 and 187 (cold prepares and the pool VM taken while waiting)", wait.count, wait.sum)
	}
}

// Only prepare events carry a source: finish and pool_vm_dead lines must not
// claim "cold", and a prepare from an executor that sent none counts as cold.
func TestEventSourceOnlyForPrepare(t *testing.T) {
	d, _ := newTestDaemon(t)
	logs := captureLog(d)
	d.record(Event{Kind: "finish", Result: ResultSuccess, Job: "11", VM: "pool-abc"})
	d.record(Event{Kind: "pool_vm_dead", Job: "12", VM: "pool-def"})
	d.record(Event{Kind: "prepare", OK: true, Seconds: 9, Job: "13"})
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		isPrepare := strings.Contains(line, "kind=prepare")
		if strings.Contains(line, "source=cold") != isPrepare {
			t.Errorf("source in %q", line)
		}
	}
	if got := sampleSum(t, d, "firerunner_job_prepare_seconds", "cold"); got.count != 1 {
		t.Errorf("prepare without a source counted as cold %v times, want 1", got.count)
	}
}

type histo struct{ count, sum float64 }

func sampleSum(t *testing.T, d *Daemon, name, label string) histo {
	t.Helper()
	mfs, err := d.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if label != "" && (len(m.GetLabel()) == 0 || m.GetLabel()[0].GetValue() != label) {
				continue
			}
			return histo{float64(m.GetHistogram().GetSampleCount()), m.GetHistogram().GetSampleSum()}
		}
	}
	return histo{}
}

func TestHealthzFailsWhenTheLoopIsStuck(t *testing.T) {
	d, _ := newTestDaemon(t)
	h := d.metricsHandler()
	get := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}
	d.tick() // Run does this before its startup work
	if code := get(); code != http.StatusOK {
		t.Fatalf("healthz at start = %d", code)
	}
	d.lastTick.Store(time.Now().Add(-stuckAfter - time.Minute).Unix())
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("healthz when startup hangs = %d, want 503", code)
	}
	d.tick()
	if code := get(); code != http.StatusOK {
		t.Fatalf("healthz after a pass = %d", code)
	}
	d.lastTick.Store(time.Now().Add(-stuckAfter - time.Minute).Unix())
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a stuck loop = %d, want 503", code)
	}
}

func TestCapacityByRoleAndLeases(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := d.cfgSnapshot()
	cfg.Network.LeasesFile = filepath.Join(t.TempDir(), "leases")
	future, past := time.Now().Add(10*time.Minute).Unix(), time.Now().Add(-time.Minute).Unix()
	leases := fmt.Sprintf("%d %s 10.200.0.11 * *\n%d aa:fc:de:ad:be:ef 10.200.0.12 * *\n%d %s 10.200.0.13 * *\n",
		future, vm.MAC("job-7"), future, past, vm.MAC("pool-1"))
	if err := os.WriteFile(cfg.Network.LeasesFile, []byte(leases), 0o644); err != nil {
		t.Fatal(err)
	}
	mvm := func(id string, mb int32, state types.MicroVMStatus_MicroVMState) *types.MicroVM {
		uid := id + "-uid"
		return &types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid, MemoryInMb: mb},
			Status: &types.MicroVMStatus{State: state}}
	}
	d.recordCapacity(cfg, []*types.MicroVM{
		mvm("job-7", 2048, types.MicroVMStatus_CREATED),
		mvm("pool-1", 2048, types.MicroVMStatus_CREATED),
		mvm("bld-3", 8192, types.MicroVMStatus_CREATED),
		mvm("pool-2", 2048, types.MicroVMStatus_DELETING), // no longer committed
		mvm("other", 1024, types.MicroVMStatus_CREATED),
	})
	mb := func(role string) float64 {
		return value(t, d.metrics.memCommitted.WithLabelValues(role)) / 1024 / 1024
	}
	if mb("job") != float64(vm.WithOverhead(2048)) || mb("pool") != float64(vm.WithOverhead(2048)) ||
		mb("builder") != float64(vm.WithOverhead(8192)) || mb("other") != float64(vm.WithOverhead(1024)) {
		t.Errorf("committed MB job=%v pool=%v builder=%v other=%v", mb("job"), mb("pool"), mb("builder"), mb("other"))
	}
	live := value(t, d.metrics.dhcpLeases.WithLabelValues("live"))
	stale := value(t, d.metrics.dhcpLeases.WithLabelValues("stale"))
	if live != 1 || stale != 1 {
		t.Errorf("leases live=%v stale=%v, want 1 and 1 (the expired one is not counted)", live, stale)
	}
}

// value reads a counter or gauge.
func value(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		t.Fatal(err)
	}
	if c := out.GetCounter(); c != nil {
		return c.GetValue()
	}
	return out.GetGauge().GetValue()
}

// series counts the series a collector exposes.
func series(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

// A thin pool usage that cannot be read is absent, not the last value read:
// a frozen gauge would keep FireRunnerThinPoolFull from ever firing. The
// failure is logged at Warn once, not on every 15 s pass.
func TestThinPoolUsageUnknownWhenLvsFails(t *testing.T) {
	d, _ := newTestDaemon(t)
	logs := captureLog(d)
	var lvsErr error
	old := thinPoolUsage
	thinPoolUsage = func(context.Context) (float64, float64, error) { return 50, 10, lvsErr }
	t.Cleanup(func() { thinPoolUsage = old })

	d.collectHost(context.Background())
	if got := value(t, d.metrics.thinPool.WithLabelValues("data")); got != 0.5 {
		t.Fatalf("thin pool data = %v, want 0.5", got)
	}
	lvsErr = errors.New("lvs: signal: killed")
	d.collectHost(context.Background())
	d.collectHost(context.Background())
	if n := series(t, d.metrics.thinPool); n != 0 {
		t.Fatalf("thin pool gauge kept %d series after lvs failed; want none (unknown)", n)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for two failed passes, want 1:\n%s", n, logs)
	}
}

// firerunner_orphans_deleted_total{reason} takes short slugs only, all of
// them shown (as 0) before the first delete.
func TestOrphanReasonsAreSlugs(t *testing.T) {
	cfg := config.Default()
	slug := regexp.MustCompile(`^[a-z][a-z_]*$`)
	known := map[string]bool{}
	for _, r := range orphanReasons {
		known[r] = true
	}
	for _, f := range []vmFacts{
		{State: "FAILED"},
		{State: "CREATED", Role: "job", Job: "job-1", InJob: true, Owned: true, JobAge: 4 * time.Hour},
		{State: "CREATED", Role: "pool", Startup: true},
		{State: "CREATED", Role: "pool", Age: time.Hour},
		{State: "CREATED", Role: "builder", Startup: true},
		{State: "CREATED", Role: "builder", Age: time.Hour},
		{State: "CREATED", Role: "run", Age: 4 * time.Hour},
		{State: "CREATED", Role: "job", Job: "job-2", Age: time.Hour},
	} {
		r := decide(f, cfg)
		if !slug.MatchString(r) || !known[r] {
			t.Errorf("decide(%+v) = %q: not one of the pre-created slugs %v", f, r, orphanReasons)
		}
	}
	d, _ := newTestDaemon(t)
	if n := series(t, d.metrics.orphansDeleted); n != len(orphanReasons) {
		t.Errorf("orphans_deleted_total has %d series before any delete, want %d", n, len(orphanReasons))
	}
}

// firerunner_flintlock_up says what the startup listing found, instead of 0
// until the first reconcile a minute later.
func TestFlintlockUpFromTheStartupListing(t *testing.T) {
	oldFor, oldRetry := startupListFor, startupRetry
	startupListFor, startupRetry = 0, time.Millisecond
	t.Cleanup(func() { startupListFor, startupRetry = oldFor, oldRetry })
	d, srv := newTestDaemon(t)
	if _, ok := d.startupList(context.Background()); !ok || value(t, d.metrics.flintlockUp) != 1 {
		t.Fatalf("flintlock_up = %v after a startup listing that worked", value(t, d.metrics.flintlockUp))
	}
	srv.FailList(errors.New("down"))
	d.metrics.flintlockUp.Set(1)
	if _, ok := d.startupList(context.Background()); ok || value(t, d.metrics.flintlockUp) != 0 {
		t.Fatalf("flintlock_up = %v after a failed startup listing", value(t, d.metrics.flintlockUp))
	}
}

// A refill that cannot ask flintlock for memory is a Warn (once per
// warnEvery), not a Debug line nobody sees.
func TestRefillFlintlockErrorIsAWarning(t *testing.T) {
	d, srv := newTestDaemon(t)
	logs := captureLog(d)
	down := status.Error(codes.Unavailable, "connection refused")
	srv.FailList(down, down)
	d.refill(context.Background())
	d.refill(context.Background())
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for two failed refills, want 1:\n%s", n, logs)
	}
	if got := value(t, d.metrics.admissionWaits); got != 2 {
		t.Fatalf("admission waits = %v, want 2", got)
	}
}

// Daemon log lines name microVMs with the key "vm", like the job events, so
// one grep finds a VM's whole story.
func TestLogLinesUseTheVMKey(t *testing.T) {
	d, _ := newTestDaemon(t)
	logs := captureLog(d)
	d.delete(context.Background(), &vm.Instance{ID: "pool-abc123", UID: "u1"}, "expired")
	if out := logs.String(); !strings.Contains(out, "vm=pool-abc123") || strings.Contains(out, " id=") {
		t.Fatalf("delete log line: %s", out)
	}
}

// labelValue reads a counter or gauge of a vector by its label values; a
// series that does not exist reads as -1.
func labelValue(t *testing.T, d *Daemon, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := d.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	return -1
}

// firerunner_jobs_running counts the jobs in progress: their state files.
func TestJobsRunning(t *testing.T) {
	dir := tempJobStates(t)
	d, _ := newTestDaemon(t)
	for _, name := range []string{"job-1.json", "job-2.json"} {
		if err := vm.SaveJobState(filepath.Join(dir, name), &vm.JobState{StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "job-3.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.countJobs()
	if got := labelValue(t, d, "firerunner_jobs_running", nil); got != 2 {
		t.Fatalf("jobs_running = %v, want 2 (the unreadable state file is no job)", got)
	}
	if err := os.Remove(filepath.Join(dir, "job-1.json")); err != nil {
		t.Fatal(err)
	}
	d.countJobs()
	if got := labelValue(t, d, "firerunner_jobs_running", nil); got != 1 {
		t.Fatalf("jobs_running after a cleanup = %v, want 1", got)
	}
}

// Free and total space of the file systems holding the saved builder caches
// and flintlock's microVM state, by the directory FireRunner uses.
func TestDiskSpaceMetrics(t *testing.T) {
	d, _ := newTestDaemon(t)
	root := t.TempDir()
	oldCache, oldVM := builderCacheDir, flintlockVMDir
	builderCacheDir = filepath.Join(root, "missing", "builder-cache") // created at the first save
	flintlockVMDir = root
	t.Cleanup(func() { builderCacheDir, flintlockVMDir = oldCache, oldVM })

	d.collectHost(context.Background())
	for _, dir := range []string{builderCacheDir, flintlockVMDir} {
		free := labelValue(t, d, "firerunner_disk_free_bytes", map[string]string{"dir": dir})
		size := labelValue(t, d, "firerunner_disk_size_bytes", map[string]string{"dir": dir})
		if size <= 0 || free < 0 || free > size {
			t.Errorf("%s: free %v of %v bytes", dir, free, size)
		}
	}

	logs := captureLog(d)
	old := diskSpace
	diskSpace = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs: input/output error") }
	t.Cleanup(func() { diskSpace = old })
	d.collectHost(context.Background())
	if n := series(t, d.metrics.diskFree) + series(t, d.metrics.diskSize); n != 0 {
		t.Fatalf("%d disk series kept after statfs failed; want none (unknown)", n)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("failed statfs not logged:\n%s", logs)
	}
}

// Every failed flintlock call is counted by RPC and gRPC code; successes and
// errors that are not flintlock's (no uid in an answer) are not.
func TestFlintlockErrorsCounted(t *testing.T) {
	d, srv := newTestDaemon(t)
	ctx := context.Background()
	srv.FailList(status.Error(codes.Unavailable, "down"))
	srv.FailDelete(status.Error(codes.NotFound, "no such microvm"))
	_, _ = d.fl.List(ctx)
	_, _ = d.fl.List(ctx) // works
	_ = d.fl.Delete(ctx, "u1")
	_, _ = d.fl.Create(ctx, &types.MicroVMSpec{Id: "pool-x"}) // the fake answers without a uid
	expired, cancel := context.WithTimeout(ctx, -time.Second)
	defer cancel()
	_, _ = d.fl.List(expired)

	for _, c := range []struct {
		op, code string
		want     float64
	}{
		{"ListMicroVMs", "Unavailable", 1},
		{"DeleteMicroVM", "NotFound", 1},
		{"ListMicroVMs", "DeadlineExceeded", 1},
	} {
		if got := labelValue(t, d, "firerunner_flintlock_errors_total", map[string]string{"op": c.op, "code": c.code}); got != c.want {
			t.Errorf("flintlock_errors_total{op=%s,code=%s} = %v, want %v", c.op, c.code, got, c.want)
		}
	}
	if n := series(t, d.metrics.flintlockErrors); n != 3 {
		t.Errorf("%d error series, want 3", n)
	}
}
