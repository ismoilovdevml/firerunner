package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// newTestDaemon builds a Daemon through New against a fake flintlockd, with
// its socket and metrics listener in places a test may bind.
func newTestDaemon(t *testing.T) (*Daemon, *flintlocktest.Server) {
	t.Helper()
	srv := flintlocktest.NewServer("")
	fl := flintlocktest.StartUnix(t, srv)
	dir, err := os.MkdirTemp("", "frd") // short: holds a unix socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfgPath := filepath.Join(dir, "config.yaml")
	// Host paths of the config are temporary too (see TestMain).
	yaml := fmt.Sprintf("flintlock:\n  endpoint: %q\n  token_file: %q\n  namespace: %q\n"+
		"daemon:\n  socket: %q\n  metrics_listen: \"127.0.0.1:0\"\nnetwork:\n  leases_file: %q\n",
		fl.Endpoint, fl.TokenFile, fl.Namespace, filepath.Join(dir, "d.sock"), filepath.Join(dir, "leases"))
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfgPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.fl.Close() })
	// Runs first (cleanups are LIFO): background work started by the test ends
	// before the connection closes and before stubbed hooks are restored.
	t.Cleanup(d.bg.Wait)
	return d, srv
}

// metricValue reads a metric the way /metrics exposes it: by name and, for
// vectors, by the value of its single label. Absent series read as 0.
func metricValue(t *testing.T, d *Daemon, name, label string) float64 {
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
			if label != "" && (len(m.GetLabel()) != 1 || m.GetLabel()[0].GetValue() != label) {
				continue
			}
			return m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	return 0
}

func pooledVM(uid, specID string, age time.Duration) *pooled {
	return &pooled{inst: &vm.Instance{ID: "pool-" + uid, UID: uid}, bornAt: time.Now().Add(-age), specID: specID}
}

func TestClaim(t *testing.T) {
	cases := []struct {
		name       string
		ready      func(fp string) []*pooled
		wantUID    string // "" = miss
		wantDelete []string
	}{
		{"empty pool is a miss", func(string) []*pooled { return nil }, "", nil},
		{"fresh VM is handed out", func(fp string) []*pooled { return []*pooled{pooledVM("u1", fp, 0)} }, "u1", nil},
		{"stale VM is skipped and deleted", func(fp string) []*pooled {
			return []*pooled{pooledVM("old", "stale", 0), pooledVM("u2", fp, 0)}
		}, "u2", []string{"old"}},
		{"only stale VMs is a miss", func(string) []*pooled {
			return []*pooled{pooledVM("old", "stale", 0)}
		}, "", []string{"old"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, srv := newTestDaemon(t)
			d.ready = c.ready(fingerprint(d.cfg))
			inst := d.Claim()

			hit := metricValue(t, d, "firerunner_pool_claims_total", "hit")
			miss := metricValue(t, d, "firerunner_pool_claims_total", "miss")
			if c.wantUID == "" {
				if inst != nil || miss != 1 || hit != 0 {
					t.Fatalf("Claim = %v, hit %v miss %v; want nil and one miss", inst, hit, miss)
				}
			} else {
				if inst == nil || inst.UID != c.wantUID || hit != 1 || miss != 0 {
					t.Fatalf("Claim = %v, hit %v miss %v; want %s and one hit", inst, hit, miss, c.wantUID)
				}
				if _, ok := d.claimed[c.wantUID]; !ok {
					t.Fatal("claimed VM must be protected from reconcile")
				}
			}
			for _, uid := range c.wantDelete {
				srv.WaitDeleted(t, uid, 2*time.Second)
			}
			if got := srv.Deleted(); len(got) != len(c.wantDelete) {
				t.Fatalf("deleted %v, want %v", got, c.wantDelete)
			}
		})
	}
}

