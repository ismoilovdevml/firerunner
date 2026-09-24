package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.VCPU != 2 || cfg.VM.KernelImage != DefaultKernelImage || len(cfg.VM.KernelCmdline) != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg.VM)
	}
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("vm:\n  vcpu: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.VCPU != 4 || cfg.VM.MemoryMB != 2048 || cfg.VM.BootTimeout != 3*time.Minute {
		t.Fatalf("partial file lost defaults: %+v", cfg.VM)
	}
}

func TestLoadRejectsUnknownKeysAndBadValues(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key": "vm:\n  cpus: 4\n",
		"invalid":     "vm:\n  vcpu: 0\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		_ = os.WriteFile(p, []byte(body), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	cfg := Default()
	cfg, err := Set(cfg, "vm.memory_mb", "4096")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = Set(cfg, "vm.boot_timeout", "90s")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = Set(cfg, "vm.kernel_cmdline.quiet", "1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.MemoryMB != 4096 || cfg.VM.BootTimeout != 90*time.Second || cfg.VM.KernelCmdline["quiet"] != "1" {
		t.Fatalf("set did not apply: %+v", cfg.VM)
	}
	if v, _ := Get(cfg, "vm.memory_mb"); v != "4096" {
		t.Fatalf("get = %q", v)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil || back.VM.BootTimeout != 90*time.Second {
		t.Fatalf("round trip: %v %+v", err, back.VM)
	}
}

func TestSetRejects(t *testing.T) {
	cases := [][2]string{
		{"vm.cpus", "4"},          // typo
		{"vm.vcpu", "abc"},        // wrong type
		{"vm.vcpu", "64"},         // out of range
		{"vm.boot_timeout", "1s"}, // too short
		{"network", "x"},          // not a leaf... replaces a section
	}
	for _, c := range cases {
		if _, err := Set(Default(), c[0], c[1]); err == nil {
			t.Errorf("Set(%s, %s): expected error", c[0], c[1])
		}
	}
}

func TestLoadMigratesLegacyKernel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	legacy := "vm:\n  kernel_image: " + legacyKernelImage + "\n  kernel_cmdline:\n    acpi: \"off\"\n    quiet: \"1\"\n"
	if err := os.WriteFile(p, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.KernelImage != DefaultKernelImage {
		t.Errorf("kernel_image = %q", cfg.VM.KernelImage)
	}
	if _, ok := cfg.VM.KernelCmdline["acpi"]; ok || cfg.VM.KernelCmdline["quiet"] != "1" {
		t.Errorf("kernel_cmdline = %v", cfg.VM.KernelCmdline)
	}

	// Another kernel keeps its arguments.
	own := "vm:\n  kernel_image: registry.example.com/kernel:5.10\n  kernel_cmdline:\n    acpi: \"off\"\n"
	if err := os.WriteFile(p, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Load(p); cfg.VM.KernelCmdline["acpi"] != "off" {
		t.Errorf("custom kernel lost acpi=off: %v", cfg.VM.KernelCmdline)
	}
}

func TestUnsetOnlyKernelCmdline(t *testing.T) {
	cfg, err := Set(Default(), "vm.kernel_cmdline.acpi", "off")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = Unset(cfg, "vm.kernel_cmdline.acpi")
	if err != nil || len(cfg.VM.KernelCmdline) != 0 {
		t.Fatalf("unset: %v %v", err, cfg.VM.KernelCmdline)
	}
	if _, err := Unset(Default(), "vm.vcpu"); err == nil {
		t.Fatal("expected error")
	}
}
