package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	types "github.com/liquidmetal-dev/flintlock/api/types"

	dto "github.com/prometheus/client_model/go"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
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
	oldWipe := builderCacheWipe
	builderCacheWipe = func(context.Context, config.Config, *vm.Instance) error { return nil }
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
	oldSlack := builderCacheSlack
	builderCacheSlack = 0
	t.Cleanup(func() {
		builderCacheSlack = oldSlack
		builderCacheDir, builderCacheSize, builderCacheSave, builderCacheLoad, diskSpace = oldDir, oldSize, oldSave, oldLoad, oldDisk
		builderCacheWipe = oldWipe
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

// saved is a saved cache file of the default builder image holding data.
func saved(data string) string { return cacheHeader(config.Default().Builder.Image) + data }

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

	if got := readFile(t, filepath.Join(dir, "7.tar")); got != saved("data-7") {
		t.Fatalf("saved cache = %q", got)
	}
	if n := cacheCount(d, "save", "ok"); n != 1 {
		t.Fatalf("save ok = %v", n)
	}
	if d.saving["7"] != nil {
		t.Fatal("save still marked in progress")
	}
	if ok, err := d.restoreBuilderCache(context.Background(), d.cfg, "7", &vm.Instance{ID: "bld-7"}); !ok || err != nil || loaded != "data-7" {
		t.Fatalf("restore = %v, %v, loaded %q", ok, err, loaded)
	}
	if n := cacheCount(d, "restore", "ok"); n != 1 {
		t.Fatalf("restore ok = %v", n)
	}
	// A project without a saved cache starts empty, without an error.
	if ok, err := d.restoreBuilderCache(context.Background(), d.cfg, "8", &vm.Instance{ID: "bld-8"}); ok || err != nil {
		t.Fatalf("restore of a cache that was never saved = %v, %v", ok, err)
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
			return fmt.Errorf("tar: unexpected EOF: %w", exec.Command("sh", "-c", "exit 1").Run())
		})
	writeFile(t, filepath.Join(dir, "7.tar"), saved("broken"), time.Hour)
	d, _ := newTestDaemon(t)
	if ok, err := d.restoreBuilderCache(context.Background(), d.cfg, "7", &vm.Instance{ID: "bld-7"}); ok || err != nil {
		t.Fatalf("restore of a refused copy = %v, %v; want false, nil (start empty)", ok, err)
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
	if err := d.makeRoomForCache("9", 30, 120, 0); err != nil {
		t.Fatal(err)
	}
	if exists("1.tar") || !exists("2.tar") || !exists("3.tar") || !exists("9.tar") || exists("4.tar.tmp-1") {
		t.Fatalf("after budget pass: %v", listDir(dir))
	}

	// Disk nearly full: 100 of 1000 bytes must stay free and 100 are, so a
	// 30-byte cache needs 2.tar (40 bytes) gone too, but not 3.tar.
	diskSpace = func(string) (uint64, uint64, error) { return 100, 1000, nil }
	if err := d.makeRoomForCache("9", 30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if exists("2.tar") || !exists("3.tar") {
		t.Fatalf("after disk pass: %v", listDir(dir))
	}

	// Nothing left to drop and still no room: refuse.
	diskSpace = func(string) (uint64, uint64, error) { return 10, 1000, nil }
	if err := d.makeRoomForCache("9", 500, 1<<30, 0); err == nil {
		t.Fatal("no error without room")
	}

	// Bytes reserved by saves still being written count against the budget:
	// 3.tar (40) + 50 reserved + 30 new > 100, and nothing is left to drop.
	diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	if err := d.makeRoomForCache("9", 30, 100, 50); err != nil {
		t.Fatal(err) // dropping 3.tar makes room
	}
	if exists("3.tar") {
		t.Fatal("reserved bytes were not counted")
	}
	if err := d.makeRoomForCache("9", 60, 100, 50); err == nil {
		t.Fatal("reservation ignored: 50 reserved + 60 new fit a 100-byte budget")
	}

	// What a save in progress has written so far is in its temp file, no
	// longer in its reservation: it still counts against the budget.
	writeFile(t, filepath.Join(dir, "5.tar.tmp-2"), strings.Repeat("w", 60), 0)
	if err := d.makeRoomForCache("9", 50, 100, 0); err == nil {
		t.Fatal("written bytes of a save in progress ignored: 60 written + 50 new fit a 100-byte budget")
	}
	if !exists("5.tar.tmp-2") {
		t.Fatal("temp file of a save in progress removed")
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
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // before bg.Wait: a failing test must not hang on the blocked save
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	<-entered
	d.mu.Lock()
	saving := d.saving["7"]
	d.mu.Unlock()
	if saving == nil || saving.uid != "uid-7" {
		t.Fatalf("project not marked while its cache is saved: %+v", saving)
	}
	if deleted(srv.Deleted(), "uid-7") {
		t.Fatal("VM deleted before its cache was copied")
	}
	free()
	select {
	case <-saving.done:
	case <-time.After(5 * time.Second):
		t.Fatal("save never finished")
	}
	if !deleted(srv.Deleted(), "uid-7") {
		t.Fatal("mark cleared before the VM was deleted")
	}
}

// Review finding 1: the VM of a deleted builder is no longer in d.builders
// while its cache is copied out; reconcile must not delete it as an orphan.
func TestReconcileKeepsBuilderWhileItsCacheIsSaved(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	release, entered := make(chan struct{}), make(chan struct{})
	stubBuilderCache(t, sizeOf(1), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
		close(entered)
		<-release
		_, err := io.WriteString(w, "x")
		return err
	}, nil)
	d, srv := newTestDaemon(t)
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // before bg.Wait: a failing test must not hang on the blocked save
	uid := "uid-7"
	srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: "bld-7", Uid: &uid},
		Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}})
	d.firstSee[uid] = time.Now().Add(-time.Hour) // long past the orphan age
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	<-entered
	d.reconcile(context.Background(), false)
	time.Sleep(200 * time.Millisecond) // a delete reconcile started would land now
	if deleted(srv.Deleted(), uid) {
		t.Fatal("reconcile deleted the builder VM while its cache was being copied")
	}
	free()
	srv.WaitDeleted(t, uid, 2*time.Second)
}

