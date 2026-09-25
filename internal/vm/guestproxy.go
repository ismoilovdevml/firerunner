package vm

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

// With proxy.enabled every microVM (job, pool and builder) sends its internet
// traffic to the host's forwarder (config.Proxy): the shell and git through
// /etc/environment, the Docker daemon through daemon.json, and containers and
// docker build through the Docker CLI's proxies setting. The forwarder adds
// the corporate proxy's credentials; a microVM never sees them.

// ContainerCABundle is where job and service containers find the microVM's CA
// bundle (the public roots plus vm.ca_file) when vm.ca_file is set.
const ContainerCABundle = "/etc/firerunner/ca-bundle.crt"

// guestCABundle is the microVM's system bundle; cloud-init adds vm.ca_file.
const guestCABundle = "/etc/ssl/certs/ca-certificates.crt"

// ProxyURL is the forwarder as microVMs reach it.
func ProxyURL(cfg config.Config) string { return "http://" + cfg.Proxy.Listen }

// NoProxy is what microVMs reach without the proxy: loopback, the bridge (the
// registry mirror, the cache: store and builders are all on the bridge
// address), the Docker networks inside the VM, the job's service aliases,
// then proxy.no_proxy.
func NoProxy(cfg config.Config, extra ...string) string {
	var out []string
	seen := map[string]bool{}
	add := func(e string) {
		if e != "" && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, e := range []string{"localhost", "127.0.0.1", "::1"} {
		add(e)
	}
	if host, _, err := net.SplitHostPort(cfg.Proxy.Listen); err == nil {
		add(host)
		if ip := net.ParseIP(host).To4(); ip != nil {
			add((&net.IPNet{IP: ip.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String())
		}
	}
	if u, err := url.Parse(cfg.VM.RegistryMirror); err == nil {
		add(u.Hostname())
	}
	for _, c := range []string{cfg.VM.DockerBIP, cfg.VM.DockerAddressPool} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			add(n.String())
		}
	}
	for _, e := range extra {
		add(e)
	}
	for _, e := range config.SplitNoProxy(cfg.Proxy.NoProxy) {
		add(e)
	}
	return strings.Join(out, ",")
}

// ProxyEnv is the proxy environment in both spellings: tools read either.
func ProxyEnv(cfg config.Config, noProxy string) []string {
	p := ProxyURL(cfg)
	return []string{"http_proxy=" + p, "https_proxy=" + p, "no_proxy=" + noProxy,
		"HTTP_PROXY=" + p, "HTTPS_PROXY=" + p, "NO_PROXY=" + noProxy}
}

// CAEnv points common tools at ContainerCABundle: OpenSSL and Go, curl,
// Python requests, pip, git and Node.js.
func CAEnv() []string {
	var env []string
	for _, k := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "PIP_CERT", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS"} {
		env = append(env, k+"="+ContainerCABundle)
	}
	return env
}

// ContainerArgs are docker run flags for job and service containers. The
// Docker CLI already passes the proxy from its config; with the proxy this
// adds the job's service aliases to NO_PROXY (so a job reaches
// http://minio:9000 directly), and with vm.ca_file the CA bundle.
func ContainerArgs(cfg config.Config, aliases []string) []string {
	var args []string
	if cfg.Proxy.Enabled && len(aliases) > 0 {
		np := NoProxy(cfg, aliases...)
		args = append(args, "-e", "NO_PROXY="+np, "-e", "no_proxy="+np)
	}
	if cfg.VM.CAFile != "" {
		args = append(args, "-v", guestCABundle+":"+ContainerCABundle+":ro")
		for _, e := range CAEnv() {
			args = append(args, "-e", e)
		}
	}
	return args
}

// DockerCLIProxies is the microVM's ~/.docker/config.json proxies section:
// the Docker CLI sets it in every container it starts and passes it to docker
// build as build arguments, so RUN steps use the proxy too.
func DockerCLIProxies(cfg config.Config, noProxy string) map[string]any {
	p := ProxyURL(cfg)
	return map[string]any{"default": map[string]string{"httpProxy": p, "httpsProxy": p, "noProxy": noProxy}}
}

// MergeDockerConfig adds the proxies section to a job's DOCKER_AUTH_CONFIG,
// which replaces ~/.docker/config.json in the microVM. A config that is not a
// JSON object is returned unchanged; one with its own proxies keeps them.
func MergeDockerConfig(cfg config.Config, auth string) string {
	if !cfg.Proxy.Enabled {
		return auth
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(auth), &m); err != nil || m == nil {
		return auth
	}
	if _, ok := m["proxies"]; !ok {
		m["proxies"] = DockerCLIProxies(cfg, NoProxy(cfg))
	}
	out, err := json.Marshal(m)
	if err != nil {
		return auth
	}
	return string(out)
}

// ReadCA reads vm.ca_file ("" when not set) and checks it holds a
// certificate, so a wrong path fails the boot instead of every TLS connection.
func ReadCA(cfg config.Config) (string, error) {
	if cfg.VM.CAFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(cfg.VM.CAFile)
	if err != nil {
		return "", fmt.Errorf("vm.ca_file: %w", err)
	}
	for rest := data; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return "", fmt.Errorf("vm.ca_file %s holds no PEM certificate", cfg.VM.CAFile)
		}
		if b.Type == "CERTIFICATE" {
			return string(data), nil
		}
	}
}
