package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// builderCacheSlack is reserved on top of a save's size (a variable for
// tests). The size is exact (see builderSizeScript), so it only has to cover
// a few bytes more, if any.
var builderCacheSlack int64 = 1 << 20

// The volume is read and written in the builder VM; buildkitd is stopped
// first, so its database is consistent. Sockets left by builds are skipped.
// The archive ends with a marker file: tar accepts an archive cut off between
// two members, so a load without the marker is incomplete and is thrown away.
//
// The size step reports the archive's exact size, which is what a save
// reserves on the host: GNU tar sizes an archive written to /dev/null without
// reading file contents (--totals). The volume's du is no measure: 20,000
// hard links under a deep path take 2 bytes of du and 133 MB of tar.
const (
	builderStop    = `docker stop -t 30 buildkitd >/dev/null`
	builderVolume  = `mp=$(docker volume inspect -f '{{.Mountpoint}}' buildkit)`
	builderMarker  = `.firerunner-complete`
	builderArchive = `m=$(mktemp -d) && touch "$m/` + builderMarker + `" && LC_ALL=C tar --warning=no-file-ignored`
	builderMembers = ` -C "$mp" . -C "$m" ` + builderMarker

	builderSizeScript = builderStop + ` && ` + builderVolume + ` && ` + builderArchive + ` --totals -cf /dev/null` + builderMembers + ` 2>&1`
	builderSaveScript = builderStop + ` && ` + builderVolume + ` && ` + builderArchive + ` -cf -` + builderMembers
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
		return parseTarTotal(out)
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

// tarTotal is the line GNU tar's --totals prints.
var tarTotal = regexp.MustCompile(`Total bytes written: ([0-9]+)`)

// parseTarTotal reads the archive size from the size step's output.
func parseTarTotal(out string) (int64, error) {
	m := tarTotal.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no archive size in %q", strings.TrimSpace(out))
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// errCacheOverrun: a save's archive outgrew the bytes it reserved.
var errCacheOverrun = errors.New("the archive outgrew its reservation")

// cacheWriter writes a save into its temp file and refuses bytes past the
// save's reservation: the size came from the guest, and the host bounds what
// it takes on trust. Written bytes leave cacheReserved as they reach the file
// (makeRoomForCache counts the temp file), so they are never counted twice.
type cacheWriter struct {
	d    *Daemon
	f    *os.File
	left int64 // reserved bytes not written yet
	over bool
}

func (w *cacheWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		w.over = true
		return 0, errCacheOverrun
	}
	n, err := w.f.Write(p)
	w.left -= int64(n)
	w.d.cacheMu.Lock()
	w.d.cacheReserved -= int64(n)
	w.d.cacheMu.Unlock()
	return n, err
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
// leaves the previous saved copy (if any) in place. Saves copy side by side,
// but each reserves its size first (see makeRoomForCache); started is when the
// builder was removed (see cacheDropped).
func (d *Daemon) saveBuilderCache(ctx context.Context, cfg config.Config, project string, inst *vm.Instance, started time.Time) {
	result := "failed"
	defer func() { d.metrics.builderCache.WithLabelValues("save", result).Inc() }()
	ctx, cancel := context.WithTimeout(ctx, builderCacheTimeout)
	defer cancel()
	start := time.Now()
	limit := builderCacheLimit(cfg)

	size, err := builderCacheSize(ctx, cfg, inst)
	if err == nil && size < 0 {
		err = fmt.Errorf("negative size %d", size)
	}
	if err != nil {
		d.log.Warn("builder cache not saved: size unknown", "project", project, "err", err)
		return
	}
	reserve := size + builderCacheSlack
	if reserve > limit {
		result = "skipped"
		d.log.Warn("builder cache not saved: larger than builder.saved_cache_gb", "project", project, "bytes", size)
		return
	}
	if err := os.MkdirAll(builderCacheDir, 0o700); err != nil {
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	d.cacheMu.Lock()
	err = d.makeRoomForCache(project, reserve, limit, d.cacheReserved)
	if err == nil {
		d.cacheReserved += reserve
	}
	d.cacheMu.Unlock()
	if err != nil {
		result = "skipped"
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	w := &cacheWriter{d: d, left: reserve}
	defer func() {
		d.cacheMu.Lock()
		d.cacheReserved -= w.left // what was written left the reservation already
		d.cacheMu.Unlock()
	}()
	tmp, err := os.CreateTemp(builderCacheDir, project+".tar.tmp-*")
	if err != nil {
		d.log.Warn("builder cache not saved", "project", project, "err", err)
		return
	}
	w.f = tmp
	err = builderCacheSave(ctx, cfg, inst, w)
	if w.over {
		// The copy is cut off at the reservation, whatever the stream says.
		err = fmt.Errorf("%w of %d bytes", errCacheOverrun, reserve)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	dropped := false
	if err == nil {
		// Under mu, as RemoveBuilders marks and deletes: an operator's
		// `builder rm` since this save started wins over the save. The
		// previous copy is set aside rather than renamed over, so its blocks
		// are freed after the lock is released.
		file, aside := builderCacheFile(project), ""
		d.mu.Lock()
		if dropped = d.cacheDropped[project].After(started); !dropped {
			aside = setAside(file)
			if err = os.Rename(tmp.Name(), file); err != nil && aside != "" {
				_ = os.Rename(aside, file) // keep the previous copy
				aside = ""
			}
		}
		d.mu.Unlock()
		if aside != "" {
			_ = unlinkCache(aside)
		}
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
// of the disk free. Saves still being written count with what they wrote (their
// temp files, which the disk no longer has free) and with reserved, the bytes
// they may still write. The project's own old copy stays until the new one
// replaces it. Callers hold cacheMu.
func (d *Daemon) makeRoomForCache(project string, size, limit, reserved int64) error {
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
			info, err := e.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > builderCacheTimeout {
				// A save cut off by a crash; no save of this project runs now (at
				// most one per project), and other projects' temp files are only
				// this old when their daemon is gone.
				_ = os.Remove(filepath.Join(builderCacheDir, name))
				continue
			}
			total += info.Size() // a save being written, or a copy set aside
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
	avail := int64(free) - reserved // still to be written by the other saves
	total += reserved
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
// completeness check refuses is deleted, so it is not tried again, and the
// volume is removed so buildkitd starts empty. A load that failed on the way
// (the SSH connection, a timeout) keeps the copy and returns an error: the VM
// may hold part of a cache and is not used, and the next builder retries.
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
		// Only the VM's own verdict (tar or the marker check: exit 1..254) says the
		// copy is bad. 255 is ssh failing, -1 ssh killed (e.g. by the OOM killer).
		if code := exitCode(err); lctx.Err() != nil || code <= 0 || code == 255 {
			// The copy may be fine. A builder started empty would, once deleted,
			// save its empty cache over it: fail this boot, the next one retries.
			return false, fmt.Errorf("saved builder cache not restored (kept, the next builder retries it): %w", err)
		}
		_ = os.Remove(file)
		d.log.Warn("saved builder cache refused and deleted, starting empty", "project", project, "err", err)
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

// dropSavedCachesLocked moves the saved cache of project ("all": every
// project) except those in keep aside, for an operator's `builder rm`, and
// returns the moved files: the caller unlinks them after releasing d.mu (see
// setAside).
func dropSavedCachesLocked(project string, keep map[string]bool) []string {
	var aside []string
	if project != "all" {
		if !keep[project] {
			if f := setAside(builderCacheFile(project)); f != "" {
				aside = append(aside, f)
			}
		}
		return aside
	}
	entries, _ := os.ReadDir(builderCacheDir)
	for _, e := range entries {
		if p, ok := strings.CutSuffix(e.Name(), ".tar"); ok && projectID.MatchString(p) && !keep[p] {
			if f := setAside(filepath.Join(builderCacheDir, e.Name())); f != "" {
				aside = append(aside, f)
			}
		}
	}
	return aside
}

// setAside renames a saved cache to a temp name next to it and returns that
// name ("" if there was nothing to move). A rename is quick; unlinking the
// last name of a large file frees its blocks, which takes a while, so it is
// done with unlinkCache after the lock POST /claim needs is released. A crash
// in between leaves a temp file that makeRoomForCache removes later.
func setAside(path string) string {
	aside := fmt.Sprintf("%s.tmp-old-%d", path, time.Now().UnixNano())
	if os.Rename(path, aside) != nil {
		return ""
	}
	return aside
}

// unlinkCache deletes a file set aside (a variable for tests).
var unlinkCache = os.Remove
