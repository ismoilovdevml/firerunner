package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChecksumFor(t *testing.T) {
	sums := "abc123  firerunner-linux-amd64\ndef456 *install.sh\n"
	if got, err := checksumFor(sums, "firerunner-linux-amd64"); err != nil || got != "abc123" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, _ := checksumFor(sums, "install.sh"); got != "def456" {
		t.Fatalf("binary-mode entry: %q", got)
	}
	if _, err := checksumFor(sums, "missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBaseURL(t *testing.T) {
	if BaseURL("latest") != "https://github.com/ismoilovdevml/firerunner/releases/latest/download" {
		t.Fatal(BaseURL("latest"))
	}
	if BaseURL("v1.2.0") != "https://github.com/ismoilovdevml/firerunner/releases/download/v1.2.0" {
		t.Fatal(BaseURL("v1.2.0"))
	}
}

// release serves checksums.txt and the binary like a GitHub release and
// counts binary downloads.
func release(t *testing.T, bin []byte, sum string) (*int, func()) {
	t.Helper()
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		case "/" + asset:
			downloads++
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	oldBase, oldExe := releaseBase, executable
	releaseBase = func(string) string { return srv.URL }
	return &downloads, func() { srv.Close(); releaseBase, executable = oldBase, oldExe }
}

// installed writes a fake current executable and points Run at it.
func installed(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "firerunner")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return path, nil }
	return path
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestCheckRunsNothingAndDownloadsNoBinary(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	downloads, done := release(t, newBin, sha(newBin))
	defer done()
	exe := installed(t, []byte("old binary"))

	res, err := Run(context.Background(), "edge", "v8", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || *downloads != 0 {
		t.Fatalf("check: changed=%v downloads=%d, want true and 0", res.Changed, *downloads)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatal("check replaced the executable")
	}
}

func TestIdenticalReleaseIsNotDownloaded(t *testing.T) {
	bin := []byte("#!/bin/sh\necho firerunner v8\n")
	downloads, done := release(t, bin, sha(bin))
	defer done()
	installed(t, bin)
	for _, check := range []bool{true, false} {
		res, err := Run(context.Background(), "edge", "v8", check)
		if err != nil || res.Changed || *downloads != 0 {
			t.Fatalf("check=%v: res=%+v err=%v downloads=%d", check, res, err, *downloads)
		}
	}
}

func TestUpgradeReplacesTheExecutable(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	_, done := release(t, newBin, sha(newBin))
	defer done()
	exe := installed(t, []byte("old binary"))
	res, err := Run(context.Background(), "edge", "v8", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.To != "v9" {
		t.Fatalf("res %+v, want changed to v9", res)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newBin) {
		t.Fatal("executable not replaced")
	}
}

func TestChecksumMismatchKeepsTheExecutable(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	_, done := release(t, newBin, sha([]byte("something else")))
	defer done()
	exe := installed(t, []byte("old binary"))
	if _, err := Run(context.Background(), "edge", "v8", false); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatal("executable replaced despite a checksum mismatch")
	}
}
