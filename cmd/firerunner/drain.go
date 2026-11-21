package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/host"
)

// `firerunner drain` empties the host for maintenance without failing a
// job: gitlab-runner stops gracefully (no new jobs; the ones it never took
// stay pending in GitLab), the daemon boots nothing new, and the command
// waits until nothing runs. `firerunner undrain` (or `drain --cancel`)
// takes jobs again. See host.StopRunnerGracefully and daemon/drain.go.

// drainDaemon is the daemon's side of a drain (daemon.Client).
type drainDaemon interface {
	SetDrain(ctx context.Context, on bool) (*daemon.DrainStatus, error)
	DrainStatus(ctx context.Context) (*daemon.DrainStatus, error)
}

// drainRunner is gitlab-runner's side (hostRunner).
type drainRunner interface {
	Stop(ctx context.Context) (bool, error)
	Resume(ctx context.Context) (host.Resume, error)
	State(ctx context.Context) (host.RunnerState, error)
	// DropDropIn removes the drain drop-in once gitlab-runner no longer
	// stops; false while it does.
	DropDropIn(ctx context.Context) (bool, error)
	// SettingsLeft: the drain's stop settings are still on disk or loaded.
	SettingsLeft(ctx context.Context) (bool, error)
}

// hostRunner is gitlab-runner's systemd unit on this host.
type hostRunner struct{}

func (hostRunner) Stop(ctx context.Context) (bool, error) { return host.StopRunnerGracefully(ctx) }
func (hostRunner) Resume(ctx context.Context) (host.Resume, error) {
	return host.ResumeRunner(ctx)
}
func (hostRunner) State(ctx context.Context) (host.RunnerState, error) {
	return host.ReadRunnerState(ctx)
}
func (hostRunner) DropDropIn(ctx context.Context) (bool, error) { return host.DropDrainDropIn(ctx) }
func (hostRunner) SettingsLeft(ctx context.Context) (bool, error) {
	return host.DrainSettingsLeft(ctx)
}

// drainer runs drain and undrain against a daemon and a gitlab-runner.
type drainer struct {
	daemon drainDaemon
	runner drainRunner
	out    io.Writer
	// poll is how often the wait looks again: the pace of the daemon's
	// main loop, which counts jobs for firerunner_jobs_running. Each look
	// costs a socket call and a systemctl show.
	poll time.Duration
	// progressEvery is how often an unchanged wait still prints its line,
	// so a long wait shows it is alive.
	progressEvery time.Duration
}

const (
	drainPoll          = 2 * time.Second
	drainProgressEvery = time.Minute
)

func newDrainer(cfg config.Config) drainer {
	return drainer{daemon: daemon.NewClient(cfg.Daemon.Socket), runner: hostRunner{}, out: os.Stdout,
		poll: drainPoll, progressEvery: drainProgressEvery}
}

// defaultDrainTimeout is the longest a job can still run: reconcile deletes
// the VM of a job older than daemon.job_max_age, on its next pass.
func defaultDrainTimeout(cfg config.Config) time.Duration {
	return cfg.Daemon.JobMaxAge + cfg.Daemon.ReconcileInterval
}

func cmdDrain(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	timeout := fs.Duration("timeout", defaultDrainTimeout(cfg),
		"how long to wait for the running jobs (default: daemon.job_max_age + daemon.reconcile_interval, the longest a job can run)")
	cancel := fs.Bool("cancel", false, "end the drain: the same as firerunner undrain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: firerunner drain [--timeout D] [--cancel]")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *cancel {
		return newDrainer(cfg).undrain(ctx, *timeout)
	}
	return newDrainer(cfg).drain(ctx, *timeout)
}

func cmdUndrain(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("undrain", flag.ContinueOnError)
	timeout := fs.Duration("timeout", defaultDrainTimeout(cfg),
		"how long to wait for a gitlab-runner that still finishes the jobs it ran when drained (default: daemon.job_max_age + daemon.reconcile_interval)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: firerunner undrain [--timeout D]")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newDrainer(cfg).undrain(ctx, *timeout)
}

