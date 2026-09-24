package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/config"
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

func TestJobIDRejectsNonNumeric(t *testing.T) {
	for _, v := range []string{"/../../../etc/x", "12 3", "1;reboot", "abc", "123456789012345678901"} {
		t.Setenv("CUSTOM_ENV_CI_JOB_ID", v)
		if id, err := jobID(); err == nil {
			t.Errorf("%q accepted as %q", v, id)
		}
	}
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "74196")
	if id, err := jobID(); err != nil || id != "job-74196" {
		t.Fatalf("numeric id: %q %v", id, err)
	}
}

func TestLeadingDashImageIsRejected(t *testing.T) {
	t.Setenv("CUSTOM_ENV_CI_JOB_ID", "77")
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
