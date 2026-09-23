// Package executor implements the GitLab Runner custom executor stages:
// one fresh microVM per job, created in prepare and deleted in cleanup.
//
// https://docs.gitlab.com/runner/executors/custom/
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

const stateDir = "/run/firerunner/jobs"

// ExitError carries the exit code gitlab-runner expects from a stage.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }

func systemFailure(err error) error {
	return &ExitError{Code: envInt("SYSTEM_FAILURE_EXIT_CODE", 2), Err: err}
}

func buildFailure(err error) error {
	return &ExitError{Code: envInt("BUILD_FAILURE_EXIT_CODE", 1), Err: err}
}

func jobID() (string, error) {
	id := os.Getenv("CUSTOM_ENV_CI_JOB_ID")
	if id == "" {
		return "", errors.New("CUSTOM_ENV_CI_JOB_ID is not set; this command is run by gitlab-runner")
	}
	return "job-" + id, nil
}

func Prepare(ctx context.Context, cfg config.Config) error {
	id, err := jobID()
	if err != nil {
		return systemFailure(err)
	}
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return systemFailure(err)
	}
	defer fl.Close()

	labels := map[string]string{
		"firerunner/job":      id,
		"firerunner/project":  strings.ReplaceAll(os.Getenv("CUSTOM_ENV_CI_PROJECT_PATH"), "/", "."),
		"firerunner/pipeline": os.Getenv("CUSTOM_ENV_CI_PIPELINE_ID"),
	}
	start := time.Now()
	fmt.Printf("Creating microVM %s (%d vCPU, %d MB, %s)\n", id, cfg.VM.VCPU, cfg.VM.MemoryMB, cfg.VM.RootFSImage)
	inst, err := vm.Boot(ctx, cfg, fl, id, labels)
	if err != nil {
		return systemFailure(err)
	}
	if err := saveState(inst); err != nil {
		return systemFailure(err)
	}
	fmt.Printf("microVM %s ready at %s in %s\n", id, inst.IP, time.Since(start).Round(100*time.Millisecond))
	return nil
}

// Run executes one stage script. gitlab-runner passes the script path and stage name.
func Run(cfg config.Config, script, stage string) error {
	id, err := jobID()
	if err != nil {
		return systemFailure(err)
	}
	inst, err := loadState(id)
	if err != nil {
		return systemFailure(err)
	}
	f, err := os.Open(script)
	if err != nil {
		return systemFailure(err)
	}
	defer f.Close()

	code, err := vm.RunScript(cfg, inst.IP, f, os.Stdout, os.Stderr)
	switch {
	case err != nil:
		return systemFailure(err)
	case code == 0:
		return nil
	case code == 255: // ssh itself failed: the microVM is gone or unreachable
		return systemFailure(fmt.Errorf("lost SSH connection to microVM %s", id))
	case isUserStage(stage):
		return buildFailure(fmt.Errorf("stage %s exited with %d", stage, code))
	default:
		return systemFailure(fmt.Errorf("stage %s exited with %d", stage, code))
	}
}

func Cleanup(cfg config.Config) error {
	id, err := jobID()
	if err != nil {
		return err
	}
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return err
	}
	defer fl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid := ""
	if inst, err := loadState(id); err == nil {
		uid = inst.UID
	} else if found, err := fl.Find(ctx, id); err == nil {
		// The job was cancelled while prepare was still booting the microVM.
		uid = found.GetSpec().GetUid()
	}
	if uid != "" {
		if err := fl.Delete(ctx, uid); err != nil {
			return fmt.Errorf("deleting microVM %s: %w", id, err)
		}
		fmt.Printf("microVM %s deleted\n", id)
	}
	if err := os.Remove(statePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Only failures of the job's own commands count as build failures.
func isUserStage(stage string) bool {
	return stage == "build_script" || stage == "after_script" || strings.HasPrefix(stage, "step_")
}

func statePath(id string) string { return filepath.Join(stateDir, id+".json") }

func saveState(inst *vm.Instance) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(inst.ID), data, 0o600)
}

func loadState(id string) (*vm.Instance, error) {
	data, err := os.ReadFile(statePath(id))
	if err != nil {
		return nil, fmt.Errorf("no microVM recorded for %s: %w", id, err)
	}
	inst := &vm.Instance{}
	return inst, json.Unmarshal(data, inst)
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
