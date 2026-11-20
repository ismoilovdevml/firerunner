package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// A job that builds images through make or a script of its own, where Run
// cannot see `docker build`, asks for the project's builder with the job
// variable FIRERUNNER_BUILDER=1: its stages then get the builder like a
// stage that runs docker build.
func TestRunAttachesTheBuilderWhenTheJobAsksForIt(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"", false},
		{"0", false},
		{"no", false},
	} {
		t.Run("FIRERUNNER_BUILDER="+c.value, func(t *testing.T) {
			cfg, _, _, calls, events := claimFixture(t, 0)
			st := &vm.JobState{Instance: vm.Instance{ID: "job-555", UID: "u1", IP: "10.200.0.55", HostKey: "ssh-ed25519 AAAA"}}
			if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "")
			t.Setenv("CUSTOM_ENV_FIRERUNNER_BUILDER", c.value)
			script := filepath.Join(t.TempDir(), "step_script")
			if err := os.WriteFile(script, []byte("make image\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Run(context.Background(), cfg, script, "step_script"); err != nil {
				t.Fatalf("Run = %v", err)
			}
			got, err := vm.LoadJobState(statePath("job-555"))
			if err != nil {
				t.Fatal(err)
			}
			if attached := got.BuilderProject == "1"; attached != c.want {
				t.Errorf("builder attached = %v, want %v (state %+v)", attached, c.want, got)
			}
			// The builder script runs under "bash", the stage under "/bin/bash".
			if sent := strings.Contains(calls(), "root@10.200.0.55 bash\n"); sent != c.want {
				t.Errorf("builder script sent = %v, want %v:\n%s", sent, c.want, calls())
			}
			var builds int
			for _, e := range events() {
				if e.Kind == "build" {
					builds++
				}
			}
			if (builds == 1) != c.want {
				t.Errorf("%d build events, want builder %v", builds, c.want)
			}
		})
	}
}
