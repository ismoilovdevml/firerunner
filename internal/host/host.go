// Package host inspects the FireRunner host and manages its GitLab runner.
package host

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	RunnerConfig = "/etc/gitlab-runner/config.toml"
	RunnerBin    = "/usr/local/bin/gitlab-runner"
	ExecutorBin  = "/usr/local/bin/firerunner"
	ThinPool     = "flintlock/thinpool"
)

// DnsmasqConfig is the microVM DHCP/DNS configuration install.sh writes.
const DnsmasqConfig = "/etc/firerunner/dnsmasq.conf"

// Services installed by install.sh, in start order.
var Services = []string{"containerd-flintlock", "firerunner-net", "firerunner-dnsmasq", "firerunner-registry",
	"firerunner-cache", "flintlockd"}

func ServiceActive(name string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

func KVM() error {
	fi, err := os.Stat("/dev/kvm")
	if err != nil {
		return errors.New("/dev/kvm not found: enable VT-x/AMD-V or nested virtualization")
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return errors.New("/dev/kvm is not a character device")
	}
	return nil
}

// ThinPoolUsage returns data and metadata usage of the devmapper pool in percent.
func ThinPoolUsage() (data, meta float64, err error) {
	out, err := exec.Command("lvs", "--noheadings", "--nosuffix", "-o", "data_percent,metadata_percent", ThinPool).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("lvs %s: %w", ThinPool, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected lvs output %q", out)
	}
	data, _ = strconv.ParseFloat(f[0], 64)
	meta, _ = strconv.ParseFloat(f[1], 64)
	return data, meta, nil
}

func IPForward() bool {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// Runner is what config.toml says about the registered runner.
type Runner struct {
	Name       string
	URL        string
	Executor   string
	Concurrent int
	// CacheType is the shared cache backend ("s3") or empty when cache: only
	// lives inside the job VM.
	CacheType   string
	CacheServer string
}

func ReadRunner() (*Runner, error) {
	f, err := os.Open(RunnerConfig)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &Runner{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "concurrent":
			r.Concurrent, _ = strconv.Atoi(v)
		case "name":
			r.Name = v
		case "url":
			r.URL = v
		case "executor":
			r.Executor = v
		}
	}
	if r.URL == "" {
		return nil, errors.New("no runner registered")
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(RunnerConfig); err == nil {
		r.CacheType, r.CacheServer = cacheFromConfig(string(b))
	}
	return r, nil
}

// RegisterRunner registers a runner that uses `firerunner executor` for every job.
// The token is a runner authentication token (glrt-...) created in GitLab.
func RegisterRunner(url, token, name string, concurrent int) error {
	if _, err := os.Stat(RunnerBin); err != nil {
		return fmt.Errorf("%s not found; re-run install.sh", RunnerBin)
	}
	if _, err := os.Stat("/etc/systemd/system/gitlab-runner.service"); err != nil {
		if err := run(RunnerBin, "install", "--user", "root", "--working-directory", "/var/lib/gitlab-runner",
			"--config", RunnerConfig, "--service", "gitlab-runner"); err != nil {
			return err
		}
	}
	if existing, err := ReadRunner(); err == nil {
		return fmt.Errorf("a runner for %s is already registered; run 'firerunner runner unregister' first", existing.URL)
	}
	if err := os.MkdirAll("/var/lib/gitlab-runner", 0o755); err != nil {
		return err
	}
	// The token goes through the environment, not argv, so local users cannot
	// read it from /proc/<pid>/cmdline while gitlab-runner registers.
	reg := exec.Command(RunnerBin, "register", "--non-interactive",
		"--config", RunnerConfig,
		"--url", url, "--name", name,
		"--executor", "custom",
		"--builds-dir", "/root/builds", "--cache-dir", "/root/cache",
		"--custom-prepare-exec", ExecutorBin, "--custom-prepare-args", "executor", "--custom-prepare-args", "prepare",
		"--custom-run-exec", ExecutorBin, "--custom-run-args", "executor", "--custom-run-args", "run",
		"--custom-cleanup-exec", ExecutorBin, "--custom-cleanup-args", "executor", "--custom-cleanup-args", "cleanup",
	)
	reg.Env = append(os.Environ(), "CI_SERVER_TOKEN="+token)
	if out, err := reg.CombinedOutput(); err != nil {
		return fmt.Errorf("gitlab-runner register: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := SetConcurrent(concurrent); err != nil {
		return err
	}
	if c, err := LocalCache(); err == nil {
		if err := SetCache(c); err != nil {
			return err
		}
	}
	if err := os.Chmod(RunnerConfig, 0o600); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", "gitlab-runner"); err != nil {
		return err
	}
	if err := run("systemctl", "restart", "gitlab-runner"); err != nil {
		return err
	}
	return run(RunnerBin, "verify", "--config", RunnerConfig)
}

// UnregisterRunner removes the runner from this host and from GitLab.
func UnregisterRunner() error {
	if err := run(RunnerBin, "unregister", "--all-runners", "--config", RunnerConfig); err != nil {
		return err
	}
	return run("systemctl", "restart", "gitlab-runner")
}

// SetConcurrent sets the global "concurrent" limit in config.toml.
func SetConcurrent(n int) error {
	if n < 1 {
		return errors.New("concurrent must be at least 1")
	}
	b, err := os.ReadFile(RunnerConfig)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "concurrent") {
			lines[i] = fmt.Sprintf("concurrent = %d", n)
			found = true
		}
	}
	if !found {
		lines = append([]string{fmt.Sprintf("concurrent = %d", n)}, lines...)
	}
	return os.WriteFile(RunnerConfig, []byte(strings.Join(lines, "\n")), 0o600)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// MemAvailableMB reads MemAvailable from /proc/meminfo.
func MemAvailableMB() (int, error) { return meminfo("MemAvailable:") }

// MemTotalMB reads MemTotal from /proc/meminfo.
func MemTotalMB() (int, error) { return meminfo("MemTotal:") }

func meminfo(key string) (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == key {
			kb, err := strconv.Atoi(fields[1])
			return kb / 1024, err
		}
	}
	return 0, fmt.Errorf("%s not found in /proc/meminfo", key)
}

// OOMKills returns how many processes the kernel OOM-killed since boot.
func OOMKills() (int64, error) {
	f, err := os.Open("/proc/vmstat")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseOOMKills(f)
}

func parseOOMKills(r io.Reader) (int64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok && k == "oom_kill" {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no oom_kill in /proc/vmstat")
}

// DHCPRangeSize returns how many addresses the dhcp-range in a dnsmasq
// configuration hands out.
func DHCPRangeSize(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseDHCPRange(f)
}

func parseDHCPRange(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "dhcp-range=")
		if !ok {
			continue
		}
		f := strings.Split(v, ",")
		if len(f) < 2 {
			break
		}
		start, end := net.ParseIP(f[0]).To4(), net.ParseIP(f[1]).To4()
		if start == nil || end == nil {
			break
		}
		n := int(binary.BigEndian.Uint32(end)) - int(binary.BigEndian.Uint32(start)) + 1
		if n <= 0 {
			break
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no IPv4 dhcp-range")
}