// drain drains the daemon, then gitlab-runner, then waits until nothing
// runs. The drain stays when the wait ends early (timeout, Ctrl-C): running
// drain again waits again.
func (dr drainer) drain(ctx context.Context, timeout time.Duration) error {
	if _, err := dr.daemon.SetDrain(ctx, true); err != nil {
		return fmt.Errorf("the daemon did not take the drain; nothing was changed (systemctl status firerunner): %w", err)
	}
	fmt.Fprintln(dr.out, "drain: the daemon boots no pool VMs and no builders")
	asked, err := dr.runner.Stop(ctx)
	switch {
	case errors.Is(err, host.ErrRunnerStopping):
		fmt.Fprintln(dr.out, "warning:", err)
	case err != nil:
		return fmt.Errorf("the daemon is drained, but gitlab-runner still takes jobs: %w\n"+
			"fix that and run firerunner drain again, or firerunner undrain", err)
	case asked:
		fmt.Fprintln(dr.out, "drain: gitlab-runner takes no new jobs and exits once its running jobs finished")
	}
	return dr.wait(ctx, timeout)
}

// wait polls until no job runs, nothing boots and gitlab-runner has
// stopped, printing a progress line when it changes (and every
// progressEvery while it does not).
func (dr drainer) wait(ctx context.Context, timeout time.Duration) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(dr.poll)
	defer tick.Stop()
	var last, line, lastStopErr string
	var lastAt time.Time
	for {
		st, rs, err := dr.look(ctx)
		if err == nil && st.Drained && rs.Running() && dr.stillDrained(ctx) {
			// Started meanwhile (by hand, or a start an undrain queued
			// behind the stop): stop it again. Not when an undrain started
			// it: undrain ends the daemon's drain before it starts
			// gitlab-runner, so the drain status read after seeing it run
			// says so (stillDrained).
			if asked, serr := dr.runner.Stop(ctx); serr != nil {
				if serr.Error() != lastStopErr {
					fmt.Fprintln(dr.out, "warning: gitlab-runner was started during the drain and could not be stopped:", serr)
					lastStopErr = serr.Error()
				}
			} else if asked {
				fmt.Fprintln(dr.out, "drain: gitlab-runner was started during the drain; stopping it again")
			}
		}
		if ctx.Err() == nil {
			var done bool
			line, done, err = drainProgress(st, rs, err)
			if err != nil {
				return err
			}
			if done {
				fmt.Fprintln(dr.out, "drained: no job runs and gitlab-runner has stopped. Safe to upgrade or reboot;")
				fmt.Fprintln(dr.out, "firerunner undrain takes jobs again (a reboot starts gitlab-runner and ends the drain)")
				return nil
			}
			if line != last || time.Since(lastAt) >= dr.progressEvery {
				fmt.Fprintf(dr.out, "waiting %s: %s\n", time.Since(start).Round(time.Second), line)
				last, lastAt = line, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			why := "stopped waiting"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				why = "not drained after " + timeout.String()
			}
			return fmt.Errorf("%s (%s). The host stays drained: firerunner drain waits again, firerunner undrain takes jobs again",
				why, line)
		case <-tick.C:
		}
	}
}

// stillDrained reads the daemon's drain status again; an error reads as not
// drained, so gitlab-runner is left alone when in doubt.
func (dr drainer) stillDrained(ctx context.Context) bool {
	st, err := dr.daemon.DrainStatus(ctx)
	return err == nil && st.Drained
}

// look reads the daemon's drain status and gitlab-runner's state.
func (dr drainer) look(ctx context.Context) (*daemon.DrainStatus, host.RunnerState, error) {
	st, err := dr.daemon.DrainStatus(ctx)
	if err != nil {
		return nil, host.RunnerState{}, fmt.Errorf("daemon not answering: %w", err)
	}
	rs, err := dr.runner.State(ctx)
	if err != nil {
		return st, host.RunnerState{}, err
	}
	return st, rs, nil
}

// errUndrained ends a wait: someone ran undrain meanwhile.
var errUndrained = errors.New("the host was undrained while drain waited")

