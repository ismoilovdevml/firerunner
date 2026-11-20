package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// sshd's pam_env reads /etc/environment once per connection, and every stage
// of a job runs over the connection opened before the services script added
// the service aliases to no_proxy there. A stage on the VM therefore gets that
// no_proxy from its own script, or its traffic to http://minio:9000 goes to
// the corporate proxy.
func TestVMStagesKeepServicesOutOfTheProxy(t *testing.T) {
	for _, c := range []struct {
		name     string
		proxy    bool
		aliases  []string
		stage    string
		wantSent bool
	}{
		{"proxy and services: user stage", true, []string{"minio", "db"}, "step_script", true},
		{"proxy and services: helper stage", true, []string{"minio"}, "get_sources", true},
		{"proxy, no services", true, nil, "step_script", false},
		{"services, no proxy", false, []string{"minio"}, "step_script", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sent := filepath.Join(t.TempDir(), "sent")
			cfg, script := runFixture(t, "cat >> "+sent+"\n")
			cfg.Proxy.Enabled = c.proxy
			st := &vm.JobState{Instance: vm.Instance{ID: "job-555", UID: "u1", IP: "10.200.0.55", HostKey: "ssh-ed25519 AAAA"}, ServiceAliases: c.aliases}
			if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
				t.Fatal(err)
			}
			if err := Run(context.Background(), cfg, script, c.stage); err != nil {
				t.Fatalf("Run = %v", err)
			}
			got, _ := os.ReadFile(sent)
			if !c.wantSent {
				if strings.Contains(string(got), "no_proxy") {
					t.Fatalf("stage script sets no_proxy without both proxy and services:\n%s", got)
				}
				return
			}
			np := vm.NoProxy(cfg, c.aliases...)
			for _, a := range c.aliases {
				if !strings.Contains(","+np+",", ","+a+",") {
					t.Fatalf("no_proxy %q lacks service alias %q", np, a)
				}
			}
			want := "export no_proxy=" + shellQuote(np) + " NO_PROXY=" + shellQuote(np) + "\n"
			if !strings.HasPrefix(string(got), want) || !strings.HasSuffix(string(got), "make test\n") {
				t.Fatalf("stage script:\n%s\nwant it to start with %q and end with the job's script", got, want)
			}
		})
	}
}