// Review finding 2: saves run one at a time, so saves side by side cannot all
// pass the budget check against the same files and free space.
func TestConcurrentSavesStayWithinBudget(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	const size = 40
	dir := stubBuilderCache(t, sizeOf(size), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
		time.Sleep(20 * time.Millisecond) // overlap the saves
		_, err := io.WriteString(w, strings.Repeat("x", size))
		return err
	}, nil)
	// Room for two saved caches (header and archive), not three.
	limit := 2*(size+int64(len(saved("")))) + 10
	oldLimit := builderCacheLimit
	builderCacheLimit = func(config.Config) int64 { return limit }
	t.Cleanup(func() { builderCacheLimit = oldLimit })
	d, _ := newTestDaemon(t)
	d.builders = map[string]*builder{}
	for _, p := range []string{"1", "2", "3", "4"} {
		d.builders[p] = readyBuilder(p, 20000+len(d.builders)+1, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))
	}
	d.expireBuilders() // all four idle: four saves at once
	d.bg.Wait()
	var total int64
	for _, name := range listDir(dir) {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += info.Size()
		}
	}
	if total > limit {
		t.Fatalf("saved %d bytes under a %d-byte budget: %v", total, limit, listDir(dir))
	}
	ok, skipped := cacheCount(d, "save", "ok"), cacheCount(d, "save", "skipped")
	if ok < 2 || ok+skipped != 4 {
		t.Fatalf("save ok = %v, skipped = %v; want at least 2 saved, the rest skipped", ok, skipped)
	}
}

// Review finding 3: `builder rm` while the project's cache is still being
// copied wins; the save does not bring the cache back.
func TestRemoveBuildersDuringSave(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	release, entered := make(chan struct{}), make(chan struct{})
	dir := stubBuilderCache(t, sizeOf(1), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
		close(entered)
		<-release
		_, err := io.WriteString(w, "x")
		return err
	}, nil)
	d, srv := newTestDaemon(t)
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // before bg.Wait: a failing test must not hang on the blocked save
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	<-entered
	d.RemoveBuilders("7", false)
	free()
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()
	if names := listDir(dir); len(names) != 0 {
		t.Fatalf("cache came back after builder rm: %v", names)
	}
	if n := cacheCount(d, "save", "skipped"); n != 1 {
		t.Fatalf("save skipped = %v", n)
	}
	if len(d.cacheDropped) != 0 {
		t.Fatalf("drop marks left behind: %v", d.cacheDropped)
	}
}

