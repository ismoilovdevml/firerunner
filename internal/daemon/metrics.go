package daemon

import (
	"net/http"

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
}

func NewMetrics() *Metrics {
	bootBuckets := []float64{2, 5, 8, 10, 12, 15, 20, 30, 45, 60, 90, 120}
	m := &Metrics{
		builders: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_builders",
			Help: "Per-project BuildKit builder microVMs (booting and ready)."}),
		builderRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_builder_requests_total",
			Help: "Jobs asking for their project's builder, by answer: ready (warm cache), booting, busy, disabled."}, []string{"state"}),
		reg:         prometheus.NewRegistry(),
		poolTarget:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_target", Help: "Configured number of pre-booted microVMs."}),
		poolReady:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_ready", Help: "Pre-booted microVMs ready to be claimed."}),
		poolBooting: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_pool_booting", Help: "Pool microVMs currently booting."}),
		claims: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_pool_claims_total",
			Help: "Pool claims by jobs; result is hit (VM handed out) or miss (job cold-boots)."}, []string{"result"}),
		bootSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "firerunner_vm_boot_seconds",
			Help: "Time from create request to SSH ready.", Buckets: bootBuckets}, []string{"kind"}),
		preloadSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "firerunner_pool_preload_seconds",
			Help: "Time to pull pool.preload_images into a pool VM.", Buckets: []float64{5, 10, 20, 30, 60, 90, 120, 180, 300, 600}}),
		bootFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_vm_boot_failures_total",
			Help: "microVMs that did not become ready."}, []string{"kind"}),
		prepareSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "firerunner_job_prepare_seconds",
			Help: "Time a job waited for its microVM.", Buckets: []float64{0.5, 1, 2, 5, 10, 15, 20, 30, 60, 120}}, []string{"source"}),
		jobs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_jobs_total",
			Help: "Finished jobs by result."}, []string{"result"}),
		jobSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "firerunner_job_duration_seconds",
			Help: "Job duration from prepare to cleanup.", Buckets: prometheus.ExponentialBuckets(5, 2, 10)}),
		microvms: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_microvms",
			Help: "microVMs known to flintlock by state."}, []string{"state"}),
		orphansDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_orphans_deleted_total",
			Help: "microVMs deleted by reconcile."}, []string{"reason"}),
		admissionWaits: prometheus.NewCounter(prometheus.CounterOpts{Name: "firerunner_admission_waits_total",
			Help: "Times a boot was postponed because host memory was short."}),
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
	for _, r := range []string{"success", "failed"} {
		m.jobs.WithLabelValues(r)
	}
	for _, k := range []string{"pool", "cold"} {
		m.bootSeconds.WithLabelValues(k)
		m.prepareSeconds.WithLabelValues(k)
		m.bootFailures.WithLabelValues(k)
	}
	m.bootFailures.WithLabelValues("preload")
	m.bootFailures.WithLabelValues("builder")
	m.bootSeconds.WithLabelValues("builder")
	for _, s := range []string{"ready", "booting", "busy", "disabled"} {
		m.builderRequests.WithLabelValues(s)
	}

	info := prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_build_info", Help: "Build version.",
		ConstLabels: prometheus.Labels{"version": Version}})
	info.Set(1)
	m.reg.MustRegister(m.poolTarget, m.poolReady, m.poolBooting, m.claims, m.bootSeconds, m.preloadSeconds, m.bootFailures,
		m.prepareSeconds, m.jobs, m.jobSeconds, m.microvms, m.orphansDeleted, m.admissionWaits,
		m.flintlockUp, m.serviceUp, m.thinPool, m.memAvailable, m.runnerConcurrent, m.builders, m.builderRequests, info,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (d *Daemon) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(d.metrics.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}
