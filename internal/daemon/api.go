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
		d.spawn(func() { d.drain(context.Background(), "refresh") })
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /builder", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		_ = json.NewEncoder(w).Encode(d.Builder(q.Get("project"), q.Get("start") != "0"))
	})
	mux.HandleFunc("DELETE /builder", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		_ = json.NewEncoder(w).Encode(d.RemoveBuilders(q.Get("project"), q.Get("force") == "1"))
	})
	mux.HandleFunc("GET /builders", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		type entry struct {
			Project, ID, IP string
			Port            int
			Ready           bool
			Idle, Age       string
		}
		out := []entry{}
		for _, b := range d.builders {
			e := entry{Project: b.Project, ID: b.Instance.ID, IP: b.Instance.IP, Port: b.Port, Ready: b.ready,
				Idle: time.Since(b.LastUsed).Round(time.Second).String()}
			if b.ready {
				e.Age = time.Since(b.BornAt).Round(time.Second).String()
			}
			out = append(out, e)
		}
		_ = json.NewEncoder(w).Encode(out)
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
	// Event fields become metric labels: only accept known values.
	if e.Source != "pool" && e.Source != "cold" {
		e.Source = "cold"
	}
	if e.Result != "success" {
		e.Result = "failed"
	}
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
