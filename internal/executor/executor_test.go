package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

func TestIsUserStage(t *testing.T) {
	for stage, want := range map[string]bool{
		"build_script": true, "step_script": true, "after_script": true,
		"prepare_script": false, "get_sources": false, "upload_artifacts_on_success": false,
	} {
		if isUserStage(stage) != want {
			t.Errorf("%s: want %v", stage, want)
		}
	}
}

func TestExitCodesFromEnvironment(t *testing.T) {
	t.Setenv("BUILD_FAILURE_EXIT_CODE", "17")
	t.Setenv("SYSTEM_FAILURE_EXIT_CODE", "18")
	var e *ExitError
	if !errors.As(buildFailure(errors.New("x")), &e) || e.Code != 17 {
		t.Fatalf("build failure code: %+v", e)
	}
	if !errors.As(systemFailure(errors.New("x")), &e) || e.Code != 18 {
		t.Fatalf("system failure code: %+v", e)
	}
}

func TestStagesRequireJobID(t *testing.T) {
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "")
	var e *ExitError
	if err := Run(context.Background(), config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) || e.Code != 2 {
		t.Fatalf("want system failure, got %v", err)
	}
}

func TestRunWithoutPrepareIsSystemFailure(t *testing.T) {
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "does-not-exist-123")
	var e *ExitError
	if err := Run(context.Background(), config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) || e.Code != 2 {
		t.Fatalf("want system failure, got %v", err)
	}
}

// writePayload points JOB_RESPONSE_FILE at a job payload like gitlab-runner writes.
func writePayload(t *testing.T, payload string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOB_RESPONSE_FILE", path)
}

func TestJobIdentityComesFromThePayload(t *testing.T) {
	writePayload(t, `{"id":74196,"token":"x","job_info":{"project_id":111,"name":"build"}}`)
	// A job can set these variables to someone else's ids; they must not matter.
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "75226")
	t.Setenv("CUSTOM_ENV_CI_PROJECT_ID", "28")
	j, err := currentJob()
	if err != nil || j.ID != "job-74196" || j.Project != "111" {
		t.Fatalf("currentJob = %+v, %v", j, err)
	}
}

func TestJobIdentityFailures(t *testing.T) {
	for name, payload := range map[string]string{
		"no id":        `{"job_info":{"project_id":1}}`,
		"negative id":  `{"id":-3}`,
		"string id":    `{"id":"../../etc/x"}`,
		"not json":     `id=5`,
		"empty object": `{}`,
	} {
		writePayload(t, payload)
		if j, err := currentJob(); err == nil {
			t.Errorf("%s: accepted as %+v", name, j)
		}
	}
	writePayload(t, `{"id":5}`) // no project: builder is skipped, the job still runs
	if j, err := currentJob(); err != nil || j.ID != "job-5" || j.Project != "" {
		t.Errorf("payload without project: %+v %v", j, err)
	}
	t.Setenv("JOB_RESPONSE_FILE", "")
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "5")
	if _, err := jobID(); err == nil {
		t.Error("CUSTOM_ENV_CI_JOB_ID must not be used without JOB_RESPONSE_FILE")
	}
}

// The VM's project and pipeline labels come from the payload too: a job sets
// CI_PROJECT_PATH and CI_PIPELINE_ID as easily as it sets CI_JOB_ID.
func TestJobLabelsComeFromThePayload(t *testing.T) {
	t.Setenv("CUSTOM_ENV_CI_PROJECT_PATH", "other-group/other-project")
	t.Setenv("CUSTOM_ENV_CI_PIPELINE_ID", "999")
	for _, c := range []struct {
		name, payload     string
		project, pipeline string // "" means no label
	}{
		{"payload has both", `{"id":7,"job_info":{"project_id":111,"project_full_path":"grp/sub/app","pipeline_id":4242}}`, "grp.sub.app", "4242"},
		{"payload without them", `{"id":7,"job_info":{"project_id":111}}`, "", ""},
		{"unreadable payload", `id=7`, "", ""},
		{"labels of another type", `{"id":7,"job_info":{"project_id":111,"project_full_path":"grp/app","pipeline_id":"x"}}`, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			writePayload(t, c.payload)
			got := jobLabels("job-7")
			if got[daemon.LabelRole] != "job" || got[daemon.LabelJob] != "job-7" {
				t.Fatalf("role/job labels %v", got)
			}
			for key, want := range map[string]string{"firerunner/project": c.project, "firerunner/pipeline": c.pipeline} {
				if v, ok := got[key]; v != want || ok != (want != "") {
					t.Errorf("%s = %q (set %v), want %q", key, v, ok, want)
				}
			}
		})
	}
	// A label field of another type drops the labels, never the job.
	writePayload(t, `{"id":7,"job_info":{"project_id":111,"pipeline_id":"x"}}`)
	if j, err := currentJob(); err != nil || j.ID != "job-7" {
		t.Fatalf("currentJob = %+v, %v", j, err)
	}
}

