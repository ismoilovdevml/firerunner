// Command firerunner manages a FireRunner host: ephemeral Firecracker microVMs
// for GitLab CI jobs, driven through flintlock.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/executor"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
	"github.com/ismoilovdevml/firerunner/internal/upgrade"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

var version = "dev"

const usage = `firerunner - ephemeral Firecracker microVMs for GitLab CI

Usage:
  firerunner status                         show host, microVM and runner state
  firerunner doctor                         check everything and explain what is wrong

  firerunner config show                    print the effective config
  firerunner config keys                    list settable keys
  firerunner config get <key>
  firerunner config set <key> <value>       e.g. vm.vcpu 4, vm.memory_mb 4096, vm.boot_timeout 2m
  firerunner config unset vm.kernel_cmdline.<arg>

  firerunner runner register --url <gitlab-url> --token <glrt-...> [--concurrent 4] [--name NAME]
  firerunner runner status
  firerunner runner concurrent <n>          max parallel jobs (= microVMs)
  firerunner runner cache [local|off|s3 --server HOST:PORT --bucket B [--insecure]]
                                            where cache: is stored (default: local S3 on this host)
  firerunner runner unregister

  firerunner vm list
  firerunner vm rm <id|uid>... | --all
  firerunner vm logs <id|uid> [-n 100]

  firerunner run [--keep] -- <command...>   boot a microVM, run a command, delete it
  firerunner pool [refresh]                 show pre-booted microVMs; refresh replaces them
                                            (after a new rootfs image under the same tag)

  firerunner daemon                         pool + reconcile + metrics (systemd: firerunner.service)
  firerunner executor prepare|run|cleanup   called by gitlab-runner (custom executor)
  firerunner upgrade [--version edge|latest|vX.Y.Z] [--check]
                                            replace this binary with a release (default: edge)
  firerunner version | -v | --version
`

func main() {
	err := dispatch(os.Args[1:])
	var exitErr *executor.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		fmt.Fprintln(os.Stderr, "firerunner:", exitErr.Err)
		os.Exit(exitErr.Code)
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "firerunner:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("firerunner", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "config":
		return cmdConfig(rest)
	case "upgrade":
		return cmdUpgrade(rest)
	}

	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	switch cmd {
	case "status":
		return cmdStatus(cfg)
	case "doctor":
		return cmdDoctor(cfg)
	case "runner":
		return cmdRunner(rest)
	case "vm":
		return cmdVM(cfg, rest)
	case "run":
		return cmdRun(cfg, rest)
	case "executor":
		return cmdExecutor(cfg, rest)
	case "daemon":
		return cmdDaemon()
	case "pool":
		dc := daemon.NewClient(cfg.Daemon.Socket)
		if len(rest) > 0 && rest[0] == "refresh" {
			if err := dc.Refresh(); err != nil {
				return fmt.Errorf("daemon not reachable (systemctl status firerunner): %w", err)
			}
			fmt.Println("idle pool VMs are being replaced")
			return nil
		}
		raw, err := dc.Pool()
		if err != nil {
			return fmt.Errorf("daemon not reachable (systemctl status firerunner): %w", err)
		}
		var out bytes.Buffer
		_ = json.Indent(&out, raw, "", "  ")
		fmt.Println(out.String())
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

// ---------------------------------------------------------------------------
// config

func cmdConfig(args []string) error {
	path := config.Path()
	cfg, err := config.Load(path)
	if err != nil && (len(args) == 0 || args[0] != "set") {
		return err
	}
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "show":
		fmt.Printf("# %s\n", path)
		for _, k := range config.Keys(cfg) {
			v, _ := config.Get(cfg, k)
			fmt.Printf("%s = %s\n", k, v)
		}
		return nil
	case sub == "keys":
		fmt.Println(strings.Join(config.Keys(cfg), "\n"))
		return nil
	case sub == "path":
		fmt.Println(path)
		return nil
	case sub == "get" && len(args) == 2:
		v, err := config.Get(cfg, args[1])
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	case sub == "set" && len(args) == 3:
		next, err := config.Set(cfg, args[1], args[2])
		if err != nil {
			return err
		}
		if err := config.Save(path, next); err != nil {
			return err
		}
		v, _ := config.Get(next, args[1])
		fmt.Printf("%s = %s (applies to the next job)\n", args[1], v)
		return nil
	case sub == "unset" && len(args) == 2:
		next, err := config.Unset(cfg, args[1])
		if err != nil {
			return err
		}
		return config.Save(path, next)
	}
	return errors.New("usage: firerunner config show|keys|path|get <key>|set <key> <value>|unset <key>")
}

