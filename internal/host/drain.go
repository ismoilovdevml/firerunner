package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Draining stops gitlab-runner gracefully: on SIGQUIT it requests no new
// jobs, waits for the running ones and exits; the jobs it never took stay
// pending in GitLab for other runners or for this one once it is back.
// Its unit cannot do that as installed (`gitlab-runner install`):
//   - Restart=always would start it again after a SIGQUIT sent by hand;
//   - systemctl stop sends SIGTERM, which cancels the running jobs;
//   - it sends that signal to the whole cgroup, so with SIGQUIT the job
//     processes (firerunner executor, ssh) would get it too and die;
//   - after 90 s (systemd's default stop timeout) it kills what is left.
//
// So drain adds a runtime drop-in: KillMode=mixed (only gitlab-runner gets
// the signal; leftovers are killed once it has exited), KillSignal=SIGQUIT,
// and no stop timeout: daemon.job_max_age bounds the jobs (reconcile deletes
// a job's VM past it, which ends the job). A runtime drop-in is gone at
// reboot, like the drain record and like the stop itself: gitlab-runner is
// enabled and starts at boot.

// RunnerUnit is gitlab-runner's systemd unit.
const RunnerUnit = "gitlab-runner"

// runnerDropInDir holds gitlab-runner's runtime drop-ins (a variable for tests).
var runnerDropInDir = "/run/systemd/system/" + RunnerUnit + ".service.d"

// runnerDropInName sorts after the drop-ins an operator is likely to have
// (GitLab's docs suggest kill.conf), so its settings are the ones that apply.
const runnerDropInName = "zz-firerunner-drain.conf"

const runnerDropIn = `# Written by firerunner drain, removed by firerunner undrain; gone at reboot.
# Stopping gitlab-runner lets its running jobs finish and takes no new ones.
[Service]
KillMode=mixed
KillSignal=SIGQUIT
TimeoutStopSec=infinity
`

// ErrRunnerStopping is returned when gitlab-runner was already being
// stopped, not gracefully: its running jobs are being cancelled.
var ErrRunnerStopping = errors.New("gitlab-runner was already stopping, not gracefully: its running jobs are cancelled")

// RunnerState is what systemd says about gitlab-runner.
type RunnerState struct {
	// Active is systemd's ActiveState: active, activating, deactivating,
	// inactive, failed, ...
	Active string
	// Graceful: a stop now would let the running jobs finish (the drain
	// drop-in is in effect).
	Graceful bool
	// DrainLoaded: systemd has the drain drop-in loaded (DropInPaths). It
	// stays loaded after the file is removed until a daemon-reload went
	// through.
	DrainLoaded bool
}

// Running reports whether gitlab-runner takes jobs or is about to.
func (s RunnerState) Running() bool {
	switch s.Active {
	case "inactive", "failed", "deactivating", "":
		return false
	}
	return true
}

// Stopping reports whether gitlab-runner is stopping: with a graceful stop,
// it finishes its running jobs first.
func (s RunnerState) Stopping() bool { return s.Active == "deactivating" }

// ReadRunnerState asks systemd for gitlab-runner's state. A host without
// the unit reads as inactive.
func ReadRunnerState(ctx context.Context) (RunnerState, error) {
	out, err := probe(ctx, "systemctl", "show", RunnerUnit, "-p", "ActiveState", "-p", "KillMode", "-p", "KillSignal",
		"-p", "TimeoutStopUSec", "-p", "DropInPaths")
	if err != nil {
		return RunnerState{}, fmt.Errorf("systemctl show %s: %w", RunnerUnit, err)
	}
	props := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			props[k] = v
		}
	}
	if props["ActiveState"] == "" {
		return RunnerState{}, fmt.Errorf("systemctl show %s: no ActiveState in %q", RunnerUnit, out)
	}
	return RunnerState{
		Active: props["ActiveState"],
		Graceful: props["KillMode"] == "mixed" && props["KillSignal"] == strconv.Itoa(int(syscall.SIGQUIT)) &&
			props["TimeoutStopUSec"] == "infinity",
		DrainLoaded: slices.Contains(strings.Fields(props["DropInPaths"]), DrainDropIn()),
	}, nil
}

