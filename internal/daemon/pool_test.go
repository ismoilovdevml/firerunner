package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

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
	yaml := fmt.Sprintf("flintlock:\n  endpoint: %q\n  token_file: %q\n  namespace: %q\n"+
		"daemon:\n  socket: %q\n  metrics_listen: \"127.0.0.1:0\"\n",
		fl.Endpoint, fl.TokenFile, fl.Namespace, filepath.Join(dir, "d.sock"))
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfgPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.fl.Close() })
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

	d.adoptPool(context.Background())
	if len(d.ready) != 1 || d.ready[0].inst.UID != "ok" {
		ids := []string{}
		for _, p := range d.ready {
			ids = append(ids, p.inst.UID)
		}
		t.Fatalf("adopted %v, want [ok]", ids)
	}
}