func TestLeadingDashImageIsRejected(t *testing.T) {
	writePayload(t, `{"id":77}`)
	t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "--privileged")
	// No state file exists, so a system failure is expected before the image check;
	// the image check itself is covered by ContainerCommand never seeing it.
	var e *ExitError
	if err := Run(context.Background(), config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) {
		t.Fatalf("want ExitError, got %v", err)
	}
}

func TestParseServicesAndAliases(t *testing.T) {
	raw := `[{"name":"postgres:16","alias":"","entrypoint":null,"command":null,"variables":{"POSTGRES_PASSWORD":"x"}},
	         {"name":"registry.example.com:5000/grp/redis:7@sha256:abc","alias":"cache, kv","entrypoint":["/bin/sh","-c"],"command":["redis-server"]}]`
	s, err := ParseServices(raw)
	if err != nil || len(s) != 2 {
		t.Fatalf("parse: %v %v", s, err)
	}
	if got := s[0].Aliases(); len(got) != 1 || got[0] != "postgres" {
		t.Fatalf("postgres aliases %v", got)
	}
	if got := s[1].Aliases(); len(got) != 2 || got[0] != "cache" || got[1] != "kv" {
		t.Fatalf("explicit aliases %v", got)
	}
	s[1].Alias = ""
	if got := s[1].Aliases(); len(got) != 2 || got[0] != "registry.example.com__grp__redis" || got[1] != "registry.example.com-grp-redis" {
		t.Fatalf("default aliases %v", got)
	}
	if none, err := ParseServices(""); err != nil || none != nil {
		t.Fatal("empty CI_JOB_SERVICES must mean no services")
	}
	if _, err := ParseServices(`[{"name":"--privileged"}]`); err == nil {
		t.Fatal("option-looking service image accepted")
	}
}

func TestServicesScript(t *testing.T) {
	sc := ServicesScript([]Service{
		{Name: "postgres:16", Variables: map[string]string{"POSTGRES_PASSWORD": "p'w"}},
		{Name: "redis:7", Entrypoint: []string{"/bin/sh", "-c"}, Command: []string{"redis-server --save ''"}},
	}, "")
	for _, want := range []string{
		"docker network create firerunner-job",
		"--network-alias 'postgres'",
		`-e 'POSTGRES_PASSWORD=p'\''w'`,
		"--entrypoint '/bin/sh' 'redis:7' '-c' 'redis-server --save '\\'''\\'''",
		">>/etc/hosts",
	} {
		if !strings.Contains(sc, want) {
			t.Errorf("script lacks %q:\n%s", want, sc)
		}
	}
	if !strings.Contains(ContainerCommand("alpine", ServiceNetwork), "--network 'firerunner-job'") || !strings.Contains(ContainerCommand("alpine", ""), "--network 'host'") {
		t.Fatal("container network")
	}
}

