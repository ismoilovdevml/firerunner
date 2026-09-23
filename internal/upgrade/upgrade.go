// Package upgrade replaces the running firerunner binary with a published release.
package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Result describes what Run did.
type Result struct {
	From, To string
	Changed  bool
}

// Run downloads the release, verifies its checksum and atomically replaces
// the current executable. With checkOnly it stops after reporting the version.
func Run(ctx context.Context, tag, current string, checkOnly bool) (*Result, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	base := BaseURL(tag)

	sums, err := fetch(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}
	want, err := checksumFor(string(sums), asset)
	if err != nil {
		return nil, err
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
	res := &Result{From: current, To: next, Changed: next != current}
	if checkOnly || !res.Changed {
		return res, nil
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return nil, fmt.Errorf("replacing %s: %w", exe, err)
	}
	return res, nil
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}
