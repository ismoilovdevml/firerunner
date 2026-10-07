package daemon

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// builderPhaseResults are the phases of a builder boot, in the order
// bootBuilder runs them, with the results each may end with: the series of
// firerunner_builder_boot_phase_seconds (all pre-created). restore ends with
// the result firerunner_builder_cache_total{op="restore"} counts; failed also
// covers a copy that could not be opened, which that counter does not count.
var builderPhaseResults = []struct {
	phase   string
	results []string
}{
	{"admit", []string{"ok", "failed"}},
	{"vm", []string{"ok", "failed"}},
	{"save_wait", []string{"ok", "timeout", "failed"}},
	{"restore", []string{"ok", "legacy", "missing", "stale", "failed"}},
	{"buildkitd", []string{"ok", "failed"}},
}

// bootPhases times the phases of one builder boot. A phase ends where the
// next begins, so the phases of a boot add up to its total; each is observed
// as it ends, so a boot that fails or is cut short keeps the phases it ran.
type bootPhases struct {
	now         func() time.Time
	hist        *prometheus.HistogramVec
	start, last time.Time
	done        []bootPhase
	result      string // ready, failed or abandoned, once finished
	total       time.Duration
}

type bootPhase struct {
	name, result string
	took         time.Duration
}

func newBootPhases(now func() time.Time, hist *prometheus.HistogramVec) *bootPhases {
	t := now()
	return &bootPhases{now: now, hist: hist, start: t, last: t}
}

// end closes the running phase as name with result.
func (p *bootPhases) end(name, result string) {
	t := p.now()
	took := t.Sub(p.last)
	p.last = t
	p.done = append(p.done, bootPhase{name: name, result: result, took: took})
	p.hist.WithLabelValues(name, result).Observe(took.Seconds())
}

// finish fixes the boot's result and total the first time it is called: a
// failed boot is finished before its VM is deleted, which is not boot time.
func (p *bootPhases) finish(result string) {
	if p.result == "" {
		p.result, p.total = result, p.now().Sub(p.start)
	}
}

// logAttrs are the fields of the boot's one log line: its result, the phase
// it failed in, each phase's seconds (and its result unless ok) and the total.
func (p *bootPhases) logAttrs() []any {
	attrs := []any{"result", p.result}
	if p.result == "failed" && len(p.done) > 0 {
		// Every failure ends the phase it happened in, then stops the boot.
		attrs = append(attrs, "failed_phase", p.done[len(p.done)-1].name)
	}
	for _, ph := range p.done {
		attrs = append(attrs, ph.name+"_s", roundSeconds(ph.took))
		if ph.result != "ok" {
			attrs = append(attrs, ph.name, ph.result)
		}
	}
	return append(attrs, "total_s", roundSeconds(p.total))
}

// roundSeconds is d in seconds to 0.1 s, the precision "took" is logged with.
func roundSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*10) / 10
}
