package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

// fakeSSH puts an ssh on PATH that runs body; pidFile gets the pid of each
// call once it started.
func fakeSSH(t *testing.T, body string) (cfg config.Config, inst *Instance, pidFile string) {
	t.Helper()
	bin := t.TempDir()
	pidFile = filepath.Join(t.TempDir(), "pid")
	script := "#!/bin/sh\necho $$ > " + pidFile + ".tmp && mv " + pidFile + ".tmp " + pidFile + "\n" + body
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldKH, oldPoll := KnownHostsDir, bootPoll
	KnownHostsDir, bootPoll = t.TempDir(), 20*time.Millisecond
	t.Cleanup(func() { KnownHostsDir, bootPoll = oldKH, oldPoll })
	cfg = config.Default()
	cfg.VM.BootTimeout = time.Minute
	return cfg, &Instance{ID: "job-9", IP: "10.200.0.9", HostKey: "ssh-ed25519 AAAA"}, pidFile
}

func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sshd that accepts the connection but never finishes the handshake: ssh
// itself only gives up after ServerAliveInterval x ServerAliveCountMax (45 s).
// Boot's wait for SSH must end with the job (ctx) and at vm.boot_timeout, and
// the hung ssh must not outlive it.
func TestWaitSSHEndsAHungProbe(t *testing.T) {
	for _, c := range []struct {
		name     string
		deadline time.Duration // from now
		cancel   bool
		want     func(error) bool
	}{
		{"job cancelled", time.Minute, true, func(err error) bool { return errors.Is(err, context.Canceled) }},
		// Not shorter: a new script's first exec takes ~0.3 s on macOS.
		{"boot timeout passes", 2 * time.Second, false, func(err error) bool { return errors.Is(err, ErrNotReady) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, inst, pidFile := fakeSSH(t, "exec sleep 30\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- waitSSH(ctx, cfg, inst, start.Add(c.deadline)) }()
			pid := readPID(t, pidFile)
			if c.cancel {
				cancel()
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("waitSSH still waits for a hung ssh")
			}
			if !c.want(err) {
				t.Fatalf("waitSSH = %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the probe's ssh (pid %d) outlived waitSSH: %v", pid, err)
			}
		})
	}
}

// sshd that refuses the first logins (still starting) is asked again.
func TestWaitSSHRetriesUntilTheVMAnswers(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	cfg, inst, _ := fakeSSH(t, "echo x >> "+count+"\n[ $(wc -l < "+count+") -ge 3 ] && exit 0\nexit 255\n")
	if err := waitSSH(context.Background(), cfg, inst, time.Now().Add(10*time.Second)); err != nil {
		t.Fatalf("waitSSH = %v", err)
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 3 {
		t.Fatalf("%d probes, want 3", strings.Count(string(b), "x"))
	}
}
