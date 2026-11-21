package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The systemd units run `registry-front --listen ADDR --upstream URL`.
func TestParseRegistryFront(t *testing.T) {
	for _, c := range []struct {
		name             string
		args             []string
		listen, upstream string
		wantErr          string // "" for success
	}{
		{"both flags", []string{"--listen", "10.200.0.1:5000", "--upstream", "http://127.0.0.1:5000"},
			"10.200.0.1:5000", "http://127.0.0.1:5000", ""},
		{"equals form", []string{"--listen=10.200.0.1:5001", "--upstream=http://127.0.0.1:5101"},
			"10.200.0.1:5001", "http://127.0.0.1:5101", ""},
		{"no listen", []string{"--upstream", "http://127.0.0.1:5000"}, "", "", "usage:"},
		{"no upstream", []string{"--listen", "10.200.0.1:5000"}, "", "", "usage:"},
		{"no flags", nil, "", "", "usage:"},
		{"extra argument", []string{"--listen", "10.200.0.1:5000", "--upstream", "http://127.0.0.1:5000", "x"}, "", "", "usage:"},
		{"unknown flag", []string{"--listen", "10.200.0.1:5000", "--upstream", "http://127.0.0.1:5000", "--tls"}, "", "", "not defined"},
		{"listen without port", []string{"--listen", "10.200.0.1", "--upstream", "http://127.0.0.1:5000"}, "", "", "--listen"},
	} {
		t.Run(c.name, func(t *testing.T) {
			listen, upstream, err := parseRegistryFront(c.args, io.Discard)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error %v, want one with %q", err, c.wantErr)
				}
				return
			}
			if err != nil || listen != c.listen || upstream != c.upstream {
				t.Fatalf("got %q, %q, %v; want %q, %q", listen, upstream, err, c.listen, c.upstream)
			}
		})
	}
}

// -h prints the flags and exits 0 (main treats flag.ErrHelp as success).
func TestParseRegistryFrontHelp(t *testing.T) {
	if _, _, err := parseRegistryFront([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error %v, want flag.ErrHelp", err)
	}
}

// The front reads no config file: its unit must start whatever state the
// config is in.
func TestRegistryFrontNeedsNoConfig(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(broken, []byte("vm: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRERUNNER_CONFIG", broken)
	err := dispatch([]string{"registry-front"})
	if err == nil || !strings.Contains(err.Error(), "usage: firerunner registry-front") {
		t.Fatalf("dispatch error %v, want the registry-front usage", err)
	}
}

// The front must not be its own registry, however the two addresses are
// written: names are resolved, and an unspecified listen address is every
// address of the host.
func TestFrontLoops(t *testing.T) {
	names := map[string][]net.IP{
		"localhost":      {net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		"mirror.example": {net.ParseIP("10.200.0.1")},
		"elsewhere":      {net.ParseIP("192.0.2.7")},
	}
	lookup := func(_ context.Context, _, host string) ([]net.IP, error) {
		if ips, ok := names[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	hostIPs := func() ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.200.0.1"), net.ParseIP("192.168.1.5")}, nil
	}
	noIPs := func() ([]net.IP, error) { return nil, errors.New("address family not supported") }
	for _, c := range []struct {
		listen, upstream string
		local            func() ([]net.IP, error)
		loops            bool
		wantErr          bool
	}{
		// The installer's layout: bridge address in front, registry on loopback.
		{"10.200.0.1:5000", "http://127.0.0.1:5100", hostIPs, false, false},
		{"10.200.0.1:5000", "http://127.0.0.1:5000", hostIPs, false, false},
		{"10.200.0.1:5000", "http://localhost:5000", hostIPs, false, false},
		{"127.0.0.1:5000", "http://127.0.0.2:5000", hostIPs, false, false},
		{"0.0.0.0:5000", "http://elsewhere:5000", hostIPs, false, false},
		{"0.0.0.0:5000", "http://127.0.0.1:5001", hostIPs, false, false},

		{"127.0.0.1:5000", "http://127.0.0.1:5000", hostIPs, true, false},
		{"127.0.0.1:5000", "http://localhost:5000", hostIPs, true, false},
		{"localhost:5000", "http://127.0.0.1:5000", hostIPs, true, false},
		{"127.0.0.1:5000", "http://[::ffff:127.0.0.1]:5000", hostIPs, true, false},
		{"127.0.0.1:5000", "http://0.0.0.0:5000", hostIPs, true, false},
		{"[::1]:5000", "http://[::]:5000", hostIPs, true, false},
		{"10.200.0.1:5000", "http://10.200.0.1:5000", hostIPs, true, false},
		{"10.200.0.1:5000", "http://mirror.example:5000", hostIPs, true, false},
		{"0.0.0.0:5000", "http://127.0.0.1:5000", hostIPs, true, false},
		{"[::]:5000", "http://10.200.0.1:5000", hostIPs, true, false},
		{":5000", "http://localhost:5000", hostIPs, true, false},
		{"0.0.0.0:5000", "http://192.168.1.5:5000", hostIPs, true, false},
		// Without the host's addresses, loopback is still known to be local.
		{"0.0.0.0:5000", "http://127.0.0.1:5000", noIPs, true, false},
		{"0.0.0.0:5000", "http://192.168.1.5:5000", noIPs, false, false},

		{"127.0.0.1:5000", "http://unknown.example:5000", hostIPs, false, true},
		{"unknown.example:5000", "http://127.0.0.1:5000", hostIPs, false, true},
		// Different ports never loop: no name is resolved.
		{"127.0.0.1:5000", "http://unknown.example:5001", hostIPs, false, false},
	} {
		t.Run(c.listen+" "+c.upstream, func(t *testing.T) {
			loops, err := frontLoops(t.Context(), c.listen, c.upstream, lookup, c.local)
			if (err != nil) != c.wantErr || loops != c.loops {
				t.Errorf("frontLoops = %v, %v; want %v, error %v", loops, err, c.loops, c.wantErr)
			}
		})
	}
}
