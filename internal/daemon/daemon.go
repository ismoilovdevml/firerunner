// Package daemon runs next to gitlab-runner and
//   - keeps a pool of pre-booted microVMs that jobs claim instantly,
//   - deletes orphaned microVMs (reconcile),
//   - exports Prometheus metrics.
//
// Pool VMs are never reused: a claimed VM belongs to one job and is deleted
// by that job's cleanup, exactly like a cold-booted VM.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// Label keys on every microVM FireRunner creates.
const (
	LabelRole = "firerunner/role" // pool | job | run
	LabelJob  = "firerunner/job"
)

type pooled struct {
	inst   *vm.Instance
	bornAt time.Time
	specID string // config fingerprint the VM was booted with
}

type Daemon struct {
	cfgPath string
	log     *slog.Logger
	metrics *Metrics

	mu       sync.Mutex
	cfg      config.Config
	cfgMod   time.Time
	fl       *flintlock.Client
	ready    []*pooled
	booting  int
	claimed  map[string]time.Time // uid -> claim time; protects it until the job writes its state
	firstSee map[string]time.Time // uid -> first time reconcile saw it
}

func New(cfgPath string, log *slog.Logger) (*Daemon, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return nil, err
	}
	d := &Daemon{cfgPath: cfgPath, log: log, cfg: cfg, fl: fl, metrics: NewMetrics(),
		claimed: map[string]time.Time{}, firstSee: map[string]time.Time{}}
	if fi, err := os.Stat(cfgPath); err == nil {
		d.cfgMod = fi.ModTime()
	}
	return d, nil
}

// Run blocks until ctx is cancelled, then deletes the idle pool.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.fl.Close()

	sock, err := d.listenSocket()
	if err != nil {
		return err
	}
	api := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: 5 * time.Second}
	metricsSrv := &http.Server{Addr: d.cfg.Daemon.MetricsListen, Handler: d.metricsHandler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- api.Serve(sock) }()
	go func() { errc <- metricsSrv.ListenAndServe() }()
	d.log.Info("daemon started", "socket", d.cfg.Daemon.Socket, "metrics", d.cfg.Daemon.MetricsListen, "pool", d.cfg.Pool.Size)

	d.reconcile(ctx, true)
	refill := time.NewTicker(2 * time.Second)
	reconcile := time.NewTicker(d.cfg.Daemon.ReconcileInterval)
	host := time.NewTicker(15 * time.Second)
	defer refill.Stop()
	defer reconcile.Stop()
	defer host.Stop()
	d.collectHost(ctx)

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = api.Shutdown(shutdownCtx)
			_ = metricsSrv.Shutdown(shutdownCtx)
			d.drain(shutdownCtx, "shutdown")
			return nil
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-refill.C:
			d.reloadConfig(ctx)
			d.expireIdle(ctx)
			d.refill(ctx)
		case <-reconcile.C:
			d.reconcile(ctx, false)
		case <-host.C:
			d.collectHost(ctx)
		}
	}
}

func (d *Daemon) listenSocket() (net.Listener, error) {
	path := d.cfg.Daemon.Socket
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return l, os.Chmod(path, 0o600)
}

// ---------------------------------------------------------------------------
// pool

// fingerprint changes whenever a setting that affects how a VM is booted changes.
func fingerprint(c config.Config) string {
	return fmt.Sprintf("%d/%d/%s/%s/%v/%s/%v", c.VM.VCPU, c.VM.MemoryMB, c.VM.KernelImage, c.VM.RootFSImage,
		c.VM.KernelCmdline, c.VM.RegistryMirror, c.Pool.PreloadImages)
}

// Claim hands a ready pool VM to a job. It returns nil when the pool is empty.
func (d *Daemon) Claim() *vm.Instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	fp := fingerprint(d.cfg)
	for len(d.ready) > 0 {
		p := d.ready[0]
		d.ready = d.ready[1:]
		if p.specID == fp {
			d.claimed[p.inst.UID] = time.Now()
			d.metrics.poolReady.Set(float64(len(d.ready)))
			d.metrics.claims.WithLabelValues("hit").Inc()
			return p.inst
		}
		// Booted with an old config: never hand it out.
		go d.delete(context.Background(), p.inst, "stale config")
	}
	d.metrics.poolReady.Set(0)
	d.metrics.claims.WithLabelValues("miss").Inc()
	return nil
}

