package daemon

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version is set by main for firerunner_build_info.
var Version = "dev"

type Metrics struct {
	reg *prometheus.Registry

	poolTarget, poolReady, poolBooting prometheus.Gauge
	claims                             *prometheus.CounterVec
	bootSeconds                        *prometheus.HistogramVec
	preloadSeconds                     prometheus.Histogram
	bootFailures                       *prometheus.CounterVec
	prepareSeconds                     *prometheus.HistogramVec
	jobs                               *prometheus.CounterVec
	jobSeconds                         prometheus.Histogram
	microvms                           *prometheus.GaugeVec
	orphansDeleted                     *prometheus.CounterVec
	admissionWaits                     prometheus.Counter
	flintlockUp                        prometheus.Gauge
	serviceUp                          *prometheus.GaugeVec
	thinPool                           *prometheus.GaugeVec
	memAvailable                       prometheus.Gauge
	runnerConcurrent                   prometheus.Gauge
	builders                           prometheus.Gauge
	builderRequests                    *prometheus.CounterVec
	admissionWait                      prometheus.Histogram
	memCommitted                       *prometheus.GaugeVec
	memCapacity                        prometheus.Gauge
	dhcpLeases                         *prometheus.GaugeVec
	dhcpCapacity                       prometheus.Gauge
	hostOOMKills                       prometheus.Counter
	loopTick                           prometheus.Gauge
	builderCache                       *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	// Cold boots take 10-20 s; boot_timeout is 180 s.
	bootBuckets := []float64{2, 5, 8, 10, 12, 14, 16, 18, 20, 25, 30, 45, 60, 90, 120, 180}
	m := &Metrics{
		builders: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_builders",
			Help: "Per-project BuildKit builder microVMs (booting and ready)."}),
		builderRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_builder_requests_total",
			Help: "Jobs asking for their project's builder, by answer: ready (warm cache), booting, busy, disabled."}, []string{"state"}),
		builderCache: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_builder_cache_total",
			Help: "Builder caches copied to the host when a builder is deleted (save) and loaded into the project's next builder (restore), by result: ok, failed, skipped (no room)."}, []string{"op", "result"}),
		reg:         prometheus.NewRegistry(),
		poolTarget:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_target", Help: "Configured number of pre-booted microVMs."}),
		poolReady:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_ready", Help: "Pre-booted microVMs ready to be claimed."}),
		poolBooting: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_booting", Help: "Pool microVMs currently booting."}),
		claims: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_pool_claims_total",
			Help: "Claim requests to the pool, by result (hit, miss). A job waiting for memory asks again every few seconds, so misses are not jobs; the share of jobs that got a warm VM is firerunner_job_prepare_seconds_count by source."}, []string{"result"}),
		bootSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "firerunner_vm_boot_seconds",
			Help: "Time from create request to SSH ready, by kind (pool, cold, builder).", Buckets: bootBuckets}, []string{"kind"}),
		preloadSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "firerunner_pool_preload_seconds",
			Help: "Time to pull pool.preload_images into a pool VM.", Buckets: []float64{5, 10, 20, 30, 60, 90, 120, 180, 300, 600, 900}}),
		bootFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_vm_boot_failures_total",
			Help: "microVMs that did not become ready, by kind (pool, cold, builder, preload, pool_dead: a pool VM that no longer answered when a job claimed it)."}, []string{"kind"}),
		prepareSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "firerunner_job_prepare_seconds",
			Help:    "Time a job waited until its microVM was ready, by source (pool, cold).",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300}}, []string{"source"}),
		jobs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_jobs_total",
			Help: "Finished jobs by result (success, script_failure: the job's own commands failed, system_failure: FireRunner or the host failed it) and reason (for system failures)."}, []string{"result", "reason"}),
		jobSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "firerunner_job_duration_seconds",
			Help:    "Job duration from prepare to cleanup.",
			Buckets: []float64{10, 20, 30, 45, 60, 90, 120, 180, 300, 450, 600, 900, 1200, 1800, 3600}}),
		admissionWait: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "firerunner_job_admission_wait_seconds",
			Help:    "Time a job that found no ready pool VM waited for host memory: cold boots, and jobs that took a pool VM that became ready while they waited. Jobs that got a ready pool VM at once are not counted.",
			Buckets: []float64{0.5, 1, 5, 10, 30, 60, 120, 180, 300}}),
		memCommitted: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_memory_committed_bytes",
			Help: "Guest memory (plus Firecracker overhead) promised to microVMs, by role (pool, job, builder, run, other)."}, []string{"role"}),
		memCapacity: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_memory_capacity_bytes",
			Help: "Host memory microVMs may commit (MemTotal minus vm.host_reserve_mb)."}),
		dhcpLeases: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_dhcp_leases",
			Help: "Unexpired DHCP leases on the microVM bridge, by state (live: held by a microVM flintlock lists, stale: anything else)."}, []string{"state"}),
		dhcpCapacity: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_dhcp_leases_capacity",
			Help: "Addresses in the microVM DHCP range."}),
		hostOOMKills: prometheus.NewCounter(prometheus.CounterOpts{Name: "firerunner_host_oom_kills_total",
			Help: "Processes the host kernel OOM-killed while the daemon ran (usually a firecracker microVM)."}),
		loopTick: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_loop_last_tick_timestamp_seconds",
			Help: "When the daemon's main loop last completed a pass (it passes every 2 s); an old value means it is stuck."}),
		microvms: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_microvms",
			Help: "microVMs known to flintlock by state."}, []string{"state"}),
		orphansDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_orphans_deleted_total",
			Help: "microVMs deleted by reconcile."}, []string{"reason"}),
		admissionWaits: prometheus.NewCounter(prometheus.CounterOpts{Name: "firerunner_admission_waits_total",
			Help: "Pool refill passes (every 2 s) that could not boot every missing pool VM because host memory was short."}),
		flintlockUp: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_flintlock_up", Help: "1 if the flintlock API answers."}),
		serviceUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_service_up",
			Help: "1 if the systemd service is active."}, []string{"service"}),
		thinPool: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_thinpool_usage_ratio",
			Help: "devmapper thin pool usage (0-1)."}, []string{"type"}),
		memAvailable:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_host_memory_available_bytes", Help: "MemAvailable on the host."}),
		runnerConcurrent: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_runner_concurrent", Help: "gitlab-runner concurrent limit."}),
	}
	// Pre-create every label combination so dashboards show 0 instead of
	// "No data" before the first job.
	for _, r := range []string{"hit", "miss"} {
		m.claims.WithLabelValues(r)
	}
	m.jobs.WithLabelValues(ResultSuccess, "none")
	m.jobs.WithLabelValues(ResultScriptFailure, "none")
	for _, r := range FailureReasons {
		m.jobs.WithLabelValues(ResultSystemFailure, r)
	}
	for _, r := range roles {
		m.memCommitted.WithLabelValues(r)
	}
	for _, s := range []string{"live", "stale"} {
		m.dhcpLeases.WithLabelValues(s)
	}
	for _, k := range []string{"pool", "cold"} {
		m.bootSeconds.WithLabelValues(k)
		m.prepareSeconds.WithLabelValues(k)
		m.bootFailures.WithLabelValues(k)
	}
	m.bootFailures.WithLabelValues("preload")
	m.bootFailures.WithLabelValues("pool_dead")
	m.bootFailures.WithLabelValues("builder")
	m.bootSeconds.WithLabelValues("builder")
	for _, s := range []string{"ready", "booting", "busy", "disabled"} {
		m.builderRequests.WithLabelValues(s)
	}
	for _, r := range []string{"ok", "failed", "skipped"} {
		m.builderCache.WithLabelValues("save", r)
	}
	m.builderCache.WithLabelValues("restore", "ok")
	m.builderCache.WithLabelValues("restore", "failed")

	info := prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_build_info", Help: "Build version.",
		ConstLabels: prometheus.Labels{"version": Version}})
	info.Set(1)
	m.reg.MustRegister(m.poolTarget, m.poolReady, m.poolBooting, m.claims, m.bootSeconds, m.preloadSeconds, m.bootFailures,
		m.prepareSeconds, m.jobs, m.jobSeconds, m.microvms, m.orphansDeleted, m.admissionWaits,
		m.flintlockUp, m.serviceUp, m.thinPool, m.memAvailable, m.runnerConcurrent, m.builders, m.builderRequests, m.builderCache, info,
		m.admissionWait, m.memCommitted, m.memCapacity, m.dhcpLeases, m.dhcpCapacity, m.hostOOMKills, m.loopTick,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// roles are the microVM roles committed memory is reported by.
var roles = []string{"pool", "job", "builder", "run", "other"}

// stuckAfter is how long the main loop may go without a pass before /healthz fails.
const stuckAfter = 2 * time.Minute

// tick records a completed pass of the main loop.
func (d *Daemon) tick() {
	now := time.Now()
	d.lastTick.Store(now.Unix())
	d.metrics.loopTick.Set(float64(now.UnixNano()) / 1e9)
}

func (d *Daemon) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(d.metrics.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// The loop passes every 2 s; a hung flintlock call or exec stops it.
		if last := d.lastTick.Load(); last > 0 && time.Since(time.Unix(last, 0)) > stuckAfter {
			http.Error(w, "main loop stuck since "+time.Unix(last, 0).UTC().Format(time.RFC3339), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
