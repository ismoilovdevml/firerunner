package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeSystemd plays systemd for gitlab-runner's unit, with the job rules a
// drain relies on. They are modelled from systemd's behaviour, not run: the
// test hosts allow no unit changes.
//   - A drop-in counts once a daemon-reload has read it (DropInPaths),
//     and a removed one until a daemon-reload went through.
//   - stop on a running unit leaves it stopping (deactivating) while its
//     jobs still run; a stop that is not graceful kills them (jobsKilled).
//   - start on a stopping unit does not start it: the start job waits
//     behind the stop (queued) and runs once the process exited (exit).
//   - stop on a stopping or stopped unit replaces a waiting start job and
//     changes nothing else.
//   - A daemon-reload while stopping applies the new settings to that stop:
//     without the drop-in its timeout is 90 s again and the jobs die.
type fakeSystemd struct {
	mu         sync.Mutex
	active     string
	queued     bool            // a start job waits behind the stop
	loaded     bool            // the drain drop-in is loaded and in effect (graceful)
	inPaths    bool            // the drain drop-in is loaded (DropInPaths), maybe overridden
	override   bool            // another drop-in overrides it: never graceful
	jobsKilled bool            // a stop or reload would have killed the running jobs
	fail       map[string]bool // systemctl verbs that fail
	verbs      []string
}

func (f *fakeSystemd) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "systemctl" || len(args) == 0 {
		return exec.CommandContext(ctx, "sh", "-c", "exit 99")
	}
	verb := args[0]
	f.verbs = append(f.verbs, verb)
	if f.fail[verb] {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'Failed to "+verb+"' >&2; exit 1")
	}
	out := ""
	switch verb {
	case "daemon-reload":
		_, err := os.Stat(filepath.Join(runnerDropInDir, runnerDropInName))
		f.inPaths = err == nil
		f.loaded = f.inPaths && !f.override
		if f.active == "deactivating" && !f.loaded {
			f.jobsKilled = true
		}
	case "stop":
		f.queued = false
		switch f.active {
		case "deactivating", "inactive", "failed":
		default:
			if !f.loaded {
				f.jobsKilled = true
			}
			f.active = "deactivating"
		}
	case "start":
		switch f.active {
		case "deactivating":
			f.queued = true
		case "inactive", "failed":
			f.active = "active"
		}
	case "show":
		killMode, signal, timeout := "control-group", "15", "1min 30s"
		if f.loaded {
			killMode, signal, timeout = "mixed", "3", "infinity"
		}
		paths := "/etc/systemd/system/gitlab-runner.service.d/firerunner-proxy.conf"
		if f.loaded || f.inPaths {
			paths += " " + filepath.Join(runnerDropInDir, runnerDropInName)
		}
		out = fmt.Sprintf("ActiveState=%s\nKillMode=%s\nKillSignal=%s\nTimeoutStopUSec=%s\nDropInPaths=%s\n",
			f.active, killMode, signal, timeout, paths)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$OUT"`)
	cmd.Env = append(os.Environ(), "OUT="+out)
	return cmd
}

// exit is gitlab-runner exiting once its last job finished: a start job
// waiting behind the stop runs now.
func (f *fakeSystemd) exit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != "deactivating" {
		return
	}
	f.active = "inactive"
	if f.queued {
		f.active, f.queued = "active", false
	}
}

// fakeRunner points the drop-in directory at a temporary one and systemctl
// at f. Every test ends with the jobs alive: no stop or reload may kill them.
func fakeRunner(t *testing.T, f *fakeSystemd) string {
	t.Helper()
	oldCmd, oldDir := command, runnerDropInDir
	command = f.command
	runnerDropInDir = filepath.Join(t.TempDir(), "gitlab-runner.service.d")
	t.Cleanup(func() {
		command, runnerDropInDir = oldCmd, oldDir
		if f.jobsKilled {
			t.Error("a stop or daemon-reload killed gitlab-runner's running jobs")
		}
	})
	return filepath.Join(runnerDropInDir, runnerDropInName)
}