func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatal("no exit error")
	}
	return err
}

// Review findings 4 and 5: a dropped SSH connection (exit 255) keeps the
// saved copy; any failed load removes the volume from the host side; if that
// fails too, the builder must not be used.
func TestRestoreFailures(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var loadErr, wipeErr error
	wipes := 0
	dir := stubBuilderCache(t, nil, nil, func(context.Context, config.Config, *vm.Instance, io.Reader) error { return loadErr })
	builderCacheWipe = func(context.Context, config.Config, *vm.Instance) error { wipes++; return wipeErr }
	d, _ := newTestDaemon(t)
	file := filepath.Join(dir, "7.tar")
	inst := &vm.Instance{ID: "bld-7"}

	writeFile(t, file, saved("copy"), time.Hour)
	loadErr = fmt.Errorf("stream: %w", exitErr(t, 255))
	if ok, err := d.restoreBuilderCache(context.Background(), d.cfg, "7", inst); ok || err == nil {
		t.Fatalf("ssh drop: %v, %v; want an error, so the boot fails and the next one retries", ok, err)
	}
	if readFile(t, file) != saved("copy") {
		t.Fatalf("ssh drop: copy %q, want it kept", readFile(t, file))
	}

	// tar or the marker check refused it: deleted, volume wiped, start empty.
	loadErr = fmt.Errorf("stream: %w", exitErr(t, 1))
	wipeErr = errors.New("ssh: no route")
	if _, err := d.restoreBuilderCache(context.Background(), d.cfg, "7", inst); err == nil {
		t.Fatal("volume could not be wiped, but the builder was declared usable")
	}
	writeFile(t, file, saved("copy"), time.Hour)
	wipeErr = nil
	if ok, err := d.restoreBuilderCache(context.Background(), d.cfg, "7", inst); ok || err != nil {
		t.Fatalf("refused copy: %v, %v; want false, nil (start empty)", ok, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) || wipes != 2 {
		t.Fatalf("refused copy kept or volume not wiped (wipes %d)", wipes)
	}
}

// bootStub records the builder VMs bootBuilder booted through stubBoot.
type bootStub struct {
	mu  sync.Mutex
	ids []string
}

func (s *bootStub) Load() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int32(len(s.ids))
}

// last is the id of the last VM booted; its uid is "uid-" + id.
func (s *bootStub) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ids) == 0 {
		return ""
	}
	return s.ids[len(s.ids)-1]
}

// stubBoot runs bootBuilder without VMs: boot, admission and buildkitd setup
// are replaced; setup returns setupErr.
func stubBoot(t *testing.T, setupErr error) *bootStub {
	t.Helper()
	boots := &bootStub{}
	oldBoot, oldFits, oldSetup := builderVMBoot, builderFits, builderSetup
	builderVMBoot = func(_ context.Context, _ config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
		boots.mu.Lock()
		boots.ids = append(boots.ids, id)
		boots.mu.Unlock()
		return &vm.Instance{ID: id, UID: "uid-" + id, IP: "10.200.0.77"}, nil
	}
	builderFits = func(context.Context, config.Config, *flintlock.Client, int) (bool, string, error) {
		return true, "", nil
	}
	builderSetup = func(context.Context, config.Config, *vm.Instance, *builderCreds) error { return setupErr }
	t.Cleanup(func() { builderVMBoot, builderFits, builderSetup = oldBoot, oldFits, oldSetup })
	return boots
}

