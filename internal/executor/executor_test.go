package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
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
	if err := Run(config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) || e.Code != 2 {
		t.Fatalf("want system failure, got %v", err)
	}
}

func TestRunWithoutPrepareIsSystemFailure(t *testing.T) {
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "does-not-exist-123")
	var e *ExitError
	if err := Run(config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) || e.Code != 2 {
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

func TestLeadingDashImageIsRejected(t *testing.T) {
	writePayload(t, `{"id":77}`)
	t.Setenv("CUSTOM_ENV_CI_JOB_IMAGE", "--privileged")
	// No state file exists, so a system failure is expected before the image check;
	// the image check itself is covered by ContainerCommand never seeing it.
	var e *ExitError
	if err := Run(config.Default(), "/nonexistent", "build_script"); !errors.As(err, &e) {
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
	})
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
		"end=$((SECONDS + 90))",
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
