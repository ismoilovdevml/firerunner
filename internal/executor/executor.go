// Package executor implements the GitLab Runner custom executor stages:
// one fresh microVM per job, taken from the daemon's pool (or cold-booted)
// in prepare and deleted in cleanup.
//
// https://docs.gitlab.com/runner/executors/custom/
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// StateDir holds one JSON file per job in progress.
const StateDir = "/run/firerunner/jobs"

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

func statePath(id string) string { return filepath.Join(StateDir, id+".json") }

func Prepare(ctx context.Context, cfg config.Config) error {
	id, err := jobID()
	if err != nil {
		return systemFailure(err)
	}
	start := time.Now()
	dc := daemon.NewClient(cfg.Daemon.Socket)

	source := "pool"
	inst := claim(cfg, dc, id)
	if inst == nil {
		source = "cold"
		inst, err = coldBoot(ctx, cfg, id)
		if err != nil {
			_ = dc.Send(daemon.Event{Kind: "prepare", Source: source})
			return systemFailure(err)
		}
	}

	st := &vm.JobState{Instance: *inst, Source: source, StartedAt: start}
	if err := vm.SaveJobState(statePath(id), st); err != nil {
		return systemFailure(err)
	}
	if auth := os.Getenv("CUSTOM_ENV_DOCKER_AUTH_CONFIG"); auth != "" {
		if err := writeDockerAuth(cfg, inst.IP, auth); err != nil {
			return systemFailure(fmt.Errorf("writing DOCKER_AUTH_CONFIG to the microVM: %w", err))
		}
	}
	took := time.Since(start)
	_ = dc.Send(daemon.Event{Kind: "prepare", Source: source, Seconds: took.Seconds(), OK: true})
	fmt.Printf("microVM %s ready at %s in %s (%s, %d vCPU, %d MB)\n",
		inst.ID, inst.IP, took.Round(100*time.Millisecond), source, cfg.VM.VCPU, cfg.VM.MemoryMB)
	return nil
}

// claim takes a pre-booted VM from the daemon and checks it still answers.
func claim(cfg config.Config, dc *daemon.Client, id string) *vm.Instance {
	inst, err := dc.Claim(id)
	if err != nil || inst == nil {
		return nil
	}
	if err := vm.SSH(cfg, inst.IP, "hostnamectl", "set-hostname", id).Run(); err != nil {
		fmt.Printf("pool VM %s did not answer (%v), booting a new one\n", inst.ID, err)
		deleteVM(cfg, inst.UID)
		return nil
	}
	return inst
}

func coldBoot(ctx context.Context, cfg config.Config, id string) (*vm.Instance, error) {
	deadline := time.Now().Add(cfg.VM.BootTimeout)
	for {
		ok, avail := host.MemoryFits(cfg.VM.MemoryMB, cfg.VM.HostReserveMB)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("host memory stays too low for a %d MB microVM (%d MB available, %d MB reserved)",
				cfg.VM.MemoryMB, avail, cfg.VM.HostReserveMB)
		}
		fmt.Printf("waiting for host memory (%d MB available)...\n", avail)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return nil, err
	}
	defer fl.Close()
	fmt.Printf("Creating microVM %s (%s)\n", id, cfg.VM.RootFSImage)
	return vm.Boot(ctx, cfg, fl, id, map[string]string{
		daemon.LabelRole:      "job",
		daemon.LabelJob:       id,
		"firerunner/project":  strings.ReplaceAll(os.Getenv("CUSTOM_ENV_CI_PROJECT_PATH"), "/", "."),
		"firerunner/pipeline": os.Getenv("CUSTOM_ENV_CI_PIPELINE_ID"),
	})
}

func writeDockerAuth(cfg config.Config, ip, auth string) error {
	cmd := vm.SSH(cfg, ip, "mkdir -p /root/.docker && umask 077 && cat > /root/.docker/config.json")
	cmd.Stdin = strings.NewReader(auth)
	return cmd.Run()
}

// Run executes one stage script. gitlab-runner passes the script path and stage name.
// With `image:` in the job, the user's scripts run inside that container in the
// microVM (like the docker executor); git and artifact stages run on the VM itself.
func Run(cfg config.Config, script, stage string) error {
	id, err := jobID()
	if err != nil {
		return systemFailure(err)
	}
	st, err := vm.LoadJobState(statePath(id))
	if err != nil {
		return systemFailure(fmt.Errorf("no microVM recorded for %s: %w", id, err))
	}
	f, err := os.Open(script)
	if err != nil {
		return systemFailure(err)
	}
	defer f.Close()

	var code int
	if image := os.Getenv("CUSTOM_ENV_CI_JOB_IMAGE"); image != "" && isUserStage(stage) {
		code, err = runInContainer(cfg, st.IP, image, f)
	} else {
		code, err = vm.RunScript(cfg, st.IP, f, os.Stdout, os.Stderr)
	}
	switch {
	case err != nil:
		return systemFailure(err)
	case code == 0:
		return nil
	case code == 255: // ssh itself failed: the microVM is gone or unreachable
		return systemFailure(fmt.Errorf("lost SSH connection to microVM %s", id))
	case isUserStage(stage):
		st.Failed = true
		_ = vm.SaveJobState(statePath(id), st)
		return buildFailure(fmt.Errorf("stage %s exited with %d", stage, code))
	default:
		return systemFailure(fmt.Errorf("stage %s exited with %d", stage, code))
	}
}

// ContainerCommand is the command run in the microVM for image jobs. The build
// and cache dirs are shared with the VM, which ran get_sources there.
// Like the docker executor, bash is used when the image has it, sh otherwise.
func ContainerCommand(image string) string {
	return "docker run --rm -i --pull missing --network host --entrypoint '' " +
		"-v /root/builds:/root/builds -v /root/cache:/root/cache " +
		shellQuote(image) + " " +
		`sh -c 'if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi'`
}

func runInContainer(cfg config.Config, ip, image string, script io.Reader) (int, error) {
	cmd := vm.SSH(cfg, ip, ContainerCommand(image))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = script, os.Stdout, os.Stderr
	return vm.ExitCode(cmd.Run())
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
	st, stErr := vm.LoadJobState(statePath(id))
	if stErr == nil {
		uid = st.UID
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
	if stErr == nil {
		result := "success"
		if st.Failed {
			result = "failed"
		}
		_ = daemon.NewClient(cfg.Daemon.Socket).Send(daemon.Event{Kind: "finish", Result: result, Seconds: time.Since(st.StartedAt).Seconds()})
	}
	if err := os.Remove(statePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func deleteVM(cfg config.Config, uid string) {
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return
	}
	defer fl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = fl.Delete(ctx, uid)
}

// Only failures of the job's own commands count as build failures.
func isUserStage(stage string) bool {
	return stage == "build_script" || stage == "after_script" || strings.HasPrefix(stage, "step_")
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