func TestBuilderScript(t *testing.T) {
	s := BuilderScript(&daemon.BuilderInfo{State: daemon.BuilderReady, Port: 20003,
		CA: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n", Cert: "CERT", Key: "KEY"})
	for _, want := range []string{
		"umask 077",
		"cat > /etc/firerunner-buildkit/key.pem <<'FIRERUNNER_EOF'\nKEY\nFIRERUNNER_EOF",
		"docker buildx create --name firerunner --driver remote",
		"servername=builder,default-load=true",
		`"tcp://$gw:20003"`,
		"cat > /usr/local/bin/docker <<'FIRERUNNER_EOF'",
		"end=$((SECONDS + 150))",
		"exec /usr/bin/docker \"$@\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("builder script lacks %q:\n%s", want, s)
		}
	}
}

func TestBuildsImages(t *testing.T) {
	for script, want := range map[string]bool{
		"docker build -t app .":                         true,
		"echo x\n  docker buildx build --push .":        true,
		"docker buildx bake":                            true,
		"docker compose -f a.yml build api":             true,
		"docker-compose build":                          true,
		"docker push harbor/x:1\ndocker image prune -f": false,
		"ssh host 'docker pull x && docker run -d x'":   false,
		"docker compose up -d":                          false,
		"npm run build":                                 false,
		"dotnet build":                                  false,
	} {
		if got := BuildsImages([]byte(script)); got != want {
			t.Errorf("BuildsImages(%q) = %v, want %v", script, got, want)
		}
	}
}

// A stage that builds in a project whose builder prepare did not attach (first
// build, or the builder was idle-expired, evicted or removed): Run starts the
// builder after reading the script to spot the build, and must still send the
// whole script, not only the BUILDX_BUILDER line.
func TestRunSendsTheWholeScriptAfterStartingABuilder(t *testing.T) {
	cfg, _ := cleanupFixture(t)
	st := &vm.JobState{Instance: vm.Instance{ID: "job-555", UID: "u1", IP: "10.200.0.55", HostKey: "ssh-ed25519 AAAA"}}
	if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
		t.Fatal(err)
	}
	oldMux := vm.MuxDir
	vm.MuxDir = t.TempDir()
	t.Cleanup(func() { vm.MuxDir = oldMux })
	t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "")

	dir, err := os.MkdirTemp("", "fre") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.Daemon.Socket = filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", cfg.Daemon.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/builder" && r.URL.Query().Get("start") == "1" {
			_ = json.NewEncoder(w).Encode(daemon.BuilderInfo{State: daemon.BuilderBooting, Port: 20001, CA: "CA", Cert: "CERT", Key: "KEY"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	// Fake ssh: record what each session gets on stdin.
	bin, sent := t.TempDir(), filepath.Join(t.TempDir(), "sent")
	fake := "#!/bin/sh\n{ echo \"--- ssh $*\"; cat; } >> " + sent + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	body := "docker build -t app .\necho pushed\n"
	script := filepath.Join(t.TempDir(), "build_script")
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cfg, script, "build_script"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	got, _ := os.ReadFile(sent)
	if !strings.HasSuffix(string(got), "/bin/bash\nexport BUILDX_BUILDER="+BuilderName+"\n"+body) {
		t.Fatalf("the stage session did not get the whole script:\n%s", got)
	}
	if st, err := vm.LoadJobState(statePath("job-555")); err != nil || st.BuilderProject != "1" {
		t.Fatalf("job state after Run: %+v, %v; want BuilderProject 1", st, err)
	}
}

// runFixture is the cleanup fixture with job 555's VM pinned (as prepare
// leaves it), no image and ssh replaced by fakeSSH: Run talks to no real VM.
func runFixture(t *testing.T, fakeSSH string) (config.Config, string) {
	t.Helper()
	cfg, _ := cleanupFixture(t)
	st := &vm.JobState{Instance: vm.Instance{ID: "job-555", UID: "u1", IP: "10.200.0.55", HostKey: "ssh-ed25519 AAAA"}}
	if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
		t.Fatal(err)
	}
	oldMux := vm.MuxDir
	vm.MuxDir = t.TempDir()
	t.Cleanup(func() { vm.MuxDir = oldMux })
	t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n"+fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := filepath.Join(t.TempDir(), "step_script")
	if err := os.WriteFile(script, []byte("make test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, script
}

// jobResult is the result cleanup will report for job 555.
func jobResult(t *testing.T) (result, reason string) {
	t.Helper()
	st, err := vm.LoadJobState(statePath("job-555"))
	if err != nil {
		t.Fatal(err)
	}
	return finishResult(st)
}

// gitlab-runner stops a cancelled or timed-out job with SIGTERM (main turns
// it into a cancelled context). The stage's ssh must end, even one that
// ignores SIGTERM, and the job must not be reported as a success.
func TestRunStoppedByCancelIsRecordedAsCanceled(t *testing.T) {
	oldGrace := sshStopGrace
	sshStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { sshStopGrace = oldGrace })
	for name, trap := range map[string]string{
		"ssh exits on SIGTERM": "",
		"ssh ignores SIGTERM":  "trap '' TERM\n",
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			cfg, script := runFixture(t, trap+"echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nexec sleep 30\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, cfg, script, "step_script") }()

			var pid int
			for deadline := time.Now().Add(10 * time.Second); pid == 0; {
				if b, err := os.ReadFile(pidFile); err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				} else if time.Now().After(deadline) {
					t.Fatal("the stage's ssh never started")
				} else {
					time.Sleep(10 * time.Millisecond)
				}
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run still waits for the stage after the job was cancelled")
			}
			var e *ExitError
			if !errors.As(err, &e) || e.Code != 2 {
				t.Fatalf("Run = %v, want a system failure exit", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the stage's ssh (pid %d) outlived the cancelled stage: %v", pid, err)
			}
			if r, why := jobResult(t); r != daemon.ResultSystemFailure || why != "canceled" {
				t.Fatalf("recorded %s/%s, want system_failure/canceled", r, why)
			}
		})
	}
}

// ssh passes on the remote exit code, so 255 is also a script whose last
// command was a failed `ssh deploy@host`. Only a microVM that no longer
// answers makes it a lost connection (a system failure GitLab may retry).
func TestRunExit255IsLostOnlyWhenTheVMIsGone(t *testing.T) {
	oldProbe := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = oldProbe })
	for _, c := range []struct {
		name, stage, probe string
		code               int // exit code Run returns
		result, reason     string
		probed             bool
	}{
		{"script exits 255, VM answers", "step_script", "exit 0", 1, daemon.ResultScriptFailure, "", true},
		{"build_script exits 255, VM answers", "build_script", "exit 0", 1, daemon.ResultScriptFailure, "", true},
		{"VM gone", "step_script", "exit 255", 2, daemon.ResultSystemFailure, "ssh_lost", true},
		{"VM hangs", "step_script", "exec sleep 30", 2, daemon.ResultSystemFailure, "ssh_lost", true},
		{"after_script exits 255, VM answers", "after_script", "exit 0", 1, daemon.ResultSuccess, "", true},
		{"helper stage: no probe", "get_sources", "exit 0", 2, daemon.ResultSystemFailure, "ssh_lost", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			probes := filepath.Join(t.TempDir(), "probes")
			cfg, script := runFixture(t, `for last; do :; done
if [ "$last" = true ]; then
    echo probe >> `+probes+`
    `+c.probe+`
fi
cat >/dev/null
exit 255
`)
			start := time.Now()
			err := Run(context.Background(), cfg, script, c.stage)
			var e *ExitError
			if !errors.As(err, &e) || e.Code != c.code {
				t.Fatalf("Run = %v, want exit %d", err, c.code)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("Run took %s: the probe is not bounded", took)
			}
			if r, why := jobResult(t); r != c.result || why != c.reason {
				t.Fatalf("recorded %s/%s, want %s/%s", r, why, c.result, c.reason)
			}
			if _, err := os.Stat(probes); (err == nil) != c.probed {
				t.Fatalf("probed = %v, want %v", err == nil, c.probed)
			}
		})
	}
}

