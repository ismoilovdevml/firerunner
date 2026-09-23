// Package config loads, edits and validates /etc/firerunner/config.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where install.sh writes the config. FIRERUNNER_CONFIG overrides it.
const DefaultPath = "/etc/firerunner/config.yaml"

type Config struct {
	Flintlock Flintlock `yaml:"flintlock"`
	VM        VM        `yaml:"vm"`
	Network   Network   `yaml:"network"`
}

type Flintlock struct {
	Endpoint  string `yaml:"endpoint"`
	TokenFile string `yaml:"token_file"`
	Namespace string `yaml:"namespace"`
}

type VM struct {
	VCPU          int               `yaml:"vcpu"`
	MemoryMB      int               `yaml:"memory_mb"`
	KernelImage   string            `yaml:"kernel_image"`
	KernelCmdline map[string]string `yaml:"kernel_cmdline"`
	RootFSImage   string            `yaml:"rootfs_image"`
	BootTimeout   time.Duration     `yaml:"boot_timeout"`
}

type Network struct {
	LeasesFile string `yaml:"leases_file"`
	SSHKey     string `yaml:"ssh_key"`
}

func Default() Config {
	return Config{
		Flintlock: Flintlock{
			Endpoint:  "127.0.0.1:9090",
			TokenFile: "/etc/firerunner/flintlock.token",
			Namespace: "firerunner",
		},
		VM: VM{
			VCPU:        2,
			MemoryMB:    2048,
			KernelImage: "ghcr.io/liquidmetal-dev/flintlock-kernel:5.10.77",
			// The 5.10 kernel cannot parse the ACPI tables of Firecracker >= 1.11.
			KernelCmdline: map[string]string{"acpi": "off"},
			RootFSImage:   "ghcr.io/ismoilovdevml/firerunner-rootfs:latest",
			BootTimeout:   3 * time.Minute,
		},
		Network: Network{
			LeasesFile: "/var/lib/misc/firerunner-dnsmasq.leases",
			SSHKey:     "/etc/firerunner/executor/id_ed25519",
		},
	}
}

// Path returns the config file location.
func Path() string {
	if p := os.Getenv("FIRERUNNER_CONFIG"); p != "" {
		return p
	}
	return DefaultPath
}

// Load reads the config file on top of the defaults. A missing file yields the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := decode(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

func decode(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty file decodes as "no changes".
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c Config) Validate() error {
	var errs []string
	if c.Flintlock.Endpoint == "" {
		errs = append(errs, "flintlock.endpoint is required")
	}
	if c.Flintlock.Namespace == "" {
		errs = append(errs, "flintlock.namespace is required")
	}
	if c.VM.VCPU < 1 || c.VM.VCPU > 32 {
		errs = append(errs, "vm.vcpu must be between 1 and 32")
	}
	if c.VM.MemoryMB < 256 || c.VM.MemoryMB > 65536 {
		errs = append(errs, "vm.memory_mb must be between 256 and 65536")
	}
	if c.VM.KernelImage == "" || c.VM.RootFSImage == "" {
		errs = append(errs, "vm.kernel_image and vm.rootfs_image are required")
	}
	if c.VM.BootTimeout < 10*time.Second {
		errs = append(errs, "vm.boot_timeout must be at least 10s")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Save writes the config atomically with 0644 permissions (it holds no secrets).
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Keys lists every settable key in dotted form, e.g. "vm.vcpu".
func Keys(cfg Config) []string {
	m, _ := toMap(cfg)
	var keys []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if mm, ok := v.(map[string]any); ok {
			for k, sub := range mm {
				walk(join(prefix, k), sub)
			}
			return
		}
		keys = append(keys, prefix)
	}
	walk("", m)
	sort.Strings(keys)
	return keys
}

// Get returns the value at a dotted key.
func Get(cfg Config, key string) (string, error) {
	m, err := toMap(cfg)
	if err != nil {
		return "", err
	}
	var cur any = m
	for _, part := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("unknown key %q", key)
		}
		if cur, ok = mm[part]; !ok {
			return "", fmt.Errorf("unknown key %q", key)
		}
	}
	out, err := yaml.Marshal(cur)
	return strings.TrimSpace(string(out)), err
}

// Set changes one dotted key; the value is parsed as YAML ("4", "3m", "off").
// New keys are only accepted under vm.kernel_cmdline.
func Set(cfg Config, key, value string) (Config, error) {
	m, err := toMap(cfg)
	if err != nil {
		return cfg, err
	}
	parts := strings.Split(key, ".")
	cur := m
	for i, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			if strings.Join(parts[:i+1], ".") == "vm.kernel_cmdline" {
				next = map[string]any{}
				cur[part] = next
			} else {
				return cfg, fmt.Errorf("unknown key %q", key)
			}
		}
		cur = next
	}
	last := parts[len(parts)-1]
	parent := strings.Join(parts[:len(parts)-1], ".")
	if _, ok := cur[last]; !ok && parent != "vm.kernel_cmdline" {
		return cfg, fmt.Errorf("unknown key %q (see: firerunner config keys)", key)
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(value), &parsed); err != nil {
		return cfg, fmt.Errorf("invalid value %q: %w", value, err)
	}
	if parent == "vm.kernel_cmdline" {
		parsed = value // kernel args are always strings
	}
	cur[last] = parsed

	data, err := yaml.Marshal(m)
	if err != nil {
		return cfg, err
	}
	out := Config{}
	if err := decode(data, &out); err != nil {
		return cfg, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	return out, out.Validate()
}

// Unset removes a vm.kernel_cmdline entry.
func Unset(cfg Config, key string) (Config, error) {
	const prefix = "vm.kernel_cmdline."
	if !strings.HasPrefix(key, prefix) {
		return cfg, fmt.Errorf("only %s* keys can be removed", prefix)
	}
	out := cfg
	out.VM.KernelCmdline = map[string]string{}
	for k, v := range cfg.VM.KernelCmdline {
		if k != strings.TrimPrefix(key, prefix) {
			out.VM.KernelCmdline[k] = v
		}
	}
	return out, nil
}

func toMap(cfg Config) (map[string]any, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, yaml.Unmarshal(data, &m)
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}
