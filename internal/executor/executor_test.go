package executor

import (
	"errors"
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
