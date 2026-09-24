// Package daemon runs next to gitlab-runner and
//   - keeps a pool of pre-booted microVMs that jobs claim instantly,
//   - deletes orphaned microVMs (reconcile),
//   - exports Prometheus metrics.
//
// Pool VMs are never reused: a claimed VM belongs to one job and is deleted
// by that job's cleanup, exactly like a cold-booted VM.
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// Label keys on every microVM FireRunner creates. flintlockd v0.15 does not
// persist labels, so ownership is decided from the id prefix (RoleOf) instead.
const (
	LabelRole = "firerunner/role" // pool | job | run
	LabelJob  = "firerunner/job"
)

// RoleOf derives the role from the microVM id FireRunner assigned:
// pool-<rand>, job-<CI_JOB_ID> or run-<rand>. Other ids are not ours.
func RoleOf(id string) (role, job string) {
	prefix, _, ok := strings.Cut(id, "-")
	if !ok {
		return "", ""
	}
	switch prefix {
	case "pool", "run":
		return prefix, ""
	case "bld":
		return "builder", ""
	case "job":
		return "job", id
	}
	return "", ""
}

type pooled struct {
	inst   *vm.Instance
	bornAt time.Time
	specID string // config fingerprint the VM was booted with
}

// preloadingVM is a pool VM that has booted and is still pulling
// pool.preload_images. A job may take it; its preload is then cancelled.
type preloadingVM struct {
	inst   *vm.Instance
	specID string
	cancel context.CancelFunc
}

// poolRecord is how the ready pool is persisted across daemon restarts.
type poolRecord struct {
	Instance vm.Instance `json:"instance"`
	BornAt   time.Time   `json:"born_at"`
	SpecID   string      `json:"spec_id"`
}

// alive reports whether an adopted pool VM still answers on its pinned key.
var alive = func(cfg config.Config, inst *vm.Instance) bool {
	return vm.SSH(cfg, inst, "true").Run() == nil
}

func (d *Daemon) poolFile() string {
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "pool.json")
}

// savePoolLocked persists the ready pool; the caller holds d.mu.
func (d *Daemon) savePoolLocked() {
	recs := make([]poolRecord, 0, len(d.ready))
	for _, p := range d.ready {
		recs = append(recs, poolRecord{Instance: *p.inst, BornAt: p.bornAt, SpecID: p.specID})
	}
	data, _ := json.Marshal(recs)
	tmp := d.poolFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, d.poolFile())
	}
}

// adoptPool takes back idle pool VMs a previous daemon run left ready, so a
// restart or upgrade does not throw the warm pool away. Anything that does not
// match (gone, other config, too old, not answering) is left for reconcile.
// vms is the flintlock listing taken at startup.
func (d *Daemon) adoptPool(vms []*types.MicroVM) {
	data, err := os.ReadFile(d.poolFile())
	if err != nil {
		return
	}
	var recs []poolRecord
	if json.Unmarshal(data, &recs) != nil {
		return
	}
	live := map[string]bool{}
	for _, v := range vms {
		if v.GetStatus().GetState().String() == "CREATED" {
			live[v.GetSpec().GetUid()] = true
		}
	}
	d.mu.Lock()
	cfg, fp := d.cfg, fingerprint(d.cfg)
	d.mu.Unlock()
	for _, r := range recs {
		inst := r.Instance
		if !live[inst.UID] || r.SpecID != fp || time.Since(r.BornAt) > cfg.Pool.MaxIdle || !alive(cfg, &inst) {
			continue
		}
		d.mu.Lock()
		d.ready = append(d.ready, &pooled{inst: &inst, bornAt: r.BornAt, specID: r.SpecID})
		d.mu.Unlock()
		d.log.Info("adopted pool VM from previous run", "id", inst.ID)
	}
	d.mu.Lock()
	d.metrics.poolReady.Set(float64(len(d.ready)))
	d.savePoolLocked()
	d.mu.Unlock()
}

