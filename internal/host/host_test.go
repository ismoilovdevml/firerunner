package host

import (
	"strings"
	"testing"
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