// The next builder of a project waits for its previous builder's copy, then
// boots, and reconcile knows its VM from the moment it exists.
func TestBootBuilderWaitsForSaveAndRestores(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var loaded, uidDuringRestore string
	var dp *Daemon
	dir := stubBuilderCache(t, nil, nil, func(_ context.Context, _ config.Config, _ *vm.Instance, r io.Reader) error {
		dp.mu.Lock()
		uidDuringRestore = dp.builders["7"].Instance.UID // what reconcile sees now
		dp.mu.Unlock()
		b, err := io.ReadAll(r)
		loaded = string(b)
		return err
	})
	boots := stubBoot(t, nil)
	d, _ := newTestDaemon(t)
	dp = d
	d.Builder("7", true) // entry with credentials; builderBoot is stubbed
	save := &cacheSave{done: make(chan struct{}), uid: "uid-old", start: time.Now()}
	d.saving["7"] = save
	done := make(chan struct{})
	go func() { d.bootBuilder(context.Background(), d.cfg, "7"); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if boots.Load() != 0 {
		t.Fatal("booted before the previous builder's cache was saved")
	}
	writeFile(t, filepath.Join(dir, "7.tar"), saved("warm"), time.Minute)
	d.mu.Lock()
	delete(d.saving, "7")
	d.mu.Unlock()
	close(save.done)
	<-done
	d.mu.Lock()
	b := d.builders["7"]
	d.mu.Unlock()
	uid := "uid-" + boots.last()
	if b == nil || !b.ready || b.Instance.UID != uid || loaded != "warm" {
		t.Fatalf("builder %+v, loaded %q", b, loaded)
	}
	if uidDuringRestore != uid {
		t.Fatalf("while its cache loaded, reconcile saw builder VM %q (would delete it as an orphan)", uidDuringRestore)
	}
}

// A saved cache is dropped only when buildkitd started and did not come up on
// it, not when setup failed for another reason (image pull, SSH).
func TestBootBuilderDropsCacheOnlyWhenBuildkitRefusesIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setupErr error
		keep     bool
	}{
		{"buildkitd down on the restored state", fmt.Errorf("starting buildkitd: %w", errBuildkitDown), false},
		{"image pull failed", errors.New("starting buildkitd: exit status 125: pull access denied"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBuilders(t, func(string) bool { return true })
			dir := stubBuilderCache(t, nil, nil, func(_ context.Context, _ config.Config, _ *vm.Instance, r io.Reader) error {
				_, err := io.ReadAll(r)
				return err
			})
			boots := stubBoot(t, tc.setupErr)
			d, srv := newTestDaemon(t)
			writeFile(t, filepath.Join(dir, "7.tar"), saved("warm"), time.Minute)
			d.Builder("7", true)
			d.bootBuilder(context.Background(), d.cfg, "7")
			srv.WaitDeleted(t, "uid-"+boots.last(), 2*time.Second)
			_, err := os.Stat(filepath.Join(dir, "7.tar"))
			if kept := err == nil; kept != tc.keep {
				t.Fatalf("cache kept = %v, want %v", kept, tc.keep)
			}
			if d.builders["7"] != nil {
				t.Fatal("failed builder kept")
			}
		})
	}
}

// Review finding 7: a project id that is not a number never reaches a path.
func TestRemoveBuildersRejectsBadProject(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	dir := stubBuilderCache(t, nil, nil, nil)
	writeFile(t, filepath.Join(dir, "x.tar"), "x", time.Hour)
	d, _ := newTestDaemon(t)
	d.RemoveBuilders("../"+filepath.Base(dir)+"/x", false)
	if readFile(t, filepath.Join(dir, "x.tar")) != "x" {
		t.Fatal("path from a project id was used")
	}
}

// Review round 2, N1: one project's slow save neither holds up another
// project's save nor keeps the project's next builder waiting forever.
func TestSlowSaveDoesNotStallOthers(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	release := make(chan struct{})
	dir := stubBuilderCache(t, sizeOf(1), func(_ context.Context, _ config.Config, inst *vm.Instance, w io.Writer) error {
		if inst.ID == "bld-1" {
			<-release // project 1's copy hangs
		}
		_, err := io.WriteString(w, "x")
		return err
	}, nil)
	d, _ := newTestDaemon(t)
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	d.builders = map[string]*builder{"2": readyBuilder("2", 20002, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	deadline := time.Now().Add(2 * time.Second)
	for readFile(t, filepath.Join(dir, "2.tar")) != saved("x") {
		if time.Now().After(deadline) {
			t.Fatal("project 2's save waited for project 1's hung save")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Project 1's next builder boots after builderSaveWait, not after the save.
	old := builderSaveWait
	builderSaveWait = 50 * time.Millisecond
	t.Cleanup(func() { builderSaveWait = old })
	boots := stubBoot(t, nil)
	d.Builder("1", true)
	done := make(chan struct{})
	go func() { d.bootBuilder(context.Background(), d.cfg, "1"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("builder boot still waiting for the hung save")
	}
	if boots.Load() != 1 {
		t.Fatalf("boots = %d", boots.Load())
	}
	free()
}

// Measured on the trial host: a builder booting right after a daemon restart
// was deleted by the startup reconcile as "left by a previous run", because
// its uid is known only once Boot returns. Its id marks it as booting.
func TestStartupReconcileSparesBootingBuilder(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	stubBuilderCache(t, nil, nil, nil)
	stubBoot(t, nil)
	d, srv := newTestDaemon(t)
	uid := "uid-new"
	var booted string
	builderVMBoot = func(ctx context.Context, _ config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
		booted = id
		srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED}})
		d.reconcile(ctx, true) // the startup reconcile runs while this VM boots
		return &vm.Instance{ID: id, UID: uid, IP: "10.200.0.77"}, nil
	}
	d.Builder("7", true)
	d.bootBuilder(context.Background(), d.cfg, "7")
	time.Sleep(200 * time.Millisecond)
	if deleted(srv.Deleted(), uid) {
		t.Fatal("startup reconcile deleted the builder VM that was booting")
	}
	if booted == "" || d.bootingIDs[booted] {
		t.Fatalf("booting mark of %q left after the boot", booted)
	}
}

// blockUnlink makes unlinking a saved cache wait until the test releases it,
// and reports each file on entered.
func blockUnlink(t *testing.T) (entered chan string, release func()) {
	t.Helper()
	entered, gate := make(chan string, 16), make(chan struct{})
	old := unlinkCache
	unlinkCache = func(path string) error {
		entered <- path
		<-gate
		return os.Remove(path)
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); unlinkCache = old })
	return entered, release
}

