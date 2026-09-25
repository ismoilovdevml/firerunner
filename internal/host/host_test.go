package host

import (
	"context"
	"os/exec"
	"strings"
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