// ---------------------------------------------------------------------------
// status / doctor

func cmdStatus(cfg config.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "FireRunner\t%s\n", version)

	kvm := "ok"
	if err := host.KVM(); err != nil {
		kvm = err.Error()
	}
	fmt.Fprintf(w, "KVM\t%s\n", kvm)

	var svc []string
	for _, s := range append(host.Services, "gitlab-runner") {
		state := "down"
		if host.ServiceActive(s) {
			state = "up"
		}
		svc = append(svc, s+" "+state)
	}
	fmt.Fprintf(w, "Services\t%s\n", strings.Join(svc, ", "))

	if data, meta, err := host.ThinPoolUsage(); err == nil {
		fmt.Fprintf(w, "Thin pool\tdata %.1f%%, metadata %.1f%%\n", data, meta)
	} else {
		fmt.Fprintf(w, "Thin pool\t%v\n", err)
	}

	fmt.Fprintf(w, "microVM size\t%d vCPU, %d MB, boot timeout %s\n", cfg.VM.VCPU, cfg.VM.MemoryMB, cfg.VM.BootTimeout)
	fmt.Fprintf(w, "Images\t%s\n\t%s\n", cfg.VM.RootFSImage, cfg.VM.KernelImage)

	if vms, err := listVMs(cfg); err == nil {
		states := map[string]int{}
		for _, v := range vms {
			states[strings.ToLower(v.GetStatus().GetState().String())]++
		}
		fmt.Fprintf(w, "microVMs\t%d %s\n", len(vms), formatCounts(states))
	} else {
		fmt.Fprintf(w, "microVMs\tflintlock unreachable: %v\n", err)
	}

	if r, err := host.ReadRunner(); err == nil {
		fmt.Fprintf(w, "Runner\t%s -> %s (executor %s, concurrent %d)\n", r.Name, r.URL, r.Executor, r.Concurrent)
		fmt.Fprintf(w, "Cache\t%s\n", cacheDesc(r))
	} else {
		fmt.Fprintf(w, "Runner\tnot registered (firerunner runner register --url ... --token glrt-...)\n")
	}
	return nil
}