type Daemon struct {
	cfgPath string
	log     *slog.Logger
	metrics *Metrics

	mu      sync.Mutex
	cfg     config.Config
	cfgMod  time.Time
	fl      *flintlock.Client
	ready   []*pooled
	booting int
	// booted pool VMs still preloading images, by uid; counted in booting
	preloading map[string]*preloadingVM
	claimed    map[string]time.Time // uid -> claim time; protects it until the job writes its state
	firstSee   map[string]time.Time // uid -> first time reconcile saw it
	builders   map[string]*builder  // project id -> BuildKit builder
	// project id -> last failed builder boot (see builderRetryAfter)
	builderFailed map[string]time.Time
	runCtx        context.Context // cancelled on shutdown; builders boot under it
	bg            sync.WaitGroup  // background goroutines that call flintlock; Run waits for them
	// busy is the projects whose builder a running job uses, as expireBuilders
	// last saw them (every 2 s), so Builder can evict without a scan under mu.
	busy map[string]bool
}

// spawn runs fn in the background and lets Run wait for it at shutdown, so
// deletes and boots in flight are not cut off when the flintlock connection closes.
func (d *Daemon) spawn(fn func()) {
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		fn()
	}()
}

// startupListFor and startupRetry bound how long a starting daemon waits for
// flintlockd, which systemd starts just before it (variables for tests).
var (
	startupListFor = time.Minute
	startupRetry   = 5 * time.Second
)

// startupList lists microVMs at startup, retrying while flintlockd is not
// answering yet. Without the listing nothing from the previous run can be
// adopted; ok reports whether it succeeded.
func (d *Daemon) startupList(ctx context.Context) (vms []*types.MicroVM, ok bool) {
	deadline := time.Now().Add(startupListFor)
	for {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vms, err := d.fl.List(lctx)
		cancel()
		if err == nil {
			return vms, true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			d.log.Error("listing microVMs at startup failed; nothing adopted from the previous run", "err", err)
			return nil, false
		}
		d.log.Warn("flintlock not answering yet, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(startupRetry):
		}
	}
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
		claimed: map[string]time.Time{}, firstSee: map[string]time.Time{}, preloading: map[string]*preloadingVM{},
		builders: map[string]*builder{}, builderFailed: map[string]time.Time{}, runCtx: context.Background()}
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

	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()
	vms, listed := d.startupList(ctx)
	if listed {
		d.adoptPool(vms)
		live := map[string]bool{}
		for _, v := range vms {
			live[v.GetSpec().GetUid()] = true
		}
		d.adoptBuilders(live)
	}
	// Only a daemon that could adopt may treat leftover pool VMs and builders
	// as a previous run's; otherwise the age-based orphan rules reclaim them.
	d.reconcile(ctx, listed)
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
			// Keep idle pool VMs for the next run (adoptPool); they are recorded in pool.json.
			d.mu.Lock()
			d.savePoolLocked()
			d.mu.Unlock()
			// Let deletes and boots in flight finish before fl.Close (deferred above).
			done := make(chan struct{})
			go func() { d.bg.Wait(); close(done) }()
			select {
			case <-done:
			case <-shutdownCtx.Done():
				d.log.Warn("flintlock calls still running at shutdown; the next start reconciles them")
			}
			return nil
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-refill.C:
			d.reloadConfig(ctx)
			d.expireIdle(ctx)
			d.expireBuilders()
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
	return fmt.Sprintf("%d/%d/%s/%s/%v/%s/%s/%s/%v", c.VM.VCPU, c.VM.MemoryMB, c.VM.KernelImage, c.VM.RootFSImage,
		c.VM.KernelCmdline, c.VM.RegistryMirror, c.VM.DockerBIP, c.VM.DockerAddressPool, c.Pool.PreloadImages)
}

