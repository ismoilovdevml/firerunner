package main

import (
	"errors"
	"flag"
	"io"
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
		{"upstream is listen", []string{"--listen", "10.200.0.1:5000", "--upstream", "http://10.200.0.1:5000"}, "", "", "--listen itself"},
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
