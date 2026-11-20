package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A drained host takes no new work for maintenance: `firerunner drain`
// stops gitlab-runner gracefully (it takes no new jobs, finishes the running
// ones) and tells the daemon, which then boots no pool VMs and no builders.
// What is already there keeps serving: ready pool VMs go to jobs gitlab-runner
// took before the drain, ready builders to their projects' jobs.
//
// The drain is recorded in drain.json next to the socket, in /run: it outlives
// a daemon restart (`firerunner upgrade` restarts the daemon while the host is
// drained) but not a reboot. That is on purpose: gitlab-runner is an enabled
// unit and starts at boot, so a drain kept past a reboot would only leave its
// jobs without a pool.

// DrainStatus is what the daemon reports about a drain (GET /drain).
type DrainStatus struct {
	Drained bool `json:"drained"`
	// Since is when the drain started; zero while not drained.
	Since time.Time `json:"since,omitzero"`
	// JobsRunning counts the jobs in progress: their state files, as
	// firerunner_jobs_running does.
	JobsRunning int `json:"jobs_running"`
	// PoolBooting and BuildersBooting are boots started before the drain
	// that have not finished.
	PoolBooting     int `json:"pool_booting"`
	BuildersBooting int `json:"builders_booting"`
}

// drainRecord is drain.json.
type drainRecord struct {
	Since time.Time `json:"since"`
}

func (d *Daemon) drainFile() string {
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "drain.json")
}

// loadDrain takes over the drain a previous daemon run recorded. A record
// that cannot be decoded still means drained, since the file's time: only a
// drain writes it, and booting VMs on a host being serviced is the worse
// mistake.
func (d *Daemon) loadDrain() {
	data, err := os.ReadFile(d.drainFile())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var rec drainRecord
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil || rec.Since.IsZero() {
		rec.Since = time.Now()
		if fi, serr := os.Stat(d.drainFile()); serr == nil {
			rec.Since = fi.ModTime()
		}
		d.log.Warn("drain record unreadable; staying drained (firerunner undrain ends it)", "file", d.drainFile(), "err", err)
	}
	d.mu.Lock()
	d.drainedSince = rec.Since
	d.mu.Unlock()
	d.metrics.drained.Set(1)
	d.log.Info("host drained: no pool refills or builder boots until firerunner undrain", "since", rec.Since)
}

// setDrained starts (on) or ends a drain. Starting one that runs keeps its
// start time. It fails, and changes nothing, when the record cannot be
// written or removed: a daemon restart would otherwise undo it.
func (d *Daemon) setDrained(on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if on == !d.drainedSince.IsZero() {
		return nil
	}
	if on {
		since := time.Now()
		data, err := json.Marshal(drainRecord{Since: since})
		if err == nil {
			err = writeFileSync(d.drainFile(), data)
		}
		if err != nil {
			return fmt.Errorf("recording the drain in %s: %w", d.drainFile(), err)
		}
		d.drainedSince = since
		d.metrics.drained.Set(1)
		d.log.Info("host drained: no pool refills or builder boots until firerunner undrain")
		return nil
	}
	if err := os.Remove(d.drainFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the drain record %s: %w", d.drainFile(), err)
	}
	d.drainedSince = time.Time{}
	d.metrics.drained.Set(0)
	d.wakeRefill()
	d.log.Info("host undrained: pool refills and builder boots resume")
	return nil
}

// drainedLocked reports whether the host is drained; the caller holds d.mu.
func (d *Daemon) drainedLocked() bool { return !d.drainedSince.IsZero() }

// drainStatus reports the drain and what still runs.
func (d *Daemon) drainStatus() DrainStatus {
	jobs := len(runningJobs()) // file reads: not under mu
	d.mu.Lock()
	defer d.mu.Unlock()
	st := DrainStatus{Drained: d.drainedLocked(), Since: d.drainedSince, JobsRunning: jobs, PoolBooting: d.booting}
	for _, b := range d.builders {
		if !b.ready {
			st.BuildersBooting++
		}
	}
	return st
}

// drainHandler serves GET, POST (drain) and DELETE (undrain) /drain; each
// answers the drain status.
func (d *Daemon) drainHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodDelete:
		if err := d.setDrained(r.Method == http.MethodPost); err != nil {
			d.log.Error("drain not changed", "method", r.Method, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(d.drainStatus())
}
