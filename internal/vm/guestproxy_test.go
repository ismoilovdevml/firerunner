package vm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

func proxyCfg() config.Config {
	cfg := config.Default()
	cfg.VM.RegistryMirror = "http://10.200.0.1:5000"
	cfg.Proxy.Enabled = true
	cfg.Proxy.NoProxy = "gitlab.corp.example, .corp.example,10.0.0.0/8"
	return cfg
}

type cloudConfig struct {
	WriteFiles []struct {
		Path, Content, Permissions string
		Append                     bool
	} `yaml:"write_files"`
	CACerts struct {
		Trusted []string `yaml:"trusted"`
	} `yaml:"ca_certs"`
}

func userData(t *testing.T, cfg config.Config, ca string) cloudConfig {
	t.Helper()
	s := Spec(cfg, "job-1", MAC("job-1"), "ssh-ed25519 AAAA k", HostKey{}, nil, ca)
	ud, err := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	if err != nil {
		t.Fatal(err)
	}
	var doc cloudConfig
	if err := yaml.Unmarshal(ud, &doc); err != nil {
		t.Fatalf("user-data is not YAML: %v\n%s", err, ud)
	}
	return doc
}

func file(t *testing.T, doc cloudConfig, path string) (content, perm string, appendTo bool) {
	t.Helper()
	for _, f := range doc.WriteFiles {
		if f.Path == path {
			return f.Content, f.Permissions, f.Append
		}
	}
	t.Fatalf("no %s in write_files", path)
	return "", "", false
}

// Local addresses always bypass the proxy: the registry mirror, cache: and
// builders on the bridge, the VM's Docker networks, then the operator's list.
func TestNoProxy(t *testing.T) {
	got := NoProxy(proxyCfg(), "postgres", "minio")
	want := "localhost,127.0.0.1,::1,10.200.0.1,10.200.0.0/24,10.201.0.0/24,10.202.0.0/16,postgres,minio," +
		"gitlab.corp.example,.corp.example,10.0.0.0/8"
	if got != want {
		t.Fatalf("NoProxy =\n %s\nwant\n %s", got, want)
	}
}

// With the proxy on, a VM gets it for SSH sessions (/etc/environment), for
// its Docker daemon (daemon.json) and for the Docker CLI (containers, build
// args); without it, none of that is written.
func TestSpecWithProxy(t *testing.T) {
	cfg := proxyCfg()
	doc := userData(t, cfg, "")
	env, _, appendTo := file(t, doc, "/etc/environment")
	if !appendTo || !strings.Contains(env, "https_proxy=http://10.200.0.1:3128\n") ||
		!strings.Contains(env, "NO_PROXY=localhost,127.0.0.1") || strings.Contains(env, "@") {
		t.Fatalf("/etc/environment (append=%v):\n%s", appendTo, env)
	}
	cli, perm, _ := file(t, doc, "/root/.docker/config.json")
	var c struct {
		Proxies map[string]map[string]string `json:"proxies"`
	}
	if err := json.Unmarshal([]byte(cli), &c); err != nil || c.Proxies["default"]["httpsProxy"] != "http://10.200.0.1:3128" || perm != "0600" {
		t.Fatalf("docker config %q perm %q: %v", cli, perm, err)
	}
	daemonJSON, _, _ := file(t, doc, "/etc/docker/daemon.json")
	var d struct {
		Proxies map[string]string `json:"proxies"`
	}
	if err := json.Unmarshal([]byte(daemonJSON), &d); err != nil || d.Proxies["https-proxy"] != "http://10.200.0.1:3128" ||
		!strings.Contains(d.Proxies["no-proxy"], "10.200.0.1") {
		t.Fatalf("daemon.json proxies: %v %v", d, err)
	}

	cfg.Proxy.Enabled = false
	doc = userData(t, cfg, "")
	if len(doc.WriteFiles) != 1 {
		t.Fatalf("proxy off still writes %d files", len(doc.WriteFiles))
	}
}

// A no_proxy value cannot break out of the YAML document: it is marshalled.
// (Values with newlines never get here: config validation refuses them.)
func TestSpecProxyValuesAreData(t *testing.T) {
	cfg := proxyCfg()
	cfg.Proxy.NoProxy = "a.corp\nruncmd: [reboot]"
	s := Spec(cfg, "job-1", MAC("job-1"), "k", HostKey{}, nil, "")
	ud, _ := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	var top map[string]any
	if err := yaml.Unmarshal(ud, &top); err != nil {
		t.Fatal(err)
	}
	if _, injected := top["runcmd"]; injected {
		t.Fatal("no_proxy injected a cloud-init key")
	}
}

