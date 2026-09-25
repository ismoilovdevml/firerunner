package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
