package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// The forwarder is watched like the other services, but only where it runs;
// each registry mirror's units come right after firerunner-registry.
func TestMonitoredServices(t *testing.T) {
	mirrors := []string{"firerunner-registry@docker.io", "firerunner-registry-front@docker.io"}
	for _, c := range []struct {
		proxy   bool
		mirrors []string
		want    string
	}{
		{false, nil, "containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd gitlab-runner"},
		{true, nil, "containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd firerunner-proxy gitlab-runner"},
		{false, mirrors, "containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry " +
			"firerunner-registry@docker.io firerunner-registry-front@docker.io firerunner-cache flintlockd gitlab-runner"},
	} {
		if got := strings.Join(MonitoredServices(c.proxy, c.mirrors), " "); got != c.want {
			t.Errorf("MonitoredServices(%v, %v) = %s, want %s", c.proxy, c.mirrors, got, c.want)
		}
	}
	// The lists are copies: appending to them never changes Services.
	_ = append(MonitoredServices(true, mirrors)[:1], "x")
	_ = append(HostServices(mirrors)[:4], "x")
	if Services[1] != "firerunner-net" || Services[4] != "firerunner-cache" {
		t.Fatalf("Services changed: %v", Services)
	}
}

// Every NAME.env in the registries' config directory is a mirror whose
// registry and front must run; nothing else there is.
func TestRegistryUnits(t *testing.T) {
	pair := func(name string) string {
		return "firerunner-registry@" + name + " firerunner-registry-front@" + name
	}
	for _, c := range []struct {
		name  string
		files []string // created in the directory (nil: no directory); a trailing / makes a directory
		want  string
	}{
		{"no directory: an install before a service per mirror", nil, ""},
		{"empty", []string{}, ""},
		{"mirrors by name", []string{"quay.io.env", "quay.io.yml", "docker.io.env", "docker.io.yml", "ghcr.io.env"},
			pair("docker.io") + " " + pair("ghcr.io") + " " + pair("quay.io")},
		{"a registry config alone is not a mirror", []string{"ghcr.io.yml"}, ""},
		{"not a mirror's name", []string{"Ghcr.io.env", ".ghcr.io.env", "ghcr.io-.env", "a b.env", ".env",
			"ghcr.io.env.bak", "x@y.env", "ghcr.io.env/", "docker.io.env"}, pair("docker.io")},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "registries.d")
			if c.files != nil {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range c.files {
				var err error
				if d, ok := strings.CutSuffix(f, "/"); ok {
					err = os.Mkdir(filepath.Join(dir, d), 0o755)
				} else {
					err = os.WriteFile(filepath.Join(dir, f), nil, 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := RegistryUnits(dir)
			if err != nil || strings.Join(got, " ") != c.want {
				t.Fatalf("RegistryUnits = %q, %v; want %q", got, err, c.want)
			}
		})
	}
	// A directory that cannot be read is an error, not "no mirrors".
	file := filepath.Join(t.TempDir(), "registries.d")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := RegistryUnits(file); err == nil {
		t.Fatalf("RegistryUnits of a file = %q, want an error", got)
	}
}

// One systemctl run answers for every unit, in order; a systemctl that fails
// or does not answer for all of them reads as all inactive.
func TestServicesActive(t *testing.T) {
	names := []string{"flintlockd", "firerunner-registry@ghcr.io", "firerunner-registry-front@ghcr.io", "firerunner-net"}
	none := []bool{false, false, false, false}
	for _, c := range []struct {
		name   string
		script string
		want   []bool
	}{
		{"each unit's state", `printf 'active\nfailed\nactivating\nreloading\n'; exit 3`, []bool{true, false, false, true}},
		{"all active", `printf 'active\nactive\nactive\nactive\n'`, []bool{true, true, true, true}},
		{"fewer answers than units", `printf 'active\nactive\n'`, none},
		{"no bus", `echo "Failed to connect to bus" >&2; exit 1`, none},
		{"hung systemctl", `printf 'active\nactive\nactive\nactive\n'; sleep 30`, none},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubCommands(t, c.script)
			start := time.Now()
			got := ServicesActiveContext(context.Background(), names)
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("took %s", took)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("ServicesActive = %v, want %v", got, c.want)
			}
		})
	}
	stubCommands(t, `exit 1`)
	if got := ServicesActiveContext(context.Background(), nil); len(got) != 0 {
		t.Fatalf("ServicesActive(nil) = %v", got)
	}
}

// Only a directory with a file system of its own mounted on it is a mount
// point: not one on its parent's file system, nor a missing one.
func TestMountPoint(t *testing.T) {
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{"/dev", true}, // devtmpfs (Linux), devfs (macOS)
		{t.TempDir(), false},
		{filepath.Join(t.TempDir(), "missing"), false},
	} {
		if got := MountPoint(c.dir); got != c.want {
			t.Errorf("MountPoint(%s) = %v, want %v", c.dir, got, c.want)
		}
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