// gitlab-runner's SIGTERM also ends the stage's ssh with 255; a cancel that
// arrives while the VM is probed is still a cancel, not a lost VM that pages.
func TestRunCancelledWhileProbingIsCanceled(t *testing.T) {
	probes := filepath.Join(t.TempDir(), "probes")
	cfg, script := runFixture(t, `for last; do :; done
if [ "$last" = true ]; then
    echo probe >> `+probes+`
    exec sleep 30
fi
cat >/dev/null
exit 255
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(probes); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var e *ExitError
	if err := Run(ctx, cfg, script, "step_script"); !errors.As(err, &e) || e.Code != 2 {
		t.Fatalf("Run = %v, want a system failure exit", err)
	}
	if r, why := jobResult(t); r != daemon.ResultSystemFailure || why != "canceled" {
		t.Fatalf("recorded %s/%s, want system_failure/canceled", r, why)
	}
}

// A job cancelled before its stage started runs nothing in the VM.
func TestRunAfterCancelStartsNoSession(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	cfg, script := runFixture(t, "touch "+ran+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var e *ExitError
	if err := Run(ctx, cfg, script, "get_sources"); !errors.As(err, &e) || e.Code != 2 {
		t.Fatalf("Run = %v, want a system failure exit", err)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ssh ran for a cancelled job")
	}
	if r, why := jobResult(t); r != daemon.ResultSystemFailure || why != "canceled" {
		t.Fatalf("recorded %s/%s, want system_failure/canceled", r, why)
	}
}

// cleanupFixture points the executor at a fake flintlockd and a temporary
// state dir holding job 555, whose microVM has uid "u1".
func cleanupFixture(t *testing.T) (config.Config, *flintlocktest.Server) {
	t.Helper()
	srv := flintlocktest.NewServer("")
	cfg := config.Default()
	cfg.Flintlock = flintlocktest.StartUnix(t, srv)
	cfg.Daemon.Socket = filepath.Join(t.TempDir(), "none.sock") // daemon down: events are dropped
	cfg.Network.LeasesFile = filepath.Join(t.TempDir(), "leases")
	oldDir, oldBackoff, oldKnown := StateDir, deleteBackoff, vm.KnownHostsDir
	StateDir, deleteBackoff, vm.KnownHostsDir = t.TempDir(), 10*time.Millisecond, t.TempDir()
	t.Cleanup(func() { StateDir, deleteBackoff, vm.KnownHostsDir = oldDir, oldBackoff, oldKnown })
	writePayload(t, `{"id":555,"job_info":{"project_id":1}}`)
	st := &vm.JobState{Instance: vm.Instance{ID: "job-555", UID: "u1"}, Source: "cold", StartedAt: time.Now()}
	if err := vm.CreateJobState(statePath("job-555"), st); err != nil {
		t.Fatal(err)
	}
	return cfg, srv
}

func TestCleanupRetriesTransientDelete(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	srv.FailDelete(status.Error(codes.Unknown, "failed reading from content store"))
	if err := Cleanup(cfg); err != nil {
		t.Fatalf("Cleanup after one transient failure = %v", err)
	}
	if got := srv.Deleted(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("deleted %v, want [u1]", got)
	}
	if _, err := os.Stat(statePath("job-555")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file still there: %v", err)
	}
}

// Nothing retries cleanup: a kept state file made reconcile treat the dead
// job's VM as running for daemon.job_max_age.
func TestCleanupDropsStateWhenDeleteKeepsFailing(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	down := status.Error(codes.Unavailable, "down")
	srv.FailDelete(down, down, down)
	err := Cleanup(cfg)
	if err == nil || !strings.Contains(err.Error(), "job-555") {
		t.Fatalf("Cleanup = %v, want the delete error", err)
	}
	if _, err := os.Stat(statePath("job-555")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file kept after a failed delete: %v", err)
	}
	if got := srv.Deleted(); len(got) != 0 {
		t.Fatalf("deleted %v, want nothing", got)
	}
}

// pinned writes the known_hosts file vm.Forget removes, so a test can see
// that the VM's host key and DHCP lease were forgotten.
func pinned(t *testing.T, id string) string {
	t.Helper()
	path := filepath.Join(vm.KnownHostsDir, id)
	if err := os.WriteFile(path, []byte(id+" ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func forgotten(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s kept: the VM's lease and host key were not forgotten", filepath.Base(path))
	}
}

// A claimed pool VM keeps its pool-<rand> flintlock id; its lease is keyed by it.
func TestCleanupForgetsAClaimedPoolVMByItsOwnID(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	st := &vm.JobState{Instance: vm.Instance{ID: "pool-a1b2c3", UID: "u2"}, Source: "pool", StartedAt: time.Now()}
	if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
		t.Fatal(err)
	}
	key := pinned(t, "pool-a1b2c3")
	if err := Cleanup(cfg); err != nil {
		t.Fatal(err)
	}
	if got := srv.Deleted(); len(got) != 1 || got[0] != "u2" {
		t.Fatalf("deleted %v, want [u2]", got)
	}
	forgotten(t, key)
}

// Cancelled while prepare was still booting: no state file, the VM is found
// by id, and its lease must be released like any other.
func TestCleanupForgetsAVMCancelledDuringBoot(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	if err := os.Remove(statePath("job-555")); err != nil {
		t.Fatal(err)
	}
	uid := "u9"
	srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: "job-555", Uid: &uid}})
	key := pinned(t, "job-555")
	if err := Cleanup(cfg); err != nil {
		t.Fatal(err)
	}
	if got := srv.Deleted(); len(got) != 1 || got[0] != "u9" {
		t.Fatalf("deleted %v, want [u9]", got)
	}
	forgotten(t, key)
}

// A VM whose delete failed may still run: its lease and key stay for reconcile.
func TestCleanupKeepsLeaseOfAVMItCouldNotDelete(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	down := status.Error(codes.Unavailable, "down")
	srv.FailDelete(down, down, down)
	key := pinned(t, "job-555")
	_ = Cleanup(cfg)
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("lease/key of a VM that was not deleted were dropped: %v", err)
	}
}

// Prepare's own deletes (a dead pool VM, services that do not parse, a state
// file it cannot write) release the address at once instead of after 15 min.
func TestDeleteVMForgetsTheVM(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	key := pinned(t, "pool-dead01")
	deleteVM(cfg, &vm.Instance{ID: "pool-dead01", UID: "u3"})
	if got := srv.Deleted(); len(got) != 1 || got[0] != "u3" {
		t.Fatalf("deleted %v, want [u3]", got)
	}
	forgotten(t, key)
}

// Job and service containers get the proxy exceptions and CA flags quoted,
// and a job with services keeps their aliases out of the proxy in the VM's
// /etc/environment for stage scripts without image:.
func TestCorporateNetworkContainerFlags(t *testing.T) {
	cmd := ContainerCommand("node:22", ServiceNetwork, "-e", "NO_PROXY=a,b", "-v", "/x:/y:ro", "-e", "it's")
	if !strings.Contains(cmd, `'-e' 'NO_PROXY=a,b' '-v' '/x:/y:ro' '-e' 'it'\''s' 'node:22'`) {
		t.Fatalf("container flags not quoted before the image: %s", cmd)
	}
	sc := ServicesScript([]Service{{Name: "minio/minio"}}, "localhost,minio", "-e", "SSL_CERT_FILE=/etc/firerunner/ca-bundle.crt")
	if !strings.Contains(sc, "sed -i -e '/^no_proxy=/d' -e '/^NO_PROXY=/d' /etc/environment") ||
		!strings.Contains(sc, "'no_proxy=localhost,minio' 'NO_PROXY=localhost,minio' >>/etc/environment") {
		t.Fatalf("no_proxy not updated for stage scripts:\n%s", sc)
	}
	if !strings.Contains(sc, "'-e' 'SSL_CERT_FILE=/etc/firerunner/ca-bundle.crt'") {
		t.Fatalf("service containers lack the CA flags:\n%s", sc)
	}
	if plain := ServicesScript([]Service{{Name: "redis"}}, ""); strings.Contains(plain, "/etc/environment") {
		t.Fatal("no proxy, yet /etc/environment is rewritten")
	}
}

