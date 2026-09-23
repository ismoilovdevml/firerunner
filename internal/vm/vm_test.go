package vm

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

func TestMACIsStableAndLocal(t *testing.T) {
	a, b := MAC("job-1"), MAC("job-1")
	if a != b || MAC("job-2") == a {
		t.Fatalf("MAC not stable/unique: %s %s", a, MAC("job-2"))
	}
	if !strings.HasPrefix(a, "aa:fc:") || len(a) != 17 {
		t.Fatalf("bad MAC %s", a)
	}
}

func TestLeaseIP(t *testing.T) {
	dir := t.TempDir()
	if ip, err := LeaseIP(filepath.Join(dir, "missing"), "aa:fc:00:00:00:01"); ip != "" || err != nil {
		t.Fatalf("missing file: %q %v", ip, err)
	}
	p := filepath.Join(dir, "leases")
	body := "1790224115 aa:fc:00:00:00:01 10.200.0.10 * 01:aa\n" +
		"1790224116 AA:FC:00:00:00:02 10.200.0.11 * 01:bb\n" +
		"1790224117 aa:fc:00:00:00:01 10.200.0.12 * 01:aa\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:01"); ip != "10.200.0.12" {
		t.Fatalf("want latest lease, got %q", ip)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:02"); ip != "10.200.0.11" {
		t.Fatalf("case-insensitive match failed: %q", ip)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:03"); ip != "" {
		t.Fatalf("unexpected lease %q", ip)
	}
}

func TestSpec(t *testing.T) {
	cfg := config.Default()
	s := Spec(cfg, "job-7", MAC("job-7"), "ssh-ed25519 AAAA test", map[string]string{"a": "b"})
	if s.GetVcpu() != 2 || s.GetMemoryInMb() != 2048 {
		t.Fatalf("size: %d %d", s.GetVcpu(), s.GetMemoryInMb())
	}
	iface := s.GetInterfaces()[0]
	if iface.GetType() != types.NetworkInterface_TAP || iface.GetGuestMac() != MAC("job-7") {
		t.Fatalf("interface: %v", iface)
	}
	if s.GetKernel().GetCmdline()["acpi"] != "off" {
		t.Fatal("acpi=off missing")
	}
	ud, err := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	if err != nil || !strings.Contains(string(ud), "ssh-ed25519 AAAA test") || !strings.HasPrefix(string(ud), "#cloud-config") {
		t.Fatalf("user-data: %v %s", err, ud)
	}
	// the spec must not share the config's cmdline map
	s.GetKernel().Cmdline["x"] = "y"
	if _, ok := cfg.VM.KernelCmdline["x"]; ok {
		t.Fatal("spec aliases config map")
	}
}