func testCA(t *testing.T) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Corp Root"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// vm.ca_file is trusted by the VM (and so by dockerd) through cloud-init, is
// mounted into containers, and a file that is not a certificate fails the
// boot instead of every TLS connection.
func TestCA(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	if ca, err := ReadCA(cfg); err != nil || ca != "" {
		t.Fatalf("no ca_file: %q %v", ca, err)
	}
	cfg.VM.CAFile = filepath.Join(dir, "corp.pem")
	if _, err := ReadCA(cfg); err == nil {
		t.Fatal("missing ca_file accepted")
	}
	if err := os.WriteFile(cfg.VM.CAFile, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCA(cfg); err == nil {
		t.Fatal("ca_file without a certificate accepted")
	}
	pemCA := testCA(t)
	if err := os.WriteFile(cfg.VM.CAFile, []byte(pemCA), 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := ReadCA(cfg)
	if err != nil || ca != pemCA {
		t.Fatalf("ReadCA: %v", err)
	}
	doc := userData(t, cfg, ca)
	if len(doc.CACerts.Trusted) != 1 || doc.CACerts.Trusted[0] != pemCA {
		t.Fatalf("ca_certs: %+v", doc.CACerts)
	}
	args := strings.Join(ContainerArgs(cfg, []string{"postgres"}), " ")
	if !strings.Contains(args, "-v /etc/ssl/certs/ca-certificates.crt:"+ContainerCABundle+":ro") ||
		!strings.Contains(args, "NODE_EXTRA_CA_CERTS="+ContainerCABundle) || strings.Contains(args, "NO_PROXY") {
		t.Fatalf("container args without proxy: %s", args)
	}
}

func TestContainerArgs(t *testing.T) {
	if args := ContainerArgs(config.Default(), []string{"postgres"}); args != nil {
		t.Fatalf("nothing configured: %v", args)
	}
	cfg := proxyCfg()
	if args := ContainerArgs(cfg, nil); args != nil {
		t.Fatalf("proxy without services: %v (the Docker CLI config already covers it)", args)
	}
	args := strings.Join(ContainerArgs(cfg, []string{"postgres", "minio"}), " ")
	if !strings.Contains(args, "-e NO_PROXY=localhost") || !strings.Contains(args, ",postgres,minio,") ||
		!strings.Contains(args, "-e no_proxy=") {
		t.Fatalf("service aliases not kept out of the proxy: %s", args)
	}
}

// DOCKER_AUTH_CONFIG replaces ~/.docker/config.json: the proxies section must
// survive, a job's own proxies win, and anything that is not an object is
// written as the job gave it.
func TestMergeDockerConfig(t *testing.T) {
	cfg := proxyCfg()
	auth := `{"auths":{"harbor.corp":{"auth":"dTpw"}}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(MergeDockerConfig(cfg, auth)), &m); err != nil || m["auths"] == nil || m["proxies"] == nil {
		t.Fatalf("merged: %v %v", m, err)
	}
	own := `{"auths":{},"proxies":{"default":{"httpProxy":"http://mine:1"}}}`
	if got := MergeDockerConfig(cfg, own); !strings.Contains(got, "http://mine:1") || strings.Contains(got, "3128") {
		t.Fatalf("job's own proxies replaced: %s", got)
	}
	for _, raw := range []string{"not json", "[1,2]", "null"} {
		if got := MergeDockerConfig(cfg, raw); got != raw {
			t.Fatalf("%q changed to %q", raw, got)
		}
	}
	cfg.Proxy.Enabled = false
	if got := MergeDockerConfig(cfg, auth); got != auth {
		t.Fatalf("proxy off changed the auth config: %s", got)
	}
}

// Docker gets insecure registries without the http:// marker; Docker tries
// TLS without a check, then plain HTTP.
func TestInsecureRegistries(t *testing.T) {
	cfg := config.Default()
	cfg.VM.InsecureRegistries = []string{"harbor.corp:443", "http://10.0.0.5:5000"}
	d := DockerDaemonConfig(cfg)
	got, _ := json.Marshal(d["insecure-registries"])
	if string(got) != `["harbor.corp:443","10.0.0.5:5000"]` {
		t.Fatalf("insecure-registries = %s", got)
	}
}

// Only certificates from vm.ca_file reach microVMs: a key concatenated into
// the same file by mistake stays on the host.
func TestReadCAKeepsOnlyCertificates(t *testing.T) {
	cfg := config.Default()
	cfg.VM.CAFile = filepath.Join(t.TempDir(), "bundle.pem")
	pemCA := testCA(t)
	key := "-----BEGIN PRIVATE KEY-----\nc2VjcmV0\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(cfg.VM.CAFile, []byte("comment\n"+key+pemCA), 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := ReadCA(cfg)
	if err != nil || strings.Contains(ca, "PRIVATE KEY") || strings.Contains(ca, "comment") || !strings.Contains(ca, "BEGIN CERTIFICATE") {
		t.Fatalf("ReadCA = %q, %v", ca, err)
	}
}
