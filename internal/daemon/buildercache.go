package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// A builder's layer cache lives in its VM and would go with it. When a builder
// is deleted because it was idle or its slot was needed, its BuildKit state is
// copied to the host first, and the project's next builder starts from it: with
// builder.max builders running, every project that builds keeps a warm cache.
// Each file only ever goes back into a builder of the same project.

// cacheSave is a deleted builder whose cache is being copied to the host; its
// VM stays until the copy ends, and reconcile must not take it (uid).
type cacheSave struct {
	done  chan struct{}
	uid   string
	start time.Time
}

// builderCacheDir holds <project>.tar files (a variable for tests).
var builderCacheDir = "/var/lib/firerunner/builder-cache"

// builderCacheLimit is builder.saved_cache_gb in bytes (a variable for tests).
var builderCacheLimit = func(c config.Config) int64 { return int64(c.Builder.SavedCacheGB) << 30 }

// builderCacheTimeout bounds one save or restore (17 GB takes about 30 s).
var builderCacheTimeout = 15 * time.Minute

// builderCacheMinFree is the share of the disk a save must leave free.
const builderCacheMinFree = 0.10

// The volume is read and written in the builder VM; the save stops buildkitd
// first, so its database is consistent. Sockets left by builds are skipped.
// The archive ends with a marker file: tar accepts an archive cut off between
// two members, so a load without the marker is incomplete and is thrown away.
const (
	builderVolume     = `mp=$(docker volume inspect -f '{{.Mountpoint}}' buildkit)`
	builderMarker     = `.firerunner-complete`
	builderSizeScript = builderVolume + ` && du -sb "$mp" | cut -f1`
	builderSaveScript = `docker stop -t 30 buildkitd >/dev/null && ` + builderVolume +
		` && m=$(mktemp -d) && touch "$m/` + builderMarker + `"` +
		` && tar --warning=no-file-ignored -cf - -C "$mp" . -C "$m" ` + builderMarker
	builderLoadScript = `docker volume create buildkit >/dev/null && ` + builderVolume +
		` && tar -C "$mp" -xf - && [ -e "$mp/` + builderMarker + `" ] && rm -f "$mp/` + builderMarker + `"` +
		` || { docker volume rm -f buildkit >/dev/null 2>&1; exit 1; }`
)

// Remote steps are variables so tests run without VMs.
var (
	builderCacheSize = func(ctx context.Context, cfg config.Config, inst *vm.Instance) (int64, error) {
		out, err := runWithContext(ctx, vm.SSH(cfg, inst, builderSizeScript))
		if err != nil {
			return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
		}
		return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	builderCacheSave = func(ctx context.Context, cfg config.Config, inst *vm.Instance, w io.Writer) error {
		return streamSSH(ctx, cfg, inst, builderSaveScript, nil, w)
	}
	builderCacheLoad = func(ctx context.Context, cfg config.Config, inst *vm.Instance, r io.Reader) error {
		return streamSSH(ctx, cfg, inst, builderLoadScript, r, io.Discard)
	}
	// builderCacheWipe removes a partly loaded volume when the load script
	// could not clean up itself (the connection dropped).
	builderCacheWipe = func(ctx context.Context, cfg config.Config, inst *vm.Instance) error {
		out, err := runWithContext(ctx, vm.SSH(cfg, inst, "docker volume rm -f buildkit >/dev/null"))
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
		}
		return nil
	}
	// diskSpace reports free and total bytes of the file system holding dir.
	diskSpace = func(dir string) (free, total uint64, err error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, 0, err
		}
		return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil //nolint:unconvert // Bsize differs by OS
	}
)

// streamSSH runs script in the VM with stdin and stdout attached.
func streamSSH(ctx context.Context, cfg config.Config, inst *vm.Instance, script string, in io.Reader, out io.Writer) error {
	cmd := vm.SSH(cfg, inst, script)
	var stderr strings.Builder
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
}

func builderCacheFile(project string) string {
	return filepath.Join(builderCacheDir, project+".tar")
}

// keepsCache reports whether a builder removed for reason should leave its
// cache for the project's next builder. An operator's removal and disabling
// builders mean "throw it away"; a builder whose VM is gone or not answering
// has nothing to copy.
func keepsCache(reason string) bool {
	switch reason {
	case "least recently used", "idle", "max age", "config changed":
		return true
	}
	return false
}