func TestExpireIdle(t *testing.T) {
	d, srv := newTestDaemon(t)
	fp, maxIdle := fingerprint(d.cfg), d.cfg.Pool.MaxIdle
	d.ready = []*pooled{
		pooledVM("idle", fp, maxIdle+time.Minute),
		pooledVM("fresh", fp, time.Minute),
		pooledVM("stale", "old-config", time.Minute),
	}
	d.expireIdle(context.Background())

	if len(d.ready) != 1 || d.ready[0].inst.UID != "fresh" {
		t.Fatalf("ready after expire: %v", d.ready)
	}
	if g := metricValue(t, d, "firerunner_pool_ready", ""); g != 1 {
		t.Fatalf("pool_ready = %v, want 1", g)
	}
	got := srv.Deleted()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "idle" || got[1] != "stale" {
		t.Fatalf("deleted %v, want [idle stale]", got)
	}
}

func TestClaimTakesPreloadingVM(t *testing.T) {
	d, _ := newTestDaemon(t)
	fp := fingerprint(d.cfg)
	cancelled := map[string]bool{}
	for _, uid := range []string{"stale", "warm"} {
		spec := fp
		if uid == "stale" {
			spec = "old-config"
		}
		d.preloading[uid] = &preloadingVM{inst: &vm.Instance{ID: "pool-" + uid, UID: uid}, specID: spec,
			cancel: func() { cancelled[uid] = true }}
	}

	inst := d.Claim()
	if inst == nil || inst.UID != "warm" {
		t.Fatalf("Claim = %v, want the preloading VM with the current config", inst)
	}
	if !cancelled["warm"] || cancelled["stale"] {
		t.Fatalf("cancelled %v, want only warm", cancelled)
	}
	if _, ok := d.preloading["warm"]; ok {
		t.Fatal("claimed VM still listed as preloading")
	}
	if _, ok := d.claimed["warm"]; !ok {
		t.Fatal("claimed VM must be protected from reconcile")
	}
	if d.Claim() != nil {
		t.Fatal("a VM with an old config must not be handed out")
	}
}

func TestExpireIdleTrimsToPoolSize(t *testing.T) {
	d, srv := newTestDaemon(t)
	fp := fingerprint(d.cfg)
	d.cfg.Pool.Size = 1
	d.ready = []*pooled{pooledVM("old", fp, 3*time.Minute), pooledVM("new", fp, time.Minute)}
	d.expireIdle(context.Background())

	if len(d.ready) != 1 || d.ready[0].inst.UID != "new" {
		t.Fatalf("ready after trim: %v", d.ready)
	}
	if got := srv.Deleted(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("deleted %v, want [old]", got)
	}
}

func TestRunShutdownKeepsAndRecordsPool(t *testing.T) {
	d, srv := newTestDaemon(t)
	fp := fingerprint(d.cfg)
	d.ready = []*pooled{pooledVM("a", fp, 0), pooledVM("b", fp, 0)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.ListCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon never reconciled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if got := srv.Deleted(); len(got) != 0 {
		t.Fatalf("shutdown deleted idle pool VMs %v; they must survive a restart", got)
	}
	var recs []poolRecord
	data, err := os.ReadFile(d.poolFile())
	if err != nil || json.Unmarshal(data, &recs) != nil || len(recs) != 2 {
		t.Fatalf("pool.json: %s %v", data, err)
	}
}

func TestAdoptPoolAfterRestart(t *testing.T) {
	d, srv := newTestDaemon(t)
	fp := fingerprint(d.cfg)
	recs := []poolRecord{
		{Instance: vm.Instance{ID: "pool-ok", UID: "ok"}, BornAt: time.Now(), SpecID: fp},
		{Instance: vm.Instance{ID: "pool-gone", UID: "gone"}, BornAt: time.Now(), SpecID: fp},
		{Instance: vm.Instance{ID: "pool-stale", UID: "stale"}, BornAt: time.Now(), SpecID: "old"},
		{Instance: vm.Instance{ID: "pool-old", UID: "old"}, BornAt: time.Now().Add(-2 * time.Hour), SpecID: fp},
		{Instance: vm.Instance{ID: "pool-dead", UID: "dead"}, BornAt: time.Now(), SpecID: fp},
	}
	data, _ := json.Marshal(recs)
	if err := os.WriteFile(d.poolFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mk := func(uid string) *types.MicroVM {
		return &types.MicroVM{Spec: &types.MicroVMSpec{Id: "pool-" + uid, Uid: &uid},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}}
	}
	srv.SetVMs(mk("ok"), mk("stale"), mk("old"), mk("dead"))
	orig := alive
	alive = func(_ config.Config, inst *vm.Instance) bool { return inst.UID != "dead" }
	t.Cleanup(func() { alive = orig })

	vms, err := d.fl.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d.adoptPool(vms)
	if len(d.ready) != 1 || d.ready[0].inst.UID != "ok" {
		ids := []string{}
		for _, p := range d.ready {
			ids = append(ids, p.inst.UID)
		}
		t.Fatalf("adopted %v, want [ok]", ids)
	}
}

// A delete stuck in flintlockd must not block its caller (the daemon loop) forever.
func TestDeleteGivesUpOnHungFlintlock(t *testing.T) {
	old := deleteTimeout
	deleteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { deleteTimeout = old })
	d, srv := newTestDaemon(t)
	srv.HangDeletes()

	done := make(chan struct{})
	go func() {
		d.delete(context.Background(), &vm.Instance{ID: "pool-hung", UID: "hung"}, "test")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delete blocked on a hung flintlockd; the daemon loop would stall")
	}
	if got := srv.Deleted(); len(got) != 0 {
		t.Fatalf("deleted %v, want nothing (the call timed out)", got)
	}
}

