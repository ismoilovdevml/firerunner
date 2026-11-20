package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseOOMKills(t *testing.T) {
	n, err := parseOOMKills(strings.NewReader("nr_free_pages 12\noom_kill 3\npgfault 9\n"))
	if err != nil || n != 3 {
		t.Fatalf("parseOOMKills = %d, %v", n, err)
	}
	if _, err := parseOOMKills(strings.NewReader("nr_free_pages 12\n")); err == nil {
		t.Fatal("a kernel without oom_kill must be an error, not 0")
	}
}

func TestParseDHCPRange(t *testing.T) {
	conf := "interface=br-fc\n# comment\ndhcp-range=10.200.0.10,10.200.0.250,255.255.255.0,15m\n"
	if n, err := parseDHCPRange(strings.NewReader(conf)); err != nil || n != 241 {
		t.Fatalf("parseDHCPRange = %d, %v, want 241", n, err)
	}
	for _, bad := range []string{"interface=br-fc\n", "dhcp-range=10.200.0.250,10.200.0.10,15m\n", "dhcp-range=::1,::2\n"} {
		if _, err := parseDHCPRange(strings.NewReader(bad)); err == nil {
			t.Errorf("parseDHCPRange(%q) accepted", bad)
		}
	}
}

// stubCommands makes host probes run script under sh instead of the real
// command, with a short timeout.
func stubCommands(t *testing.T, script string) {
	t.Helper()
	oldCmd, oldTimeout := command, commandTimeout
	command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	commandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { command, commandTimeout = oldCmd, oldTimeout })
}

func TestThinPoolUsage(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		data, meta float64
		wantErr    bool
	}{
		{"usage", `echo "  41.50   7.25"`, 41.5, 7.25, false},
		{"lvs fails", `echo "Volume group not found" >&2; exit 5`, 0, 0, true},
		{"unexpected output", `echo "  41.50"`, 0, 0, true},
		{"not a number", `echo "  n/a   7.25"`, 0, 0, true},
		// A hung lvs (LVM stuck on a device) must not hang the daemon's loop.
		{"hung lvs", `sleep 30`, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubCommands(t, c.script)
			start := time.Now()
			data, meta, err := ThinPoolUsageContext(context.Background())
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("took %s", took)
			}
			if (err != nil) != c.wantErr || data != c.data || meta != c.meta {
				t.Fatalf("ThinPoolUsage = %v, %v, %v; want %v, %v, err %v", data, meta, err, c.data, c.meta, c.wantErr)
			}
		})
	}
}

func TestServiceActive(t *testing.T) {
	for _, c := range []struct {
		script string
		want   bool
	}{{"exit 0", true}, {"exit 3", false}, {"sleep 30", false}} {
		stubCommands(t, c.script)
		start := time.Now()
		if got := ServiceActiveContext(context.Background(), "flintlockd"); got != c.want {
			t.Errorf("%q: ServiceActive = %v, want %v", c.script, got, c.want)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%q: took %s", c.script, took)
		}
	}
}

// The forwarder is watched like the other services, but only where it runs.
func TestMonitoredServices(t *testing.T) {
	for _, c := range []struct {
		proxy bool
		want  string
	}{
		{false, "containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd gitlab-runner"},
		{true, "containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd firerunner-proxy gitlab-runner"},
	} {
		if got := strings.Join(MonitoredServices(c.proxy), " "); got != c.want {
			t.Errorf("MonitoredServices(%v) = %s, want %s", c.proxy, got, c.want)
		}
	}
	// The list is a copy: appending to it never changes Services.
	_ = append(MonitoredServices(true)[:1], "x")
	if Services[1] != "firerunner-net" {
		t.Fatalf("Services changed: %v", Services)
	}
}

// gitlab-runner register writes the runner token into config.toml: the file
// must be 0600 from the moment it holds it, not from a chmod afterwards.
func TestRunRootOnly(t *testing.T) {
	// A common umask, as a root shell has: files are created 0644.
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	for _, c := range []struct {
		name     string
		existing bool
	}{{"created by the command", false}, {"left 0644 by an earlier run", true}} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if c.existing {
				if err := os.WriteFile(path, []byte("concurrent = 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// What the command sees while it writes: the mode right after.
			cmd := exec.Command("sh", "-c", `echo 'token = "glrt-x"' >> "$1" && stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"`, "sh", path)
			out, err := runRootOnly(cmd, path)
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if mode := strings.TrimSpace(string(out)); mode != "600" {
				t.Errorf("mode while the token was written: %s, want 600", mode)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode after: %v, %v", fi.Mode().Perm(), err)
			}
		})
	}
	if got := syscall.Umask(0o022); got != 0o022 {
		t.Errorf("umask left at %o", got)
	}
}

func TestWriteRootOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRootOnly(path, []byte("concurrent = 4\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "concurrent = 4\n" {
		t.Fatalf("content %q, %v", b, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}
