package daemon

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
	start time.Time // when the builder was removed
	// inst and specID are recorded in builders.json when the daemon's stop
	// cuts the copy off (cut); see keepCutSave.
	inst   vm.Instance
	specID string
	cut    bool
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
	// The load exits loadArchiveRefused only when tar fails or the archive
	// ends without the marker, and loadVMFailed when the VM cannot take a
	// cache at all (dockerd, the volume). Either way the volume is removed.
	builderLoadUndo   = `{ docker volume rm -f buildkit >/dev/null 2>&1; exit `
	builderLoadScript = `{ docker volume create buildkit >/dev/null && ` + builderVolume + `; } || ` + builderLoadUndo + `4; }; ` +
		`tar -C "$mp" -xf - || ` + builderLoadUndo + `3; }; ` +
		`[ -e "$mp/` + builderMarker + `" ] || ` + builderLoadUndo + `3; }; ` +
		`rm -f "$mp/` + builderMarker + `" || ` + builderLoadUndo + `4; }`
)

// Exit statuses of builderLoadScript. tar also fails when it cannot write
// (the VM's disk full, an I/O error), not only on a broken archive, so
// loadArchiveRefused alone does not condemn the saved copy: the host reads it
// again (verifyCacheArchive) before deleting it.
const (
	loadArchiveRefused = 3
	loadVMFailed       = 4
)

// errCacheCorrupt: a saved cache file is not a whole archive.
var errCacheCorrupt = errors.New("saved builder cache is not a complete archive")

// verifyCacheArchive reads the saved cache f from its start the way a restore
// does (the header, then the archive) and reports errCacheCorrupt when the
// archive is broken or ends without the marker; any other error (ctx ended)
// means it could not tell, and nil that the file is whole.
func verifyCacheArchive(ctx context.Context, f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	readCacheHeader(r) // a legacy copy has none: nothing is consumed then
	tr := tar.NewReader(r)
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next() // reads past the previous member's data: a cut-off file fails here
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errCacheCorrupt, err)
		}
		last = h.Name
	}
	if path.Clean(last) != builderMarker {
		return fmt.Errorf("%w: it ends without %s", errCacheCorrupt, builderMarker)
	}
	return nil
}

// Remote steps are variables so tests run without VMs.
var (
	builderCacheSize = func(ctx context.Context, cfg config.Config, inst *vm.Instance) (int64, error) {
		out, err := runWithContext(ctx, vm.SSHStream(cfg, inst, builderSizeScript))
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
	cmd := vm.SSHStream(cfg, inst, script)
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
	// Written bytes move from the reservation to the temp file in one step
	// under cacheMu: makeRoomForCache counts both, and between the two it
	// counted them twice and turned other saves away.
	w.d.cacheMu.Lock()
	defer w.d.cacheMu.Unlock()
	n, err := w.f.Write(p)
	w.left -= int64(n)
	w.d.cacheReserved -= int64(n)
	return n, err
}

// A saved cache starts with one line naming the builder image that wrote it,
// then the archive. It is only restored into a builder of the same image: a
// new builder.image may fix a BuildKit flaw the old state carries.
const builderCacheMagic = "firerunner-builder-cache/1 "

// legacyBuilderImage wrote every cache saved before caches named their image:
// it was the only builder.image until then. A cache without a header is
// restored as its cache while builder.image is still this one.
const legacyBuilderImage = "moby/buildkit:v0.33.0"

func cacheHeader(image string) string { return builderCacheMagic + strconv.Quote(image) + "\n" }

// readCacheHeader reads the header of a saved cache and returns its image;
// ok is false for a file without one (saved before images were recorded).
func readCacheHeader(r *bufio.Reader) (image string, ok bool) {
	if magic, err := r.Peek(len(builderCacheMagic)); err != nil || string(magic) != builderCacheMagic {
		return "", false
	}
	line, err := r.ReadSlice('\n') // at most the reader's buffer: a header is short
	if err != nil {
		return "", false
	}
	image, err = strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(string(line), builderCacheMagic)))
	return image, err == nil
}

func builderCacheFile(project string) string {
	return filepath.Join(builderCacheDir, project+".tar")
}

