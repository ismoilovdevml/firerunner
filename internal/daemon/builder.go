package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// Builder states returned to the executor.
const (
	BuilderReady    = "ready"    // use it
	BuilderBooting  = "booting"  // build locally this time
	BuilderBusy     = "busy"     // all builder slots in use
	BuilderDisabled = "disabled" // builder.enabled is false
	BuilderNone     = "none"     // the project has no builder and none was started
)

// BuilderServerName is the TLS name in every builder certificate; job VMs
// reach builders through the bridge address, so the name is not a host name.
const BuilderServerName = "builder"

// builderMaxAge replaces a builder even when busy, so it picks up new images.
const builderMaxAge = 7 * 24 * time.Hour

// builderRetryAfter keeps a project from booting its builder again right after
// a failed boot; jobs poll while they wait, and each poll would start a boot.
const builderRetryAfter = 2 * time.Minute

// BuilderInfo is what a job needs to use its project's builder.
type BuilderInfo struct {
	State string `json:"state"`
	Port  int    `json:"port,omitempty"`
	CA    string `json:"ca,omitempty"`
	Cert  string `json:"cert,omitempty"`
	Key   string `json:"key,omitempty"`
}

// builder is one project's BuildKit microVM. Each builder has its own CA, so a
// job's client certificate is only accepted by its own project's builder.
type builder struct {
	Project  string      `json:"project"`
	Instance vm.Instance `json:"instance"`
	Port     int         `json:"port"`
	CA       string      `json:"ca"`
	Cert     string      `json:"cert"`
	Key      string      `json:"key"`
	SpecID   string      `json:"spec_id"`
	BornAt   time.Time   `json:"born_at"`
	LastUsed time.Time   `json:"last_used"`
	ready    bool
	// creds are created with the entry, so jobs can be configured for the
	// builder while it is still starting (only a starting builder needs them).
	creds *builderCreds
	// strikes counts consecutive reconciles whose buildkitd probe failed.
	strikes int
}

// builderStrikes consecutive failed probes drop a builder. One missed 3 s dial
// (a builder VM busy with a heavy build) must not throw its cache away.
const builderStrikes = 2

var projectID = regexp.MustCompile(`^[0-9]{1,20}$`)

// builderSpec changes when a setting that changes what a builder runs changes.
// Size (vcpu, memory) is left out on purpose: a new size applies to new
// builders, and resizing must not throw away every project's warm cache.
func builderSpec(c config.Config) string {
	return fmt.Sprintf("v2|%s|%s|%s|%s|%d", c.Builder.Image,
		c.VM.KernelImage, c.VM.RootFSImage, c.VM.RegistryMirror, c.Builder.CacheMB)
}

// legacyBuilderSpec is builderSpec before v2 (it included the size), so
// builders recorded by an older daemon are not replaced after an upgrade.
func legacyBuilderSpec(c config.Config) string {
	return fmt.Sprintf("%d/%d/%s/%s/%s/%s/%d", c.Builder.VCPU, c.Builder.MemoryMB, c.Builder.Image,
		c.VM.KernelImage, c.VM.RootFSImage, c.VM.RegistryMirror, c.Builder.CacheMB)
}

// builderConfig is the VM config a builder boots with.
func builderConfig(c config.Config) config.Config {
	c.VM.VCPU, c.VM.MemoryMB = c.Builder.VCPU, c.Builder.MemoryMB
	return c
}