// coldBootFixture points coldBoot at a fake flintlockd and temporary host
// paths, with short waits; the returned counter counts microVM boots.
func coldBootFixture(t *testing.T, bootTimeout time.Duration) (config.Config, *flintlocktest.Server, *atomic.Int32) {
	t.Helper()
	srv := flintlocktest.NewServer("u1")
	cfg := config.Default()
	cfg.Flintlock = flintlocktest.StartUnix(t, srv)
	cfg.VM.BootTimeout = bootTimeout
	oldAdmission, oldPoll, oldBoot := vm.AdmissionFile, admissionPoll, bootVM
	vm.AdmissionFile, admissionPoll = filepath.Join(t.TempDir(), "admission.json"), 10*time.Millisecond
	var boots atomic.Int32
	bootVM = func(ctx context.Context, cfg config.Config, _ *flintlock.Client, id string, _ map[string]string) (*vm.Instance, error) {
		boots.Add(1)
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > cfg.VM.BootTimeout+flintlockCallTimeout {
			return nil, fmt.Errorf("boot of %s without a deadline (flintlock create unbounded)", id)
		}
		return &vm.Instance{ID: id, UID: "u1"}, nil
	}
	t.Cleanup(func() { vm.AdmissionFile, admissionPoll, bootVM = oldAdmission, oldPoll, oldBoot })
	return cfg, srv, &boots
}