// Claim hands a ready pool VM to a job, or else one that has booted but is
// still preloading images; that preload is cancelled, because a job should not
// wait for memory held by a VM that is only warming its image cache. nil means
// the pool has nothing to give.
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
			d.savePoolLocked()
			d.metrics.claims.WithLabelValues("hit").Inc()
			return p.inst
		}
		// Booted with an old config: never hand it out.
		inst := p.inst
		d.spawn(func() { d.delete(context.Background(), inst, "stale config") })
	}
	d.metrics.poolReady.Set(0)
	for uid, p := range d.preloading {
		if p.specID != fp {
			continue
		}
		delete(d.preloading, uid)
		p.cancel()
		d.claimed[uid] = time.Now()
		d.metrics.claims.WithLabelValues("hit").Inc()
		d.log.Info("pool VM claimed before its preload finished", "id", p.inst.ID)
		return p.inst
	}
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
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ok, why, err := vm.Fits(fctx, cfg, d.fl, extra)
		cancel()
		if err != nil || !ok {
			d.log.Debug("pool refill waits for memory", "why", why, "err", err)
			d.metrics.admissionWaits.Inc()
			return
		}
		d.mu.Lock()
		d.booting++
		d.metrics.poolBooting.Set(float64(d.booting))
		d.mu.Unlock()
		d.spawn(func() { d.bootOne(ctx, cfg) })
	}
}