// drainProgress turns what look read into the progress line, and says
// whether the host is drained. A daemon or systemctl that does not answer
// is retried (the daemon restarts during an upgrade); an undrain meanwhile
// ends the wait.
func drainProgress(st *daemon.DrainStatus, rs host.RunnerState, lookErr error) (line string, done bool, err error) {
	if lookErr != nil {
		return lookErr.Error() + "; retrying", false, nil
	}
	if !st.Drained {
		return "", false, errUndrained
	}
	var parts []string
	if st.JobsRunning > 0 {
		parts = append(parts, plural(st.JobsRunning, "job")+" running")
	}
	switch {
	case rs.Running():
		parts = append(parts, "gitlab-runner is running and takes jobs (firerunner drain stops it)")
	case rs.Stopping():
		parts = append(parts, "gitlab-runner finishing its jobs")
	}
	if st.PoolBooting > 0 {
		parts = append(parts, plural(st.PoolBooting, "pool VM")+" booting")
	}
	if st.BuildersBooting > 0 {
		parts = append(parts, plural(st.BuildersBooting, "builder")+" booting")
	}
	if len(parts) == 0 {
		return "nothing runs", true, nil
	}
	return strings.Join(parts, ", "), false, nil
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// undrain ends the daemon's drain, then starts gitlab-runner again if
// drain stopped it. The daemon goes first: a drain still waiting stops a
// gitlab-runner it sees running only while the daemon is drained, so it
// cannot stop the one undrain starts. When gitlab-runner still finishes the
// jobs it ran when drained, it starts once they are done; undrain then waits
// for that, at most timeout, to remove the drain's drop-in, which would make
// every later stop of gitlab-runner wait for its jobs.
func (dr drainer) undrain(ctx context.Context, timeout time.Duration) error {
	if _, err := dr.daemon.SetDrain(ctx, false); err != nil {
		return fmt.Errorf("the daemon did not end the drain; nothing was changed (systemctl status firerunner): %w", err)
	}
	fmt.Fprintln(dr.out, "undrain: the daemon boots pool VMs and builders again")
	res, rerr := dr.runner.Resume(ctx)
	switch {
	case rerr != nil:
		rerr = fmt.Errorf("gitlab-runner: %w", rerr)
	case res == host.ResumeStarted:
		fmt.Fprintln(dr.out, "undrain: gitlab-runner started; it takes jobs again")
	case res == host.ResumeDropped:
		fmt.Fprintln(dr.out, "undrain: the drain's stop settings an earlier undrain could not unload are removed")
	case res == host.ResumeAfterStop:
		fmt.Fprintln(dr.out, "undrain: gitlab-runner still finishes the jobs it ran when drained and cannot take jobs before;\n"+
			"it starts again once they are done")
	default:
		if rs, err := dr.runner.State(ctx); err == nil && !rs.Running() {
			fmt.Fprintln(dr.out, "undrain: gitlab-runner was not stopped by drain and is not running; start it with: sudo systemctl start gitlab-runner")
		}
	}
	if rerr == nil && res == host.ResumeAfterStop {
		return dr.waitDropIn(ctx, timeout)
	}
	return rerr
}

// waitDropIn waits until gitlab-runner no longer stops and then removes
// the drain drop-in. Until then its stop settings stay: removing them
// earlier would end the stop by killing the jobs.
func (dr drainer) waitDropIn(ctx context.Context, timeout time.Duration) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(dr.poll)
	defer tick.Stop()
	var last string
	var lastAt time.Time
	for {
		gone, err := dr.runner.DropDropIn(ctx)
		if ctx.Err() == nil {
			if gone && err == nil {
				fmt.Fprintln(dr.out, "undrain: gitlab-runner finished those jobs and starts again; the drain's stop settings are removed")
				return nil
			}
			line := "gitlab-runner finishing the jobs it ran when drained"
			if err != nil {
				line = err.Error() + "; retrying"
			}
			if line != last || time.Since(lastAt) >= dr.progressEvery {
				fmt.Fprintf(dr.out, "waiting %s: %s\n", time.Since(start).Round(time.Second), line)
				last, lastAt = line, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			why := "stopped waiting"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				why = "still after " + timeout.String()
			}
			return fmt.Errorf("gitlab-runner still finishes the jobs it ran when drained (%s). It starts once they are done, "+
				"but keeps the drain's stop settings (%s), so every stop of it waits for its jobs. "+
				"Once 'systemctl is-active gitlab-runner' no longer prints deactivating, run: sudo firerunner undrain", why, host.DrainDropIn())
		case <-tick.C:
		}
	}
}

// drainLine is the Drain line of firerunner status.
func drainLine(ctx context.Context, d drainDaemon, r drainRunner) string {
	st, err := d.DrainStatus(ctx)
	if err != nil {
		return "unknown: daemon not reachable"
	}
	if !st.Drained {
		if left, err := r.SettingsLeft(ctx); err == nil && left {
			return "no, but gitlab-runner keeps the drain's stop settings (" + host.DrainDropIn() +
				"): every stop of it waits for its jobs; firerunner undrain removes them once it no longer stops"
		}
		return "no"
	}
	since := "since " + st.Since.Local().Format("2006-01-02 15:04")
	rs, rerr := r.State(ctx)
	if rerr != nil {
		return fmt.Sprintf("%s: %s running; gitlab-runner unknown (%v)", since, plural(st.JobsRunning, "job"), rerr)
	}
	line, done, _ := drainProgress(st, rs, nil)
	if done {
		return since + ": nothing runs, safe to upgrade (firerunner undrain takes jobs again)"
	}
	return since + ": " + line
}
