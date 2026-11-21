// Package host inspects the FireRunner host and manages its GitLab runner.
package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	RunnerConfig = "/etc/gitlab-runner/config.toml"
	RunnerBin    = "/usr/local/bin/gitlab-runner"
	ExecutorBin  = "/usr/local/bin/firerunner"
	ThinPool     = "flintlock/thinpool"
)

// DnsmasqConfig is the microVM DHCP/DNS configuration install.sh writes.
const DnsmasqConfig = "/etc/firerunner/dnsmasq.conf"

// Services installed by install.sh, in start order. firerunner-registry only
// starts the registry mirrors (or, on hosts not yet moved to a service per
// mirror, runs them all): see RegistryUnits for each mirror's own units.
var Services = []string{"containerd-flintlock", "firerunner-net", "firerunner-dnsmasq", RegistryService,
	"firerunner-cache", "flintlockd"}

// RegistryService starts every registry mirror's units (their PartOf=).
const RegistryService = "firerunner-registry"

// RegistryConfDir holds, for every registry mirror install.sh runs, NAME.yml
// (its registry) and NAME.env (its front).
const RegistryConfDir = "/etc/firerunner/registries.d"

// ProxyService is the forwarder's unit; install.sh runs it only with a
// corporate proxy (proxy.enabled).
const ProxyService = "firerunner-proxy"

// HostServices are Services with the registry mirrors' units (RegistryUnits)
// right after firerunner-registry. A new slice each call.
func HostServices(registryUnits []string) []string {
	out := make([]string, 0, len(Services)+len(registryUnits))
	for _, s := range Services {
		out = append(out, s)
		if s == RegistryService {
			out = append(out, registryUnits...)
		}
	}
	return out
}

// MonitoredServices are the units status and the daemon's
// firerunner_service_up report: HostServices, the forwarder where
// proxy.enabled is on (absent, not down, elsewhere), and gitlab-runner. A new
// slice each call.
func MonitoredServices(proxyEnabled bool, registryUnits []string) []string {
	out := HostServices(registryUnits)
	if proxyEnabled {
		out = append(out, ProxyService)
	}
	return append(out, "gitlab-runner")
}

// RegistryUnits are the systemd units of the registry mirrors configured in
// dir (RegistryConfDir): firerunner-registry@NAME and
// firerunner-registry-front@NAME for every NAME.env, by NAME. The files, not
// what systemd has loaded, say which mirrors must run: a mirror whose unit
// never started is down, not missing, and install.sh removes the files of a
// mirror it stops. Names install.sh would not write are skipped, so a stray
// file adds no series; the installer's port range (5000-5099) bounds them to
// 100. No dir is no mirror: an install from before a service per mirror.
func RegistryUnits(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry mirrors: %w", err)
	}
	var out []string
	for _, e := range entries { // sorted by file name
		name, ok := strings.CutSuffix(e.Name(), ".env")
		if !ok || e.IsDir() || !registryName(name) {
			continue
		}
		out = append(out, RegistryService+"@"+name, RegistryService+"-front@"+name)
	}
	return out, nil
}

// registryName reports whether s is a mirror name install.sh writes: docker.io
// or a registry host name ([a-z0-9]([a-z0-9.-]*[a-z0-9])?). It is also a
// systemd instance name and a label value, so nothing else gets through.
func registryName(s string) bool {
	if s == "" || strings.ContainsAny(s[:1]+s[len(s)-1:], ".-") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

// commandTimeout bounds each systemctl and lvs run: systemd or LVM stuck on
// a device must not stall the caller, the daemon's main loop included.
var commandTimeout = 10 * time.Second

// command builds the commands of host probes (a variable for tests).
var command = exec.CommandContext

// probe runs name with commandTimeout and returns its standard output.
func probe(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := command(ctx, name, args...)
	cmd.WaitDelay = time.Second // children holding stdout must not keep Output waiting
	return cmd.Output()
}

func ServiceActive(name string) bool { return ServiceActiveContext(context.Background(), name) }

// ServiceActiveContext reports whether a systemd unit is active; a systemctl
// that does not answer within commandTimeout reads as inactive.
func ServiceActiveContext(ctx context.Context, name string) bool {
	_, err := probe(ctx, "systemctl", "is-active", "--quiet", name)
	return err == nil
}

// ServicesActiveContext reports, for each of names in order, whether its
// systemd unit is active. One systemctl run answers for all of them: the
// daemon's main loop probes every unit, a pair per registry mirror included,
// and a hung systemd must stall it for one commandTimeout, not one per unit.
// A systemctl that fails or does not answer for every unit within
// commandTimeout reads as all inactive.
func ServicesActiveContext(ctx context.Context, names []string) []bool {
	up := make([]bool, len(names))
	if len(names) == 0 {
		return up
	}
	// It exits 0 if any unit is active (3 or 4 when none is), and prints
	// one state per unit either way. Killed (timeout) or cut off, what it
	// printed is not an answer.
	out, err := probe(ctx, "systemctl", append([]string{"is-active"}, names...)...)
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() < 0) {
		return up
	}
	states := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(states) != len(names) {
		return up
	}
	for i, s := range states {
		// What `systemctl is-active` itself counts as active.
		up[i] = s == "active" || s == "reloading" || s == "refreshing"
	}
	return up
}

