package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/host"
)

// fakeDrainDaemon answers DrainStatus from a script, one entry per call;
// the last one repeats. A nil entry is a daemon that does not answer.
type fakeDrainDaemon struct {
	setErr   error
	statuses []*daemon.DrainStatus
	calls    []string // "drain", "undrain", "status"
}

var errDown = errors.New("connection refused")

func (f *fakeDrainDaemon) SetDrain(_ context.Context, on bool) (*daemon.DrainStatus, error) {
	if on {
		f.calls = append(f.calls, "drain")
	} else {
		f.calls = append(f.calls, "undrain")
	}
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &daemon.DrainStatus{Drained: on}, nil
}

func (f *fakeDrainDaemon) DrainStatus(context.Context) (*daemon.DrainStatus, error) {
	f.calls = append(f.calls, "status")
	st := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	if st == nil {
		return nil, errDown
	}
	return st, nil
}

// fakeDrainRunner plays gitlab-runner; State answers from a script like
// fakeDrainDaemon.
type fakeDrainRunner struct {
	stopAsked bool
	stopErr   error
	resume    host.Resume
	resumeErr error
	states    []string // ActiveState per State call
	calls     []string
}

func (f *fakeDrainRunner) Stop(context.Context) (bool, error) {
	f.calls = append(f.calls, "stop")
	return f.stopAsked, f.stopErr
}

func (f *fakeDrainRunner) Resume(context.Context) (host.Resume, error) {
	f.calls = append(f.calls, "resume")
	return f.resume, f.resumeErr
}

func (f *fakeDrainRunner) State(context.Context) (host.RunnerState, error) {
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return host.RunnerState{Active: s, Graceful: true}, nil
}

func drained(jobs int) *daemon.DrainStatus {
	return &daemon.DrainStatus{Drained: true, Since: time.Now(), JobsRunning: jobs}
}

func testDrainer(d *fakeDrainDaemon, r *fakeDrainRunner) (drainer, *bytes.Buffer) {
	var out bytes.Buffer
	return drainer{daemon: d, runner: r, out: &out, poll: time.Millisecond, progressEvery: time.Hour}, &out
}

func TestDrain(t *testing.T) {
	cases := []struct {
		name     string
		d        *fakeDrainDaemon
		r        *fakeDrainRunner
		timeout  time.Duration
		wantErr  string   // "" for none
		want     []string // in the output, in this order
		notWant  string
		runnerOp []string
	}{
		{name: "waits until the jobs finished and gitlab-runner stopped",
			d: &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(2), drained(1), drained(0), drained(0)}},
			r: &fakeDrainRunner{stopAsked: true, states: []string{"deactivating", "deactivating", "deactivating", "inactive"}},
			want: []string{"gitlab-runner takes no new jobs", "2 jobs running, gitlab-runner finishing its jobs",
				"1 job running, gitlab-runner finishing its jobs", "waiting", "gitlab-runner finishing its jobs", "drained: no job runs"},
			runnerOp: []string{"stop"}},
		// gitlab-runner may have taken a job it has not prepared yet: no
		// state file, so only its exit says the host is empty.
		{name: "no job state but gitlab-runner still stopping",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(0)}},
			r:        &fakeDrainRunner{stopAsked: true, states: []string{"deactivating"}},
			timeout:  20 * time.Millisecond,
			wantErr:  "not drained after",
			runnerOp: []string{"stop"}},
		{name: "boots from before the drain are waited for",
			d: &fakeDrainDaemon{statuses: []*daemon.DrainStatus{
				{Drained: true, PoolBooting: 1, BuildersBooting: 2}, drained(0)}},
			r:        &fakeDrainRunner{states: []string{"inactive"}},
			want:     []string{"1 pool VM booting, 2 builders booting", "drained:"},
			runnerOp: []string{"stop"}},
		{name: "daemon refuses: gitlab-runner untouched",
			d:       &fakeDrainDaemon{setErr: errDown},
			r:       &fakeDrainRunner{},
			wantErr: "nothing was changed"},
		// A stop that would not be graceful is refused by host; the daemon
		// stays drained and the operator is told.
		{name: "gitlab-runner cannot be stopped gracefully",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(0)}},
			r:        &fakeDrainRunner{stopErr: errors.New("systemd would not stop it gracefully")},
			wantErr:  "still takes jobs",
			runnerOp: []string{"stop"}},
		{name: "gitlab-runner already stopping with SIGTERM: warn and wait",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(1), drained(0)}},
			r:        &fakeDrainRunner{stopErr: host.ErrRunnerStopping, states: []string{"deactivating", "inactive"}},
			want:     []string{"warning: gitlab-runner was already stopping", "drained:"},
			runnerOp: []string{"stop"}},
		// The daemon restarts during `firerunner upgrade`: retried.
		{name: "daemon not answering for a while",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(1), nil, drained(0)}},
			r:        &fakeDrainRunner{states: []string{"inactive"}},
			want:     []string{"1 job running", "daemon not answering", "retrying", "drained:"},
			runnerOp: []string{"stop"}},
		{name: "gitlab-runner started again meanwhile",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(0)}},
			r:        &fakeDrainRunner{states: []string{"active"}},
			timeout:  20 * time.Millisecond,
			wantErr:  "takes jobs",
			runnerOp: []string{"stop"}},
		{name: "undrained meanwhile",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(1), {Drained: false}}},
			r:        &fakeDrainRunner{states: []string{"inactive"}},
			wantErr:  "undrained while drain waited",
			runnerOp: []string{"stop"}},
		{name: "timeout: the drain stays, the unchanged line printed once",
			d:        &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(1)}},
			r:        &fakeDrainRunner{states: []string{"deactivating"}},
			timeout:  20 * time.Millisecond,
			wantErr:  "The host stays drained",
			want:     []string{"1 job running"},
			notWant:  "1 job running, gitlab-runner finishing its jobs\nwaiting",
			runnerOp: []string{"stop"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dr, out := testDrainer(c.d, c.r)
			timeout := c.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			err := dr.drain(context.Background(), timeout)
			if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("drain error %v, want %q\n%s", err, c.wantErr, out)
			}
			rest := out.String()
			for _, w := range c.want {
				i := strings.Index(rest, w)
				if i < 0 {
					t.Fatalf("output lacks %q (in order):\n%s", w, out)
				}
				rest = rest[i+len(w):]
			}
			if c.notWant != "" && strings.Contains(out.String(), c.notWant) {
				t.Fatalf("output has %q:\n%s", c.notWant, out)
			}
			if strings.Join(c.r.calls, ",") != strings.Join(c.runnerOp, ",") {
				t.Fatalf("gitlab-runner calls %v, want %v", c.r.calls, c.runnerOp)
			}
			if len(c.d.calls) == 0 || c.d.calls[0] != "drain" {
				t.Fatalf("daemon calls %v: the daemon is drained first", c.d.calls)
			}
			for _, call := range c.d.calls {
				if call == "undrain" {
					t.Fatal("drain undrained the daemon")
				}
			}
		})
	}
}

