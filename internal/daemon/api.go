package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Event is sent by `firerunner executor` so job metrics live in the daemon.
type Event struct {
	Kind    string  `json:"kind"`   // prepare | finish
	Source  string  `json:"source"` // pool | cold (prepare)
	Result  string  `json:"result"` // success | failed (finish)
	Seconds float64 `json:"seconds"`
	OK      bool    `json:"ok"`
}

// apiHandler serves the local unix socket (root only).
func (d *Daemon) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /claim", func(w http.ResponseWriter, r *http.Request) {
		inst := d.Claim()
		if inst == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		d.log.Info("pool VM claimed", "id", inst.ID, "job", r.URL.Query().Get("job"))
		_ = json.NewEncoder(w).Encode(inst)
	})
	mux.HandleFunc("POST /event", func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&e); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.record(e)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pool/refresh", func(w http.ResponseWriter, r *http.Request) {
		// Replace every idle VM, e.g. after a new rootfs image was published under the same tag.
		go d.drain(context.Background(), "refresh")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /pool", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		type entry struct {
			ID, IP string
			Idle   string
		}
		out := struct {
			Target, Booting int
			Ready           []entry
		}{Target: d.cfg.Pool.Size, Booting: d.booting}
		for _, p := range d.ready {
			out.Ready = append(out.Ready, entry{p.inst.ID, p.inst.IP, time.Since(p.bornAt).Round(time.Second).String()})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

func (d *Daemon) record(e Event) {
	switch e.Kind {
	case "prepare":
		if e.OK {
			d.metrics.prepareSeconds.WithLabelValues(e.Source).Observe(e.Seconds)
			if e.Source == "cold" {
				d.metrics.bootSeconds.WithLabelValues("cold").Observe(e.Seconds)
			}
		} else {
			d.metrics.bootFailures.WithLabelValues(e.Source).Inc()
		}
	case "finish":
		d.metrics.jobs.WithLabelValues(e.Result).Inc()
		d.metrics.jobSeconds.Observe(e.Seconds)
	}
}