// saveBuilderCache copies the builder's BuildKit state to the host. Any failure
// leaves the previous saved copy (if any) in place. Saves run one at a time;
// started is when the builder was removed (see cacheDropped).
func (d *Daemon) saveBuilderCache(ctx context.Context, cfg config.Config, project string, inst *vm.Instance, started time.Time) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	result := "failed"
	defer func() { d.metrics.builderCache.WithLabelValues("save", result).Inc() }()
	ctx, cancel := context.WithTimeout(ctx, builderCacheTimeout)
	defer cancel()
	start := time.Now()
	limit := builderCacheLimit(cfg)

	size, err := builderCacheSize(ctx, cfg, inst)
	if err != nil {
		d.log.Warn("builder cache not saved: size unknown", "project", project, "err", err)
		return
	}
	if size > limit {
		result = "skipped"
		d.log.Warn("builder cache not saved: larger than builder.saved_cache_gb", "project", project, "bytes", size)
		return
	}
	if err := os.MkdirAll(builderCacheDir, 0o700); err != nil {
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	if err := d.makeRoomForCache(project, size, limit); err != nil {
		result = "skipped"
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	tmp, err := os.CreateTemp(builderCacheDir, project+".tar.tmp-*")
	if err != nil {
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	err = builderCacheSave(ctx, cfg, inst, tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	dropped := false
	if err == nil {
		// Under mu, as RemoveBuilders marks and deletes: an operator's
		// `builder rm` since this save started wins over the save.
		d.mu.Lock()
		if dropped = d.cacheDropped[project].After(started); !dropped {
			err = os.Rename(tmp.Name(), builderCacheFile(project))
		}
		d.mu.Unlock()
	}
	if dropped {
		_ = os.Remove(tmp.Name())
		result = "skipped"
		d.log.Info("builder cache not saved: removed by the operator meanwhile", "project", project)
		return
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	result = "ok"
	d.log.Info("builder cache saved", "project", project, "bytes", size,
		"took", time.Since(start).Round(100*time.Millisecond).String())
}

// makeRoomForCache deletes other projects' saved caches, least recently used
// first, until a new one of size fits into limit and leaves builderCacheMinFree
// of the disk free. The project's own old copy stays until the new one replaces it.
func (d *Daemon) makeRoomForCache(project string, size, limit int64) error {
	type saved struct {
		path string
		size int64
		used time.Time
	}
	entries, err := os.ReadDir(builderCacheDir)
	if err != nil {
		return err
	}
	var others []saved
	var total int64
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".tar.tmp-") {
			// A save cut off by a crash; no save of this project runs now (at most
			// one per project), and other projects' temp files are only this old
			// when their daemon is gone.
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > builderCacheTimeout {
				_ = os.Remove(filepath.Join(builderCacheDir, name))
			}
			continue
		}
		if !strings.HasSuffix(name, ".tar") || name == project+".tar" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, saved{filepath.Join(builderCacheDir, name), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(others, func(i, j int) bool { return others[i].used.Before(others[j].used) })

	free, disk, err := diskSpace(builderCacheDir)
	if err != nil {
		return err
	}
	minFree := int64(float64(disk) * builderCacheMinFree)
	avail := int64(free)
	for len(others) > 0 && (total+size > limit || avail-size < minFree) {
		o := others[0]
		others = others[1:]
		if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= o.size
		avail += o.size
		d.log.Info("saved builder cache dropped to make room", "file", filepath.Base(o.path), "bytes", o.size)
	}
	if total+size > limit || avail-size < minFree {
		return fmt.Errorf("no room: %d bytes needed, %d free", size, avail)
	}
	return nil
}

// restoreBuilderCache loads the project's saved cache into a new builder VM
// before buildkitd starts, and reports whether it did. A copy that tar or the
// completeness check refuses is deleted, so it is not tried again; one that
// failed on the way (the SSH connection) is kept. After a failed load the
// volume is removed, so buildkitd starts empty; err means the VM may still
// hold part of a cache and must not be used.
func (d *Daemon) restoreBuilderCache(ctx context.Context, cfg config.Config, project string, inst *vm.Instance) (bool, error) {
	file := builderCacheFile(project)
	f, err := os.Open(file)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.log.Warn("saved builder cache unreadable", "project", project, "err", err)
		}
		return false, nil
	}
	defer f.Close()
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	lctx, cancel := context.WithTimeout(ctx, builderCacheTimeout)
	defer cancel()
	start := time.Now()
	if err := builderCacheLoad(lctx, cfg, inst, f); err != nil {
		d.metrics.builderCache.WithLabelValues("restore", "failed").Inc()
		if lctx.Err() == nil && exitCode(err) != 255 { // 255: ssh itself failed
			_ = os.Remove(file)
			d.log.Warn("saved builder cache refused and deleted, starting empty", "project", project, "err", err)
		} else {
			d.log.Warn("saved builder cache not restored (kept for the next builder)", "project", project, "err", err)
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		wctx, wcancel := context.WithTimeout(ctx, time.Minute)
		defer wcancel()
		if werr := builderCacheWipe(wctx, cfg, inst); werr != nil {
			return false, fmt.Errorf("partly restored builder cache could not be removed: %w", werr)
		}
		return false, nil
	}
	// The modification time orders caches for makeRoomForCache: this one is in use.
	now := time.Now()
	_ = os.Chtimes(file, now, now)
	d.metrics.builderCache.WithLabelValues("restore", "ok").Inc()
	d.log.Info("builder cache restored", "project", project, "bytes", size,
		"took", time.Since(start).Round(100*time.Millisecond).String())
	return true, nil
}

// dropSavedCaches deletes the saved cache of project ("all": every project)
// except those in keep, for an operator's `builder rm`.
func dropSavedCaches(project string, keep map[string]bool) {
	if project != "all" {
		if !keep[project] {
			_ = os.Remove(builderCacheFile(project))
		}
		return
	}
	entries, _ := os.ReadDir(builderCacheDir)
	for _, e := range entries {
		if p, ok := strings.CutSuffix(e.Name(), ".tar"); ok && projectID.MatchString(p) && !keep[p] {
			_ = os.Remove(filepath.Join(builderCacheDir, e.Name()))
		}
	}
}