// MountPoint reports whether a file system other than its parent's is mounted
// on dir (not "/"). A missing dir is not one.
func MountPoint(dir string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(dir, &st) != nil || syscall.Stat(filepath.Dir(dir), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
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
	return ThinPoolUsageContext(context.Background())
}

// ThinPoolUsageContext is ThinPoolUsage with lvs bounded by ctx and commandTimeout.
func ThinPoolUsageContext(ctx context.Context) (data, meta float64, err error) {
	out, err := probe(ctx, "lvs", "--noheadings", "--nosuffix", "-o", "data_percent,metadata_percent", ThinPool)
	if err != nil {
		return 0, 0, fmt.Errorf("lvs %s: %w", ThinPool, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected lvs output %q", out)
	}
	data, derr := strconv.ParseFloat(f[0], 64)
	meta, merr := strconv.ParseFloat(f[1], 64)
	if derr != nil || merr != nil {
		return 0, 0, fmt.Errorf("unexpected lvs output %q", out)
	}
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
	b, err := os.ReadFile(RunnerConfig)
	if err != nil {
		return nil, err
	}
	return parseRunner(string(b))
}

// parseRunner reads the global concurrent and the first [[runners]] entry:
// keys of that table only, not same-named ones in its sub-tables (a docker
// service's name) and never a comment.
func parseRunner(conf string) (*Runner, error) {
	r := &Runner{}
	runner := -1 // tableN of the first [[runners]]
	for _, s := range tomlStatements(conf) {
		switch {
		case s.header:
			if s.table == "[[runners]]" && runner < 0 {
				runner = s.tableN
			}
		case s.tableN == 0 && s.key == "concurrent":
			r.Concurrent, _ = strconv.Atoi(s.value)
		case s.tableN == runner:
			v, _ := tomlUnquote(s.value)
			switch s.key {
			case "name":
				r.Name = v
			case "url":
				r.URL = v
			case "executor":
				r.Executor = v
			}
		}
	}
	if r.URL == "" {
		return nil, errors.New("no runner registered")
	}
	r.CacheType, r.CacheServer = cacheFromConfig(conf)
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
	if out, err := runRootOnly(reg, RunnerConfig); err != nil {
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

// runRootOnly runs cmd, which writes secrets to path, so that path is never
// readable by others, not even until a later chmod: an existing path is made
// 0600 first, and a path cmd creates is 0600 at most (umask 077 while it
// runs, inherited by cmd). The umask is process-wide: call it only where no
// other goroutine creates files, as in the CLI.
func runRootOnly(cmd *exec.Cmd, path string) ([]byte, error) {
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	return cmd.CombinedOutput()
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
	return writeRootOnly(RunnerConfig, []byte(setConcurrent(string(b), n)))
}

// setConcurrent replaces the value of the root table's concurrent key, and
// nothing else: not a key that only starts with "concurrent", not one in a
// table, not its comment.
func setConcurrent(conf string, n int) string {
	for _, s := range tomlStatements(conf) {
		if !s.header && s.tableN == 0 && s.key == "concurrent" {
			return conf[:s.valStart] + strconv.Itoa(n) + conf[s.end:]
		}
	}
	return fmt.Sprintf("concurrent = %d\n", n) + conf
}

// writeRootOnly replaces path atomically with a 0600 file. gitlab-runner
// reloads config.toml when it changes and must never read half of it (it
// holds the runner token and the cache keys).
func writeRootOnly(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
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