func (d *Daemon) refill(ctx context.Context) {
	d.mu.Lock()
	cfg := d.cfg
	missing := cfg.Pool.Size - len(d.ready) - d.booting
	d.mu.Unlock()
	d.metrics.poolTarget.Set(float64(cfg.Pool.Size))

	for i := 0; i < missing; i++ {
		// VMs created in this loop are not listed by flintlock yet.
		extra := i * cfg.VM.MemoryMB * 105 / 100
		ok, why, err := vm.Fits(ctx, cfg, d.fl, extra)
		if err != nil || !ok {
			d.log.Debug("pool refill waits for memory", "why", why, "err", err)
			d.metrics.admissionWaits.Inc()
			return
		}
		d.mu.Lock()
		d.booting++
		d.metrics.poolBooting.Set(float64(d.booting))
		d.mu.Unlock()
		go d.bootOne(ctx, cfg)
	}
}

func (d *Daemon) bootOne(ctx context.Context, cfg config.Config) {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := "pool-" + hex.EncodeToString(suffix)
	start := time.Now()
	inst, err := vm.Boot(ctx, cfg, d.fl, id, map[string]string{LabelRole: "pool"})

	d.mu.Lock()
	defer d.mu.Unlock()
	d.booting--
	d.metrics.poolBooting.Set(float64(d.booting))
	if err != nil {
		d.metrics.bootFailures.WithLabelValues("pool").Inc()
		d.log.Error("pool boot failed", "id", id, "err", err)
		return
	}
	d.metrics.bootSeconds.WithLabelValues("pool").Observe(time.Since(start).Seconds())
	if len(cfg.Pool.PreloadImages) > 0 {
		// Pull outside the lock: it can take a while and jobs may claim other VMs meanwhile.
		d.mu.Unlock()
		pullStart := time.Now()
		err := preload(ctx, cfg, inst.IP)
		d.mu.Lock()
		if err != nil {
			d.metrics.bootFailures.WithLabelValues("preload").Inc()
			d.log.Error("image preload failed, VM kept without it", "id", id, "err", err)
		} else {
			d.metrics.preloadSeconds.Observe(time.Since(pullStart).Seconds())
		}
	}
	d.ready = append(d.ready, &pooled{inst: inst, bornAt: time.Now(), specID: fingerprint(cfg)})
	d.metrics.poolReady.Set(float64(len(d.ready)))
	d.log.Info("pool VM ready", "id", id, "ip", inst.IP, "boot", time.Since(start).Round(100*time.Millisecond).String())
}