func TestStartupListRetries(t *testing.T) {
	oldFor, oldRetry := startupListFor, startupRetry
	startupListFor, startupRetry = time.Second, 10*time.Millisecond
	t.Cleanup(func() { startupListFor, startupRetry = oldFor, oldRetry })

	d, srv := newTestDaemon(t)
	down := status.Error(codes.Unavailable, "flintlockd starting")
	srv.FailList(down, down)
	if _, ok := d.startupList(context.Background()); !ok || srv.ListCalls() != 3 {
		t.Fatalf("ok %v after %d calls; want success on the third", ok, srv.ListCalls())
	}

	d2, srv2 := newTestDaemon(t)
	startupListFor = 50 * time.Millisecond
	for i := 0; i < 100; i++ {
		srv2.FailList(down)
	}
	start := time.Now()
	if _, ok := d2.startupList(context.Background()); ok {
		t.Fatal("startupList succeeded while flintlock was down")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("startupList gave up after %s, want about startupListFor", took)
	}
}

// When the startup listing fails, nothing was adopted, so reconcile must not
// treat a previous run's builder as garbage: it would throw away its cache.
func TestStartupListFailureKeepsBuilders(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	oldFor, oldRetry := startupListFor, startupRetry
	startupListFor, startupRetry = 0, 10*time.Millisecond
	t.Cleanup(func() { startupListFor, startupRetry = oldFor, oldRetry })

	d, srv := newTestDaemon(t)
	uid := "bld-uid"
	srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: "bld-7", Uid: &uid},
		Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}})
	srv.FailList(status.Error(codes.Unavailable, "down")) // only the startup listing fails

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.ListCalls() < 2 { // startup list, then reconcile's
		if time.Now().After(deadline) {
			t.Fatal("daemon never reconciled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := srv.Deleted(); len(got) != 0 {
		t.Fatalf("deleted %v after a failed startup listing; want the builder kept", got)
	}
}

// Shutdown waits for background flintlock calls before closing the connection.
func TestRunWaitsForBackgroundWork(t *testing.T) {
	d, srv := newTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.ListCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var finished atomic.Bool
	d.spawn(func() {
		time.Sleep(300 * time.Millisecond)
		d.delete(context.Background(), &vm.Instance{ID: "pool-late", UID: "late"}, "test")
		finished.Store(true)
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !finished.Load() {
		t.Fatal("Run returned before a background delete finished")
	}
	if got := srv.Deleted(); len(got) != 1 || got[0] != "late" {
		t.Fatalf("deleted %v, want [late] (connection closed too early?)", got)
	}
}

// A pool VM nobody owns (e.g. a job took it and its cleanup could not delete
// it) is reclaimed even while the daemon boots other pool VMs; only the VMs it
// is booting itself are spared.
func TestReconcileReclaimsOrphanWhileBooting(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	d, srv := newTestDaemon(t)
	mk := func(id, uid string) *types.MicroVM {
		return &types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}}
	}
	srv.SetVMs(mk("pool-b00t", "b"), mk("pool-0rph", "o"))
	old := time.Now().Add(-10 * time.Minute) // older than vm.boot_timeout
	d.firstSee["b"], d.firstSee["o"] = old, old
	d.booting, d.bootingIDs["pool-b00t"] = 1, true

	d.reconcile(context.Background(), false)
	srv.WaitDeleted(t, "o", 2*time.Second)
	if got := srv.Deleted(); len(got) != 1 {
		t.Fatalf("deleted %v, want only the orphan [o]", got)
	}
}

// Pool VMs being booted and a builder being admitted never count on the same
// memory: every admission sees what the others reserved until their boots
// returned. Without it a refill and a builder boot (and cold boots of jobs)
// listed flintlock before each other's creates landed and over-committed.
func TestPoolAndBuilderAdmissionsShareTheHost(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.cfg.VM.MemoryMB, d.cfg.Pool.Size = 256, 2
	release := make(chan struct{})
	var booting sync.WaitGroup
	booting.Add(2)
	var mu sync.Mutex
	var reserved []int // extraMB of every builder admission check
	oldBoot, oldFits := poolBoot, builderFits
	t.Cleanup(func() { poolBoot, builderFits = oldBoot, oldFits })
	poolBoot = func(context.Context, config.Config, *flintlock.Client, string, map[string]string) (*vm.Instance, error) {
		booting.Done()
		<-release
		return nil, errors.New("stub: boot failed")
	}
	builderFits = func(_ context.Context, _ config.Config, _ *flintlock.Client, extraMB int) (bool, string, error) {
		mu.Lock()
		defer mu.Unlock()
		reserved = append(reserved, extraMB)
		return true, "", nil
	}
	lastReserved := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reserved[len(reserved)-1]
	}

	d.refill(context.Background())
	booting.Wait()
	bcfg := builderConfig(d.cfgSnapshot())
	drop7, err := d.admitBuilder(context.Background(), bcfg, "bld-7")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lastReserved(), 2*vm.WithOverhead(256); got != want {
		t.Fatalf("builder admitted next to %d MB reserved, want the booting pool VMs' %d", got, want)
	}
	close(release)
	d.bg.Wait() // the pool boots returned (and failed)
	drop8, err := d.admitBuilder(context.Background(), bcfg, "bld-8")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lastReserved(), vm.WithOverhead(bcfg.VM.MemoryMB); got != want {
		t.Fatalf("after the pool boots returned: %d MB reserved, want only bld-7's %d", got, want)
	}
	drop7()
	drop8()
	drop9, err := d.admitBuilder(context.Background(), bcfg, "bld-9")
	if err != nil {
		t.Fatal(err)
	}
	drop9()
	if got := lastReserved(); got != 0 {
		t.Fatalf("%d MB still reserved after every boot returned", got)
	}
}