func noPoolVM() *vm.Instance { return nil }

// flintlockd restarting (a few failed calls) costs the job a short wait for
// host memory, not the job.
func TestColdBootWaitsOutFlintlockErrors(t *testing.T) {
	cfg, srv, boots := coldBootFixture(t, 5*time.Second)
	down := status.Error(codes.Unavailable, "connection refused")
	srv.FailList(down, down)
	inst, source, _, err := coldBoot(context.Background(), cfg, "job-7", noPoolVM)
	if err != nil || inst == nil || source != "cold" {
		t.Fatalf("coldBoot = %v, %s, %v; want the VM booted after flintlock came back", inst, source, err)
	}
	if boots.Load() != 1 || srv.ListCalls() != 3 {
		t.Fatalf("boots %d after %d listings; want 1 after 3", boots.Load(), srv.ListCalls())
	}
}

// flintlockd down for the whole admission wait fails the job as a flintlock
// error, without booting.
func TestColdBootFlintlockDownUntilDeadline(t *testing.T) {
	cfg, srv, boots := coldBootFixture(t, 200*time.Millisecond)
	for i := 0; i < 1000; i++ {
		srv.FailList(status.Error(codes.Unavailable, "connection refused"))
	}
	start := time.Now()
	_, _, _, err := coldBoot(context.Background(), cfg, "job-7", noPoolVM)
	if err == nil || prepareReason(err) != "flintlock_error" {
		t.Fatalf("coldBoot = %v (reason %s); want a flintlock_error", err, prepareReason(err))
	}
	if took := time.Since(start); took > 3*time.Second || srv.ListCalls() < 2 {
		t.Fatalf("gave up after %s and %d listings; want retries until vm.boot_timeout", took, srv.ListCalls())
	}
	if boots.Load() != 0 {
		t.Fatal("booted without an admission")
	}
}

