package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestProxyValidation(t *testing.T) {
	ok := Default()
	ok.Proxy.Enabled = true
	ok.Proxy.NoProxy = "gitlab.corp, .corp,10.0.0.0/8"
	ok.VM.InsecureRegistries = []string{"harbor.corp:443", "http://10.0.0.5:5000"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid proxy config refused: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"newline in no_proxy":        func(c *Config) { c.Proxy.NoProxy = "a.corp\nLD_PRELOAD=/x.so" },
		"assignment in no_proxy":     func(c *Config) { c.Proxy.NoProxy = "a=b" },
		"quote in no_proxy":          func(c *Config) { c.Proxy.NoProxy = `a"b` },
		"listen without port":        func(c *Config) { c.Proxy.Listen = "10.200.0.1" },
		"listen on a name":           func(c *Config) { c.Proxy.Listen = "bridge:3128" },
		"no upstream file":           func(c *Config) { c.Proxy.UpstreamFile = "" },
		"registry with a path":       func(c *Config) { c.VM.InsecureRegistries = []string{"harbor.corp/v2"} },
		"registry with https scheme": func(c *Config) { c.VM.InsecureRegistries = []string{"https://harbor.corp"} },
		"empty registry":             func(c *Config) { c.VM.InsecureRegistries = []string{""} },
		"registry with a newline":    func(c *Config) { c.VM.InsecureRegistries = []string{"a\nb"} },
	} {
		c := ok
		c.VM.InsecureRegistries = append([]string(nil), ok.VM.InsecureRegistries...)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// With the proxy off, its fields are not checked (defaults stay valid).
	off := Default()
	off.Proxy.Listen = "nonsense"
	if err := off.Validate(); err != nil {
		t.Fatalf("proxy off: %v", err)
	}
}

// A config written by this version stays loadable by v0.1.0 and older while
// the newer settings are unused: those binaries refuse unknown keys.
func TestSaveLeavesOutUnusedNewKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Default()
	cfg.Pool.Size = 3
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, key := range []string{"proxy:", "ca_file:", "insecure_registries:", "egress_deny:", "job_max_vcpu:", "job_max_memory_mb:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("unused %s written:\n%s", key, data)
		}
	}
	got, err := Load(path)
	if err != nil || got.Pool.Size != 3 || got.Proxy.Listen != "10.200.0.1:3128" || len(got.Proxy.ConnectPorts) != 1 {
		t.Fatalf("reload: %+v %v", got.Proxy, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}

	cfg.Proxy.Enabled = true
	cfg.VM.CAFile = "/etc/firerunner/ca.pem"
	cfg.Network.EgressDeny = []string{"192.168.0.0/16"}
	cfg.VM.JobMaxVCPU, cfg.VM.JobMaxMemoryMB = 4, 4096
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	for _, key := range []string{"proxy:", "ca_file:", "egress_deny:", "job_max_vcpu:", "job_max_memory_mb:"} {
		if !strings.Contains(string(data), key) {
			t.Errorf("used %s not written", key)
		}
	}
	if got, err := Load(path); err != nil || !got.Proxy.Enabled || got.Network.EgressDeny[0] != "192.168.0.0/16" || len(got.Proxy.ConnectPorts) != 1 {
		t.Fatalf("reload with proxy: %+v %v", got, err)
	}
	// connect_ports is unknown to v0.1.0: written only when not the default.
	if strings.Contains(string(data), "connect_ports") {
		t.Fatalf("default connect_ports written:\n%s", data)
	}
}

// Values end up in systemd units (where %h expands), shell scripts, TOML and
// nft: only plain host, domain, CIDR and port characters are accepted.
func TestCorporateValuesAllowList(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"systemd specifier in no_proxy": func(c *Config) { c.Proxy.NoProxy = "a%h.corp" },
		"hash in no_proxy":              func(c *Config) { c.Proxy.NoProxy = "a#b" },
		"pipe in no_proxy":              func(c *Config) { c.Proxy.NoProxy = "a|b&c" },
		"control char in registry":      func(c *Config) { c.VM.InsecureRegistries = []string{"harbor\x01:443"} },
		"bad egress_deny":               func(c *Config) { c.Network.EgressDeny = []string{"10.0.0.0/33"} },
		"proxy listen without a port":   func(c *Config) { c.Proxy.Enabled = true; c.Proxy.Listen = "10.200.0.1" },
		"connect port 0":                func(c *Config) { c.Proxy.Enabled = true; c.Proxy.ConnectPorts = []int{0} },
	} {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Default()
	ok.Proxy.NoProxy = "gitlab.corp,.corp,*.corp,10.0.0.0/8,fd00::/8,host_1,::1"
	ok.VM.InsecureRegistries = []string{"harbor.corp:443", "http://10.0.0.5:5000", "registry"}
	ok.Network.EgressDeny = []string{"192.168.0.0/16", "10.1.2.3"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid values refused: %v", err)
	}
}

func TestJobMaxSizeKeys(t *testing.T) {
	cfg, err := Set(Default(), "vm.job_max_memory_mb", "4096")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = Set(cfg, "vm.job_max_vcpu", "4"); err != nil {
		t.Fatal(err)
	}
	if v, _ := Get(cfg, "vm.job_max_memory_mb"); v != "4096" {
		t.Fatalf("vm.job_max_memory_mb = %q", v)
	}
	for _, c := range []struct{ key, value string }{
		{"vm.job_max_memory_mb", "-1"}, {"vm.job_max_memory_mb", "70000"}, {"vm.job_max_memory_mb", "4G"},
		{"vm.job_max_vcpu", "-1"}, {"vm.job_max_vcpu", "33"},
	} {
		if _, err := Set(Default(), c.key, c.value); err == nil {
			t.Errorf("Set(%s, %s) accepted", c.key, c.value)
		}
	}
}

func TestFlintlockTLSKeys(t *testing.T) {
	for name, mutate := range map[string]func(c *Config){
		"cert without key": func(c *Config) { c.Flintlock.TLSCAFile, c.Flintlock.TLSCertFile = "/ca.pem", "/c.pem" },
		"key without cert": func(c *Config) { c.Flintlock.TLSCAFile, c.Flintlock.TLSKeyFile = "/ca.pem", "/c.key" },
		"cert without CA":  func(c *Config) { c.Flintlock.TLSCertFile, c.Flintlock.TLSKeyFile = "/c.pem", "/c.key" },
	} {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Default()
	ok.Flintlock.TLSCAFile = "/etc/firerunner/flintlock-tls/ca.pem"
	if err := ok.Validate(); err != nil {
		t.Fatalf("server-only TLS refused: %v", err)
	}
	ok.Flintlock.TLSCertFile, ok.Flintlock.TLSKeyFile = "/c.pem", "/c.key"
	if err := ok.Validate(); err != nil {
		t.Fatalf("mutual TLS refused: %v", err)
	}

	// Unused, the keys stay out of the saved config (v0.1.0 refuses them).
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "tls_") {
		t.Fatalf("unused TLS keys written:\n%s", data)
	}
	if err := Save(path, ok); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Flintlock.TLSCAFile != ok.Flintlock.TLSCAFile || got.Flintlock.TLSKeyFile != "/c.key" {
		t.Fatalf("reload: %+v %v", got.Flintlock, err)
	}
}