// Unlinking a large saved cache frees its blocks, which takes a while; it
// never happens while POST /claim waits for the daemon lock.
func TestCacheUnlinkNeverBlocksClaims(t *testing.T) {
	wait := func(t *testing.T, entered chan string) string {
		t.Helper()
		select {
		case f := <-entered:
			return f
		case <-time.After(2 * time.Second):
			t.Fatal("the old copy was never unlinked on its own (renamed over or removed under the lock)")
		}
		return ""
	}
	t.Run("builder rm drops a saved cache", func(t *testing.T) {
		stubBuilders(t, func(string) bool { return true })
		dir := stubBuilderCache(t, nil, nil, nil)
		writeFile(t, filepath.Join(dir, "7.tar"), "old", time.Hour)
		writeFile(t, filepath.Join(dir, "8.tar"), "old", time.Hour)
		d, _ := newTestDaemon(t)
		entered, release := blockUnlink(t)
		done := make(chan struct{})
		go func() { d.RemoveBuilders("all", false); close(done) }()
		wait(t, entered)
		claimWithin(t, d, "unlinking a dropped cache")
		release()
		<-done
		if names := listDir(dir); len(names) != 0 {
			t.Fatalf("left behind: %v", names)
		}
	})
	t.Run("a save replaces the previous copy", func(t *testing.T) {
		stubBuilders(t, func(string) bool { return true })
		dir := stubBuilderCache(t, sizeOf(3), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
			_, err := io.WriteString(w, "new")
			return err
		}, nil)
		writeFile(t, filepath.Join(dir, "7.tar"), "old", time.Hour)
		d, _ := newTestDaemon(t)
		entered, release := blockUnlink(t)
		d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
		d.expireBuilders()
		wait(t, entered)
		claimWithin(t, d, "unlinking the replaced copy")
		release()
		d.bg.Wait()
		if got := readFile(t, filepath.Join(dir, "7.tar")); !strings.HasSuffix(got, "new") {
			t.Fatalf("saved cache = %q", got)
		}
		if names := listDir(dir); len(names) != 1 {
			t.Fatalf("left behind: %v", names)
		}
	})
}

// reservedBytes is what saves in progress may still write (cacheReserved).
func reservedBytes(d *Daemon) int64 {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	return d.cacheReserved
}

