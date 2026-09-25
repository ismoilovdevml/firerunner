package main

import (
	"os/exec"
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
