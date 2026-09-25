package upgrade

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
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

// sig says how the fake release answers checksums.txt.sig.
type sig int

const (
	signed   sig = iota // a signature of checksums.txt by the test key
	unsigned            // 404, like releases published before signing
	badSig              // a signature by another key
	sigError            // 500
)

// release serves checksums.txt, its signature and the binary like a GitHub
// release, signed with a key of the test's own, and counts binary downloads.
func release(t *testing.T, bin []byte, sum string, s sig) (*int, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	sums := fmt.Sprintf("%s  %s\n", sum, asset)
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprint(w, sums)
		case "/checksums.txt.sig":
			switch s {
			case signed:
				_, _ = w.Write(ed25519.Sign(priv, []byte(sums)))
			case badSig:
				_, _ = w.Write(ed25519.Sign(other, []byte(sums)))
			case sigError:
				http.Error(w, "boom", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		case "/" + asset:
			downloads++
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	oldBase, oldExe, oldKey := releaseBase, executable, releaseKey
	releaseBase = func(string) string { return srv.URL }
	releaseKey = func() (ed25519.PublicKey, error) { return pub, nil }
	return &downloads, func() { srv.Close(); releaseBase, executable, releaseKey = oldBase, oldExe, oldKey }
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
	downloads, done := release(t, newBin, sha(newBin), signed)
	defer done()
	exe := installed(t, []byte("old binary"))

	res, err := Run(context.Background(), "edge", "v8", Options{CheckOnly: true})
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
	downloads, done := release(t, bin, sha(bin), signed)
	defer done()
	installed(t, bin)
	for _, check := range []bool{true, false} {
		res, err := Run(context.Background(), "edge", "v8", Options{CheckOnly: check})
		if err != nil || res.Changed || *downloads != 0 {
			t.Fatalf("check=%v: res=%+v err=%v downloads=%d", check, res, err, *downloads)
		}
	}
}

func TestUpgradeReplacesTheExecutable(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner edge-abc1234\n")
	_, done := release(t, newBin, sha(newBin), signed)
	defer done()
	exe := installed(t, []byte("old binary"))
	res, err := Run(context.Background(), "edge", "v8", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.To != "edge-abc1234" || res.Unsigned {
		t.Fatalf("res %+v, want a signed change to edge-abc1234", res)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newBin) {
		t.Fatal("executable not replaced")
	}
}

func TestChecksumMismatchKeepsTheExecutable(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	_, done := release(t, newBin, sha([]byte("something else")), signed)
	defer done()
	exe := installed(t, []byte("old binary"))
	if _, err := Run(context.Background(), "edge", "v8", Options{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatal("executable replaced despite a checksum mismatch")
	}
}

func TestUnsignedReleaseIsRefused(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	downloads, done := release(t, newBin, sha(newBin), unsigned)
	defer done()
	exe := installed(t, []byte("old binary"))
	for _, check := range []bool{true, false} {
		_, err := Run(context.Background(), "v0.1.1", "v8", Options{CheckOnly: check})
		if err == nil || !strings.Contains(err.Error(), "before releases were signed") || !strings.Contains(err.Error(), "--allow-unsigned") {
			t.Fatalf("check=%v: err = %v, want refused as unsigned", check, err)
		}
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" || *downloads != 0 {
		t.Fatalf("unsigned release: executable replaced or binary downloaded (%d)", *downloads)
	}
}

func TestUnsignedReleaseWithAllowUnsigned(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v0.1.1\n")
	_, done := release(t, newBin, sha(newBin), unsigned)
	defer done()
	exe := installed(t, []byte("old binary"))
	res, err := Run(context.Background(), "v0.1.1", "v8", Options{AllowUnsigned: true})
	if err != nil || !res.Changed || !res.Unsigned {
		t.Fatalf("res %+v err %v, want an unsigned upgrade", res, err)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newBin) {
		t.Fatal("executable not replaced")
	}
}

func TestBadSignatureIsRefusedEvenWithAllowUnsigned(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	downloads, done := release(t, newBin, sha(newBin), badSig)
	defer done()
	exe := installed(t, []byte("old binary"))
	for _, opts := range []Options{{}, {AllowUnsigned: true}, {CheckOnly: true, AllowUnsigned: true}} {
		if _, err := Run(context.Background(), "edge", "v8", opts); err == nil || !strings.Contains(err.Error(), "does not match its signature") {
			t.Fatalf("%+v: err = %v, want a bad signature", opts, err)
		}
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" || *downloads != 0 {
		t.Fatalf("bad signature: executable replaced or binary downloaded (%d)", *downloads)
	}
}

// A signature error that is not a 404 must not pass as "unsigned".
func TestSignatureDownloadErrorIsNotUnsigned(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	_, done := release(t, newBin, sha(newBin), sigError)
	defer done()
	exe := installed(t, []byte("old binary"))
	_, err := Run(context.Background(), "edge", "v8", Options{AllowUnsigned: true})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the download error", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatal("executable replaced")
	}
}

// The signature covers checksums.txt: changing a checksum after signing is caught
// before the binary is downloaded.
func TestTamperedChecksumsAreRefused(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner v9\n")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	good := fmt.Sprintf("%s  %s\n", sha([]byte("the real binary")), asset)
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", sha(newBin), asset) // swapped by an attacker
		case "/checksums.txt.sig":
			_, _ = w.Write(ed25519.Sign(priv, []byte(good)))
		default:
			downloads++
			_, _ = w.Write(newBin)
		}
	}))
	defer srv.Close()
	oldBase, oldExe, oldKey := releaseBase, executable, releaseKey
	defer func() { releaseBase, executable, releaseKey = oldBase, oldExe, oldKey }()
	releaseBase = func(string) string { return srv.URL }
	releaseKey = func() (ed25519.PublicKey, error) { return pub, nil }
	exe := installed(t, []byte("old binary"))
	if _, err := Run(context.Background(), "edge", "v8", Options{}); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v, want a bad signature", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" || downloads != 0 {
		t.Fatalf("tampered checksums: executable replaced or binary downloaded (%d)", downloads)
	}
}