// Builder returns the project's builder. With start it boots one when there
// is none; without, it only reports an existing one, so projects that never
// build do not occupy a builder. It never blocks on a boot.
func (d *Daemon) Builder(project string, start bool) BuilderInfo {
	if !projectID.MatchString(project) {
		return BuilderInfo{State: BuilderDisabled}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.cfg
	if !cfg.Builder.Enabled || cfg.Builder.Max == 0 {
		return BuilderInfo{State: BuilderDisabled}
	}
	if _, ok := d.builders[project]; !ok && !start {
		return BuilderInfo{State: BuilderNone}
	}
	if at, ok := d.builderFailed[project]; ok && time.Since(at) < builderRetryAfter {
		d.metrics.builderRequests.WithLabelValues(BuilderBusy).Inc()
		return BuilderInfo{State: BuilderBusy}
	}
	if b, ok := d.builders[project]; ok {
		// LastUsed only matters at idle_ttl scale: persist it at most once a
		// minute instead of writing builders.json for every job.
		persist := time.Since(b.LastUsed) > time.Minute
		b.LastUsed = time.Now()
		if !b.ready {
			d.metrics.builderRequests.WithLabelValues(BuilderBooting).Inc()
			return BuilderInfo{State: BuilderBooting, Port: b.Port, CA: b.CA, Cert: b.Cert, Key: b.Key}
		}
		if persist {
			d.saveBuildersLocked()
		}
		d.metrics.builderRequests.WithLabelValues(BuilderReady).Inc()
		return BuilderInfo{State: BuilderReady, Port: b.Port, CA: b.CA, Cert: b.Cert, Key: b.Key}
	}
	if len(d.builders) >= cfg.Builder.Max && !d.evictLRULocked() {
		d.metrics.builderRequests.WithLabelValues(BuilderBusy).Inc()
		return BuilderInfo{State: BuilderBusy}
	}
	port := d.freePortLocked(cfg)
	if port == 0 {
		d.metrics.builderRequests.WithLabelValues(BuilderBusy).Inc()
		return BuilderInfo{State: BuilderBusy}
	}
	creds, err := newBuilderCreds()
	if err != nil {
		d.log.Error("builder credentials", "err", err)
		return BuilderInfo{State: BuilderBusy}
	}
	b := &builder{Project: project, Port: port, LastUsed: time.Now(),
		CA: creds.caPEM, Cert: creds.clientCert, Key: creds.clientKey, creds: creds}
	d.builders[project] = b
	d.metrics.builderRequests.WithLabelValues(BuilderBooting).Inc()
	ctx, boot := d.runCtx, builderBoot
	d.spawn(func() { boot(d, ctx, cfg, project) })
	// The job gets the port and credentials now; its `docker build` waits for
	// the builder, so jobs that do not build never wait.
	return BuilderInfo{State: BuilderBooting, Port: b.Port, CA: b.CA, Cert: b.Cert, Key: b.Key}
}

// builderBoot is a variable so tests can skip real VM boots.
var builderBoot = (*Daemon).bootBuilder

// evictLRULocked deletes the least recently used ready builder that has been
// idle for 10 minutes and that no running job uses. It reports whether a slot was freed.
func (d *Daemon) evictLRULocked() bool {
	// A job that started since expireBuilders last looked has just bumped its
	// builder's LastUsed, so it is not idle enough to be evicted either way.
	busy := d.busy
	if busy == nil {
		busy = busyBuilders() // before the first expireBuilders
	}
	var lru *builder
	for _, b := range d.builders {
		if b.ready && !busy[b.Project] && time.Since(b.LastUsed) > 10*time.Minute && (lru == nil || b.LastUsed.Before(lru.LastUsed)) {
			lru = b
		}
	}
	if lru == nil {
		return false
	}
	d.removeBuilderLocked(lru, "least recently used")
	return true
}

func (d *Daemon) freePortLocked(cfg config.Config) int {
	used := map[int]bool{}
	for _, b := range d.builders {
		used[b.Port] = true
	}
	for p := cfg.Builder.PortBase + 1; p <= cfg.Builder.PortBase+cfg.Builder.Max; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// removeBuilderLocked forgets a builder and deletes its VM in the background.
// The port mapping and the VM go away in the background, so the caller's lock
// (and with it POST /claim) never waits for nft or flintlock. For reasons that
// keep the cache, its state is copied to the host before the VM is deleted; the
// project's next builder waits for that copy (see bootBuilder).
func (d *Daemon) removeBuilderLocked(b *builder, reason string) {
	delete(d.builders, b.Project)
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	project, port, inst, nft := b.Project, b.Port, b.Instance, nftRun
	cfg, ctx := d.cfg, d.runCtx
	var saved *cacheSave
	if keepsCache(reason) && cfg.Builder.SavedCacheGB > 0 && inst.UID != "" && d.saving[project] == nil {
		saved = &cacheSave{done: make(chan struct{}), uid: inst.UID, start: time.Now()}
		d.saving[project] = saved
	}
	d.spawn(func() {
		_ = nft("delete", "element", "inet", "firerunner", "builders", fmt.Sprintf("{ %d }", port))
		if saved != nil {
			if ctx.Err() == nil {
				d.saveBuilderCache(ctx, cfg, project, &inst, saved.start)
			}
			defer func() {
				d.mu.Lock()
				delete(d.saving, project)
				delete(d.cacheDropped, project)
				d.mu.Unlock()
				close(saved.done)
			}()
		}
		if inst.UID != "" {
			d.delete(context.Background(), &inst, "builder: "+reason)
		}
	})
}

// Removal is the result of an operator's builder removal, by project id.
type Removal struct {
	Removed []string `json:"removed"`
	Skipped []string `json:"skipped"` // a running job builds on it; force removes it
}

// RemoveBuilders deletes the ready builder of project ("all": every ready
// builder) with its layer cache, and the saved cache of a deleted builder.
// Builders a running job builds on are kept unless force is set: deleting one
// fails that job's docker build.
func (d *Daemon) RemoveBuilders(project string, force bool) Removal {
	out := Removal{Removed: []string{}, Skipped: []string{}}
	if project != "all" && !projectID.MatchString(project) {
		return out
	}
	busy := busyBuilders() // file reads: outside the lock
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.builders {
		if !b.ready || (project != "all" && b.Project != project) {
			continue
		}
		if busy[b.Project] && !force {
			out.Skipped = append(out.Skipped, b.Project)
			continue
		}
		d.removeBuilderLocked(b, "removed by operator")
		out.Removed = append(out.Removed, b.Project)
	}
	keep := map[string]bool{}
	for _, p := range out.Skipped {
		keep[p] = true
	}
	// A save still running would bring a dropped cache back: mark it.
	for p := range d.saving {
		if !keep[p] && (project == "all" || p == project) {
			d.cacheDropped[p] = time.Now()
		}
	}
	dropSavedCaches(project, keep)
	sort.Strings(out.Removed)
	sort.Strings(out.Skipped)
	return out
}

func (d *Daemon) bootBuilder(ctx context.Context, cfg config.Config, project string) {
	start := time.Now()
	bcfg := builderConfig(cfg)
	fail := func(inst *vm.Instance, err error) {
		d.metrics.bootFailures.WithLabelValues("builder").Inc()
		d.log.Error("builder boot failed", "project", project, "err", err)
		d.mu.Lock()
		if b, ok := d.builders[project]; ok && !b.ready {
			delete(d.builders, project)
		}
		d.builderFailed[project] = time.Now()
		d.mu.Unlock()
		if inst != nil {
			d.delete(context.Background(), inst, "builder setup failed")
		}
	}
	// The project's previous builder may still be copying its cache out.
	d.mu.Lock()
	saving := d.saving[project]
	d.mu.Unlock()
	if saving != nil {
		select {
		case <-saving.done:
		case <-ctx.Done():
		}
	}
	if err := d.waitBuilderFits(ctx, bcfg); err != nil {
		fail(nil, err)
		return
	}
	inst, err := builderVMBoot(ctx, bcfg, d.fl, "bld-"+project, map[string]string{LabelRole: "builder"})
	if err != nil {
		fail(nil, err)
		return
	}
	d.mu.Lock()
	var creds *builderCreds
	if b, ok := d.builders[project]; ok {
		creds = b.creds
		// Known to reconcile from now on: loading a large cache takes longer
		// than reconcile waits before it deletes a VM nobody owns.
		b.Instance = *inst
	}
	d.mu.Unlock()
	if creds == nil {
		d.spawn(func() { d.delete(context.Background(), inst, "builder no longer needed") })
		return
	}
	restored, err := d.restoreBuilderCache(ctx, bcfg, project, inst)
	if err != nil {
		fail(inst, err) // the VM may hold part of a cache: start over in a new one
		return
	}
	if err := builderSetup(ctx, bcfg, inst, creds); err != nil {
		if restored && errors.Is(err, errBuildkitDown) {
			// buildkitd ran but did not come up on the restored state: do not
			// start every later builder of the project from it.
			_ = os.Remove(builderCacheFile(project))
			d.log.Warn("saved builder cache dropped: buildkitd did not start with it", "project", project)
		}
		fail(inst, err)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.builders[project]
	if !ok || ctx.Err() != nil {
		// Removed while booting, or shutting down.
		d.spawn(func() { d.delete(context.Background(), inst, "builder no longer needed") })
		return
	}
	b.Instance, b.creds = *inst, nil
	b.SpecID, b.BornAt, b.ready = builderSpec(cfg), time.Now(), true
	if err := mapBuilderPort(b); err != nil {
		d.log.Error("builder port mapping failed", "project", project, "err", err)
	}
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	d.metrics.bootSeconds.WithLabelValues("builder").Observe(time.Since(start).Seconds())
	d.log.Info("builder ready", "project", project, "id", inst.ID, "ip", inst.IP, "port", b.Port,
		"took", time.Since(start).Round(100*time.Millisecond).String())
}

// VM boot, admission and buildkitd setup are variables so tests can run
// bootBuilder without VMs.
var (
	builderVMBoot = vm.Boot
	builderFits   = vm.Fits
	builderSetup  = setupBuildkit
)

// errBuildkitDown: buildkitd was started but never listened.
var errBuildkitDown = errors.New("buildkitd did not start listening")

// builderFitWait is how long a builder boot waits for host memory: a builder
// deleted to free its slot holds its memory until its cache is copied out.
var builderFitWait = 2 * time.Minute

func (d *Daemon) waitBuilderFits(ctx context.Context, bcfg config.Config) error {
	deadline := time.Now().Add(builderFitWait)
	for {
		ok, why, err := builderFits(ctx, bcfg, d.fl, 0)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("no host memory for a builder (%s, %v)", why, err)
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
}

// setupBuildkit starts buildkitd with mutual TLS in the builder VM. Docker Hub
// pulls go through the host's mirror like everywhere else.
func setupBuildkit(ctx context.Context, cfg config.Config, inst *vm.Instance, c *builderCreds) error {
	toml := ""
	if mirror := strings.TrimPrefix(strings.TrimPrefix(cfg.VM.RegistryMirror, "http://"), "https://"); mirror != "" {
		toml = fmt.Sprintf("[registry.\"docker.io\"]\n  mirrors = [%q]\n[registry.%q]\n  http = true\n", mirror, mirror)
	}
	files := map[string]string{"ca.pem": c.caPEM, "server.pem": c.serverCert, "server.key": c.serverKey, "buildkitd.toml": toml}
	var script strings.Builder
	script.WriteString("set -e\numask 077\nmkdir -p /etc/buildkit\n")
	for _, name := range sortedNames(files) {
		fmt.Fprintf(&script, "cat > /etc/buildkit/%s <<'FIRERUNNER_EOF'\n%sFIRERUNNER_EOF\n", name, ensureNL(files[name]))
	}
	fmt.Fprintf(&script, "docker run -d --name buildkitd --privileged --restart always -p 1234:1234 "+
		"-v /etc/buildkit:/etc/buildkit:ro -v buildkit:/var/lib/buildkit %s "+
		"--addr tcp://0.0.0.0:1234 --config /etc/buildkit/buildkitd.toml "+
		"--tlscacert /etc/buildkit/ca.pem --tlscert /etc/buildkit/server.pem --tlskey /etc/buildkit/server.key "+
		"--oci-worker-gc --oci-worker-gc-keepstorage %d >/dev/null\n", shellQuote(cfg.Builder.Image), cfg.Builder.CacheMB)
	script.WriteString("for i in $(seq 1 60); do (</dev/tcp/127.0.0.1/1234) 2>/dev/null && exit 0; sleep 1; done\n" +
		"docker logs --tail 20 buildkitd >&2; exit 3\n")

	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := vm.SSH(cfg, inst, "bash")
	cmd.Stdin = strings.NewReader(script.String())
	out, err := runWithContext(sctx, cmd)
	if err != nil {
		if exitCode(err) == 3 {
			err = errBuildkitDown
		}
		return fmt.Errorf("starting buildkitd: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// exitCode is the exit status of a finished command, or -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func runWithContext(ctx context.Context, cmd *exec.Cmd) (string, error) {
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return out.String(), ctx.Err()
	}
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func ensureNL(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// nftRun is a variable so tests can record nft calls.
var nftRun = func(args ...string) error {
	out, err := exec.Command("nft", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mapBuilderPort points <bridge address>:port at the builder (install.sh
// creates the map and the DNAT rule). Re-adding an existing mapping is a no-op.
func mapBuilderPort(b *builder) error {
	if net.ParseIP(b.Instance.IP).To4() == nil {
		return fmt.Errorf("builder %s has no IPv4 address", b.Instance.ID)
	}
	_ = nftRun("delete", "element", "inet", "firerunner", "builders", fmt.Sprintf("{ %d }", b.Port))
	return nftRun("add", "element", "inet", "firerunner", "builders",
		fmt.Sprintf("{ %d : %s . 1234 }", b.Port, b.Instance.IP))
}

// expireBuilders deletes builders idle longer than builder.idle_ttl, older
// than builderMaxAge, or booted with settings that changed and idle for 10
// minutes, and deletes all of them when builders are disabled.
func (d *Daemon) expireBuilders() {
	busy := busyBuilders()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy = busy
	cfg := d.cfg
	spec := builderSpec(cfg)
	for _, b := range d.builders {
		if !b.ready {
			continue
		}
		if busy[b.Project] {
			// A running job builds here (maybe for longer than the idle limits).
			b.LastUsed = time.Now()
			continue
		}
		idle := time.Since(b.LastUsed)
		switch {
		case !cfg.Builder.Enabled:
			d.removeBuilderLocked(b, "builders disabled")
		case idle > cfg.Builder.IdleTTL:
			d.removeBuilderLocked(b, "idle")
		case time.Since(b.BornAt) > builderMaxAge && idle > 10*time.Minute:
			d.removeBuilderLocked(b, "max age")
		case b.SpecID != spec && idle > 10*time.Minute:
			d.removeBuilderLocked(b, "config changed")
		}
	}
}

// checkBuilders re-adds port mappings (restarting firerunner-net clears them)
// and drops builders whose VM is gone, or whose buildkitd missed builderStrikes
// probes in a row while no job uses it. present is the flintlock listing taken
// at listedAt; a builder that became ready after that is not in it yet.
func (d *Daemon) checkBuilders(present map[string]bool, listedAt time.Time) {
	busy := busyBuilders()
	d.mu.Lock()
	var check []builder
	for _, b := range d.builders {
		if b.ready {
			check = append(check, *b)
		}
	}
	d.mu.Unlock()
	// Probe without the lock (a hung builder must not stall job claims) and
	// all at once (64 silent builders must not take 64 x 3 s).
	gone, answers := map[string]bool{}, map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range check {
		if !present[b.Instance.UID] {
			if b.BornAt.Before(listedAt) {
				gone[b.Project] = true
			}
			continue
		}
		wg.Add(1)
		go func(project, ip string) {
			defer wg.Done()
			ok := tcpOpen(ip, 1234)
			mu.Lock()
			answers[project] = ok
			mu.Unlock()
		}(b.Project, b.Instance.IP)
	}
	wg.Wait()

	d.mu.Lock()
	var remap []builder
	for _, b := range d.builders {
		if !b.ready {
			continue
		}
		answered, probed := answers[b.Project]
		switch {
		case gone[b.Project]:
			d.removeBuilderLocked(b, "VM gone")
			continue
		case !probed:
			continue // became ready after the listing: check it next time
		case answered:
			b.strikes = 0
		default:
			b.strikes++
			if b.strikes >= builderStrikes && !busy[b.Project] {
				d.removeBuilderLocked(b, "not answering")
				continue
			}
			d.log.Warn("builder not answering", "project", b.Project, "id", b.Instance.ID,
				"strikes", b.strikes, "in_use", busy[b.Project])
		}
		remap = append(remap, *b)
	}
	d.mu.Unlock()
	// Two nft execs per builder: outside the lock, so job claims never wait.
	for i := range remap {
		if err := mapBuilderPort(&remap[i]); err != nil {
			d.log.Error("builder port mapping failed", "project", remap[i].Project, "err", err)
		}
	}
}

// answersWithin probes a builder's buildkitd up to tries times.
func answersWithin(ip string, tries int) bool {
	for i := 0; i < tries; i++ {
		if tcpOpen(ip, 1234) {
			return true
		}
	}
	return false
}

// tcpOpen is a variable so tests do not need a listener.
var tcpOpen = func(ip string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (d *Daemon) buildersFile() string {
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "builders.json")
}

// saveBuildersLocked records ready builders (with their client keys, 0600) so a
// restarted daemon keeps them and their caches.
func (d *Daemon) saveBuildersLocked() {
	var list []*builder
	for _, b := range d.builders {
		if b.ready {
			list = append(list, b)
		}
	}
	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	file := d.buildersFile()
	tmp := file + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, file)
	}
}

// adoptBuilders takes over builders recorded by a previous daemon run.
func (d *Daemon) adoptBuilders(live map[string]bool) {
	data, err := os.ReadFile(d.buildersFile())
	if err != nil {
		return
	}
	var list []*builder
	if json.Unmarshal(data, &list) != nil {
		return
	}
	var keep []*builder
	for _, b := range list {
		if !projectID.MatchString(b.Project) || !live[b.Instance.UID] {
			continue
		}
		// Two tries: one missed probe must not cost a project its warm cache.
		if !answersWithin(b.Instance.IP, 2) {
			d.log.Warn("builder from previous run not answering, left for reconcile", "project", b.Project, "id", b.Instance.ID)
			continue
		}
		keep = append(keep, b)
	}
	d.mu.Lock()
	for _, b := range keep {
		b.ready = true
		if b.SpecID == legacyBuilderSpec(d.cfg) {
			b.SpecID = builderSpec(d.cfg)
		}
		d.builders[b.Project] = b
		d.log.Info("adopted builder from previous run", "project", b.Project, "id", b.Instance.ID)
	}
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	d.mu.Unlock()
	for _, b := range keep {
		if err := mapBuilderPort(b); err != nil {
			d.log.Error("builder port mapping failed", "project", b.Project, "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// certificates

type builderCreds struct {
	caPEM, serverCert, serverKey, clientCert, clientKey string
}

// newBuilderCreds creates a fresh CA with one server and one client
// certificate (ECDSA P-256, valid 30 days, longer than builderMaxAge).
func newBuilderCreds() (*builderCreds, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "firerunner builder CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(cn string, usage x509.ExtKeyUsage, dns []string) (string, string, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(30 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     dns,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return "", "", err
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return "", "", err
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), nil
	}
	c := &builderCreds{caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))}
	if c.serverCert, c.serverKey, err = leaf(BuilderServerName, x509.ExtKeyUsageServerAuth, []string{BuilderServerName}); err != nil {
		return nil, err
	}
	if c.clientCert, c.clientKey, err = leaf("firerunner job", x509.ExtKeyUsageClientAuth, nil); err != nil {
		return nil, err
	}
	return c, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