func (d *Daemon) bootOne(ctx context.Context, cfg config.Config) {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := "pool-" + hex.EncodeToString(suffix)
	start := time.Now()

	// The VM counts as booting until it is fully ready (including preload),
	// otherwise refill sees a gap and boots extra VMs.
	inst, err := vm.Boot(ctx, cfg, d.fl, id, map[string]string{LabelRole: "pool"})
	if err == nil {
		d.metrics.bootSeconds.WithLabelValues("pool").Observe(time.Since(start).Seconds())
		if len(cfg.Pool.PreloadImages) > 0 {
			pctx, cancel := context.WithCancel(ctx)
			d.mu.Lock()
			d.preloading[inst.UID] = &preloadingVM{inst: inst, specID: fingerprint(cfg), cancel: cancel}
			d.mu.Unlock()
			pullStart := time.Now()
			perr := preload(pctx, cfg, inst)
			cancel()
			d.mu.Lock()
			_, stillOurs := d.preloading[inst.UID]
			delete(d.preloading, inst.UID)
			d.mu.Unlock()
			switch {
			case !stillOurs:
				// A job took the VM during the preload (Claim).
				d.mu.Lock()
				d.booting--
				d.metrics.poolBooting.Set(float64(d.booting))
				d.mu.Unlock()
				return
			case perr != nil:
				d.metrics.bootFailures.WithLabelValues("preload").Inc()
				d.log.Error("image preload failed, VM kept without it", "id", id, "err", perr)
			default:
				d.metrics.preloadSeconds.Observe(time.Since(pullStart).Seconds())
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.booting--
	d.metrics.poolBooting.Set(float64(d.booting))
	switch {
	case err != nil:
		d.metrics.bootFailures.WithLabelValues("pool").Inc()
		d.log.Error("pool boot failed", "id", id, "err", err)
	case ctx.Err() != nil:
		// Shutting down: do not hand out a VM nobody will drain.
		d.spawn(func() { d.delete(context.Background(), inst, "shutdown") })
	default:
		d.ready = append(d.ready, &pooled{inst: inst, bornAt: time.Now(), specID: fingerprint(cfg)})
		d.metrics.poolReady.Set(float64(len(d.ready)))
		d.savePoolLocked()
		d.log.Info("pool VM ready", "id", id, "ip", inst.IP, "took", time.Since(start).Round(100*time.Millisecond).String())
	}
}

// preload pulls pool.preload_images into the VM's Docker.
func preload(ctx context.Context, cfg config.Config, inst *vm.Instance) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	args := "set -e"
	for _, img := range cfg.Pool.PreloadImages {
		args += "; docker pull -q " + shellQuote(img)
	}
	cmd := vm.SSH(cfg, inst, args)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		// Timeout or a job claimed the VM: stop pulling (docker cancels the
		// pull when its client goes away).
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// expireIdle recycles pool VMs that sat idle too long or were booted with an
// old config, and the oldest ones above pool.size (after the size was lowered).
func (d *Daemon) expireIdle(ctx context.Context) {
	d.mu.Lock()
	fp, maxIdle, size := fingerprint(d.cfg), d.cfg.Pool.MaxIdle, d.cfg.Pool.Size
	var keep, drop []*pooled
	for _, p := range d.ready {
		if p.specID != fp || time.Since(p.bornAt) > maxIdle {
			drop = append(drop, p)
		} else {
			keep = append(keep, p)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].bornAt.After(keep[j].bornAt) })
	if len(keep) > size {
		drop = append(drop, keep[size:]...)
		keep = keep[:size]
	}
	d.ready = keep
	d.metrics.poolReady.Set(float64(len(d.ready)))
	d.savePoolLocked()
	d.mu.Unlock()
	for _, p := range drop {
		d.delete(ctx, p.inst, "expired")
	}
}

func (d *Daemon) drain(ctx context.Context, reason string) {
	d.mu.Lock()
	drop := d.ready
	d.ready = nil
	d.savePoolLocked()
	d.mu.Unlock()
	for _, p := range drop {
		d.delete(ctx, p.inst, reason)
	}
}

// deleteTimeout bounds one flintlock delete. flintlockd stops Firecracker and
// removes the containerd snapshot; when containerd or device-mapper hangs the
// call would never return and would stall the daemon loop (no refill, no
// reconcile). Reconcile retries on its next pass. A variable for tests.
var deleteTimeout = 60 * time.Second

func (d *Daemon) delete(ctx context.Context, inst *vm.Instance, reason string) {
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if err := d.fl.Delete(ctx, inst.UID); err != nil {
		d.log.Error("delete failed", "id", inst.ID, "reason", reason, "err", err)
		return
	}
	vm.Forget(d.cfgSnapshot(), inst.ID)
	d.log.Info("deleted microVM", "id", inst.ID, "reason", reason)
}

func (d *Daemon) cfgSnapshot() config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
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
	Owned   bool   // in the pool, a builder, just claimed, or referenced by a job state file
	InJob   bool   // referenced by a job state file: a job runs on it
	Age     time.Duration
	Startup bool // first reconcile of this daemon
	Booting int  // pool VMs this daemon is booting right now
}

// decide returns why a microVM should be deleted, or "" to keep it.
func decide(f vmFacts, cfg config.Config) string {
	switch {
	case f.State == "FAILED":
		return "failed"
	case f.InJob && f.Age > cfg.Daemon.JobMaxAge:
		return "older than daemon.job_max_age"
	case f.Owned:
		// Idle pool VMs are bounded by pool.max_idle (expireIdle), builders by
		// builder.idle_ttl and builderMaxAge (expireBuilders): never by job_max_age.
		return ""
	case f.Role == "pool" && f.Startup:
		// A fresh daemon owns no pool VMs: these were left by a previous run.
		return "pool VM from a previous daemon run"
	case f.Role == "pool" && f.Booting == 0 && f.Age > cfg.VM.BootTimeout:
		return "orphaned pool VM"
	case f.Role == "builder" && f.Startup:
		return "builder from a previous daemon run"
	case f.Role == "builder" && f.Age > 2*cfg.VM.BootTimeout+5*time.Minute:
		return "orphaned builder"
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
	listedAt := time.Now()
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
	for _, b := range d.builders {
		if b.Instance.UID != "" {
			owned[b.Instance.UID] = true
		}
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
	inJob := map[string]bool{}
	for uid, file := range jobStates() {
		if present[uid] {
			owned[uid], inJob[uid] = true, true
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
		uid := v.GetSpec().GetUid()
		state := v.GetStatus().GetState().String()
		states[state]++
		d.mu.Lock()
		first, ok := d.firstSee[uid]
		if !ok {
			first = time.Now()
			d.firstSee[uid] = first
		}
		d.mu.Unlock()

		role, job := RoleOf(v.GetSpec().GetId())
		reason := decide(vmFacts{State: state, Role: role, Job: job, Owned: owned[uid], InJob: inJob[uid],
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
	d.checkBuilders(present, listedAt)
}

// jobStateGlob matches the state files `executor prepare` writes (a variable for tests).
var jobStateGlob = "/run/firerunner/jobs/*.json"

// busyBuilders returns the projects whose builder a running job uses.
func busyBuilders() map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob(jobStateGlob)
	for _, f := range files {
		if st, err := vm.LoadJobState(f); err == nil && st.BuilderProject != "" {
			out[st.BuilderProject] = true
		}
	}
	return out
}

// jobStates maps microVM uid -> state file for jobs in progress (written by `executor prepare`).
func jobStates() map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(jobStateGlob)
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
