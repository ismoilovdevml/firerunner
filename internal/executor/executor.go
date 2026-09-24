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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
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

var numericID = regexp.MustCompile(`^[0-9]{1,20}$`)

// jobID derives the microVM id from the GitLab job id. It becomes a host file
// name, a guest hostname and a flintlock id, so only a plain number is accepted.
func jobID() (string, error) {
	id := os.Getenv("CUSTOM_ENV_CI_JOB_ID")
	if id == "" {
		return "", errors.New("CUSTOM_ENV_CI_JOB_ID is not set; this command is run by gitlab-runner")
	}
	if !numericID.MatchString(id) {
		return "", fmt.Errorf("CUSTOM_ENV_CI_JOB_ID %q is not a numeric job id", id)
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
		inst, source, err = coldBoot(ctx, cfg, id, func() *vm.Instance { return claim(cfg, dc, id) })
		if err != nil {
			_ = dc.Send(daemon.Event{Kind: "prepare", Source: source})
			return systemFailure(err)
		}
	}

	services, err := ParseServices(os.Getenv("CUSTOM_ENV_CI_JOB_SERVICES"))
	if err != nil {
		deleteVM(cfg, inst.UID)
		return buildFailure(err)
	}
	st := &vm.JobState{Instance: *inst, Source: source, StartedAt: start}
	if len(services) > 0 {
		st.Network = ServiceNetwork
	}
	// Exclusive: a job must never replace another job's VM binding.
	if err := vm.CreateJobState(statePath(id), st); err != nil {
		deleteVM(cfg, inst.UID)
		return systemFailure(err)
	}
	if auth := os.Getenv("CUSTOM_ENV_DOCKER_AUTH_CONFIG"); auth != "" {
		if err := writeDockerAuth(cfg, inst, auth); err != nil {
			return systemFailure(fmt.Errorf("writing DOCKER_AUTH_CONFIG to the microVM: %w", err))
		}
	}
	if len(services) > 0 {
		cmd := vm.SSH(cfg, inst, "/bin/bash")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(ServicesScript(services)), os.Stdout, os.Stderr
		if code, err := vm.ExitCode(cmd.Run()); err != nil || code != 0 {
			return systemFailure(fmt.Errorf("starting services failed (exit %d): %v", code, err))
		}
	}
	// Shell-mode jobs run docker on the VM; their builds can use the project's
	// warm BuildKit builder. A failure here only costs the cache, never the job.
	if os.Getenv("CUSTOM_ENV_CI_JOB_IMAGE") == "" {
		if st.Builder = useBuilder(cfg, dc, inst, os.Getenv("CUSTOM_ENV_CI_PROJECT_ID")); st.Builder {
			_ = vm.SaveJobState(statePath(id), st)
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
	if err := vm.SSH(cfg, inst, "hostnamectl", "set-hostname", id).Run(); err != nil {
		fmt.Printf("pool VM %s did not answer (%v), booting a new one\n", inst.ID, err)
		deleteVM(cfg, inst.UID)
		return nil
	}
	return inst
}

// coldBoot boots a VM for the job once the host has memory for it. While it
// waits, pool VMs that finish booting are taken instead (source "pool").
func coldBoot(ctx context.Context, cfg config.Config, id string, fromPool func() *vm.Instance) (*vm.Instance, string, error) {
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return nil, "cold", err
	}
	defer fl.Close()
	deadline := time.Now().Add(cfg.VM.BootTimeout)
	for {
		ok, why, err := vm.Fits(ctx, cfg, fl, 0)
		if err != nil {
			return nil, "cold", err
		}
		if ok {
			break
		}
		if inst := fromPool(); inst != nil {
			return inst, "pool", nil
		}
		if time.Now().After(deadline) {
			return nil, "cold", fmt.Errorf("no host memory for another microVM within %s (%s); lower runner concurrent or pool.size", cfg.VM.BootTimeout, why)
		}
		fmt.Printf("waiting for host memory (%s)...\n", why)
		select {
		case <-ctx.Done():
			return nil, "cold", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	fmt.Printf("Creating microVM %s (%s)\n", id, cfg.VM.RootFSImage)
	inst, err := vm.Boot(ctx, cfg, fl, id, map[string]string{
		daemon.LabelRole:      "job",
		daemon.LabelJob:       id,
		"firerunner/project":  strings.ReplaceAll(os.Getenv("CUSTOM_ENV_CI_PROJECT_PATH"), "/", "."),
		"firerunner/pipeline": os.Getenv("CUSTOM_ENV_CI_PIPELINE_ID"),
	})
	return inst, "cold", err
}

// BuilderName is the buildx builder created in job VMs for the project's builder.
const BuilderName = "firerunner"

// useBuilder points the VM's buildx at the project's builder when it is ready.
func useBuilder(cfg config.Config, dc *daemon.Client, inst *vm.Instance, project string) bool {
	info, err := dc.Builder(project)
	switch {
	case err != nil || info.State == daemon.BuilderDisabled:
		return false
	case info.State == daemon.BuilderBooting:
		fmt.Println("Docker layer cache: this project's builder is starting; this job builds without it")
		return false
	case info.State != daemon.BuilderReady:
		fmt.Println("Docker layer cache: all builders are busy; this job builds without it")
		return false
	}
	cmd := vm.SSH(cfg, inst, "bash")
	cmd.Stdin = strings.NewReader(BuilderScript(info))
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("Docker layer cache: could not attach the builder (%v): %s\n", err, strings.TrimSpace(string(out)))
		return false
	}
	fmt.Printf("Docker layer cache: using this project's builder (warm cache)\n")
	return true
}

// BuilderScript creates the buildx builder in the job VM. Builders are reached
// through the bridge address (the VM's default gateway), which forwards the
// port to the project's builder VM.
func BuilderScript(info *daemon.BuilderInfo) string {
	var b strings.Builder
	b.WriteString("set -e\numask 077\nmkdir -p /etc/firerunner-buildkit\n")
	for _, f := range []struct{ name, data string }{{"ca.pem", info.CA}, {"cert.pem", info.Cert}, {"key.pem", info.Key}} {
		fmt.Fprintf(&b, "cat > /etc/firerunner-buildkit/%s <<'FIRERUNNER_EOF'\n%s\nFIRERUNNER_EOF\n", f.name, strings.TrimSpace(f.data))
	}
	fmt.Fprintf(&b, "gw=$(ip -4 route show default | awk '{print $3; exit}')\n"+
		"docker buildx create --name %s --driver remote "+
		"--driver-opt cacert=/etc/firerunner-buildkit/ca.pem,cert=/etc/firerunner-buildkit/cert.pem,key=/etc/firerunner-buildkit/key.pem,servername=%s,default-load=true "+
		"\"tcp://$gw:%d\" >/dev/null\n", BuilderName, daemon.BuilderServerName, info.Port)
	return b.String()
}

func writeDockerAuth(cfg config.Config, inst *vm.Instance, auth string) error {
	cmd := vm.SSH(cfg, inst, "mkdir -p /root/.docker && umask 077 && cat > /root/.docker/config.json")
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
		if strings.HasPrefix(image, "-") {
			return buildFailure(fmt.Errorf("invalid image %q", image))
		}
		code, err = runInContainer(cfg, &st.Instance, image, st.Network, f)
	} else {
		var script io.Reader = f
		if st.Builder && isUserStage(stage) {
			// `docker build` only uses a buildx builder named in the environment.
			script = io.MultiReader(strings.NewReader("export BUILDX_BUILDER="+BuilderName+"\n"), f)
		}
		code, err = vm.RunScript(cfg, &st.Instance, script, os.Stdout, os.Stderr)
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
// With services the container joins their network so their aliases resolve.
func ContainerCommand(image, network string) string {
	if network == "" {
		network = "host"
	}
	return "docker run --rm -i --pull missing --network " + shellQuote(network) + " --entrypoint '' " +
		"-v /root/builds:/root/builds -v /root/cache:/root/cache " +
		shellQuote(image) + " " +
		`sh -c 'if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi'`
}

func runInContainer(cfg config.Config, inst *vm.Instance, image, network string, script io.Reader) (int, error) {
	cmd := vm.SSH(cfg, inst, ContainerCommand(image, network))
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
		vm.Forget(cfg, st.ID)
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