func TestEmbeddedReleaseKey(t *testing.T) {
	if _, err := ParseKey(releaseKeyPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKey([]byte("not a key")); err == nil {
		t.Fatal("garbage parsed as a key")
	}
	// An EC key in the file would be a mistake, not a key to trust.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ec := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if _, err := ParseKey(ec); err == nil || !strings.Contains(err.Error(), "not Ed25519") {
		t.Fatalf("EC key: err = %v, want refused", err)
	}
}

// install.sh verifies releases with its own copy of the key (it runs before
// any firerunner binary is on the host); both must be the same key.
func TestInstallerHasTheReleaseKey(t *testing.T) {
	sh, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sh, bytes.TrimSpace(releaseKeyPEM)) {
		t.Fatal("install.sh does not contain internal/upgrade/release-signing.pub")
	}
}

// Only the releases published before signing may lack a signature; for any
// other one --allow-unsigned does not help: it is being published or forged.
func TestMissingSignatureOfASignedReleaseIsNeverAllowed(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho firerunner edge-abc1234\n")
	downloads, done := release(t, newBin, sha(newBin), unsigned)
	defer done()
	exe := installed(t, []byte("old binary"))
	for _, tag := range []string{"edge", "latest", "v0.2.0"} {
		_, err := Run(context.Background(), tag, "v8", Options{AllowUnsigned: true})
		if err == nil || !strings.Contains(err.Error(), "no signature") || strings.Contains(err.Error(), "--allow-unsigned") {
			t.Fatalf("%s: err = %v, want refused without a way around", tag, err)
		}
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" || *downloads != 0 {
		t.Fatalf("executable replaced or binary downloaded (%d)", *downloads)
	}
}

// An unsigned release is refused even when it is the binary already installed.
func TestUnsignedIdenticalReleaseIsRefused(t *testing.T) {
	bin := []byte("#!/bin/sh\necho firerunner edge-abc1234\n")
	_, done := release(t, bin, sha(bin), unsigned)
	defer done()
	installed(t, bin)
	if _, err := Run(context.Background(), "edge", "edge-abc1234", Options{CheckOnly: true}); err == nil {
		t.Fatal("an unsigned edge passed because it matched the installed binary")
	}
}

// A signed release of another version served in place of the one asked for
// (an older one, to roll a host back) is not installed.
func TestAnotherSignedReleaseIsNotInstalled(t *testing.T) {
	for _, c := range []struct{ tag, says string }{
		{"v0.3.0", "v0.2.0"},
		{"edge", "v0.2.0"},
		{"latest", "edge-abc1234"},
	} {
		newBin := []byte("#!/bin/sh\necho firerunner " + c.says + "\n")
		_, done := release(t, newBin, sha(newBin), signed)
		exe := installed(t, []byte("old binary"))
		_, err := Run(context.Background(), c.tag, "v8", Options{})
		done()
		if err == nil || !strings.Contains(err.Error(), "not installing it") {
			t.Fatalf("%s saying %s: err = %v, want refused", c.tag, c.says, err)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old binary" {
			t.Fatalf("%s saying %s: executable replaced", c.tag, c.says)
		}
	}
	newBin := []byte("#!/bin/sh\necho firerunner v0.3.0\n")
	_, done := release(t, newBin, sha(newBin), signed)
	defer done()
	installed(t, []byte("old binary"))
	if res, err := Run(context.Background(), "v0.3.0", "v8", Options{}); err != nil || res.To != "v0.3.0" {
		t.Fatalf("the release asked for: res %+v err %v", res, err)
	}
}
