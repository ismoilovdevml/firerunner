package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// stubBuilderCache points the saved-cache directory at a temp dir and replaces
// the remote steps; a nil func keeps the current one.
func stubBuilderCache(t *testing.T,
	size func(context.Context, config.Config, *vm.Instance) (int64, error),
	save func(context.Context, config.Config, *vm.Instance, io.Writer) error,
	load func(context.Context, config.Config, *vm.Instance, io.Reader) error) string {
	t.Helper()
	oldDir, oldSize, oldSave, oldLoad, oldDisk := builderCacheDir, builderCacheSize, builderCacheSave, builderCacheLoad, diskSpace
	builderCacheDir = t.TempDir()
	if size != nil {
		builderCacheSize = size
	}
	if save != nil {
		builderCacheSave = save
	}
	if load != nil {
		builderCacheLoad = load
	}
	diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	t.Cleanup(func() {
		builderCacheDir, builderCacheSize, builderCacheSave, builderCacheLoad, diskSpace = oldDir, oldSize, oldSave, oldLoad, oldDisk
	})
	return builderCacheDir
}

func sizeOf(n int64) func(context.Context, config.Config, *vm.Instance) (int64, error) {
	return func(context.Context, config.Config, *vm.Instance) (int64, error) { return n, nil }
}

func writeFile(t *testing.T, path, data string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func cacheCount(d *Daemon, op, result string) float64 {
	m := &dto.Metric{}
	if err := d.metrics.builderCache.WithLabelValues(op, result).Write(m); err != nil {
		return -1
	}
	return m.GetCounter().GetValue()
}

// An idle builder's cache is copied out before its VM goes, and the project's
// next builder gets exactly that copy back.
func TestBuilderCacheSavedOnExpiryAndRestored(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var loaded string
	dir := stubBuilderCache(t, sizeOf(6),
		func(_ context.Context, _ config.Config, inst *vm.Instance, w io.Writer) error {
			_, err := io.WriteString(w, "data-"+strings.TrimPrefix(inst.ID, "bld-"))
			return err
		},
		func(_ context.Context, _ config.Config, _ *vm.Instance, r io.Reader) error {
			b, err := io.ReadAll(r)
			loaded = string(b)
			return err
		})
	d, srv := newTestDaemon(t)
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()

	if got := readFile(t, filepath.Join(dir, "7.tar")); got != "data-7" {
		t.Fatalf("saved cache = %q", got)
	}
	if n := cacheCount(d, "save", "ok"); n != 1 {
		t.Fatalf("save ok = %v", n)
	}
	if d.saving["7"] != nil {
		t.Fatal("save still marked in progress")
	}
	if !d.restoreBuilderCache(context.Background(), d.cfg, "7", &vm.Instance{ID: "bld-7"}) || loaded != "data-7" {
		t.Fatalf("restore loaded %q", loaded)
	}
	if n := cacheCount(d, "restore", "ok"); n != 1 {
		t.Fatalf("restore ok = %v", n)
	}
	// A project without a saved cache starts empty, without an error.
	if d.restoreBuilderCache(context.Background(), d.cfg, "8", &vm.Instance{ID: "bld-8"}) {
		t.Fatal("restored a cache that was never saved")
	}
}

// A save cut off half way leaves the previous copy and no temp file, and the
// builder VM is still deleted.
func TestBuilderCacheFailedSaveKeepsPreviousCopy(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	dir := stubBuilderCache(t, sizeOf(100),
		func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
			_, _ = io.WriteString(w, "partial")
			return errors.New("ssh: connection reset")
		}, nil)
	writeFile(t, filepath.Join(dir, "7.tar"), "previous", time.Hour)
	d, srv := newTestDaemon(t)
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()

	if got := readFile(t, filepath.Join(dir, "7.tar")); got != "previous" {
		t.Fatalf("saved cache = %q, want the previous copy", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left files behind: %v", entries)
	}
	if n := cacheCount(d, "save", "failed"); n != 1 {
		t.Fatalf("save failed = %v", n)
	}
}

// Caches bigger than the whole budget are not saved; builders removed by the
// operator or with builders disabled are not saved at all.
func TestBuilderCacheSkippedCases(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	saves := 0
	dir := stubBuilderCache(t, sizeOf(2<<30),
		func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
			saves++
			_, err := io.WriteString(w, "x")
			return err
		}, nil)
	d, srv := newTestDaemon(t)
	d.cfg.Builder.SavedCacheGB = 1
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()
	if saves != 0 || cacheCount(d, "save", "skipped") != 1 {
		t.Fatalf("over-budget cache: saves=%d skipped=%v", saves, cacheCount(d, "save", "skipped"))
	}

	d.cfg.Builder.SavedCacheGB = 100
	d.builders = map[string]*builder{"8": readyBuilder("8", 20002, time.Hour, builderSpec(d.cfg))}
	d.RemoveBuilders("8", false)
	srv.WaitDeleted(t, "uid-8", 2*time.Second)
	d.mu.Lock()
	d.cfg.Builder.Enabled = false
	d.builders = map[string]*builder{"9": readyBuilder("9", 20003, time.Hour, builderSpec(d.cfg))}
	d.mu.Unlock()
	d.expireBuilders()
	srv.WaitDeleted(t, "uid-9", 2*time.Second)
	d.bg.Wait()
	if saves != 0 {
		t.Fatalf("saved %d caches for operator removal / disabled builders", saves)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files: %v", entries)
	}
}