// preload pulls pool.preload_images into the VM's Docker.
func preload(ctx context.Context, cfg config.Config, ip string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	args := "set -e"
	for _, img := range cfg.Pool.PreloadImages {
		args += "; docker pull -q " + shellQuote(img)
	}
	cmd := vm.SSH(cfg, ip, args)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// expireIdle recycles pool VMs that sat idle too long or were booted with an old config.
func (d *Daemon) expireIdle(ctx context.Context) {
	d.mu.Lock()
	fp, maxIdle := fingerprint(d.cfg), d.cfg.Pool.MaxIdle
	keep := d.ready[:0]
	var drop []*pooled
	for _, p := range d.ready {
		if p.specID != fp || time.Since(p.bornAt) > maxIdle {
			drop = append(drop, p)
		} else {
			keep = append(keep, p)
		}
	}
	d.ready = keep
	d.metrics.poolReady.Set(float64(len(d.ready)))
	d.mu.Unlock()
	for _, p := range drop {
		d.delete(ctx, p.inst, "expired")
	}
}

func (d *Daemon) drain(ctx context.Context, reason string) {
	d.mu.Lock()
	drop := d.ready
	d.ready = nil
	d.mu.Unlock()
	for _, p := range drop {
		d.delete(ctx, p.inst, reason)
	}
}

func (d *Daemon) delete(ctx context.Context, inst *vm.Instance, reason string) {
	if err := d.fl.Delete(ctx, inst.UID); err != nil {
		d.log.Error("delete failed", "id", inst.ID, "reason", reason, "err", err)
		return
	}
	d.log.Info("deleted microVM", "id", inst.ID, "reason", reason)
}

// reloadConfig picks up `firerunner config set` without a restart.
func (d *Daemon) reloadConfig(ctx context.Context) {
	fi, err := os.Stat(d.cfgPath)
	if err != nil || !fi.ModTime().After(d.cfgMod) {
		return
	}
	cfg, err := config.Load(d.cfgPath)
	if err != nil {
		d.log.Error("config reload failed, keeping the old config", "err", err)
		return
	}
	d.mu.Lock()
	d.cfg, d.cfgMod = cfg, fi.ModTime()
	d.mu.Unlock()
	d.log.Info("config reloaded", "pool", cfg.Pool.Size, "vcpu", cfg.VM.VCPU, "memory_mb", cfg.VM.MemoryMB)
}

// ---------------------------------------------------------------------------
// reconcile: delete microVMs nobody owns

// vmFacts is what reconcile knows about one microVM.
type vmFacts struct {
	State   string // flintlock state, e.g. CREATED, FAILED
	Role    string // firerunner/role label
	Job     string // firerunner/job label
	Owned   bool   // in the pool, just claimed, or referenced by a job state file
	Age     time.Duration
	Startup bool // first reconcile of this daemon
	Booting int  // pool VMs this daemon is booting right now
}

// decide returns why a microVM should be deleted, or "" to keep it.
func decide(f vmFacts, cfg config.Config) string {
	switch {
	case f.State == "FAILED":
		return "failed"
	case f.Owned && f.Age > cfg.Daemon.JobMaxAge:
		return "older than daemon.job_max_age"
	case f.Owned:
		return ""
	case f.Role == "pool" && f.Startup:
		// A fresh daemon owns no pool VMs: these were left by a previous run.
		return "pool VM from a previous daemon run"
	case f.Role == "pool" && f.Booting == 0 && f.Age > cfg.VM.BootTimeout:
		return "orphaned pool VM"
	case f.Role == "run" && f.Age > cfg.Daemon.JobMaxAge:
		return "abandoned `firerunner run` VM"
	case f.Job != "" && f.Age > 2*cfg.VM.BootTimeout:
		return "job VM without a running job"
	}
	return ""
}

func (d *Daemon) reconcile(ctx context.Context, startup bool) {
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vms, err := d.fl.List(lctx)
	if err != nil {
		d.metrics.flintlockUp.Set(0)
		d.log.Error("reconcile: listing microVMs failed", "err", err)
		return
	}
	d.metrics.flintlockUp.Set(1)

	d.mu.Lock()
	cfg := d.cfg
	owned := map[string]bool{}
	for _, p := range d.ready {
		owned[p.inst.UID] = true
	}
	for uid, at := range d.claimed {
		if time.Since(at) < 5*time.Minute {
			owned[uid] = true
		} else {
			delete(d.claimed, uid)
		}
	}
	booting := d.booting
	d.mu.Unlock()

	present := map[string]bool{}
	for _, v := range vms {
		present[v.GetSpec().GetUid()] = true
	}
	for uid, file := range jobStates() {
		if present[uid] {
			owned[uid] = true
			continue
		}
		// The job's VM is gone (cleanup failed or the host rebooted): drop the stale state.
		if fi, err := os.Stat(file); err == nil && time.Since(fi.ModTime()) > 10*time.Minute {
			_ = os.Remove(file)
			d.log.Info("removed stale job state", "file", file)
		}
	}

	states := map[string]float64{}
	for _, v := range vms {
		uid, labels := v.GetSpec().GetUid(), v.GetSpec().GetLabels()
		state := v.GetStatus().GetState().String()
		states[state]++
		d.mu.Lock()
		first, ok := d.firstSee[uid]
		if !ok {
			first = time.Now()
			d.firstSee[uid] = first
		}
		d.mu.Unlock()

		reason := decide(vmFacts{State: state, Role: labels[LabelRole], Job: labels[LabelJob], Owned: owned[uid],
			Age: time.Since(first), Startup: startup, Booting: booting}, cfg)
		if reason != "" {
			d.metrics.orphansDeleted.WithLabelValues(reason).Inc()
			d.delete(ctx, &vm.Instance{ID: v.GetSpec().GetId(), UID: uid}, reason)
		}
	}
	d.mu.Lock()
	for uid := range d.firstSee {
		if !present[uid] {
			delete(d.firstSee, uid)
		}
	}
	d.mu.Unlock()
	d.metrics.microvms.Reset()
	for s, n := range states {
		d.metrics.microvms.WithLabelValues(s).Set(n)
	}
}

// jobStates maps microVM uid -> state file for jobs in progress (written by `executor prepare`).
func jobStates() map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob("/run/firerunner/jobs/*.json")
	for _, f := range files {
		if st, err := vm.LoadJobState(f); err == nil {
			out[st.UID] = f
		}
	}
	return out
}

func (d *Daemon) collectHost(ctx context.Context) {
	for _, s := range append(host.Services, "gitlab-runner") {
		up := 0.0
		if host.ServiceActive(s) {
			up = 1
		}
		d.metrics.serviceUp.WithLabelValues(s).Set(up)
	}
	if data, meta, err := host.ThinPoolUsage(); err == nil {
		d.metrics.thinPool.WithLabelValues("data").Set(data / 100)
		d.metrics.thinPool.WithLabelValues("metadata").Set(meta / 100)
	}
	if avail, err := host.MemAvailableMB(); err == nil {
		d.metrics.memAvailable.Set(float64(avail) * 1024 * 1024)
	}
	if r, err := host.ReadRunner(); err == nil {
		d.metrics.runnerConcurrent.Set(float64(r.Concurrent))
	}
}