func putDropIn(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(runnerDropIn), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStopRunnerGracefully(t *testing.T) {
	cases := []struct {
		name       string
		sd         *fakeSystemd
		dropIn     bool // the drain drop-in is there before
		asked      bool
		wantErr    error // nil: none; errAny: some error
		verbs      []string
		active     string // afterwards
		wantDropIn bool   // afterwards
	}{
		{name: "running: drop-in, reload, check, stop", sd: &fakeSystemd{active: "active"},
			asked: true, verbs: []string{"show", "daemon-reload", "show", "stop"}, active: "deactivating", wantDropIn: true},
		{name: "restarting after a crash", sd: &fakeSystemd{active: "activating"},
			asked: true, verbs: []string{"show", "daemon-reload", "show", "stop"}, active: "deactivating", wantDropIn: true},
		{name: "drop-in already loaded: no reload", sd: &fakeSystemd{active: "active", loaded: true}, dropIn: true,
			asked: true, verbs: []string{"show", "show", "stop"}, active: "deactivating", wantDropIn: true},
		// Asked again: an undrain meanwhile may have queued a start behind
		// the stop (TestDrainAgainCancelsAQueuedStart).
		{name: "already stopping gracefully (drain again)", sd: &fakeSystemd{active: "deactivating", loaded: true}, dropIn: true,
			asked: true, verbs: []string{"show", "stop"}, active: "deactivating", wantDropIn: true},
		{name: "already stopping, a start queued by undrain", sd: &fakeSystemd{active: "deactivating", loaded: true, queued: true}, dropIn: true,
			asked: true, verbs: []string{"show", "stop"}, active: "deactivating", wantDropIn: true},
		{name: "already stopped by drain", sd: &fakeSystemd{active: "inactive", loaded: true}, dropIn: true,
			verbs: []string{"show"}, active: "inactive", wantDropIn: true},
		{name: "not running: left alone", sd: &fakeSystemd{active: "inactive"},
			verbs: []string{"show"}, active: "inactive"},
		{name: "failed: left alone", sd: &fakeSystemd{active: "failed"},
			verbs: []string{"show"}, active: "failed"},
		{name: "someone else stops it with SIGTERM", sd: &fakeSystemd{active: "deactivating"},
			wantErr: ErrRunnerStopping, verbs: []string{"show"}, active: "deactivating"},
		// A stop that is not graceful cancels every running job: never.
		{name: "another drop-in overrides it", sd: &fakeSystemd{active: "active", override: true},
			wantErr: errAny, verbs: []string{"show", "daemon-reload", "show", "daemon-reload"}, active: "active"},
		{name: "daemon-reload fails", sd: &fakeSystemd{active: "active", fail: map[string]bool{"daemon-reload": true}},
			wantErr: errAny, verbs: []string{"show", "daemon-reload"}, active: "active"},
		{name: "systemctl show fails", sd: &fakeSystemd{active: "active", fail: map[string]bool{"show": true}},
			wantErr: errAny, verbs: []string{"show"}, active: "active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := c.sd
			path := fakeRunner(t, sd)
			if c.dropIn {
				putDropIn(t, path)
			}
			asked, err := StopRunnerGracefully(context.Background())
			checkErr(t, err, c.wantErr)
			if asked != c.asked {
				t.Errorf("asked = %v, want %v", asked, c.asked)
			}
			if !slices.Equal(sd.verbs, c.verbs) {
				t.Errorf("systemctl calls %v, want %v", sd.verbs, c.verbs)
			}
			if sd.active != c.active || sd.queued {
				t.Errorf("gitlab-runner %s (start queued: %v), want %s and none queued", sd.active, sd.queued, c.active)
			}
			got, rerr := os.ReadFile(path)
			if (rerr == nil) != c.wantDropIn {
				t.Fatalf("drop-in there: %v, want %v", rerr == nil, c.wantDropIn)
			}
			if c.wantDropIn && string(got) != runnerDropIn {
				t.Fatalf("drop-in:\n%s", got)
			}
		})
	}
}