// A hung flintlockd (it accepts and never answers) does not hold the job: every
// call has its own deadline, and the job fails as a flintlock error.
func TestColdBootBoundsEachFlintlockCall(t *testing.T) {
	cfg, _, boots := coldBootFixture(t, 300*time.Millisecond)
	dir, err := os.MkdirTemp("", "hung") // short: holds a unix socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "fl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // never answers
		}
	}()
	cfg.Flintlock.Endpoint = "unix://" + filepath.Join(dir, "fl.sock")
	old := flintlockCallTimeout
	flintlockCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { flintlockCallTimeout = old })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // the job's own limit
	defer cancel()
	start := time.Now()
	_, _, _, err = coldBoot(ctx, cfg, "job-7", noPoolVM)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("coldBoot took %s on a hung flintlockd; want about vm.boot_timeout", took)
	}
	if err == nil || prepareReason(err) != "flintlock_error" {
		t.Fatalf("coldBoot = %v (reason %s); want a flintlock_error", err, prepareReason(err))
	}
	if boots.Load() != 0 {
		t.Fatal("booted without an admission")
	}
}

// Other admissions holding the host-wide admission lock until the deadline
// fail the job as an admission timeout, not as a flintlock error: flintlock
// was never asked.
func TestColdBootAdmissionLockBusyUntilDeadline(t *testing.T) {
	cfg, srv, boots := coldBootFixture(t, 300*time.Millisecond)
	old := flintlockCallTimeout
	flintlockCallTimeout = 50 * time.Millisecond // each admission's wait for the lock
	t.Cleanup(func() { flintlockCallTimeout = old })
	lock, err := os.OpenFile(vm.AdmissionFile+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = coldBoot(context.Background(), cfg, "job-7", noPoolVM)
	if !errors.Is(err, vm.ErrAdmissionBusy) || prepareReason(err) != "admission_timeout" {
		t.Fatalf("coldBoot = %v (reason %s); want an admission_timeout", err, prepareReason(err))
	}
	if boots.Load() != 0 || srv.ListCalls() != 0 {
		t.Fatalf("%d boots, %d listings without the lock; want none", boots.Load(), srv.ListCalls())
	}
}

// A boot cut off by the cold boot's own bound is a VM that did not come up
// (vm_boot); a boot cut off because the job was cancelled stays "canceled".
func TestColdBootBoundedBootReason(t *testing.T) {
	for _, c := range []struct {
		name       string
		cancelJob  bool
		wantReason string
	}{
		{"our bound", false, "vm_boot"},
		{"job cancelled", true, "canceled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _, _ := coldBootFixture(t, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bootVM = func(bctx context.Context, _ config.Config, _ *flintlock.Client, _ string, _ map[string]string) (*vm.Instance, error) {
				if c.cancelJob {
					cancel()
					<-bctx.Done()
					return nil, bctx.Err()
				}
				return nil, fmt.Errorf("waiting for SSH: %w", context.DeadlineExceeded)
			}
			_, _, _, err := coldBoot(ctx, cfg, "job-7", noPoolVM)
			if got := prepareReason(err); got != c.wantReason {
				t.Fatalf("coldBoot = %v, reason %s; want %s", err, got, c.wantReason)
			}
		})
	}
}
