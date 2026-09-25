// Package upgrade replaces the running firerunner binary with a published release.
package upgrade

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	repo  = "ismoilovdevml/firerunner"
	asset = "firerunner-linux-amd64"
)

// BaseURL returns the download location of a release.
// "latest" is the newest stable release, "edge" the rolling build of main.
func BaseURL(tag string) string {
	if tag == "latest" {
		return "https://github.com/" + repo + "/releases/latest/download"
	}
	return "https://github.com/" + repo + "/releases/download/" + tag
}

// releaseKeyPEM is the public half of the key release.yml signs checksums.txt
// with (checksums.txt.sig, a raw Ed25519 signature of its exact bytes).
//
//go:embed release-signing.pub
var releaseKeyPEM []byte

// ParseKey returns the Ed25519 public key in a PEM (SPKI) block.
func ParseKey(pemBytes []byte) (ed25519.PublicKey, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("no PEM block in the release signing key")
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("release signing key: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("release signing key is %T, not Ed25519", k)
	}
	return pub, nil
}

// releaseBase, executable and releaseKey are variables so tests need no
// GitHub, do not replace the test binary and sign with a key of their own.
var (
	releaseKey  = func() (ed25519.PublicKey, error) { return ParseKey(releaseKeyPEM) }
	releaseBase = BaseURL
	executable  = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
)

// Options says how Run treats a release.
type Options struct {
	// CheckOnly downloads and runs nothing but checksums.txt and its signature.
	CheckOnly bool
	// AllowUnsigned accepts a release without a signature: the ones published
	// before releases were signed. A signature that does not verify is never
	// accepted.
	AllowUnsigned bool
}

// Result describes what Run did.
type Result struct {
	From, To string
	Changed  bool
	// Unsigned: the release had no signature and Options.AllowUnsigned took it.
	Unsigned bool
}

// errNotFound is a download that does not exist (HTTP 404).
var errNotFound = errors.New("not found")

// Run downloads the release, verifies the signature of its checksums and the
// checksum of the binary, and atomically replaces the current executable.
// Whether the release differs from the running binary is decided by checksum,
// so CheckOnly downloads and runs nothing but checksums.txt and its signature,
// and an identical release is not downloaded at all.
func Run(ctx context.Context, tag, current string, opts Options) (*Result, error) {
	exe, err := executable()
	if err != nil {
		return nil, err
	}
	base := releaseBase(tag)

	sums, err := fetch(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}
	unsigned, err := verifySums(ctx, base, tag, sums, opts.AllowUnsigned)
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(string(sums), asset)
	if err != nil {
		return nil, err
	}
	have, err := fileSHA256(exe)
	if err != nil {
		return nil, err
	}
	if have == want {
		return &Result{From: current, To: current, Unsigned: unsigned}, nil
	}
	if opts.CheckOnly {
		// The new version string would need running the new binary (as root).
		return &Result{From: current, To: tag, Changed: true, Unsigned: unsigned}, nil
	}

	// Download next to the executable so the final rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".firerunner-upgrade-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	bin, err := fetch(ctx, base+"/"+asset)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bin)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, want)
	}
	if _, err := tmp.Write(bin); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, tmp.Name(), "version").Output()
	if err != nil {
		return nil, fmt.Errorf("downloaded binary does not run: %w", err)
	}
	next := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "firerunner"))
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return nil, fmt.Errorf("replacing %s: %w", exe, err)
	}
	return &Result{From: current, To: next, Changed: true, Unsigned: unsigned}, nil
}

// verifySums checks checksums.txt against the release signature. It reports
// whether the release is unsigned, which is an error unless allowUnsigned.
func verifySums(ctx context.Context, base, tag string, sums []byte, allowUnsigned bool) (bool, error) {
	key, err := releaseKey()
	if err != nil {
		return false, err
	}
	sig, err := fetch(ctx, base+"/checksums.txt.sig")
	if errors.Is(err, errNotFound) {
		if allowUnsigned {
			return true, nil
		}
		return false, fmt.Errorf("release %s is not signed (releases published before signing are not); --allow-unsigned installs it anyway", tag)
	}
	if err != nil {
		return false, fmt.Errorf("release %s: %w", tag, err)
	}
	if !ed25519.Verify(key, sums, sig) {
		return false, fmt.Errorf("release %s: checksums.txt does not match its signature, not installing it "+
			"(while a release is being published this can happen for a minute: try again)", tag)
	}
	return false, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checksumFor(sums, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s in checksums.txt", name)
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}