// The guest's size is the only figure a save reserves disk for, and the host
// cannot check it: a copy that grows past its reservation is cut off and
// removed, and a negative size is refused, with the previous copy kept.
func TestBuilderCacheSaveBoundedByReservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int64
		write int
	}{
		{"archive larger than reserved", 10, 64 << 10},
		{"negative size", -5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBuilders(t, func(string) bool { return true })
			var written atomic.Int64
			dir := stubBuilderCache(t, sizeOf(tc.size), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
				chunk := []byte(strings.Repeat("x", 1024))
				for i := 0; i < tc.write/len(chunk)+1; i++ {
					n, err := w.Write(chunk)
					written.Add(int64(n))
					if err != nil {
						return err
					}
				}
				return nil
			}, nil)
			writeFile(t, filepath.Join(dir, "7.tar"), "previous", time.Hour)
			d, srv := newTestDaemon(t)
			d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
			d.expireBuilders()
			srv.WaitDeleted(t, "uid-7", 2*time.Second)
			d.bg.Wait()
			if got := readFile(t, filepath.Join(dir, "7.tar")); got != "previous" {
				t.Fatalf("saved cache = %d bytes, want the previous copy", len(got))
			}
			if names := listDir(dir); len(names) != 1 {
				t.Fatalf("left behind: %v", names)
			}
			if n, bound := written.Load(), max(tc.size, 0)+builderCacheSlack; n > bound {
				t.Fatalf("%d bytes reached the host for a %d-byte reservation", n, bound)
			}
			if n := cacheCount(d, "save", "failed"); n != 1 {
				t.Fatalf("save failed = %v", n)
			}
			if r := reservedBytes(d); r != 0 {
				t.Fatalf("%d bytes still reserved", r)
			}
		})
	}
}

// A save in progress reserves its size once: the bytes it has written are on
// the disk (statfs no longer counts them as free) and are no longer reserved,
// so a second save is not refused for want of room the first one counts twice.
func TestParallelSavesCountWrittenBytesOnce(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	wrote, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	dir := stubBuilderCache(t,
		func(_ context.Context, _ config.Config, inst *vm.Instance) (int64, error) {
			if inst.ID == "bld-1" {
				return 500, nil
			}
			return 300, nil
		},
		func(_ context.Context, _ config.Config, inst *vm.Instance, w io.Writer) error {
			if inst.ID != "bld-1" {
				_, err := io.WriteString(w, strings.Repeat("b", 300))
				return err
			}
			if _, err := io.WriteString(w, strings.Repeat("a", 500)); err != nil {
				return err
			}
			close(wrote)
			<-release // still streaming (e.g. waiting for tar to exit)
			return nil
		}, nil)
	// A disk whose free space is what the files in the cache directory leave,
	// and that must keep 10% free: after the first save wrote its 500 bytes
	// (and header h), 300+h more fit, but not if the 500+h were counted twice.
	h := uint64(len(saved("")))
	disk := 1200 + 2*h
	diskSpace = func(string) (uint64, uint64, error) {
		used := uint64(0)
		for _, n := range listDir(dir) {
			if info, err := os.Stat(filepath.Join(dir, n)); err == nil {
				used += uint64(info.Size())
			}
		}
		return disk - used, disk, nil
	}
	d, _ := newTestDaemon(t)
	t.Cleanup(free) // before bg.Wait: a failing test must not hang on the blocked save
	spec := builderSpec(d.cfg)
	d.builders = map[string]*builder{"1": readyBuilder("1", 20001, d.cfg.Builder.IdleTTL+time.Hour, spec)}
	d.expireBuilders()
	<-wrote // 500+h bytes on disk
	d.mu.Lock()
	d.builders = map[string]*builder{"2": readyBuilder("2", 20002, d.cfg.Builder.IdleTTL+time.Hour, spec)}
	d.mu.Unlock()
	d.expireBuilders()
	deadline := time.Now().Add(2 * time.Second)
	for cacheCount(d, "save", "ok")+cacheCount(d, "save", "skipped") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("second save never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := cacheCount(d, "save", "ok"); n != 1 {
		t.Fatalf("second save: ok %v, skipped %v; it fits the free space left", n, cacheCount(d, "save", "skipped"))
	}
	free()
	d.bg.Wait()
	if cacheCount(d, "save", "ok") != 2 || reservedBytes(d) != 0 {
		t.Fatalf("save ok = %v, reserved = %d", cacheCount(d, "save", "ok"), reservedBytes(d))
	}
}