// tempJobStates points the job state glob at a temporary directory and
// returns it.
func tempJobStates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	return dir
}

// daemon.job_max_age counts from the job's start (its state file), not from
// when this daemon first saw the VM: a pool VM idles before a job claims it,
// and a restarted daemon sees every VM anew.
func TestJobMaxAgeCountsFromTheJobStart(t *testing.T) {
	cases := []struct {
		name               string
		started, firstSeen time.Duration // ago
		deleted            bool
	}{
		{"young job on a pool VM seen long ago", time.Hour, 4 * time.Hour, false},
		{"job past max age, seen only since a restart", 4 * time.Hour, time.Minute, true},
		{"young job", time.Hour, time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := tempJobStates(t)
			d, srv := newTestDaemon(t)
			uid := "u1"
			srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: "pool-a1b2c3", Uid: &uid},
				Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}})
			st := &vm.JobState{Instance: vm.Instance{ID: "pool-a1b2c3", UID: uid}, Source: "pool",
				StartedAt: time.Now().Add(-c.started)}
			if err := vm.SaveJobState(filepath.Join(dir, "job-9.json"), st); err != nil {
				t.Fatal(err)
			}
			d.firstSee[uid] = time.Now().Add(-c.firstSeen)
			d.reconcile(context.Background(), false)
			if got := len(srv.Deleted()) == 1; got != c.deleted {
				t.Fatalf("deleted = %v (%v), want %v", got, srv.Deleted(), c.deleted)
			}
		})
	}
}
