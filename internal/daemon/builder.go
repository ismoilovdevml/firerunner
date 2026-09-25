package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
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

// builderMaxAge replaces a builder so it picks up new images: once it is older,
// expireBuilders removes it when no running job uses it and it has been idle
// for 10 minutes (a busy builder is never replaced).
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
	// Unsaved marks a record of a removed builder whose cache copy the
	// daemon's stop cut off: its VM was kept, and the next run copies the
	// cache and deletes it. It is when the builder was removed.
	Unsaved time.Time `json:"unsaved,omitzero"`
	ready   bool
	// creds are created with the entry, so jobs can be configured for the
	// builder while it is still starting (only a starting builder needs them).
	creds *builderCreds
	// strikes counts consecutive reconciles whose buildkitd probe failed.
	strikes int
	// dropped: an operator's `builder rm` came while the builder booted. The
	// saved cache it may be loading (from a file that is open, so unlinking it
	// does not stop the load) must not stay in it; see bootBuilder.
	dropped bool
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
		c.VM.KernelImage, c.VM.RootFSImage, c.VM.RegistryMirror, c.Builder.CacheMB) + proxySpec(c)
}

// specImage is the builder image in a builderSpec fingerprint, "" for a
// fingerprint of another form (legacyBuilderSpec).
func specImage(spec string) string {
	rest, ok := strings.CutPrefix(spec, "v2|")
	if !ok {
		return ""
	}
	image, _, _ := strings.Cut(rest, "|")
	return image
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

func (d *Daemon) portUsedLocked(port int) bool {
	for _, b := range d.builders {
		if b.Port == port {
			return true
		}
	}
	return false
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
	d.retireLocked(b.Project, b.Port, b.Instance, b.SpecID, "builder: "+reason, keepsCache(reason), time.Now())
}

// retireLocked unmaps a removed builder's port (0: none), copies its cache
// to the host if save is set and the cache is worth it, and deletes its VM,
// all in the background. removed is when the builder was removed.
func (d *Daemon) retireLocked(project string, port int, inst vm.Instance, specID, reason string, save bool, removed time.Time) {
	nft, cfg, ctx := nftRun, d.cfg, d.runCtx
	// Only a builder.image builder's cache is worth copying: a new image's
	// builder would not take it (see restoreBuilderCache).
	image := specImage(specID)
	var saved *cacheSave
	if save && cfg.Builder.SavedCacheGB > 0 && inst.UID != "" && d.saving[project] == nil && image == cfg.Builder.Image {
		saved = &cacheSave{done: make(chan struct{}), uid: inst.UID, start: removed, inst: inst, specID: specID}
		d.saving[project] = saved
	}
	d.spawn(func() {
		if port != 0 {
			_, _ = nft("", "delete", "element", "inet", "firerunner", "builders", fmt.Sprintf("{ %d }", port))
		}
		if saved != nil {
			// A copy the daemon's stop cuts off (or that could not start
			// before it) keeps the VM for the next run.
			if (ctx.Err() != nil || d.saveBuilderCache(ctx, cfg, project, &inst, image, saved.start)) && d.keepCutSave(project, saved) {
				return
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
			d.delete(context.Background(), &inst, reason)
		}
	})
}

// keepCutSave keeps the VM of a cache copy the daemon's stop cut off, unless
// an operator's `builder rm` dropped the project's cache meanwhile: the save
// stays in d.saving, marked cut, and builders.json records it, so the next
// run copies the cache and deletes the VM (see adoptBuilders). It reports
// whether it kept the VM.
func (d *Daemon) keepCutSave(project string, s *cacheSave) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cacheDropped[project].After(s.start) {
		return false
	}
	s.cut = true
	d.saveBuildersLocked()
	close(s.done)
	d.log.Info("builder cache copy stopped by shutdown; the VM is kept and the next start copies it",
		"project", project, "vm", s.inst.ID)
	return true
}

// Removal is the result of an operator's builder removal, by project id.
type Removal struct {
	Removed []string `json:"removed"`
	Skipped []string `json:"skipped"` // a running job builds on it; force removes it
}

// RemoveBuilders deletes the ready builder of project ("all": every ready
// builder) with its layer cache, and the saved cache of a deleted builder.
// Builders a running job builds on are kept unless force is set: deleting one
// fails that job's docker build. A builder still booting keeps booting, but
// without the saved cache it may be loading (see builder.dropped).
func (d *Daemon) RemoveBuilders(project string, force bool) Removal {
	out := Removal{Removed: []string{}, Skipped: []string{}}
	if project != "all" && !projectID.MatchString(project) {
		return out
	}
	busy := busyBuilders() // file reads: outside the lock
	d.mu.Lock()
	for _, b := range d.builders {
		if project != "all" && b.Project != project {
			continue
		}
		if !b.ready {
			b.dropped = true // its saved cache is dropped below
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
	aside := dropSavedCachesLocked(project, keep)
	d.mu.Unlock()
	for _, f := range aside {
		_ = unlinkCache(f)
	}
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
		// A slow save must not keep the project without a builder: after
		// builderSaveWait the new builder starts from the older copy, if any.
		select {
		case <-saving.done:
		case <-time.After(builderSaveWait):
			d.log.Warn("builder boots before its previous cache copy finished", "project", project)
		case <-ctx.Done():
		}
	}
	// Host memory is reserved under the VM's own id until flintlock lists it.
	id := builderVMID(project)
	release, err := d.admitBuilder(ctx, bcfg, id)
	if err != nil {
		fail(nil, err)
		return
	}
	defer release()
	// A daemon that just started reconciles builder VMs it did not adopt as
	// left over: this one is ours, though its uid is not known until Boot returns.
	d.mu.Lock()
	d.bootingIDs[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.bootingIDs, id)
		d.mu.Unlock()
	}()
	inst, err := builderVMBoot(ctx, bcfg, d.fl, id, map[string]string{LabelRole: "builder"})
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
	d.mu.Lock()
	dropped := d.builders[project] != nil && d.builders[project].dropped
	d.mu.Unlock()
	if restored && dropped {
		// `builder rm` threw the saved cache away while it loaded.
		if err := wipeBuilderCache(ctx, bcfg, inst); err != nil {
			fail(inst, fmt.Errorf("saved builder cache removed by the operator could not be wiped: %w", err))
			return
		}
		restored = false
		d.log.Info("builder cache removed by the operator while it loaded: wiped, starting empty", "project", project)
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

	// The port is mapped before jobs see the builder ready, and outside the
	// lock: nft must not hold up POST /claim. A booting builder keeps its port.
	d.mu.Lock()
	port := 0
	if b, ok := d.builders[project]; ok {
		port = b.Port
	}
	d.mu.Unlock()
	if port != 0 {
		if err := mapBuilderPorts(builder{Port: port, Instance: *inst}); err != nil {
			d.log.Error("builder port mapping failed", "project", project, "err", err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.builders[project]
	if ok && restored && b.dropped {
		// `builder rm` came after the wipe check, while buildkitd started on
		// the removed cache: do not use this builder; the next job starts one.
		delete(d.builders, project)
		ok = false
		d.log.Info("builder dropped: its cache was removed by the operator while it started", "project", project)
	}
	if !ok || ctx.Err() != nil {
		// Removed while booting, or shutting down. The port leads to this VM
		// unless another builder took it meanwhile.
		unmap := port != 0 && (ok || !d.portUsedLocked(port))
		d.spawn(func() {
			if unmap {
				_, _ = nftRun("", "delete", "element", "inet", "firerunner", "builders", fmt.Sprintf("{ %d }", port))
			}
			d.delete(context.Background(), inst, "builder no longer needed")
		})
		return
	}
	b.Instance, b.creds = *inst, nil
	b.SpecID, b.BornAt, b.ready = builderSpec(cfg), time.Now(), true
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	d.metrics.bootSeconds.WithLabelValues("builder").Observe(time.Since(start).Seconds())
	d.log.Info("builder ready", "project", project, "vm", inst.ID, "ip", inst.IP, "port", b.Port,
		"took", time.Since(start).Round(100*time.Millisecond).String())
}

// builderVMID is a new VM id for one boot of project's builder. The id alone
// sets the VM's MAC, DHCP lease and pinned key file, so every boot gets its
// own: the project's previous builder may still exist (copying its cache out,
// a failed delete, not adopted after a restart), and a MAC derived from the
// public project id alone could be taken by any guest.
func builderVMID(project string) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return "bld-" + project + "-" + hex.EncodeToString(suffix)
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

// builderSaveWait bounds how long a builder boot waits for the project's
// previous builder to finish copying its cache out.
var builderSaveWait = 5 * time.Minute

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
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := vm.SSH(cfg, inst, "bash")
	cmd.Stdin = strings.NewReader(buildkitScript(cfg, c))
	out, err := runWithContext(sctx, cmd)
	if err != nil {
		if exitCode(err) == 3 {
			err = errBuildkitDown
		}
		return fmt.Errorf("starting buildkitd: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// buildkitScript writes the builder's certificates and config and starts
// buildkitd; it exits 3 when buildkitd never listens (errBuildkitDown).
func buildkitScript(cfg config.Config, c *builderCreds) string {
	toml := buildkitdTOML(cfg)
	files := map[string]string{"ca.pem": c.caPEM, "server.pem": c.serverCert, "server.key": c.serverKey, "buildkitd.toml": toml}
	var script strings.Builder
	script.WriteString("set -e\numask 077\nmkdir -p /etc/buildkit\n")
	for _, name := range sortedNames(files) {
		fmt.Fprintf(&script, "cat > /etc/buildkit/%s <<'FIRERUNNER_EOF'\n%sFIRERUNNER_EOF\n", name, ensureNL(files[name]))
	}
	// With the proxy, buildkitd pulls base images and fetches ADD URLs through
	// it and trusts the proxy's CA (the VM's bundle holds it); RUN steps get
	// the proxy as build arguments from the job's Docker CLI.
	var proxyArgs []string
	if cfg.Proxy.Enabled {
		for _, e := range vm.ProxyEnv(cfg, vm.NoProxy(cfg)) {
			proxyArgs = append(proxyArgs, "-e", shellQuote(e))
		}
	}
	if cfg.VM.CAFile != "" {
		// The builder VM's bundle holds vm.ca_file (cloud-init).
		proxyArgs = append(proxyArgs, "-v", "/etc/ssl/certs:/etc/ssl/certs:ro")
	}
	fmt.Fprintf(&script, "docker run -d --name buildkitd --privileged --restart always -p 1234:1234 %s"+
		"-v /etc/buildkit:/etc/buildkit:ro -v buildkit:/var/lib/buildkit %s "+
		"--addr tcp://0.0.0.0:1234 --config /etc/buildkit/buildkitd.toml "+
		"--tlscacert /etc/buildkit/ca.pem --tlscert /etc/buildkit/server.pem --tlskey /etc/buildkit/server.key "+
		"--oci-worker-gc --oci-worker-gc-keepstorage %d >/dev/null\n", joinArgs(proxyArgs), shellQuote(cfg.Builder.Image), cfg.Builder.CacheMB)
	script.WriteString("for i in $(seq 1 180); do (</dev/tcp/127.0.0.1/1234) 2>/dev/null && exit 0; sleep 1; done\n" +
		"docker logs --tail 20 buildkitd >&2; exit 3\n")
	return script.String()
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

// nftRun runs nft with args and script on its standard input ("" for none)
// and returns what it printed; a variable so tests can emulate nft.
var nftRun = func(script string, args ...string) (string, error) {
	cmd := exec.Command("nft", args...)
	if script != "" {
		cmd.Stdin = strings.NewReader(script)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// mapBuilderPorts points <bridge address>:port at each builder's buildkitd
// (install.sh creates the map and the DNAT rule; restarting firerunner-net
// empties it). It lists the map once and leaves mappings in place alone; the
// missing and wrong ones change in one nft transaction, so a wrong mapping is
// replaced without a moment in which the port maps to nothing.
func mapBuilderPorts(bs ...builder) error {
	if len(bs) == 0 {
		return nil
	}
	out, err := nftRun("", "list", "map", "inet", "firerunner", "builders")
	if err != nil {
		return err
	}
	have := parseBuilderMap(out)
	var errs []error
	var script strings.Builder
	for _, b := range bs {
		ip := net.ParseIP(b.Instance.IP).To4()
		if ip == nil {
			errs = append(errs, fmt.Errorf("builder %s has no IPv4 address", b.Instance.ID))
			continue
		}
		to, mapped := have[b.Port]
		if to == ip.String()+" . 1234" {
			continue
		}
		if mapped {
			// nft refuses to add a key that maps elsewhere.
			fmt.Fprintf(&script, "delete element inet firerunner builders { %d }\n", b.Port)
		}
		fmt.Fprintf(&script, "add element inet firerunner builders { %d : %s . 1234 }\n", b.Port, ip)
	}
	if script.Len() > 0 {
		if _, err := nftRun(script.String(), "-f", "-"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// parseBuilderMap reads `nft list map inet firerunner builders`: port ->
// target ("10.200.0.13 . 1234"). Elements it cannot read are left out, so
// they count as missing.
func parseBuilderMap(out string) map[int]string {
	m := map[int]string{}
	_, rest, ok := strings.Cut(strings.Join(strings.Fields(out), " "), "elements = {")
	if !ok {
		return m
	}
	body, _, _ := strings.Cut(rest, "}")
	for _, e := range strings.Split(body, ",") {
		key, to, ok := strings.Cut(e, ":")
		port, err := strconv.Atoi(strings.TrimSpace(key))
		if !ok || err != nil {
			continue
		}
		m[port] = strings.Join(strings.Fields(to), " ")
	}
	return m
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

// checkBuilders adopts the previous run's builders if that has not happened
// yet, restores port mappings (restarting firerunner-net clears them)
// and drops builders whose VM is gone, or whose buildkitd missed builderStrikes
// probes in a row while no job uses it. present is the flintlock listing taken
// at listedAt; a builder that became ready after that is not in it yet.
func (d *Daemon) checkBuilders(present map[string]bool, listedAt time.Time) {
	d.mu.Lock()
	adopted := d.buildersAdopted
	d.mu.Unlock()
	if !adopted {
		// The startup listing failed, so builders.json was not read then. This
		// listing worked, and reconcile has not deleted any of those builders
		// yet: they are first seen now, far younger than the orphan age.
		d.adoptBuilders(present)
	}
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
			d.log.Warn("builder not answering", "project", b.Project, "vm", b.Instance.ID,
				"strikes", b.strikes, "in_use", busy[b.Project])
		}
		remap = append(remap, *b)
	}
	d.mu.Unlock()
	// nft runs outside the lock, so job claims never wait for it.
	if err := mapBuilderPorts(remap...); err != nil {
		d.log.Error("builder port mapping failed", "err", err)
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
// restarted daemon keeps them and their caches. Before adoptBuilders has read
// the file it holds the previous run's builders, and is left alone.
func (d *Daemon) saveBuildersLocked() {
	if !d.buildersAdopted {
		return
	}
	var list []*builder
	for _, b := range d.builders {
		if b.ready {
			list = append(list, b)
		}
	}
	for project, s := range d.saving {
		if s.cut {
			list = append(list, &builder{Project: project, Instance: s.inst, SpecID: s.specID, Unsaved: s.start})
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

// adoptBuilders takes over builders recorded by a previous daemon run; live
// is the set of microVM uids flintlock lists. Run calls it after the startup
// listing, or checkBuilders at the first listing that works.
func (d *Daemon) adoptBuilders(live map[string]bool) {
	var list []*builder
	data, err := os.ReadFile(d.buildersFile())
	if err == nil {
		err = json.Unmarshal(data, &list)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.Warn("builders of the previous run not adopted", "err", err)
		list = nil
	}
	var keep, cut []*builder
	for _, b := range list {
		if !projectID.MatchString(b.Project) || !live[b.Instance.UID] {
			continue
		}
		if !b.Unsaved.IsZero() {
			cut = append(cut, b) // its buildkitd is stopped: not probed
			continue
		}
		// Two tries: one missed probe must not cost a project its warm cache.
		if !answersWithin(b.Instance.IP, 2) {
			d.log.Warn("builder from previous run not answering, left for reconcile", "project", b.Project, "vm", b.Instance.ID)
			continue
		}
		keep = append(keep, b)
	}
	// Jobs may have asked for builders since the daemon started: a project
	// with a new builder keeps it (reconcile removes the old VM), and an old
	// builder whose port a new one took gets another port.
	d.mu.Lock()
	var adopted []builder
	for _, b := range keep {
		if _, taken := d.builders[b.Project]; taken {
			d.log.Warn("builder from previous run not adopted: the project has a new one", "project", b.Project, "vm", b.Instance.ID)
			continue
		}
		if d.portUsedLocked(b.Port) {
			old := b.Port
			if b.Port = d.freePortLocked(d.cfg); b.Port == 0 {
				d.log.Warn("builder from previous run not adopted: no free port", "project", b.Project, "vm", b.Instance.ID)
				continue
			}
			d.log.Info("builder from previous run moved to a free port", "project", b.Project, "from", old, "to", b.Port)
		}
		b.ready = true
		if b.SpecID == legacyBuilderSpec(d.cfg) {
			b.SpecID = builderSpec(d.cfg)
		}
		d.builders[b.Project] = b
		adopted = append(adopted, *b)
		d.log.Info("adopted builder from previous run", "project", b.Project, "vm", b.Instance.ID)
	}
	for _, b := range cut {
		// The previous run stopped while it copied this removed builder's
		// cache: copy it now, then delete the VM.
		d.log.Info("resuming a builder cache copy cut off by the previous run's stop", "project", b.Project, "vm", b.Instance.ID)
		d.retireLocked(b.Project, 0, b.Instance, b.SpecID, "builder: cache copied after a restart", true, b.Unsaved)
	}
	d.buildersAdopted = true
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	d.mu.Unlock()
	if err := mapBuilderPorts(adopted...); err != nil {
		d.log.Error("builder port mapping failed", "err", err)
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

// buildkitdTOML points Docker Hub at the host's mirror (plain HTTP) and sets
// vm.insecure_registries: http://host:port as plain HTTP, host:port as TLS
// without a certificate check. BuildKit, unlike Docker, does not fall back
// from one to the other. One table per registry: TOML refuses duplicates.
func buildkitdTOML(cfg config.Config) string {
	var b strings.Builder
	seen := map[string]bool{}
	table := func(host string, plainHTTP bool) {
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		if plainHTTP {
			fmt.Fprintf(&b, "[registry.%q]\n  http = true\n", host)
		} else {
			fmt.Fprintf(&b, "[registry.%q]\n  insecure = true\n", host)
		}
	}
	mirror := strings.TrimPrefix(strings.TrimPrefix(cfg.VM.RegistryMirror, "http://"), "https://")
	if mirror != "" {
		fmt.Fprintf(&b, "[registry.\"docker.io\"]\n  mirrors = [%q]\n", mirror)
		// Only a plain-HTTP mirror needs a table; an https:// one keeps its
		// certificate checked unless it is listed in vm.insecure_registries.
		if strings.HasPrefix(cfg.VM.RegistryMirror, "http://") {
			table(mirror, true)
		}
	}
	for _, r := range cfg.VM.InsecureRegistries {
		table(config.RegistryHost(r), strings.HasPrefix(r, "http://"))
	}
	return b.String()
}

// joinArgs joins already-quoted arguments with a trailing space ("" for none).
func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.Join(args, " ") + " "
}