// The drop-in makes a stop graceful only with all three settings: SIGQUIT
// alone (GitLab's suggested kill.conf) still signals every job process.
func TestReadRunnerState(t *testing.T) {
	cases := []struct {
		out      string
		graceful bool
	}{
		{"ActiveState=active\nKillMode=mixed\nKillSignal=3\nTimeoutStopUSec=infinity\n", true},
		{"ActiveState=active\nKillMode=control-group\nKillSignal=3\nTimeoutStopUSec=infinity\n", false},
		{"ActiveState=active\nKillMode=mixed\nKillSignal=15\nTimeoutStopUSec=infinity\n", false},
		{"ActiveState=active\nKillMode=mixed\nKillSignal=3\nTimeoutStopUSec=2h\n", false},
	}
	for _, c := range cases {
		stubCommands(t, "printf '"+strings.ReplaceAll(c.out, "\n", `\n`)+"'")
		st, err := ReadRunnerState(context.Background())
		if err != nil || st.Active != "active" || st.Graceful != c.graceful {
			t.Errorf("%q: %+v, %v; want graceful %v", c.out, st, err, c.graceful)
		}
	}
	stubCommands(t, "echo nothing useful")
	if _, err := ReadRunnerState(context.Background()); err == nil {
		t.Error("output without ActiveState accepted")
	}
}

func TestResumeRunner(t *testing.T) {
	cases := []struct {
		name       string
		sd         *fakeSystemd
		dropIn     bool
		want       Resume
		wantErr    bool
		verbs      []string
		active     string
		queued     bool
		wantDropIn bool
	}{
		// gitlab-runner was stopped before the drain: the operator's choice.
		{name: "drain did not stop it", sd: &fakeSystemd{active: "inactive"},
			want: ResumeNothing, verbs: []string{"show"}, active: "inactive"},
		// An earlier undrain removed the file but its reload failed: the
		// settings are still loaded. Only the reload is done again; that
		// undrain started gitlab-runner already.
		{name: "file gone, settings still loaded", sd: &fakeSystemd{active: "active", loaded: true},
			want: ResumeDropped, verbs: []string{"show", "show", "daemon-reload", "show"}, active: "active"},
		{name: "file gone, settings loaded, still stopping", sd: &fakeSystemd{active: "deactivating", loaded: true},
			want: ResumeAfterStop, verbs: []string{"show", "show"}, active: "deactivating"},
		{name: "file gone, settings loaded, reload fails again",
			sd:   &fakeSystemd{active: "active", loaded: true, fail: map[string]bool{"daemon-reload": true}},
			want: ResumeNothing, wantErr: true, verbs: []string{"show", "show", "daemon-reload"}, active: "active"},
		{name: "stopped by drain", sd: &fakeSystemd{active: "inactive", loaded: true}, dropIn: true,
			want: ResumeStarted, verbs: []string{"start", "show", "daemon-reload", "show"}, active: "active"},
		// The start waits behind the stop; a reload without the drop-in
		// would end the stop by killing the jobs, so it stays.
		{name: "still finishing its jobs", sd: &fakeSystemd{active: "deactivating", loaded: true}, dropIn: true,
			want: ResumeAfterStop, verbs: []string{"start", "show"}, active: "deactivating", queued: true, wantDropIn: true},
		// Kept, so undrain can be run again.
		{name: "start fails", sd: &fakeSystemd{active: "inactive", loaded: true, fail: map[string]bool{"start": true}}, dropIn: true,
			want: ResumeNothing, wantErr: true, verbs: []string{"start"}, active: "inactive", wantDropIn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := c.sd
			path := fakeRunner(t, sd)
			if c.dropIn {
				putDropIn(t, path)
			}
			got, err := ResumeRunner(context.Background())
			if got != c.want || (err != nil) != c.wantErr {
				t.Errorf("ResumeRunner = %v, %v; want %v, error %v", got, err, c.want, c.wantErr)
			}
			if !slices.Equal(sd.verbs, c.verbs) {
				t.Errorf("systemctl calls %v, want %v", sd.verbs, c.verbs)
			}
			if sd.active != c.active || sd.queued != c.queued {
				t.Errorf("gitlab-runner %s (start queued: %v), want %s (%v)", sd.active, sd.queued, c.active, c.queued)
			}
			if _, err := os.Stat(path); (err == nil) != c.wantDropIn {
				t.Errorf("drop-in there: %v, want %v", err == nil, c.wantDropIn)
			}
		})
	}
}