// saveBuilderCache copies the builder's BuildKit state to the host, under a
// header naming the builder's image. Any failure leaves the previous saved
// copy (if any) in place. Saves copy side by side, but each reserves its size
// first (see makeRoomForCache); started is when the builder was removed (see
// cacheDropped). It reports whether ctx ended (the daemon stops) before the
// copy finished; such a save is not counted as failed.
func (d *Daemon) saveBuilderCache(ctx context.Context, cfg config.Config, project string, inst *vm.Instance, image string, started time.Time) (stopped bool) {
	result := "failed"
	parent := ctx
	defer func() {
		if stopped = result == "failed" && parent.Err() != nil; !stopped {
			d.metrics.builderCache.WithLabelValues("save", result).Inc()
		}
	}()
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
	header := cacheHeader(image)
	// The size is the guest's word: it is checked against the budget before
	// anything is added to it, so no size wraps around into a small (or
	// negative) reservation.
	if limit <= 0 || size > limit-int64(len(header))-builderCacheSlack {
		result = "skipped"
		d.log.Warn("builder cache not saved: larger than builder.saved_cache_gb", "project", project, "bytes", size)
		return
	}
	reserve := size + int64(len(header)) + builderCacheSlack
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
	if _, err = io.WriteString(w, header); err == nil {
		err = builderCacheSave(ctx, cfg, inst, w)
	}
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
		// previous copy gets a second name before the new one is renamed over
		// it: <project>.tar never goes missing (a builder booting meanwhile
		// restores without the lock, and would start cold), and the old
		// blocks are freed when that name is unlinked, after the lock is
		// released.
		file, aside := builderCacheFile(project), ""
		d.mu.Lock()
		if dropped = d.cacheDropped[project].After(started); !dropped {
			aside = linkAside(file)
			err = renameCache(tmp.Name(), file) // failed: the previous copy stays in place
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
	return false
}

// makeRoomForCache deletes other projects' saved caches, least recently used
// first, until a new one of size fits into limit and leaves builderCacheMinFree
// of the disk free. Saves still being written count with what they wrote (their
// temp files, which the disk no longer has free) and with reserved, the bytes
// they may still write. The project's own old copy stays until the new one
// replaces it. A size that is not positive or exceeds limit is refused before
// anything is deleted: the sums below would wrap around and read as room that
// every other cache has to make. Callers hold cacheMu.
func (d *Daemon) makeRoomForCache(project string, size, limit, reserved int64) error {
	if size <= 0 || size > limit {
		return fmt.Errorf("no room for a %d-byte cache in a %d-byte budget", size, limit)
	}
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
		d.metrics.builderCache.WithLabelValues("evict", "ok").Inc()
		d.log.Info("saved builder cache dropped to make room", "file", filepath.Base(o.path), "bytes", o.size)
	}
	if total+size > limit || avail-size < minFree {
		return fmt.Errorf("no room: %d bytes needed, %d free", size, avail)
	}
	return nil
}

// restoreBuilderCache loads the project's saved cache into a new builder VM
// before buildkitd starts, and returns the result it counts in
// firerunner_builder_cache_total: ok or legacy when it loaded the copy,
// missing, stale or failed when the builder starts empty or the error is not
// nil (failed also when the copy cannot be opened). A copy written by
// another builder image than cfg's is deleted unread. A copy that tar or the
// completeness check refuses, and that the host cannot read whole either
// (verifyCacheArchive), is deleted, so it is not tried again, and the volume
// is removed so buildkitd starts empty. Any other failed load (the SSH
// connection, a timeout, dockerd, the VM's disk) keeps the copy and returns
// an error: the VM may hold part of a cache and is not used, and the next
// builder retries.
func (d *Daemon) restoreBuilderCache(ctx context.Context, cfg config.Config, project string, inst *vm.Instance) (string, error) {
	file := builderCacheFile(project)
	f, err := os.Open(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.metrics.builderCache.WithLabelValues("restore", "missing").Inc()
			return "missing", nil
		}
		d.log.Warn("saved builder cache unreadable", "project", project, "err", err)
		return "failed", nil
	}
	defer f.Close()
	// The copy as it was before this restore touched it: what the drop and
	// the refusal count compare against, and the last-used time a failed
	// load gives back.
	loaded, err := f.Stat()
	if err != nil {
		d.log.Warn("saved builder cache unreadable", "project", project, "err", err)
		return "failed", nil
	}
	size := loaded.Size()
	r := bufio.NewReader(f)
	magic, _ := r.Peek(len(builderCacheMagic))
	legacy := string(magic) != builderCacheMagic && cfg.Builder.Image == legacyBuilderImage
	if image, ok := readCacheHeader(r); !legacy && (!ok || image != cfg.Builder.Image) {
		d.dropSavedCache(project, loaded)
		d.metrics.builderCache.WithLabelValues("restore", "stale").Inc()
		d.log.Info("saved builder cache dropped: written by another builder image, starting empty",
			"project", project, "cache_image", image, "image", cfg.Builder.Image)
		return "stale", nil
	}
	// The modification time orders caches for makeRoomForCache, oldest
	// dropped first: this one is in use from now on. Touched after the load
	// instead, it would be the first candidate for another project's save
	// during a long load.
	now := time.Now()
	_ = os.Chtimes(file, now, now)
	lctx, cancel := context.WithTimeout(ctx, builderCacheTimeout)
	defer cancel()
	start := time.Now()
	if err := builderCacheLoad(lctx, cfg, inst, r); err != nil {
		d.metrics.builderCache.WithLabelValues("restore", "failed").Inc()
		// Only tar refusing the archive (loadArchiveRefused) may mean the copy
		// is bad; ssh failing (255), ssh killed (-1, e.g. by the OOM killer) or
		// the VM not taking the cache (loadVMFailed: dockerd, the volume) do
		// not. tar fails the same way on a full VM disk, so the host reads
		// the copy again and only deletes it if that read fails as well.
		keep := lctx.Err() != nil || exitCode(err) != loadArchiveRefused
		if !keep {
			verr := verifyCacheArchive(lctx, f)
			switch {
			case errors.Is(verr, errCacheCorrupt):
			case verr != nil:
				keep = true
				err = fmt.Errorf("%w (the host could not read the copy again: %v)", err, verr)
			case d.noteRefusal(project, loaded) < builderCacheRefusals:
				keep = true // whole, but tar refused it: maybe once
			default:
				// Whole, and refused again: the VM cannot hold it (a disk
				// too small for it). Kept, it would fail every boot.
				err = fmt.Errorf("%w (refused %d times though whole: the builder's disk cannot hold it)", err, builderCacheRefusals)
			}
		}
		if keep {
			// The copy may be fine. A builder started empty would, once deleted,
			// save its empty cache over it: fail this boot, the next one retries.
			// It was not used: the LRU may drop it before copies that were.
			d.untouchSavedCache(project, loaded)
			return "failed", fmt.Errorf("saved builder cache not restored (kept, the next builder retries it): %w", err)
		}
		d.dropSavedCache(project, loaded)
		d.log.Warn("saved builder cache refused and deleted, starting empty", "project", project, "err", err)
		if werr := wipeBuilderCache(ctx, cfg, inst); werr != nil {
			return "failed", fmt.Errorf("partly restored builder cache could not be removed: %w", werr)
		}
		return "failed", nil
	}
	d.forgetRefusals(project)
	result := "ok"
	if legacy {
		result = "legacy" // its next save writes the header
	}
	d.metrics.builderCache.WithLabelValues("restore", result).Inc()
	d.log.Info("builder cache restored", "project", project, "bytes", size,
		"took", time.Since(start).Round(100*time.Millisecond).String())
	return result, nil
}