func cmdDoctor(cfg config.Config) error {
	failed := 0
	check := func(name string, err error, hint string) {
		if err == nil {
			fmt.Printf("  ok    %s\n", name)
			return
		}
		failed++
		fmt.Printf("  FAIL  %s: %v\n", name, err)
		if hint != "" {
			fmt.Printf("        -> %s\n", hint)
		}
	}

	fmt.Println("Host")
	check("/dev/kvm", host.KVM(), "enable VT-x/AMD-V (bare metal) or nested virtualization (VM)")
	for _, s := range host.Services {
		var err error
		if !host.ServiceActive(s) {
			err = errors.New("not running")
		}
		check("service "+s, err, "journalctl -u "+s+" -n 50")
	}
	var fwd error
	if !host.IPForward() {
		fwd = errors.New("net.ipv4.ip_forward=0")
	}
	check("IP forwarding", fwd, "systemctl restart firerunner-net")
	data, meta, err := host.ThinPoolUsage()
	if err == nil && (data > 80 || meta > 80) {
		err = fmt.Errorf("data %.1f%%, metadata %.1f%%", data, meta)
	}
	check("thin pool below 80%", err, "remove unused images or extend the flintlock volume group")

	fmt.Println("flintlock")
	_, err = listVMs(cfg)
	check("API "+cfg.Flintlock.Endpoint, err, "systemctl status flintlockd; token in "+cfg.Flintlock.TokenFile)
	_, err = os.Stat(cfg.Network.SSHKey)
	check("executor SSH key", err, "re-run install.sh")

	fmt.Println("GitLab runner")
	if r, err := host.ReadRunner(); err != nil {
		fmt.Println("  warn  not registered yet")
		fmt.Println("        -> firerunner runner register --url <gitlab-url> --token <glrt-...>")
	} else {
		var err error
		if !host.ServiceActive("gitlab-runner") {
			err = errors.New("not running")
		}
		check("service gitlab-runner", err, "journalctl -u gitlab-runner -n 50")
		switch local, _ := host.LocalCache(); {
		case r.CacheType == "":
			fmt.Println("  warn  no shared cache: cache: is lost after each job")
			fmt.Println("        -> firerunner runner cache local")
		case local != nil && r.CacheServer == local.Server:
			var err error
			if !host.ServiceActive("firerunner-cache") {
				err = errors.New("not running")
			}
			check("cache server "+local.Server, err, "journalctl -u firerunner-cache -n 50")
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("\nAll checks passed. Smoke test: firerunner run -- uname -a")
	return nil
}

// ---------------------------------------------------------------------------
// runner

func cmdRunner(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "register":
		fs := flag.NewFlagSet("runner register", flag.ContinueOnError)
		url := fs.String("url", "", "GitLab URL, e.g. https://gitlab.example.com")
		token := fs.String("token", os.Getenv("FIRERUNNER_RUNNER_TOKEN"), "runner authentication token (glrt-...)")
		hostname, _ := os.Hostname()
		name := fs.String("name", "firerunner-"+hostname, "runner name shown in GitLab")
		concurrent := fs.Int("concurrent", 4, "max parallel jobs")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *url == "" || *token == "" {
			return errors.New("--url and --token are required (create the runner in GitLab: Settings > CI/CD > Runners > New runner)")
		}
		if err := host.RegisterRunner(*url, *token, *name, *concurrent); err != nil {
			return err
		}
		fmt.Printf("Runner %s registered with %s (%d concurrent microVM jobs)\n", *name, *url, *concurrent)
		return nil
	case "status":
		r, err := host.ReadRunner()
		if err != nil {
			return err
		}
		state := "down"
		if host.ServiceActive("gitlab-runner") {
			state = "up"
		}
		fmt.Printf("name        %s\nurl         %s\nexecutor    %s\nconcurrent  %d\ncache       %s\nservice     %s\n",
			r.Name, r.URL, r.Executor, r.Concurrent, cacheDesc(r), state)
		return nil
	case "cache":
		return cmdRunnerCache(args[1:])
	case "concurrent":
		if len(args) != 2 {
			return errors.New("usage: firerunner runner concurrent <n>")
		}
		var n int
		if _, err := fmt.Sscan(args[1], &n); err != nil {
			return err
		}
		if err := host.SetConcurrent(n); err != nil {
			return err
		}
		fmt.Printf("concurrent = %d (gitlab-runner reloads it automatically)\n", n)
		return nil
	case "unregister":
		return host.UnregisterRunner()
	}
	return errors.New("usage: firerunner runner register|status|concurrent|cache|unregister")
}

func cacheDesc(r *host.Runner) string {
	if r.CacheType == "" {
		return "none (cache: stays in the job VM)"
	}
	return r.CacheType + " " + r.CacheServer
}

func cmdRunnerCache(args []string) error {
	if len(args) == 0 {
		r, err := host.ReadRunner()
		if err != nil {
			return err
		}
		fmt.Println(cacheDesc(r))
		return nil
	}
	var c *host.Cache
	switch args[0] {
	case "local":
		var err error
		if c, err = host.LocalCache(); err != nil {
			return err
		}
	case "off":
	case "s3":
		fs := flag.NewFlagSet("runner cache s3", flag.ContinueOnError)
		server := fs.String("server", "", "S3 endpoint host:port, reachable from the host and from the microVMs")
		bucket := fs.String("bucket", "", "existing bucket")
		insecure := fs.Bool("insecure", false, "use plain HTTP")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c = &host.Cache{Server: *server, Bucket: *bucket, Insecure: *insecure,
			AccessKey: os.Getenv("FIRERUNNER_CACHE_ACCESS_KEY"), SecretKey: os.Getenv("FIRERUNNER_CACHE_SECRET_KEY")}
		if c.AccessKey == "" || c.SecretKey == "" {
			return errors.New("set FIRERUNNER_CACHE_ACCESS_KEY and FIRERUNNER_CACHE_SECRET_KEY")
		}
	default:
		return errors.New("usage: firerunner runner cache [local|off|s3 --server HOST:PORT --bucket B [--insecure]]")
	}
	if err := host.SetCache(c); err != nil {
		return err
	}
	if c == nil {
		fmt.Println("shared cache off (gitlab-runner reloads config.toml automatically)")
	} else {
		fmt.Printf("cache: stored in s3://%s at %s (gitlab-runner reloads config.toml automatically)\n", c.Bucket, c.Server)
	}
	return nil
}

// ---------------------------------------------------------------------------
// vm

func listVMs(cfg config.Config) ([]*types.MicroVM, error) {
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return nil, err
	}
	defer fl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fl.List(ctx)
}