// StopRunnerGracefully asks gitlab-runner to stop taking jobs and to exit
// once its running jobs finished; it does not wait for that. It reports
// whether it asked: false when gitlab-runner was not running. It never
// stops gitlab-runner when systemd would not stop it gracefully.
//
// A gitlab-runner already stopping gracefully is asked again: an undrain
// meanwhile may have queued a start behind that stop, which would start it
// on a drained host once its jobs finished. A stop job replaces a start job
// that waits, and for a unit already stopping it changes nothing else.
func StopRunnerGracefully(ctx context.Context) (bool, error) {
	st, err := ReadRunnerState(ctx)
	if err != nil {
		return false, err
	}
	switch {
	case st.Stopping() && st.Graceful:
		return true, systemctl(ctx, "stop", "--no-block", RunnerUnit)
	case st.Stopping():
		return false, ErrRunnerStopping
	case !st.Running():
		return false, nil
	}
	path := DrainDropIn()
	if cur, err := os.ReadFile(path); err != nil || string(cur) != runnerDropIn {
		if err := os.MkdirAll(runnerDropInDir, 0o755); err != nil {
			return false, err
		}
		if err := writeRootOnly(path, []byte(runnerDropIn)); err != nil {
			return false, err
		}
		if err := systemctl(ctx, "daemon-reload"); err != nil {
			_ = os.Remove(path)
			return false, err
		}
	}
	if st, err = ReadRunnerState(ctx); err != nil {
		return false, err
	}
	if !st.Graceful {
		// Another drop-in overrides it: a stop would cancel the jobs.
		_ = os.Remove(path)
		_ = systemctl(ctx, "daemon-reload")
		return false, fmt.Errorf("not stopping %s: systemd would not stop it gracefully with %s (check: systemctl cat %s)",
			RunnerUnit, path, RunnerUnit)
	}
	return true, systemctl(ctx, "stop", "--no-block", RunnerUnit)
}

// Resume is what ResumeRunner did.
type Resume int

const (
	// ResumeNothing: drain had not stopped gitlab-runner.
	ResumeNothing Resume = iota
	// ResumeStarted: gitlab-runner is starting.
	ResumeStarted
	// ResumeAfterStop: gitlab-runner still finishes the jobs it ran when
	// drained (a stopping gitlab-runner cannot take jobs again); it starts
	// once it has exited. The drain drop-in is still there: DropDrainDropIn
	// removes it once gitlab-runner no longer stops.
	ResumeAfterStop
)

// DrainDropIn is the drop-in drain adds to gitlab-runner's unit.
func DrainDropIn() string { return filepath.Join(runnerDropInDir, runnerDropInName) }

// HasDrainDropIn reports whether the drain drop-in is in place: every stop
// of gitlab-runner then waits for its jobs, up to daemon.job_max_age.
func HasDrainDropIn() bool {
	_, err := os.Stat(DrainDropIn())
	return err == nil
}

// ResumeRunner starts gitlab-runner again if drain stopped it (the drain
// drop-in is there), then removes the drop-in. While gitlab-runner still
// stops, the start waits behind that stop and the drop-in stays
// (ResumeAfterStop): a reload without it would end that stop by killing the
// jobs; DropDrainDropIn removes it later. A start that fails keeps it too,
// so undrain can be run again. The Resume tells what was done, also with an
// error.
func ResumeRunner(ctx context.Context) (Resume, error) {
	if !HasDrainDropIn() {
		return ResumeNothing, nil
	}
	if err := systemctl(ctx, "start", "--no-block", RunnerUnit); err != nil {
		return ResumeNothing, err
	}
	dropped, err := DropDrainDropIn(ctx)
	switch {
	case err != nil:
		return ResumeStarted, err
	case !dropped:
		return ResumeAfterStop, nil
	}
	return ResumeStarted, nil
}

// DropDrainDropIn removes the drain drop-in once gitlab-runner no longer
// stops, and reports whether it is gone: neither on disk nor loaded by
// systemd. While gitlab-runner stops it stays: a reload without it would set
// the stop timeout back to 90 s and kill the jobs at its end. A file removed
// whose daemon-reload failed is still loaded: the next call reloads again.
func DropDrainDropIn(ctx context.Context) (bool, error) {
	st, err := ReadRunnerState(ctx)
	if err != nil {
		return false, err
	}
	onDisk := HasDrainDropIn()
	switch {
	case !onDisk && !st.DrainLoaded:
		return true, nil
	case st.Stopping():
		return false, nil
	}
	if onDisk {
		if err := os.Remove(DrainDropIn()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return false, err
	}
	if st, err = ReadRunnerState(ctx); err != nil {
		return false, err
	}
	return !st.DrainLoaded, nil
}

// DrainSettingsLeft reports whether gitlab-runner still has the drain's stop
// settings, on disk or loaded: every stop of it then waits for its jobs.
func DrainSettingsLeft(ctx context.Context) (bool, error) {
	if HasDrainDropIn() {
		return true, nil
	}
	st, err := ReadRunnerState(ctx)
	return st.DrainLoaded, err
}

// systemctl runs systemctl with commandTimeout; the error carries its output.
func systemctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := command(ctx, "systemctl", args...)
	cmd.WaitDelay = time.Second // children holding the output must not keep it waiting
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
