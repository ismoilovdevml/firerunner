package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

func TestMACIsStableAndLocal(t *testing.T) {
	a, b := MAC("job-1"), MAC("job-1")
	if a != b || MAC("job-2") == a {
		t.Fatalf("MAC not stable/unique: %s %s", a, MAC("job-2"))
	}
	if !strings.HasPrefix(a, "aa:fc:") || len(a) != 17 {
		t.Fatalf("bad MAC %s", a)
	}
}

func TestLeaseIP(t *testing.T) {
	dir := t.TempDir()
	if ip, err := LeaseIP(filepath.Join(dir, "missing"), "aa:fc:00:00:00:01"); ip != "" || err != nil {
		t.Fatalf("missing file: %q %v", ip, err)
	}
	p := filepath.Join(dir, "leases")
	body := "1790224115 aa:fc:00:00:00:01 10.200.0.10 * 01:aa\n" +
		"1790224116 AA:FC:00:00:00:02 10.200.0.11 * 01:bb\n" +
		"1790224117 aa:fc:00:00:00:01 10.200.0.12 * 01:aa\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:01"); ip != "10.200.0.12" {
		t.Fatalf("want latest lease, got %q", ip)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:02"); ip != "10.200.0.11" {
		t.Fatalf("case-insensitive match failed: %q", ip)
	}
	if ip, _ := LeaseIP(p, "aa:fc:00:00:00:03"); ip != "" {
		t.Fatalf("unexpected lease %q", ip)
	}
}

func TestSpec(t *testing.T) {
	cfg := config.Default()
	cfg.VM.KernelCmdline["quiet"] = "1"
	s := Spec(cfg, "job-7", MAC("job-7"), "ssh-ed25519 AAAA test", HostKey{Private: "PRIV", Public: "ssh-ed25519 HOST"}, map[string]string{"a": "b"}, "")
	if s.GetVcpu() != 2 || s.GetMemoryInMb() != 2048 {
		t.Fatalf("size: %d %d", s.GetVcpu(), s.GetMemoryInMb())
	}
	iface := s.GetInterfaces()[0]
	if iface.GetType() != types.NetworkInterface_TAP || iface.GetGuestMac() != MAC("job-7") {
		t.Fatalf("interface: %v", iface)
	}
	if s.GetKernel().GetCmdline()["quiet"] != "1" || s.GetKernel().GetImage() != config.DefaultKernelImage {
		t.Fatalf("kernel: %v", s.GetKernel())
	}
	ud, err := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	if err != nil || !strings.Contains(string(ud), "ssh-ed25519 AAAA test") || !strings.HasPrefix(string(ud), "#cloud-config") {
		t.Fatalf("user-data: %v %s", err, ud)
	}
	// the spec must not share the config's cmdline map
	s.GetKernel().Cmdline["x"] = "y"
	if _, ok := cfg.VM.KernelCmdline["x"]; ok {
		t.Fatal("spec aliases config map")
	}
}

func TestDockerDaemonConfigAvoidsDefaultBridge(t *testing.T) {
	cfg := config.Default()
	cfg.VM.RegistryMirror = "http://10.200.0.1:5000"
	s := Spec(cfg, "job-9", MAC("job-9"), "ssh-ed25519 AAAA k", HostKey{}, nil, "")
	ud, _ := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	var doc struct {
		WriteFiles []struct{ Path, Content string } `yaml:"write_files"`
	}
	if err := yaml.Unmarshal(ud, &doc); err != nil || len(doc.WriteFiles) != 1 || doc.WriteFiles[0].Path != "/etc/docker/daemon.json" {
		t.Fatalf("write_files: %v %+v", err, doc)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(doc.WriteFiles[0].Content), &d); err != nil {
		t.Fatal(err)
	}
	if d["bip"] != "10.201.0.1/24" || !strings.Contains(fmt.Sprint(d["default-address-pools"]), "10.202.0.0/16") ||
		fmt.Sprint(d["registry-mirrors"]) != "[http://10.200.0.1:5000]" {
		t.Fatalf("daemon.json: %v", d)
	}
}

func TestCloudInitIsMarshalledNotFormatted(t *testing.T) {
	cfg := config.Default()
	evil := "job-1\nruncmd:\n  - touch /pwned"
	s := Spec(cfg, evil, MAC(evil), "ssh-ed25519 AAAA k", HostKey{Private: "P", Public: "ssh-ed25519 H"}, nil, "")
	ud, _ := base64.StdEncoding.DecodeString(s.GetMetadata()["user-data"])
	var doc map[string]any
	if err := yaml.Unmarshal(ud, &doc); err != nil {
		t.Fatal(err)
	}
	if _, injected := doc["runcmd"]; injected || doc["hostname"] != evil {
		t.Fatalf("id escaped its YAML scalar: %v", doc)
	}
	keys, _ := doc["ssh_keys"].(map[string]any)
	if keys["ed25519_public"] != "ssh-ed25519 H" || keys["ed25519_private"] != "P" {
		t.Fatalf("host key not injected: %v", doc["ssh_keys"])
	}
}

func TestNewHostKey(t *testing.T) {
	k, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Public, "ssh-ed25519 ") || !strings.Contains(k.Private, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("unexpected key format: %q", k.Public)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Public)); err != nil {
		t.Fatal(err)
	}
}