func cmdVM(cfg config.Config, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return err
	}
	defer fl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch args[0] {
	case "list", "ls":
		vms, err := fl.List(ctx)
		if err != nil {
			return err
		}
		sort.Slice(vms, func(i, j int) bool { return vms[i].GetSpec().GetId() < vms[j].GetSpec().GetId() })
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tUID\tROLE\tSTATE\tVCPU\tMEMORY\tIP")
		for _, v := range vms {
			s := v.GetSpec()
			var mac string
			if ifs := s.GetInterfaces(); len(ifs) > 0 {
				mac = ifs[len(ifs)-1].GetGuestMac()
			}
			ip, _ := vm.LeaseIP(cfg.Network.LeasesFile, mac)
			role, _ := daemon.RoleOf(s.GetId())
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d MB\t%s\n", s.GetId(), s.GetUid(), dash(role),
				strings.ToLower(v.GetStatus().GetState().String()), s.GetVcpu(), s.GetMemoryInMb(), dash(ip))
		}
		return w.Flush()
	case "rm", "delete":
		refs := args[1:]
		if len(refs) == 1 && refs[0] == "--all" {
			vms, err := fl.List(ctx)
			if err != nil {
				return err
			}
			refs = nil
			for _, v := range vms {
				refs = append(refs, v.GetSpec().GetUid())
			}
		}
		if len(refs) == 0 {
			return errors.New("usage: firerunner vm rm <id|uid>... | --all")
		}
		failed := 0
		for _, ref := range refs {
			v, err := fl.Find(ctx, ref)
			if err == nil {
				err = fl.Delete(ctx, v.GetSpec().GetUid())
			}
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "deleting %s: %v\n", ref, err)
				continue
			}
			vm.RemoveKnownHosts(v.GetSpec().GetId())
			fmt.Printf("deleted %s (%s)\n", v.GetSpec().GetId(), v.GetSpec().GetUid())
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d deletes failed", failed, len(refs))
		}
		return nil
	case "logs":
		fs := flag.NewFlagSet("vm logs", flag.ContinueOnError)
		lines := fs.Int("n", 100, "number of lines")
		if len(args) < 2 {
			return errors.New("usage: firerunner vm logs <id|uid> [-n 100]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		v, err := fl.Find(ctx, args[1])
		if err != nil {
			return err
		}
		s := v.GetSpec()
		path := filepath.Join("/var/lib/flintlock/vm", s.GetNamespace(), s.GetId(), s.GetUid(), "firecracker.stdout")
		return tail(path, *lines, os.Stdout)
	}
	return errors.New("usage: firerunner vm list|rm|logs")
}

