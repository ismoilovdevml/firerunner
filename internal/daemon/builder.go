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
}

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

// Builder returns the project's builder, starting one when there is none.
// It never blocks on a boot: a job whose builder is not ready builds locally.
func (d *Daemon) Builder(project string) BuilderInfo {
	if !projectID.MatchString(project) {
		return BuilderInfo{State: BuilderDisabled}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.cfg
	if !cfg.Builder.Enabled || cfg.Builder.Max == 0 {
		return BuilderInfo{State: BuilderDisabled}
	}
	if at, ok := d.builderFailed[project]; ok && time.Since(at) < builderRetryAfter {
		d.metrics.builderRequests.WithLabelValues(BuilderBusy).Inc()
		return BuilderInfo{State: BuilderBusy}
	}
	if b, ok := d.builders[project]; ok {
		b.LastUsed = time.Now()
		if !b.ready {
			return BuilderInfo{State: BuilderBooting}
		}
		d.saveBuildersLocked()
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
	d.builders[project] = &builder{Project: project, Port: port, LastUsed: time.Now()}
	d.metrics.builderRequests.WithLabelValues(BuilderBooting).Inc()
	go builderBoot(d, d.runCtx, cfg, project)
	return BuilderInfo{State: BuilderBooting}
}

// builderBoot is a variable so tests can skip real VM boots.
var builderBoot = (*Daemon).bootBuilder

// evictLRULocked deletes the least recently used ready builder that has been
// idle for 10 minutes and that no running job uses. It reports whether a slot was freed.
func (d *Daemon) evictLRULocked() bool {
	busy := busyBuilders()
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
func (d *Daemon) removeBuilderLocked(b *builder, reason string) {
	delete(d.builders, b.Project)
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
	_ = nftRun("delete", "element", "inet", "firerunner", "builders", fmt.Sprintf("{ %d }", b.Port))
	if b.Instance.UID != "" {
		inst := b.Instance
		go d.delete(context.Background(), &inst, "builder: "+reason)
	}
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
	if ok, why, err := vm.Fits(ctx, bcfg, d.fl, 0); err != nil || !ok {
		fail(nil, fmt.Errorf("no host memory for a builder (%s, %v)", why, err))
		return
	}
	inst, err := vm.Boot(ctx, bcfg, d.fl, "bld-"+project, map[string]string{LabelRole: "builder"})
	if err != nil {
		fail(nil, err)
		return
	}
	creds, err := newBuilderCreds()
	if err != nil {
		fail(inst, err)
		return
	}
	if err := setupBuildkit(ctx, bcfg, inst, creds); err != nil {
		fail(inst, err)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.builders[project]
	if !ok || ctx.Err() != nil {
		// Removed while booting, or shutting down.
		go d.delete(context.Background(), inst, "builder no longer needed")
		return
	}
	b.Instance, b.CA, b.Cert, b.Key = *inst, creds.caPEM, creds.clientCert, creds.clientKey
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
		"docker logs --tail 20 buildkitd >&2; exit 1\n")

	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := vm.SSH(cfg, inst, "bash")
	cmd.Stdin = strings.NewReader(script.String())
	out, err := runWithContext(sctx, cmd)
	if err != nil {
		return fmt.Errorf("starting buildkitd: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
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
// and drops builders whose buildkitd stopped answering.
func (d *Daemon) checkBuilders(present map[string]bool) {
	d.mu.Lock()
	var check []builder
	for _, b := range d.builders {
		if b.ready {
			check = append(check, *b)
		}
	}
	d.mu.Unlock()
	// Probe without the lock: a hung builder must not stall job claims.
	dead := map[string]bool{}
	for _, b := range check {
		if !present[b.Instance.UID] || !tcpOpen(b.Instance.IP, 1234) {
			dead[b.Project] = true
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.builders {
		if !b.ready {
			continue
		}
		if dead[b.Project] {
			d.removeBuilderLocked(b, "not answering")
			continue
		}
		if err := mapBuilderPort(b); err != nil {
			d.log.Error("builder port mapping failed", "project", b.Project, "err", err)
		}
	}
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
		if projectID.MatchString(b.Project) && live[b.Instance.UID] && tcpOpen(b.Instance.IP, 1234) {
			keep = append(keep, b)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range keep {
		b.ready = true
		if b.SpecID == legacyBuilderSpec(d.cfg) {
			b.SpecID = builderSpec(d.cfg)
		}
		d.builders[b.Project] = b
		if err := mapBuilderPort(b); err != nil {
			d.log.Error("builder port mapping failed", "project", b.Project, "err", err)
		}
		d.log.Info("adopted builder from previous run", "project", b.Project, "id", b.Instance.ID)
	}
	d.saveBuildersLocked()
	d.metrics.builders.Set(float64(len(d.builders)))
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