// Ctrl-C ends the wait, not the drain.
func TestDrainInterrupted(t *testing.T) {
	d := &fakeDrainDaemon{statuses: []*daemon.DrainStatus{drained(1)}}
	dr, _ := testDrainer(d, &fakeDrainRunner{states: []string{"deactivating"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)
	err := dr.drain(ctx, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "stopped waiting") || !strings.Contains(err.Error(), "stays drained") {
		t.Fatalf("interrupted drain: %v", err)
	}
}

func TestUndrain(t *testing.T) {
	cases := []struct {
		name    string
		d       *fakeDrainDaemon
		r       *fakeDrainRunner
		wantErr string
		want    []string
	}{
		{name: "gitlab-runner started", d: &fakeDrainDaemon{}, r: &fakeDrainRunner{resume: host.ResumeStarted},
			want: []string{"gitlab-runner started", "boots pool VMs and builders again"}},
		{name: "gitlab-runner still finishing", d: &fakeDrainDaemon{}, r: &fakeDrainRunner{resume: host.ResumeAfterStop},
			want: []string{"starts again once they are done", "boots pool VMs and builders again"}},
		// Stopped before the drain: left alone, but the operator is told.
		{name: "gitlab-runner not stopped by drain", d: &fakeDrainDaemon{},
			r:    &fakeDrainRunner{resume: host.ResumeNothing, states: []string{"inactive"}},
			want: []string{"systemctl start gitlab-runner", "boots pool VMs and builders again"}},
		// Both sides are tried: one failing does not keep the other drained.
		{name: "gitlab-runner fails, daemon still undrained", d: &fakeDrainDaemon{},
			r:       &fakeDrainRunner{resumeErr: errors.New("start failed")},
			wantErr: "gitlab-runner: start failed", want: []string{"boots pool VMs and builders again"}},
		{name: "daemon fails", d: &fakeDrainDaemon{setErr: errDown}, r: &fakeDrainRunner{resume: host.ResumeStarted},
			wantErr: "daemon is still drained", want: []string{"gitlab-runner started"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dr, out := testDrainer(c.d, c.r)
			err := dr.undrain(context.Background())
			if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("undrain error %v, want %q\n%s", err, c.wantErr, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("output lacks %q:\n%s", w, out)
				}
			}
			if strings.Join(c.d.calls, ",") != "undrain" {
				t.Fatalf("daemon calls %v, want one undrain", c.d.calls)
			}
		})
	}
}

func TestDrainLine(t *testing.T) {
	cases := []struct {
		name   string
		status *daemon.DrainStatus
		runner string
		want   string
	}{
		{"daemon down", nil, "active", "unknown"},
		{"not drained", &daemon.DrainStatus{}, "active", "no"},
		{"drained and empty", drained(0), "inactive", "safe to upgrade"},
		{"drained, jobs left", drained(3), "deactivating", "3 jobs running, gitlab-runner finishing its jobs"},
		{"drained but gitlab-runner back", drained(0), "active", "takes jobs"},
	}
	for _, c := range cases {
		d := &fakeDrainDaemon{statuses: []*daemon.DrainStatus{c.status}}
		got := drainLine(context.Background(), d, &fakeDrainRunner{states: []string{c.runner}})
		if !strings.Contains(got, c.want) || (c.want == "no" && got != "no") {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCmdDrainArgs(t *testing.T) {
	cfg := config.Default()
	if got, want := defaultDrainTimeout(cfg), 3*time.Hour+time.Minute; got != want {
		t.Errorf("default timeout %s, want job_max_age + reconcile_interval = %s", got, want)
	}
	for _, args := range [][]string{{"now"}, {"--timeout", "0s"}, {"--timeout", "-1m"}, {"--timeout", "soon"}} {
		if err := cmdDrain(cfg, args); err == nil {
			t.Errorf("drain %q accepted", args)
		}
	}
	if err := cmdUndrain(cfg, []string{"now"}); err == nil {
		t.Error("undrain with an argument accepted")
	}
}