// The size a save reserves is GNU tar's own count of the archive, from its
// --totals line; anything else is an error, not a size.
func TestParseTarTotal(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want int64
		ok   bool
	}{
		{"Total bytes written: 18831360 (18MiB, 1.9GiB/s)\n", 18831360, true},
		{"tar: ./a/sock: socket ignored\nTotal bytes written: 10240 (10KiB, 20MiB/s)\n", 10240, true},
		{"", 0, false},
		{"Total bytes written: -5\n", 0, false},
		{"Total bytes written: 99999999999999999999999\n", 0, false},
		{"Error response from daemon: No such container: buildkitd\n", 0, false},
	} {
		got, err := parseTarTotal(tc.out)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseTarTotal(%q) = %d, %v; want %d, ok %v", tc.out, got, err, tc.want, tc.ok)
		}
	}
	// The size is taken the way the save archives: buildkitd stopped, the same
	// members, and tar's messages in English.
	for _, want := range []string{"docker stop -t 30 buildkitd", "LC_ALL=C tar", "--totals -cf /dev/null", `-C "$mp" . -C "$m" ` + builderMarker} {
		if !strings.Contains(builderSizeScript, want) {
			t.Errorf("size script lacks %q: %s", want, builderSizeScript)
		}
	}
}

// A saved cache goes back only into a builder running the image that wrote
// it: a new builder.image may fix a BuildKit flaw its old state carries. A
// cache of another image, or one saved before images were recorded, is
// deleted and the builder starts empty.
func TestSavedCacheIsTiedToTheBuilderImage(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	var loaded []string
	dir := stubBuilderCache(t, sizeOf(4), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
		_, err := io.WriteString(w, "warm")
		return err
	}, func(_ context.Context, _ config.Config, _ *vm.Instance, r io.Reader) error {
		b, err := io.ReadAll(r)
		loaded = append(loaded, string(b))
		return err
	})
	d, srv := newTestDaemon(t)
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, d.cfg.Builder.IdleTTL+time.Hour, builderSpec(d.cfg))}
	d.expireBuilders()
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()
	file := filepath.Join(dir, "7.tar")
	inst := &vm.Instance{ID: "bld-7-00000001"}

	same := d.cfg
	if ok, err := d.restoreBuilderCache(context.Background(), same, "7", inst); !ok || err != nil || strings.Join(loaded, ",") != "warm" {
		t.Fatalf("same image: restored %v, %v, loaded %q (want exactly the archive)", ok, err, loaded)
	}

	bumped := d.cfg
	bumped.Builder.Image = "moby/buildkit:v0.99.0"
	if ok, err := d.restoreBuilderCache(context.Background(), bumped, "7", inst); ok || err != nil {
		t.Fatalf("new image: restored %v, %v; want an empty start", ok, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) || len(loaded) != 1 {
		t.Fatalf("cache of the old image kept (%v) or loaded (%q)", err, loaded)
	}

	writeFile(t, file, "tar data saved before images were recorded", time.Hour)
	if ok, err := d.restoreBuilderCache(context.Background(), same, "7", inst); ok || err != nil {
		t.Fatalf("cache without an image: restored %v, %v; want an empty start", ok, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) || len(loaded) != 1 {
		t.Fatalf("cache without an image kept (%v) or loaded (%q)", err, loaded)
	}
	if n := cacheCount(d, "restore", "stale"); n != 2 {
		t.Fatalf("restore stale = %v, want 2", n)
	}
}

// A builder removed because builder.image changed is not copied out at all:
// no builder of the new image would take its cache.
func TestBuilderOfAnOldImageIsNotSaved(t *testing.T) {
	stubBuilders(t, func(string) bool { return true })
	saves := 0
	dir := stubBuilderCache(t, sizeOf(1), func(_ context.Context, _ config.Config, _ *vm.Instance, w io.Writer) error {
		saves++
		_, err := io.WriteString(w, "x")
		return err
	}, nil)
	d, srv := newTestDaemon(t)
	old := d.cfg
	old.Builder.Image = "moby/buildkit:v0.1.0"
	d.builders = map[string]*builder{"7": readyBuilder("7", 20001, time.Hour, builderSpec(old))}
	d.expireBuilders() // config changed, idle for an hour
	srv.WaitDeleted(t, "uid-7", 2*time.Second)
	d.bg.Wait()
	if saves != 0 || len(listDir(dir)) != 0 {
		t.Fatalf("saved %d times, files %v", saves, listDir(dir))
	}
}

func TestSpecImage(t *testing.T) {
	cfg := config.Default()
	cfg.Builder.Image = "registry.example:5000/buildkit:v1"
	if got := specImage(builderSpec(cfg)); got != cfg.Builder.Image {
		t.Fatalf("specImage = %q", got)
	}
	if got := specImage(legacyBuilderSpec(cfg)); got != "" {
		t.Fatalf("legacy spec: %q, want unknown", got)
	}
}
