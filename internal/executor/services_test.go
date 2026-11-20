package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// The services script waits for every exposed TCP port of every service at
// once, each for up to 30 s like the Docker executor's health check, so a job
// waits at most about 30 s for its services, not 30 s per port. Each port's
// report and a silent service's logs stay together, in a fixed order.
// It runs under bash with a fake docker (two services: db exposing 5432 and
// 8080, cache exposing 6379) and a fake timeout that logs when each check
// starts and ends, takes 2 s and finds only 6379 open. The log, not the wall
// clock, shows the overlap: macOS takes seconds to first run a new script.
func TestServicesScriptWaitsForAllPortsAtOnce(t *testing.T) {
	bin, checks := t.TempDir(), filepath.Join(t.TempDir(), "checks")
	fakes := map[string]string{
		"docker": `#!/bin/bash
case "$1" in
ps) echo db cache ;;
inspect)
    c=${!#}
    case "$3" in
    *IPAddress*) [ "$c" = db ] && echo 10.0.0.2 || echo 10.0.0.3 ;;
    *ExposedPorts*) [ "$c" = db ] && echo "5432/tcp 8080/tcp 53/udp " || echo "6379/tcp " ;;
    *Config.Image*) echo "image-$c" ;;
    esac ;;
logs) echo "log of ${!#}" ;;
esac
exit 0
`,
		"timeout": `#!/bin/bash
[ "$1" = 30 ] || { echo "timeout $1, want 30" >&2; exit 2; }
echo start >>` + checks + `
sleep 2
echo end >>` + checks + `
case "$4" in */6379\)*) exit 0 ;; esac
exit 124
`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sc := ServicesScript([]Service{{Name: "postgres:16", Alias: "db"}, {Name: "redis:7", Alias: "cache"}}, "")
	cmd := exec.Command("bash", "-c", sc)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if log, _ := os.ReadFile(checks); strings.Join(strings.Fields(string(log)), ",") != "start,start,start,end,end,end" {
		t.Errorf("port checks %q: they did not all run at once", strings.Fields(string(log)))
	}
	var reports []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "port") || strings.HasPrefix(l, "  log of") || strings.Contains(l, "listening") {
			reports = append(reports, l)
		}
	}
	want := []string{
		"WARNING: service image-db did not open port 5432 within 30s", "  log of db",
		"WARNING: service image-db did not open port 8080 within 30s", "  log of db",
		"Service image-cache is listening on 6379",
	}
	if strings.Join(reports, "\n") != strings.Join(want, "\n") {
		t.Errorf("reports:\n%s\nwant:\n%s\nfull output:\n%s", strings.Join(reports, "\n"), strings.Join(want, "\n"), out)
	}
}

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
