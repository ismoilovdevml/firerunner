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

// fakeSystemd plays systemctl for gitlab-runner's unit: a drop-in counts
// once a daemon-reload has read it, stop leaves the unit stopping (its jobs
// still run), start starts it unless it is still stopping.
type fakeSystemd struct {
	mu       sync.Mutex
	active   string
	loaded   bool            // the drain drop-in was in place at the last daemon-reload
	override bool            // another drop-in overrides it: never graceful
	fail     map[string]bool // systemctl verbs that fail
	verbs    []string
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
		f.loaded = err == nil && !f.override
	case "stop":
		f.active = "deactivating"
	case "start":
		if f.active != "deactivating" {
			f.active = "active"
		}
	case "show":
		killMode, signal, timeout := "control-group", "15", "1min 30s"
		if f.loaded {
			killMode, signal, timeout = "mixed", "3", "infinity"
		}
		out = fmt.Sprintf("ActiveState=%s\nKillMode=%s\nKillSignal=%s\nTimeoutStopUSec=%s\n", f.active, killMode, signal, timeout)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$OUT"`)
	cmd.Env = append(os.Environ(), "OUT="+out)
	return cmd
}

// fakeRunner points the drop-in directory at a temporary one and systemctl
// at f.
func fakeRunner(t *testing.T, f *fakeSystemd) string {
	t.Helper()
	oldCmd, oldDir := command, runnerDropInDir
	command = f.command
	runnerDropInDir = filepath.Join(t.TempDir(), "gitlab-runner.service.d")
	t.Cleanup(func() { command, runnerDropInDir = oldCmd, oldDir })
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
		{name: "already stopping gracefully (drain again)", sd: &fakeSystemd{active: "deactivating", loaded: true}, dropIn: true,
			verbs: []string{"show"}, active: "deactivating", wantDropIn: true},
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
			if sd.active != c.active {
				t.Errorf("gitlab-runner %s, want %s", sd.active, c.active)
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
		wantDropIn bool
	}{
		// gitlab-runner was stopped before the drain: the operator's choice.
		{name: "drain did not stop it", sd: &fakeSystemd{active: "inactive"},
			want: ResumeNothing, active: "inactive"},
		{name: "stopped by drain", sd: &fakeSystemd{active: "inactive", loaded: true}, dropIn: true,
			want: ResumeStarted, verbs: []string{"show", "start", "daemon-reload"}, active: "active"},
		// A reload without the drop-in would end the stop by killing the jobs.
		{name: "still finishing its jobs", sd: &fakeSystemd{active: "deactivating", loaded: true}, dropIn: true,
			want: ResumeAfterStop, verbs: []string{"show", "start"}, active: "deactivating", wantDropIn: true},
		// Kept, so undrain can be run again.
		{name: "start fails", sd: &fakeSystemd{active: "inactive", loaded: true, fail: map[string]bool{"start": true}}, dropIn: true,
			want: ResumeNothing, wantErr: true, verbs: []string{"show", "start"}, active: "inactive", wantDropIn: true},
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
			if sd.active != c.active {
				t.Errorf("gitlab-runner %s, want %s", sd.active, c.active)
			}
			if _, err := os.Stat(path); (err == nil) != c.wantDropIn {
				t.Errorf("drop-in there: %v, want %v", err == nil, c.wantDropIn)
			}
		})
	}
}

// A drain then an undrain leave gitlab-runner as it was, drop-in gone.
func TestDrainThenResumeRunner(t *testing.T) {
	sd := fakeSystemd{active: "active"}
	path := fakeRunner(t, &sd)
	if asked, err := StopRunnerGracefully(context.Background()); !asked || err != nil {
		t.Fatalf("StopRunnerGracefully = %v, %v", asked, err)
	}
	sd.mu.Lock()
	sd.active = "inactive" // its jobs finished, it exited
	sd.mu.Unlock()
	if got, err := ResumeRunner(context.Background()); got != ResumeStarted || err != nil {
		t.Fatalf("ResumeRunner = %v, %v", got, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || sd.active != "active" || sd.loaded {
		t.Fatalf("after undrain: drop-in %v, gitlab-runner %s, drop-in loaded %v", err, sd.active, sd.loaded)
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