func TestDropDrainDropIn(t *testing.T) {
	cases := []struct {
		name       string
		sd         *fakeSystemd
		dropIn     bool
		gone       bool
		wantErr    bool
		verbs      []string
		wantDropIn bool
		loaded     bool // afterwards: systemd still has it loaded
	}{
		{name: "none there", sd: &fakeSystemd{active: "active"}, gone: true, verbs: []string{"show"}},
		{name: "still stopping: kept", sd: &fakeSystemd{active: "deactivating", loaded: true}, dropIn: true,
			verbs: []string{"show"}, wantDropIn: true, loaded: true},
		{name: "started again: removed", sd: &fakeSystemd{active: "active", loaded: true}, dropIn: true,
			gone: true, verbs: []string{"show", "daemon-reload", "show"}},
		{name: "stopped: removed", sd: &fakeSystemd{active: "inactive", loaded: true}, dropIn: true,
			gone: true, verbs: []string{"show", "daemon-reload", "show"}},
		// Removed but still loaded: the stop settings still apply, so it is
		// not gone; a later call reloads again.
		{name: "daemon-reload fails: not gone", sd: &fakeSystemd{active: "active", loaded: true, fail: map[string]bool{"daemon-reload": true}}, dropIn: true,
			wantErr: true, verbs: []string{"show", "daemon-reload"}, loaded: true},
		{name: "file gone but still loaded: reloaded", sd: &fakeSystemd{active: "active", loaded: true},
			gone: true, verbs: []string{"show", "daemon-reload", "show"}},
		{name: "systemctl show fails: kept", sd: &fakeSystemd{active: "active", loaded: true, fail: map[string]bool{"show": true}}, dropIn: true,
			wantErr: true, verbs: []string{"show"}, wantDropIn: true, loaded: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := c.sd
			path := fakeRunner(t, sd)
			if c.dropIn {
				putDropIn(t, path)
			}
			gone, err := DropDrainDropIn(context.Background())
			if gone != c.gone || (err != nil) != c.wantErr {
				t.Errorf("DropDrainDropIn = %v, %v; want %v, error %v", gone, err, c.gone, c.wantErr)
			}
			if !slices.Equal(sd.verbs, c.verbs) {
				t.Errorf("systemctl calls %v, want %v", sd.verbs, c.verbs)
			}
			if HasDrainDropIn() != c.wantDropIn {
				t.Errorf("drop-in there: %v, want %v", HasDrainDropIn(), c.wantDropIn)
			}
			if sd.loaded != c.loaded {
				t.Errorf("drop-in loaded: %v, want %v", sd.loaded, c.loaded)
			}
		})
	}
}

