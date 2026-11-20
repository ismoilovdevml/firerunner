package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

func TestParseBuilderRm(t *testing.T) {
	ok := []struct {
		args    []string
		project string
		force   bool
	}{
		{[]string{"101"}, "101", false},
		{[]string{"--all"}, "all", false},
		{[]string{"101", "--force"}, "101", true},
		{[]string{"--force", "--all"}, "all", true},
	}
	for _, c := range ok {
		project, force, err := parseBuilderRm(c.args)
		if err != nil || project != c.project || force != c.force {
			t.Errorf("parseBuilderRm(%q) = %q, %v, %v; want %q, %v", c.args, project, force, err, c.project, c.force)
		}
	}
	for _, bad := range [][]string{
		nil,
		{"--force"},
		{"101", "102"},
		{"--all", "101"},
		{"-f", "101"},
		{"101", "--al"},
	} {
		if project, _, err := parseBuilderRm(bad); err == nil {
			t.Errorf("parseBuilderRm(%q) = %q, want a usage error", bad, project)
		}
	}
}

func TestRunCommandLine(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"env | grep -i proxy"}, "env | grep -i proxy"},
		{[]string{"uname", "-a"}, "'uname' '-a'"},
		{[]string{"sh", "-c", "env | grep x; echo 'hi'"}, `'sh' '-c' 'env | grep x; echo '\''hi'\'''`},
	} {
		if got := runCommandLine(tc.in); got != tc.want {
			t.Errorf("runCommandLine(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	// What bash makes of it: the argv comes back unchanged.
	out, err := exec.Command("bash", "-c", runCommandLine([]string{"printf", "%s|", "a b", "it's", "$HOME"})).Output()
	if err != nil || string(out) != "a b|it's|$HOME|" {
		t.Fatalf("round trip through bash: %q %v", out, err)
	}
}

// A job writes to its microVM's console; `vm logs` must not hand its escape
// sequences to the operator's terminal.
func TestVMLogsNeutraliseTerminalControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firecracker.stdout")
	console := "old line\r\n" +
		"[  OK  ] Started ssh.service\r\n" +
		"title \x1b]0;pwned\x07 clear \x1b[2J\x1b[H bell \x07\r\n" +
		"c1 \u009b31m del \x7f bad \xff cr-overwrite\rfake\ttab\r\n"
	if err := os.WriteFile(path, []byte(console), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tail(path, 3, &out); err != nil {
		t.Fatal(err)
	}
	want := "[  OK  ] Started ssh.service\n" +
		`title \x1b]0;pwned\x07 clear \x1b[2J\x1b[H bell \x07` + "\n" +
		`c1 \u009b31m del \x7f bad \xff cr-overwrite\x0dfake` + "\ttab\n"
	if out.String() != want {
		t.Fatalf("vm logs printed\n%q\nwant\n%q", out.String(), want)
	}
	if err := tail(filepath.Join(t.TempDir(), "missing"), 10, &out); err == nil {
		t.Fatal("tail of a missing console log succeeded")
	}
}

func TestConsoleText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain text, ü, ✓, 日本\n\tindented", "plain text, ü, ✓, 日本\n\tindented"},
		{"a\r\nb\r\n", "a\nb\n"},
		{"progress 10%\rprogress 99%", `progress 10%\x0dprogress 99%`},
		{"\x1b[31mred\x1b[0m", `\x1b[31mred\x1b[0m`},
		{"nul\x00 del\x7f bs\x08", `nul\x00 del\x7f bs\x08`},
		{"csi \u009b nel \u0085", `csi \u009b nel \u0085`},
		{"raw csi byte \x9b[2J", `raw csi byte \x9b[2J`},
		{"replacement char � stays", "replacement char � stays"},
		{"", ""},
	} {
		if got := consoleText(c.in); got != c.want {
			t.Errorf("consoleText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A runner token in argv is readable by every local user (ps,
// /proc/<pid>/cmdline): register refuses it before touching the host, and
// never repeats it in the error.
func TestRunnerRegisterRejectsATokenInArgv(t *testing.T) {
	t.Setenv("FIRERUNNER_RUNNER_TOKEN", "")
	for _, args := range [][]string{
		{"register", "--url", "https://gitlab.example.com", "--token", "glrt-secret123"},
		{"register", "--url", "https://gitlab.example.com", "--token=glrt-secret123"},
		{"register", "--token", "glrt-secret123"},
	} {
		err := cmdRunner(args)
		if err == nil || !strings.Contains(err.Error(), "--token -") || !strings.Contains(err.Error(), "FIRERUNNER_RUNNER_TOKEN") {
			t.Errorf("cmdRunner(%q) = %v, want the argv token refused with how to pass it", args, err)
		}
		if err != nil && strings.Contains(err.Error(), "glrt-secret123") {
			t.Errorf("the error repeats the token: %v", err)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stdin closed") }

func TestRunnerToken(t *testing.T) {
	for _, c := range []struct {
		name, arg, env string
		stdin          io.Reader
		want           string // "" means an error
	}{
		{"stdin", "-", "", strings.NewReader("glrt-abc\n"), "glrt-abc"},
		{"stdin without newline", "-", "", strings.NewReader("  glrt-abc \t"), "glrt-abc"},
		{"stdin: first line only", "-", "", strings.NewReader("glrt-abc\nmore\n"), "glrt-abc"},
		{"stdin wins over the environment", "-", "glrt-env", strings.NewReader("glrt-abc\n"), "glrt-abc"},
		{"environment", "", "glrt-env", strings.NewReader("ignored\n"), "glrt-env"},
		{"empty stdin", "-", "", strings.NewReader(""), ""},
		{"blank line on stdin", "-", "", strings.NewReader("\n"), ""},
		{"stdin fails", "-", "", failingReader{}, ""},
		{"nothing at all", "", "", strings.NewReader("glrt-abc\n"), ""},
		{"token in argv", "glrt-abc", "", strings.NewReader(""), ""},
		{"token in argv with the environment set", "glrt-abc", "glrt-env", strings.NewReader(""), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := runnerToken(c.arg, c.env, c.stdin)
			switch {
			case c.want == "" && err == nil:
				t.Fatalf("runnerToken = %q, want an error", got)
			case c.want != "" && (err != nil || got != c.want):
				t.Fatalf("runnerToken = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// Keys that only make sense together are set by one `config set`; one of them
// alone is refused and leaves the file as it was.
func TestConfigSetSeveralKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FIRERUNNER_CONFIG", path)
	if err := cmdConfig([]string{"set", "flintlock.tls_cert_file", "/tls/client.crt"}); err == nil {
		t.Fatal("a client certificate without its key and CA was saved")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused set wrote the config: %v", err)
	}
	if err := cmdConfig([]string{"set", "flintlock.tls_ca_file", "/tls/ca.crt",
		"flintlock.tls_cert_file", "/tls/client.crt", "flintlock.tls_key_file", "/tls/client.key"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tls_ca_file: /tls/ca.crt", "tls_cert_file: /tls/client.crt", "tls_key_file: /tls/client.key"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lacks %q:\n%s", want, data)
		}
	}
	for _, bad := range [][]string{
		{"set", "pool.size"},
		{"set", "pool.size", "2", "vm.vcpu"},
		{"set", "pool.size", "2", "no.such_key", "1"},
	} {
		if err := cmdConfig(bad); err == nil {
			t.Errorf("config %q: want an error", bad)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != string(data) {
		t.Error("a refused set changed the config")
	}
}

// install.sh writes registry hosts (dots in the key) into vm.registry_mirrors;
// `config show` prints them like every other key.
func TestConfigShowDottedMapKeys(t *testing.T) {
	cfg := config.Default()
	cfg.VM.RegistryMirrors = map[string]string{"ghcr.io": "http://10.200.0.1:5001"}
	cfg.VM.KernelCmdline = map[string]string{"ipv6.disable": "1"}
	var out bytes.Buffer
	if err := showConfig(&out, "/etc/firerunner/config.yaml", cfg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# /etc/firerunner/config.yaml\n", "\nvm.registry_mirrors.ghcr.io = http://10.200.0.1:5001\n",
		"\nvm.kernel_cmdline.ipv6.disable = \"1\"\n", "\nvm.vcpu = 2\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

// config set may fix a value Validate refuses, but a file it cannot parse is
// left alone: setting one key must never replace the user's file with defaults.
func TestConfigSetOnABrokenFile(t *testing.T) {
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"yaml syntax error", "vm:\n  vcpu: 4\n  memory_mb: [\n", false},
		{"unknown key", "vm:\n  vcpu: 4\n  cpus: 4\n", false},
		{"invalid value", "vm:\n  vcpu: 0\n  memory_mb: 4096\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			t.Setenv("FIRERUNNER_CONFIG", path)
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			err := cmdConfig([]string{"set", "vm.vcpu", "2"})
			got, _ := os.ReadFile(path)
			if !c.ok {
				if err == nil || string(got) != c.body {
					t.Fatalf("set on an unparsable file: err %v, file now:\n%s", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("set could not fix an invalid value: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil || cfg.VM.VCPU != 2 || cfg.VM.MemoryMB != 4096 {
				t.Fatalf("after set: vcpu %d, memory_mb %d, %v", cfg.VM.VCPU, cfg.VM.MemoryMB, err)
			}
		})
	}
}

// The whole argument is the number: "4abc" is a typo, not 4.
func TestParseConcurrent(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int // 0: an error
	}{
		{"4", 4}, {"64", 64},
		{"4abc", 0}, {"4 5", 0}, {"4.5", 0}, {"", 0}, {"abc", 0}, {"0", 0}, {"-2", 0}, {" 4", 0},
	} {
		n, err := parseConcurrent(c.in)
		if (c.want == 0) != (err != nil) || (err == nil && n != c.want) {
			t.Errorf("parseConcurrent(%q) = %d, %v; want %d", c.in, n, err, c.want)
		}
	}
}

// config set says what the change needs, from what reads the key: jobs and
// the daemon's reload see most keys, but some are read once at a service's
// start, and the firewall has its own copy of the egress list.
func TestApplyNote(t *testing.T) {
	// Patterns: the daemon's unit is firerunner, not firerunner-proxy.
	const nextJob, daemon, forwarder = `applies to the next job`, `systemctl restart firerunner($|[^-])`, `systemctl restart firerunner-proxy`
	for _, c := range []struct {
		key  string
		want []string // patterns the note must match
		not  []string // patterns it must not match
	}{
		{"vm.vcpu", []string{nextJob}, []string{"restart"}},
		{"vm.kernel_cmdline.ipv6.disable", []string{nextJob}, []string{"restart"}},
		{"pool.size", []string{nextJob}, []string{"restart"}},
		{"daemon.job_max_age", []string{nextJob}, []string{"restart"}},
		{"daemon.metrics_listen", []string{daemon}, []string{nextJob}},
		{"daemon.reconcile_interval", []string{daemon}, []string{nextJob}},
		{"daemon.socket", []string{daemon}, nil},
		{"flintlock.endpoint", []string{daemon}, nil},
		{"flintlock.tls_ca_file", []string{daemon}, nil},
		{"flintlock.namespace", []string{daemon}, nil},
		{"proxy.egress_strict", []string{forwarder}, []string{nextJob}},
		{"proxy.connect_ports", []string{forwarder}, []string{nextJob}},
		{"proxy.upstream_file", []string{forwarder}, []string{nextJob}},
		{"proxy.listen", []string{forwarder, "next job"}, nil},
		{"network.egress_deny", []string{forwarder, "FR_EGRESS_DENY", "install.sh"}, []string{nextJob}},
		{"proxy.enabled", []string{"install.sh"}, nil},
		{"proxy.no_proxy", []string{"next job", "install.sh"}, nil},
	} {
		note := applyNote(c.key)
		for _, w := range c.want {
			if !regexp.MustCompile(w).MatchString(note) {
				t.Errorf("applyNote(%s) = %q, want it to match %q", c.key, note, w)
			}
		}
		for _, n := range c.not {
			if regexp.MustCompile(n).MatchString(note) {
				t.Errorf("applyNote(%s) = %q, must not match %q", c.key, note, n)
			}
		}
	}
}
