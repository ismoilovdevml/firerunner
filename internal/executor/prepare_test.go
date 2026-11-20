package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// prepareFixture makes Prepare cold-boot a fake microVM for the job payload
// (id 31) and run fakeSSH (a sh script body) for every ssh. The daemon has no
// pool VM and a ready builder; the job has no image, services or
// DOCKER_AUTH_CONFIG until the test sets them.
func prepareFixture(t *testing.T, payload, fakeSSH string) config.Config {
	t.Helper()
	cfg, _, _ := coldBootFixture(t, 5*time.Second)
	cfg.Network.LeasesFile = filepath.Join(t.TempDir(), "leases")
	oldDir, oldKnown, oldMux := StateDir, vm.KnownHostsDir, vm.MuxDir
	StateDir, vm.KnownHostsDir, vm.MuxDir = t.TempDir(), t.TempDir(), t.TempDir()
	t.Cleanup(func() { StateDir, vm.KnownHostsDir, vm.MuxDir = oldDir, oldKnown, oldMux })
	writePayload(t, payload)
	t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "")
	t.Setenv("CUSTOM_ENV_CI_JOB_SERVICES", "")
	t.Setenv("CUSTOM_ENV_DOCKER_AUTH_CONFIG", "")

	dir, err := os.MkdirTemp("", "fre") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.Daemon.Socket = filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", cfg.Daemon.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/builder" {
			_ = json.NewEncoder(w).Encode(daemon.BuilderInfo{State: daemon.BuilderReady, Port: 20001, CA: "CA", Cert: "CERT", Key: "KEY"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n"+fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cfg
}

// A job cancelled while prepare sets up its microVM (DOCKER_AUTH_CONFIG, the
// services, the builder) is a cancelled job: prepare stops the guest command
// at once and records "canceled", which the system-failure alerts leave out,
// instead of waiting for the command and blaming the step it was in.
func TestPrepareCancelledDuringGuestSetupIsCanceled(t *testing.T) {
	oldGrace := sshStopGrace
	sshStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { sshStopGrace = oldGrace })
	for _, c := range []struct {
		name  string
		hang  string // shell pattern of the guest command that never ends
		setup func(t *testing.T)
	}{
		{"writing DOCKER_AUTH_CONFIG", "'mkdir -p /root/.docker'*", func(t *testing.T) {
			t.Setenv("CUSTOM_ENV_DOCKER_AUTH_CONFIG", `{"auths":{}}`)
		}},
		{"starting services", "'/bin/bash'", func(t *testing.T) {
			t.Setenv("CUSTOM_ENV_CI_JOB_SERVICES", `[{"name":"postgres:16"}]`)
		}},
		{"attaching the builder", "'bash'", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			cfg := prepareFixture(t, `{"id":31,"job_info":{"project_id":5}}`,
				"for last; do :; done\ncase \"$last\" in\n"+c.hang+")\n"+
					"    echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\n    exec sleep 30 ;;\n"+
					"esac\ncat >/dev/null\nexit 0\n")
			if c.setup != nil {
				c.setup(t)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Prepare(ctx, cfg) }()
			var pid int
			for deadline := time.Now().Add(10 * time.Second); pid == 0; {
				if b, err := os.ReadFile(pidFile); err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				} else if time.Now().After(deadline) {
					t.Fatal("the guest command never started")
				} else {
					time.Sleep(10 * time.Millisecond)
				}
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Prepare still waits for the guest command after the job was cancelled")
			}
			var e *ExitError
			if !errors.As(err, &e) || e.Code != 2 {
				t.Fatalf("Prepare = %v, want a system failure exit", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the guest command's ssh (pid %d) outlived the cancelled prepare: %v", pid, err)
			}
			st, err := vm.LoadJobState(statePath("job-31"))
			if err != nil {
				t.Fatal(err)
			}
			if r, why := finishResult(st); r != daemon.ResultSystemFailure || why != "canceled" {
				t.Fatalf("recorded %s/%s, want system_failure/canceled", r, why)
			}
		})
	}
}