func TestSSHPinsHostKey(t *testing.T) {
	KnownHostsDir = t.TempDir()
	inst := &Instance{ID: "job-5", IP: "10.200.0.9", HostKey: "ssh-ed25519 AAAAHOST"}
	args := strings.Join(SSH(config.Default(), inst, "true").Args, " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "HostKeyAlias=job-5", "root@10.200.0.9"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %s in %s", want, args)
		}
	}
	if strings.Contains(args, "StrictHostKeyChecking=no") {
		t.Fatal("host key checking disabled")
	}
	b, err := os.ReadFile(filepath.Join(KnownHostsDir, "job-5"))
	if err != nil || string(b) != "job-5 ssh-ed25519 AAAAHOST\n" {
		t.Fatalf("known_hosts: %q %v", b, err)
	}
	RemoveKnownHosts("job-5")
	if _, err := os.Stat(filepath.Join(KnownHostsDir, "job-5")); !os.IsNotExist(err) {
		t.Fatal("known_hosts not removed")
	}
}

// A VM with a recorded host key is never reached unverified: when its key
// cannot be pinned, the command fails instead of connecting to whoever answers
// on the IP (the session may carry the job's secrets). Only a VM without a
// recorded key, from before host keys were pinned, connects unverified.
func TestSSHFailsClosedWhenTheKeyCannotBePinned(t *testing.T) {
	old := KnownHostsDir
	t.Cleanup(func() { KnownHostsDir = old })
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	KnownHostsDir = filepath.Join(file, "known_hosts") // cannot be created
	cfg := config.Default()

	pinned := &Instance{ID: "job-5", IP: "10.200.0.9", HostKey: "ssh-ed25519 AAAAHOST"}
	cmd := SSH(cfg, pinned, "true")
	if args := strings.Join(cmd.Args, " "); strings.Contains(args, "StrictHostKeyChecking=no") ||
		!strings.Contains(args, "StrictHostKeyChecking=yes") {
		t.Fatalf("known key not pinned, connecting unverified: %s", args)
	}
	err := cmd.Run()
	if err == nil || !strings.Contains(err.Error(), "host key of job-5") {
		t.Fatalf("Run = %v; want it to fail on the host key before connecting", err)
	}
	if cmd.Process != nil {
		t.Fatal("ssh was started")
	}
	if code, err := RunScript(cfg, pinned, strings.NewReader("echo secret"), nil, nil); code != -1 || err == nil {
		t.Fatalf("RunScript = %d, %v; want -1 with the error", code, err)
	}

	legacy := &Instance{ID: "job-6", IP: "10.200.0.10"}
	if args := strings.Join(SSH(cfg, legacy, "true").Args, " "); !strings.Contains(args, "StrictHostKeyChecking=no") {
		t.Fatalf("a VM without a recorded key must still be reachable: %s", args)
	}
}

func TestForgetReleasesLease(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Network.LeasesFile = filepath.Join(dir, "leases")
	mac := MAC("job-42")
	leases := "1790286904 aa:fc:00:00:00:01 10.200.0.9 * *\n" +
		"1790286905 " + mac + " 10.200.0.77 * 01:" + mac + "\n"
	if err := os.WriteFile(cfg.Network.LeasesFile, []byte(leases), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	oldRelease, oldDev := releaseLease, routeDev
	t.Cleanup(func() { releaseLease, routeDev = oldRelease, oldDev })
	releaseLease = func(args ...string) error { got = args; return nil }
	routeDev = func(ip string) string { return "br-fc" }

	Forget(cfg, "job-42")
	want := []string{"br-fc", "10.200.0.77", mac, "01:" + mac}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("dhcp_release %v, want %v", got, want)
	}

	got = nil
	Forget(cfg, "job-43") // no lease: nothing to release
	if got != nil {
		t.Fatalf("released %v for a VM without a lease", got)
	}
}

func TestWaitTCP(t *testing.T) {
	old := bootPoll
	bootPoll = 20 * time.Millisecond
	t.Cleanup(func() { bootPoll = old })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	t.Run("closed port hits the deadline", func(t *testing.T) {
		if err := waitTCP(context.Background(), addr, time.Now().Add(100*time.Millisecond)); err == nil {
			t.Fatal("waitTCP = nil for a closed port")
		}
	})
	t.Run("port that opens later is seen quickly", func(t *testing.T) {
		ready := make(chan net.Listener, 1)
		go func() {
			time.Sleep(150 * time.Millisecond)
			l, err := net.Listen("tcp", addr)
			if err != nil {
				ready <- nil
				return
			}
			ready <- l
		}()
		start := time.Now()
		err := waitTCP(context.Background(), addr, time.Now().Add(5*time.Second))
		if l := <-ready; l != nil {
			defer l.Close()
		} else {
			t.Skip("could not re-bind the port")
		}
		if err != nil {
			t.Fatalf("waitTCP = %v", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("noticed the open port after %s", took)
		}
	})
	t.Run("cancel wins over the deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitTCP(ctx, addr, time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
			t.Fatalf("waitTCP = %v, want context.Canceled", err)
		}
	})
}
