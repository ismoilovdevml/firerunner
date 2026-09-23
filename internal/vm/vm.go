// Package vm boots a single-use microVM through flintlock and talks to it over SSH.
package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
)

// Instance is a booted microVM.
type Instance struct {
	ID  string `json:"id"`
	UID string `json:"uid"`
	IP  string `json:"ip"`
}

// Boot creates the microVM and waits until it answers on SSH.
// On failure the microVM is deleted before returning.
func Boot(ctx context.Context, cfg config.Config, fl *flintlock.Client, id string, labels map[string]string) (*Instance, error) {
	pub, err := os.ReadFile(cfg.Network.SSHKey + ".pub")
	if err != nil {
		return nil, fmt.Errorf("reading SSH public key: %w", err)
	}
	mac := MAC(id)
	uid, err := fl.Create(ctx, Spec(cfg, id, mac, strings.TrimSpace(string(pub)), labels))
	if err != nil {
		return nil, fmt.Errorf("creating microVM %s: %w", id, err)
	}
	inst := &Instance{ID: id, UID: uid}

	fail := func(err error) (*Instance, error) {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = fl.Delete(dctx, uid)
		return nil, err
	}

	deadline := time.Now().Add(cfg.VM.BootTimeout)
	for inst.IP == "" {
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("microVM %s got no DHCP lease within %s (see: firerunner vm logs %s)", id, cfg.VM.BootTimeout, id))
		}
		if inst.IP, err = LeaseIP(cfg.Network.LeasesFile, mac); err != nil {
			return fail(err)
		}
		if inst.IP == "" {
			if err := sleep(ctx, time.Second); err != nil {
				return fail(err)
			}
		}
	}
	for SSH(cfg, inst.IP, "true").Run() != nil {
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("microVM %s at %s did not answer SSH within %s", id, inst.IP, cfg.VM.BootTimeout))
		}
		if err := sleep(ctx, time.Second); err != nil {
			return fail(err)
		}
	}
	return inst, nil
}

// Spec builds the flintlock request for a job microVM.
func Spec(cfg config.Config, id, mac, sshPubKey string, labels map[string]string) *types.MicroVMSpec {
	userData := fmt.Sprintf(`#cloud-config
hostname: %s
disable_root: false
users:
  - name: root
    ssh_authorized_keys:
      - %s
bootcmd:
  - ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
`, id, sshPubKey)
	if cfg.VM.RegistryMirror != "" {
		userData += fmt.Sprintf(`write_files:
  - path: /etc/docker/daemon.json
    content: '{"registry-mirrors": ["%s"]}'
`, cfg.VM.RegistryMirror)
	}
	metaData := fmt.Sprintf("instance_id: %s/%s\nlocal_hostname: %s\nplatform: liquid_metal\n", cfg.Flintlock.Namespace, id, id)

	kernelFile := "boot/vmlinux"
	rootfs := cfg.VM.RootFSImage
	cmdline := map[string]string{}
	for k, v := range cfg.VM.KernelCmdline {
		cmdline[k] = v
	}
	return &types.MicroVMSpec{
		Id:         id,
		Labels:     labels,
		Vcpu:       int32(cfg.VM.VCPU),
		MemoryInMb: int32(cfg.VM.MemoryMB),
		Kernel: &types.Kernel{
			Image:            cfg.VM.KernelImage,
			Filename:         &kernelFile,
			Cmdline:          cmdline,
			AddNetworkConfig: true,
		},
		RootVolume: &types.Volume{
			Id:     "root",
			Source: &types.VolumeSource{ContainerSource: &rootfs},
		},
		Interfaces: []*types.NetworkInterface{{
			DeviceId: "eth1",
			Type:     types.NetworkInterface_TAP,
			GuestMac: &mac,
		}},
		Metadata: map[string]string{
			"meta-data": base64.StdEncoding.EncodeToString([]byte(metaData)),
			"user-data": base64.StdEncoding.EncodeToString([]byte(userData)),
			// Without vendor-data cloud-init retries the metadata service ~10s per boot.
			"vendor-data": base64.StdEncoding.EncodeToString([]byte("#cloud-config\n{}\n")),
		},
	}
}

// MAC derives a stable, locally administered MAC from the microVM id,
// so its DHCP lease can be found without asking the guest.
func MAC(id string) string {
	h := sha256.Sum256([]byte(id))
	return fmt.Sprintf("aa:fc:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3])
}

// LeaseIP returns the address dnsmasq leased to mac, or "" if there is none yet.
func LeaseIP(leasesFile, mac string) (string, error) {
	f, err := os.Open(leasesFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	ip := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// <expiry> <mac> <ip> <hostname> <client-id>
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && strings.EqualFold(fields[1], mac) {
			ip = fields[2]
		}
	}
	return ip, sc.Err()
}

// SSH returns a command that runs args in the microVM as root.
func SSH(cfg config.Config, ip string, args ...string) *exec.Cmd {
	base := []string{
		"-q", "-i", cfg.Network.SSHKey,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15",
		"root@" + ip,
	}
	return exec.Command("ssh", append(base, args...)...)
}

// RunScript streams a shell script into the microVM and returns its exit code.
func RunScript(cfg config.Config, ip string, script io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := SSH(cfg, ip, "/bin/bash")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = script, stdout, stderr
	return ExitCode(cmd.Run())
}

// ExitCode turns the error of exec.Cmd.Run into an exit code; err is only
// returned when the command could not run at all.
func ExitCode(err error) (int, error) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