// Like gitlab-runner's Docker executor (Build.GetServiceVariables), service
// containers get the job's variables whose effective entry in the payload is
// public (.gitlab-ci.yml variables, GitLab's public predefined ones), under
// the service's own variables. Never a non-public one (CI_JOB_TOKEN, CI/CD
// settings variables), not even a public one a non-public entry overrides,
// and nothing gitlab-runner adds itself, `secrets:` variables included.
// variables: {POSTGRES_PASSWORD: test} with services: [postgres:16] works as
// on the Docker executor.
//
// A public variable's references are expanded against what the service gets
// only: NPM_TOKEN: $CI_JOB_TOKEN must not carry the job token into the
// service, as the value gitlab-runner expanded (CUSTOM_ENV_NPM_TOKEN) would.
func TestPrepareGivesServicesTheJobsPublicVariables(t *testing.T) {
	sent := filepath.Join(t.TempDir(), "services")
	cfg := prepareFixture(t, `{"id":31,"job_info":{"project_id":5},"variables":[
		{"key":"CI_JOB_TOKEN","value":"job-token","public":false,"masked":true},
		{"key":"CI_PROJECT_NAME","value":"app","public":true},
		{"key":"POSTGRES_PASSWORD","value":"test","public":true},
		{"key":"DB_HOST","value":"db","public":true},
		{"key":"DATABASE_URL","value":"postgres://$DB_HOST/x","public":true},
		{"key":"MODE","value":"from-yaml","public":true},
		{"key":"DEPLOY_KEY","value":"settings-secret","public":false},
		{"key":"MODE","value":"settings-mode","public":false},
		{"key":"POSTGRES_DB","value":"job-db","public":true},
		{"key":"CA_PEM","value":"pem-data","public":true,"file":true},
		{"key":"VAULT_PW","value":"from-yaml","public":true},
		{"key":"NPM_TOKEN","value":"$CI_JOB_TOKEN","public":true},
		{"key":"DB_PASSWORD","value":"settings-pw","public":false,"masked":true},
		{"key":"APP_DSN","value":"postgres://app:${DB_PASSWORD}@db/app","public":true},
		{"key":"PG_URL","value":"postgres://$POSTGRES_USER@db","public":true},
		{"key":"RAW_VAL","value":"keep $DB_HOST","public":true,"raw":true},
		{"key":"PRICE","value":"5$$","public":true}
	],"secrets":{"VAULT_PW":{"vault":{"engine":{"name":"kv-v2","path":"kv"},"path":"db","field":"pw"}}}}`, "for last; do :; done\nif [ \"$last\" = /bin/bash ]; then cat > "+sent+"; else cat >/dev/null; fi\nexit 0\n")
	for k, v := range map[string]string{
		"CI_JOB_TOKEN": "job-token", "CI_PROJECT_NAME": "app", "POSTGRES_PASSWORD": "test", "DB_HOST": "db",
		"DATABASE_URL": "postgres://db/x", // as gitlab-runner expanded it
		"MODE":         "settings-mode", "DEPLOY_KEY": "settings-secret", "POSTGRES_DB": "job-db", "CA_PEM": "pem-data",
		"CI_BUILDS_DIR": "/builds",      // gitlab-runner's own, not in the payload
		"VAULT_PW":      "vault-secret", // the secrets: variable overrides the public one
		// As gitlab-runner expanded them, against every variable.
		"NPM_TOKEN": "job-token", "DB_PASSWORD": "settings-pw", "APP_DSN": "postgres://app:settings-pw@db/app",
		"PG_URL": "postgres://@db", "RAW_VAL": "keep $DB_HOST", "PRICE": "5$",
	} {
		t.Setenv("CUSTOM_ENV_"+k, v)
	}
	t.Setenv("CUSTOM_ENV_CI_JOB_SERVICES", `[{"name":"postgres:16","variables":{"POSTGRES_DB":"svc-db","POSTGRES_USER":"svc-user"}}]`)

	if err := Prepare(context.Background(), cfg); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	script, err := os.ReadFile(sent)
	if err != nil {
		t.Fatalf("no services script: %v", err)
	}
	for _, want := range []string{"'POSTGRES_PASSWORD=test'", "'CI_PROJECT_NAME=app'", "'DB_HOST=db'", "'DATABASE_URL=postgres://db/x'", "'POSTGRES_DB=svc-db'",
		"'NPM_TOKEN='", "'APP_DSN=postgres://app:@db/app'", "'PG_URL=postgres://svc-user@db'", "'RAW_VAL=keep $DB_HOST'", "'PRICE=5$'",
	} {
		if !strings.Contains(string(script), "-e "+want) {
			t.Errorf("service lacks %s:\n%s", want, script)
		}
	}
	for _, leak := range []string{"job-token", "settings-secret", "MODE=", "job-db", "CA_PEM", "pem-data", "CI_BUILDS_DIR", "CI_JOB_SERVICES", "vault-secret", "settings-pw"} {
		if strings.Contains(string(script), leak) {
			t.Errorf("service gets %q:\n%s", leak, script)
		}
	}
}