// `builder rm` throws saved caches away too, except for builders it kept
// because a job is building on them.
func TestRemoveBuildersDropsSavedCaches(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	dir := stubBuilderCache(t, nil, nil, nil)
	for _, p := range []string{"5", "6", "7"} {
		writeFile(t, filepath.Join(dir, p+".tar"), p, time.Hour)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "keep", time.Hour)
	d, _ := newTestDaemon(t)

	d.RemoveBuilders("5", false)
	if _, err := os.Stat(filepath.Join(dir, "5.tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("saved cache of a project without a builder kept")
	}
	busyJob(t, "7")
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, 0, builderSpec(d.cfg))}
	out := d.RemoveBuilders("all", false)
	if len(out.Skipped) != 1 || out.Skipped[0] != "7" {
		t.Fatalf("removal = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "6.tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rm all kept a saved cache")
	}
	if readFile(t, filepath.Join(dir, "7.tar")) != "7" || readFile(t, filepath.Join(dir, "notes.txt")) != "keep" {
		t.Fatal("rm all dropped a busy project's cache or a foreign file")
	}
}

// A copy that does not load is deleted, so the next builder does not retry it;
// the builder starts empty.
func TestBuilderCacheFailedRestoreDropsCopy(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	dir := stubBuilderCache(t, nil, nil,
		func(context.Context, config.Config, *vm.Instance, io.Reader) error {
			return errors.New("tar: unexpected EOF")
		})
	writeFile(t, filepath.Join(dir, "7.tar"), "broken", time.Hour)
	d, _ := newTestDaemon(t)
	if d.restoreBuilderCache(context.Background(), d.cfg, "7", &vm.Instance{ID: "bld-7"}) {
		t.Fatal("reported a failed restore as done")
	}
	if _, err := os.Stat(filepath.Join(dir, "7.tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("broken copy kept")
	}
	if n := cacheCount(d, "restore", "failed"); n != 1 {
		t.Fatalf("restore failed = %v", n)
	}
}

// makeRoomForCache drops the least recently used copies of other projects
// until the new one fits the budget and leaves 10% of the disk free.
func TestMakeRoomForCache(t *testing.T) {
	dir := stubBuilderCache(t, nil, nil, nil)
	writeFile(t, filepath.Join(dir, "1.tar"), strings.Repeat("a", 40), 3*time.Hour) // oldest
	writeFile(t, filepath.Join(dir, "2.tar"), strings.Repeat("b", 40), 2*time.Hour)
	writeFile(t, filepath.Join(dir, "3.tar"), strings.Repeat("c", 40), time.Hour)
	writeFile(t, filepath.Join(dir, "9.tar"), strings.Repeat("o", 40), 5*time.Hour) // own copy: replaced, not dropped
	writeFile(t, filepath.Join(dir, "4.tar.tmp-1"), "stale", 2*builderCacheTimeout)
	d, _ := newTestDaemon(t)
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

	// Budget 120 with 120 used by others: a 30-byte cache needs the oldest gone.
	if err := d.makeRoomForCache("9", 30, 120); err != nil {
		t.Fatal(err)
	}
	if exists("1.tar") || !exists("2.tar") || !exists("3.tar") || !exists("9.tar") || exists("4.tar.tmp-1") {
		t.Fatalf("after budget pass: %v", listDir(dir))
	}

	// Disk nearly full: 100 of 1000 bytes must stay free and 100 are, so a
	// 30-byte cache needs 2.tar (40 bytes) gone too, but not 3.tar.
	diskSpace = func(string) (uint64, uint64, error) { return 100, 1000, nil }
	if err := d.makeRoomForCache("9", 30, 1<<30); err != nil {
		t.Fatal(err)
	}
	if exists("2.tar") || !exists("3.tar") {
		t.Fatalf("after disk pass: %v", listDir(dir))
	}

	// Nothing left to drop and still no room: refuse.
	diskSpace = func(string) (uint64, uint64, error) { return 10, 1000, nil }
	if err := d.makeRoomForCache("9", 500, 1<<30); err == nil {
		t.Fatal("no error without room")
	}
}

func deleted(uids []string, uid string) bool {
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

// busyJob makes a running job build on project's builder.
func busyJob(t *testing.T, project string) {
	t.Helper()
	dir := t.TempDir()
	old := jobStateGlob
	jobStateGlob = filepath.Join(dir, "*.json")
	t.Cleanup(func() { jobStateGlob = old })
	if err := vm.SaveJobState(filepath.Join(dir, "job-1.json"), &vm.JobState{BuilderProject: project}); err != nil {
		t.Fatal(err)
	}
}

func listDir(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The next builder of a project waits for the copy: removeBuilderLocked marks
// the project while saving and clears the mark only after the VM is deleted.
func TestBuilderCacheSaveMarksProjectUntilVMDeleted(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	release := make(chan struct{})
	entered := make(chan struct{})
	stubBuilderCache(t, sizeOf(1),
		func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
			close(entered)
			<-release
			_, err := io.WriteString(w, "x")
			return err
		}, nil)
	d, srv := newTestDaemon(t)
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	<-entered
	d.mu.Lock()
	saving := d.saving["7"]
	d.mu.Unlock()
	if saving == nil {
		t.Fatal("project not marked while its cache is saved")
	}
	if deleted(srv.Deleted(), "uid-7") {
		t.Fatal("VM deleted before its cache was copied")
	}
	close(release)
	select {
	case <-saving:
	case <-time.After(5 * time.Second):
		t.Fatal("save never finished")
	}
	if !deleted(srv.Deleted(), "uid-7") {
		t.Fatal("mark cleared before the VM was deleted")
	}
}