// builderCacheRefusals is how many times tar may refuse a copy the host reads
// whole before it is deleted: the first refusal may be passing (the VM's disk
// briefly full), a second one of the same file is taken as permanent.
const builderCacheRefusals = 2

// cacheRefusal counts tar's refusals of one saved copy, known by the file's
// identity before the restore touched it (see sameCopy).
type cacheRefusal struct {
	copy os.FileInfo
	n    int
}

// sameCopy: a and b are the same saved copy, unchanged. Another save renames
// a new file into place, which changes the inode.
func sameCopy(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// noteRefusal counts one refusal of the project's copy loaded and returns
// how many there were in a row; another copy starts at 1.
func (d *Daemon) noteRefusal(project string, loaded os.FileInfo) int {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	r := d.cacheRefused[project]
	if r.copy == nil || !sameCopy(r.copy, loaded) {
		r = cacheRefusal{copy: loaded}
	}
	r.n++
	d.cacheRefused[project] = r
	return r.n
}

// forgetRefusals drops the project's refusal count: its copy loaded, or is
// gone. Under cacheMu, not mu: a restore may run while a save holds mu.
func (d *Daemon) forgetRefusals(project string) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	delete(d.cacheRefused, project)
}

// untouchSavedCache gives the project's copy whose load failed the
// last-used time it had before (loaded), unless a save put a new copy in its
// place meanwhile.
func (d *Daemon) untouchSavedCache(project string, loaded os.FileInfo) {
	file := builderCacheFile(project)
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, err := os.Stat(file); err == nil && os.SameFile(loaded, cur) {
		_ = os.Chtimes(file, loaded.ModTime(), loaded.ModTime())
	}
}

// dropSavedCache deletes the project's saved cache file, as it was when a
// boot loaded it (loaded), unless a save put a new copy in its place
// meanwhile (the boot stops waiting for a slow save after builderSaveWait).
// The check and the rename are under mu, as saves rename under it; the
// blocks are freed after. The copy's refusal count goes with it.
func (d *Daemon) dropSavedCache(project string, loaded os.FileInfo) {
	file := builderCacheFile(project)
	aside := ""
	d.mu.Lock()
	if cur, err := os.Stat(file); err == nil && os.SameFile(loaded, cur) {
		aside = setAside(file)
	}
	d.mu.Unlock()
	if aside != "" {
		d.forgetRefusals(project)
		_ = unlinkCache(aside)
	}
}

// wipeBuilderCache removes the volume a restore loaded (all or part of), so
// buildkitd starts empty.
func wipeBuilderCache(ctx context.Context, cfg config.Config, inst *vm.Instance) error {
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return builderCacheWipe(wctx, cfg, inst)
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

// linkAside gives a saved cache a second, temporary name, so the file stays
// in place and renaming a new copy over it does not free its blocks (see
// setAside), and returns that name ("" if there was nothing to link, or the
// link failed: then the rename over it frees them).
func linkAside(path string) string {
	aside := fmt.Sprintf("%s.tmp-old-%d", path, time.Now().UnixNano())
	if os.Link(path, aside) != nil {
		return ""
	}
	return aside
}

// unlinkCache deletes a file set aside (a variable for tests).
var unlinkCache = os.Remove

// renameCache puts a finished save in place (a variable for tests).
var renameCache = os.Rename
