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
