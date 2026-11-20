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

	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

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
			cfg, _, _ := coldBootFixture(t, 5*time.Second)
			cfg.Network.LeasesFile = filepath.Join(t.TempDir(), "leases")
			oldDir, oldKnown, oldMux := StateDir, vm.KnownHostsDir, vm.MuxDir
			StateDir, vm.KnownHostsDir, vm.MuxDir = t.TempDir(), t.TempDir(), t.TempDir()
			t.Cleanup(func() { StateDir, vm.KnownHostsDir, vm.MuxDir = oldDir, oldKnown, oldMux })
			writePayload(t, `{"id":31,"job_info":{"project_id":5}}`)
			t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "")
			t.Setenv("CUSTOM_ENV_CI_JOB_SERVICES", "")
			t.Setenv("CUSTOM_ENV_DOCKER_AUTH_CONFIG", "")
			if c.setup != nil {
				c.setup(t)
			}

			// The daemon has no pool VM (a cold boot) and a ready builder.
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

			bin, pidFile := t.TempDir(), filepath.Join(t.TempDir(), "pid")
			fake := "#!/bin/sh\nfor last; do :; done\ncase \"$last\" in\n" + c.hang + ")\n" +
				"    echo $$ > " + pidFile + ".tmp && mv " + pidFile + ".tmp " + pidFile + "\n    exec sleep 30 ;;\n" +
				"esac\ncat >/dev/null\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

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