// A drain then an undrain leave gitlab-runner as it was, drop-in gone.
func TestDrainThenResumeRunner(t *testing.T) {
	sd := &fakeSystemd{active: "active"}
	path := fakeRunner(t, sd)
	if asked, err := StopRunnerGracefully(context.Background()); !asked || err != nil {
		t.Fatalf("StopRunnerGracefully = %v, %v", asked, err)
	}
	sd.exit() // its jobs finished
	if got, err := ResumeRunner(context.Background()); got != ResumeStarted || err != nil {
		t.Fatalf("ResumeRunner = %v, %v", got, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || sd.active != "active" || sd.loaded {
		t.Fatalf("after undrain: drop-in %v, gitlab-runner %s, drop-in loaded %v", err, sd.active, sd.loaded)
	}
}

// Undrain while gitlab-runner still finishes its jobs: it starts once they
// are done, and only then does the drop-in go, so its stops after that wait
// for no jobs.
func TestUndrainWhileStoppingDropsTheDropInAfterwards(t *testing.T) {
	sd := &fakeSystemd{active: "active"}
	fakeRunner(t, sd)
	ctx := context.Background()
	if _, err := StopRunnerGracefully(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := ResumeRunner(ctx); got != ResumeAfterStop || err != nil {
		t.Fatalf("ResumeRunner while stopping = %v, %v", got, err)
	}
	if gone, err := DropDrainDropIn(ctx); gone || err != nil || !HasDrainDropIn() {
		t.Fatalf("DropDrainDropIn while stopping = %v, %v; want the drop-in kept", gone, err)
	}
	sd.exit()
	if sd.active != "active" {
		t.Fatalf("gitlab-runner %s after its stop, want started by the queued start", sd.active)
	}
	if gone, err := DropDrainDropIn(ctx); !gone || err != nil || HasDrainDropIn() || sd.loaded {
		t.Fatalf("DropDrainDropIn after the stop = %v, %v; drop-in there %v, loaded %v", gone, err, HasDrainDropIn(), sd.loaded)
	}
}

// Drain again after an undrain that queued a start behind the stop: the
// stop replaces that start, so gitlab-runner does not come back on a
// drained host when its jobs are done.
func TestDrainAgainCancelsAQueuedStart(t *testing.T) {
	sd := &fakeSystemd{active: "active"}
	fakeRunner(t, sd)
	ctx := context.Background()
	if _, err := StopRunnerGracefully(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := ResumeRunner(ctx); got != ResumeAfterStop || !sd.queued {
		t.Fatalf("undrain while stopping = %v, start queued %v", got, sd.queued)
	}
	if asked, err := StopRunnerGracefully(ctx); !asked || err != nil {
		t.Fatalf("drain again = %v, %v", asked, err)
	}
	sd.exit()
	if sd.active != "inactive" {
		t.Fatalf("gitlab-runner %s after its jobs finished on a drained host, want inactive", sd.active)
	}
}

// errAny stands for "some error" in test tables.
var errAny = errors.New("any error")

func checkErr(t *testing.T, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case want == errAny && err == nil, want != nil && want != errAny && !errors.Is(err, want):
		t.Fatalf("error %v, want %v", err, want)
	}
}

// A daemon-reload that fails after the file went leaves the stop settings
// loaded: undrain must not report them gone, and the next try reloads.
func TestDropDrainDropInRetriesAFailedReload(t *testing.T) {
	sd := &fakeSystemd{active: "active", loaded: true, fail: map[string]bool{"daemon-reload": true}}
	putDropIn(t, fakeRunner(t, sd))
	ctx := context.Background()
	if gone, err := DropDrainDropIn(ctx); gone || err == nil {
		t.Fatalf("first try with a failing reload = %v, %v", gone, err)
	}
	if left, err := DrainSettingsLeft(ctx); !left || err != nil {
		t.Fatalf("DrainSettingsLeft after a failed reload = %v, %v; want left", left, err)
	}
	sd.mu.Lock()
	sd.fail = nil
	sd.mu.Unlock()
	if gone, err := DropDrainDropIn(ctx); !gone || err != nil || sd.loaded {
		t.Fatalf("second try = %v, %v; loaded %v", gone, err, sd.loaded)
	}
	if left, err := DrainSettingsLeft(ctx); left || err != nil {
		t.Fatalf("DrainSettingsLeft after the reload = %v, %v", left, err)
	}
}

// DropInPaths tells whether systemd has the drain drop-in loaded.
func TestReadRunnerStateDrainLoaded(t *testing.T) {
	for _, c := range []struct {
		paths string
		want  bool
	}{
		{"/etc/systemd/system/gitlab-runner.service.d/kill.conf " + DrainDropIn(), true},
		{"/etc/systemd/system/gitlab-runner.service.d/kill.conf", false},
		{"", false},
		{DrainDropIn() + ".bak", false},
	} {
		stubCommands(t, "printf 'ActiveState=active\\nDropInPaths="+c.paths+"\\n'")
		st, err := ReadRunnerState(context.Background())
		if err != nil || st.DrainLoaded != c.want {
			t.Errorf("DropInPaths=%q: %+v, %v; want loaded %v", c.paths, st, err, c.want)
		}
	}
}

// Undrain again after one whose daemon-reload failed: the file is gone but
// systemd still has the stop settings loaded. The second undrain unloads
// them instead of reporting that nothing is left.
func TestUndrainAgainAfterAFailedReload(t *testing.T) {
	sd := &fakeSystemd{active: "inactive", loaded: true, fail: map[string]bool{"daemon-reload": true}}
	putDropIn(t, fakeRunner(t, sd))
	ctx := context.Background()
	if _, err := ResumeRunner(ctx); err == nil {
		t.Fatal("first undrain with a failing reload reported no error")
	}
	if left, err := DrainSettingsLeft(ctx); HasDrainDropIn() || !left || err != nil {
		t.Fatalf("after the failed reload: file there %v, settings left %v (%v); want file gone, settings loaded", HasDrainDropIn(), left, err)
	}
	sd.mu.Lock()
	sd.fail = nil
	sd.mu.Unlock()
	if got, err := ResumeRunner(ctx); got != ResumeDropped || err != nil {
		t.Fatalf("second undrain = %v, %v; want ResumeDropped", got, err)
	}
	if left, err := DrainSettingsLeft(ctx); left || err != nil || sd.active != "active" {
		t.Fatalf("after the second undrain: settings left %v (%v), gitlab-runner %s", left, err, sd.active)
	}
}