// ---------------------------------------------------------------------------
// run: ad-hoc smoke test

func cmdRun(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	keep := fs.Bool("keep", false, "keep the microVM after the command finishes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := fs.Args()
	if len(command) == 0 {
		return errors.New("usage: firerunner run [--keep] -- <command...>")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return err
	}
	defer fl.Close()

	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := "run-" + hex.EncodeToString(suffix)
	start := time.Now()
	fmt.Fprintf(os.Stderr, "booting %s (%d vCPU, %d MB)...\n", id, cfg.VM.VCPU, cfg.VM.MemoryMB)
	inst, err := vm.Boot(ctx, cfg, fl, id, map[string]string{daemon.LabelRole: "run"})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s ready at %s in %s\n", id, inst.IP, time.Since(start).Round(100*time.Millisecond))

	code, runErr := vm.RunScript(cfg, inst, strings.NewReader(strings.Join(command, " ")+"\n"), os.Stdout, os.Stderr)
	if *keep {
		fmt.Fprintf(os.Stderr, "kept %s: ssh -i %s -o UserKnownHostsFile=%s/%s -o HostKeyAlias=%s root@%s   (delete: firerunner vm rm %s)\n",
			id, cfg.Network.SSHKey, vm.KnownHostsDir, id, id, inst.IP, id)
	} else {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fl.Delete(dctx, inst.UID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: deleting %s: %v\n", id, err)
		} else {
			vm.RemoveKnownHosts(id)
			fmt.Fprintf(os.Stderr, "deleted %s (total %s)\n", id, time.Since(start).Round(100*time.Millisecond))
		}
	}
	if runErr != nil {
		return runErr
	}
	if code != 0 {
		return &executor.ExitError{Code: code, Err: fmt.Errorf("command exited with %d", code)}
	}
	return nil
}

// ---------------------------------------------------------------------------
// executor

func cmdExecutor(cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firerunner executor prepare|run <script> <stage>|cleanup")
	}
	switch args[0] {
	case "prepare":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return executor.Prepare(ctx, cfg)
	case "run":
		if len(args) != 3 {
			return errors.New("usage: firerunner executor run <script> <stage>")
		}
		return executor.Run(cfg, args[1], args[2])
	case "cleanup":
		return executor.Cleanup(cfg)
	}
	return fmt.Errorf("unknown executor stage %q", args[0])
}

// ---------------------------------------------------------------------------
// upgrade

func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	tag := fs.String("version", "edge", "release to install: edge (main), latest (newest stable) or a tag like v1.2.0")
	check := fs.Bool("check", false, "only show which version would be installed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := upgrade.Run(context.Background(), *tag, version, *check)
	if err != nil {
		return err
	}
	switch {
	case !res.Changed:
		fmt.Printf("firerunner %s is already the %s release\n", res.From, *tag)
	case *check:
		fmt.Printf("firerunner %s -> %s available (run: firerunner upgrade --version %s)\n", res.From, res.To, *tag)
	default:
		fmt.Printf("firerunner upgraded %s -> %s\n", res.From, res.To)
		if host.ServiceActive("firerunner") {
			if err := exec.Command("systemctl", "restart", "firerunner").Run(); err != nil {
				return fmt.Errorf("restarting firerunner.service: %w", err)
			}
			fmt.Println("firerunner.service restarted")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// daemon

func cmdDaemon() error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.Version = version
	d, err := daemon.New(config.Path(), log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}

// ---------------------------------------------------------------------------
// helpers

func formatCounts(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tail(path string, n int, w io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	_, err = fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}
