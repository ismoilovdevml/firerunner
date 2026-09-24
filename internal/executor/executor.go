// Package executor implements the GitLab Runner custom executor stages:
// one fresh microVM per job, taken from the daemon's pool (or cold-booted)
// in prepare and deleted in cleanup.
//
// https://docs.gitlab.com/runner/executors/custom/
package executor

import (
	"bytes"
	"context"
	"encoding/json"
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

// StateDir holds one JSON file per job in progress (a variable for tests).
var StateDir = "/run/firerunner/jobs"

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

// job is who the running job is, as gitlab-runner received it from GitLab.
type job struct {
	ID      string // microVM id: "job-<CI job id>"
	Project string // GitLab project id, "" if unknown
}

// currentJob reads the job from JOB_RESPONSE_FILE, which gitlab-runner writes
// from the job payload for every stage. CUSTOM_ENV_CI_JOB_ID and
// CUSTOM_ENV_CI_PROJECT_ID must not be used: they are job variables, and a job
// can set them to another job's or project's id (to reach that job's VM or
// that project's builder). Job variables always carry the CUSTOM_ENV_ prefix,
// so they cannot replace JOB_RESPONSE_FILE itself.
func currentJob() (job, error) {
	path := os.Getenv("JOB_RESPONSE_FILE")
	if path == "" {
		return job{}, errors.New("JOB_RESPONSE_FILE is not set; this command is run by gitlab-runner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return job{}, fmt.Errorf("reading the job payload: %w", err)
	}
	var payload struct {
		ID      int64 `json:"id"`
		JobInfo struct {
			ProjectID int64 `json:"project_id"`
		} `json:"job_info"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return job{}, fmt.Errorf("parsing the job payload: %w", err)
	}
	if payload.ID <= 0 {
		return job{}, errors.New("the job payload has no job id")
	}
	j := job{ID: "job-" + strconv.FormatInt(payload.ID, 10)}
	if payload.JobInfo.ProjectID > 0 {
		j.Project = strconv.FormatInt(payload.JobInfo.ProjectID, 10)
	}
	return j, nil
}

// jobID is the microVM id of the running job. It becomes a host file name, a
// guest hostname and a flintlock id; currentJob only yields "job-<number>".
func jobID() (string, error) {
	j, err := currentJob()
	return j.ID, err
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

	// One SSH connection for the rest of the job (every later stage reuses it).
	if err := vm.StartMux(cfg, inst); err != nil {
		fmt.Fprintf(os.Stderr, "ssh multiplexing unavailable (%v); stages connect one by one\n", err)
	}

	services, err := ParseServices(os.Getenv("CUSTOM_ENV_CI_JOB_SERVICES"))
	if err != nil {
		deleteVM(cfg, inst)
		return buildFailure(err)
	}
	st := &vm.JobState{Instance: *inst, Source: source, StartedAt: start}
	if len(services) > 0 {
		st.Network = ServiceNetwork
	}
	// Exclusive: a job must never replace another job's VM binding.
	if err := vm.CreateJobState(statePath(id), st); err != nil {
		deleteVM(cfg, inst)
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
	// warm BuildKit builder. Prepare only attaches a builder the project already
	// has; Run starts one when a stage actually builds. A failure here only costs
	// the cache, never the job.
	if os.Getenv("CUSTOM_ENV_CI_JOB_IMAGE") == "" {
		if j, err := currentJob(); err == nil && j.Project != "" && useBuilder(cfg, dc, inst, j.Project, false) {
			st.BuilderProject = j.Project
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
		deleteVM(cfg, inst)
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

// useBuilder points the VM's buildx at the project's builder, ready or still
// starting. Nothing waits here: jobs that never build (checks, deploys) start at
// once, and the docker wrapper (BuilderScript) waits for a starting builder only
// when the job actually builds.
func useBuilder(cfg config.Config, dc *daemon.Client, inst *vm.Instance, project string, start bool) bool {
	info, err := dc.Builder(project, start)
	switch {
	case err != nil || info.State == daemon.BuilderDisabled || info.State == daemon.BuilderNone:
		return false
	case (info.State == daemon.BuilderReady || info.State == daemon.BuilderBooting) && info.Port > 0 && info.Key != "":
	default:
		fmt.Println("Docker layer cache: all builders are busy; this job builds without it")
		return false
	}
	cmd := vm.SSH(cfg, inst, "bash")
	cmd.Stdin = strings.NewReader(BuilderScript(info))
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("Docker layer cache: could not attach the builder (%v): %s\n", err, strings.TrimSpace(string(out)))
		return false
	}
	if info.State == daemon.BuilderReady {
		fmt.Println("Docker layer cache: using this project's builder (warm cache)")
	} else {
		fmt.Println("Docker layer cache: this project's builder is starting; docker build will wait for it")
	}
	return true
}

// buildCommand matches a command in a stage script that builds images.
var buildCommand = regexp.MustCompile(`\bdocker(\s+buildx)?\s+build\b|\bdocker\s+buildx\s+bake\b|\bdocker(-|\s+)compose\b[^\n]*\bbuild\b`)

// BuildsImages reports whether a stage script runs docker build (or buildx
// build/bake, compose build). Only such stages need the project's builder.
func BuildsImages(script []byte) bool { return buildCommand.Match(script) }

// BuilderWait is how long `docker build` in a job waits for a starting builder.
const BuilderWait = 90

// BuilderScript creates the buildx builder in the job VM and a docker wrapper
// that, for build commands, waits until the builder answers (a full mTLS
// handshake with the job's certificate) and otherwise builds locally. Builders
// are reached through the bridge address (the VM's default gateway), which
// forwards the port to the project's builder VM.
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
	fmt.Fprintf(&b, `cat > /usr/local/bin/docker <<'FIRERUNNER_EOF'
#!/bin/bash
# FireRunner: docker build uses this project's builder; wait for it while it starts.
if [ -n "$BUILDX_BUILDER" ] && { [ "$1" = build ] || [ "$1" = buildx ]; }; then
    if [ ! -e /run/firerunner-builder-ok ] && [ ! -e /run/firerunner-builder-down ]; then
        end=$((SECONDS + %d))
        while [ "$SECONDS" -lt "$end" ]; do
            if timeout 5 /usr/bin/docker buildx inspect --bootstrap "$BUILDX_BUILDER" >/dev/null 2>&1; then
                touch /run/firerunner-builder-ok
                break
            fi
            sleep 1
        done
        [ -e /run/firerunner-builder-ok ] || touch /run/firerunner-builder-down
    fi
    if [ -e /run/firerunner-builder-down ]; then
        echo "FireRunner: this project's builder did not answer; building without the layer cache" >&2
        unset BUILDX_BUILDER
    fi
fi
exec /usr/bin/docker "$@"
FIRERUNNER_EOF
chmod 0755 /usr/local/bin/docker
`, BuilderWait)
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
		if st.BuilderProject == "" && isUserStage(stage) {
			// The stage builds images: give the project a builder now (its
			// docker wrapper waits while the builder starts).
			if body, rerr := io.ReadAll(f); rerr == nil {
				script = bytes.NewReader(body)
				if BuildsImages(body) {
					if j, jerr := currentJob(); jerr == nil && j.Project != "" &&
						useBuilder(cfg, daemon.NewClient(cfg.Daemon.Socket), &st.Instance, j.Project, true) {
						st.BuilderProject = j.Project
						_ = vm.SaveJobState(statePath(id), st)
					}
				}
			}
		}
		if st.BuilderProject != "" && isUserStage(stage) {
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

	// vmID is the flintlock id: job-<id> for a cold boot, pool-<rand> for a
	// claimed pool VM. The DHCP lease and pinned key are keyed by it.
	uid, vmID := "", id
	st, stErr := vm.LoadJobState(statePath(id))
	if stErr == nil {
		uid, vmID = st.UID, st.ID
	} else if found, err := fl.Find(ctx, id); err == nil {
		// The job was cancelled while prepare was still booting the microVM.
		uid = found.GetSpec().GetUid()
	}
	if stErr == nil {
		vm.StopMux(&st.Instance)
	}
	var delErr error
	if uid != "" {
		if delErr = deleteWithRetry(ctx, fl, uid); delErr == nil {
			fmt.Printf("microVM %s deleted\n", id)
			// A VM that is still running keeps its lease and pinned key until
			// the daemon deletes it (Daemon.delete forgets them then).
			vm.Forget(cfg, vmID)
		}
	}
	if stErr == nil {
		result := "success"
		if st.Failed {
			result = "failed"
		}
		_ = daemon.NewClient(cfg.Daemon.Socket).Send(daemon.Event{Kind: "finish", Result: result, Seconds: time.Since(st.StartedAt).Seconds()})
	}
	// The state file goes even when the delete failed: nothing retries cleanup,
	// and while the file exists reconcile treats the VM as a running job and
	// keeps it (and its memory) for daemon.job_max_age. Without the file the
	// daemon reclaims it as an orphan within minutes.
	if err := os.Remove(statePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(delErr, err)
	}
	if delErr != nil {
		return fmt.Errorf("deleting microVM %s (the daemon reclaims it): %w", id, delErr)
	}
	return nil
}

// deleteBackoff is the pause before each retry of a failed delete (a variable for tests).
var deleteBackoff = time.Second

// deleteWithRetry retries a delete that flintlockd refuses transiently (it
// fails calls while it garbage-collects another VM's spec).
func deleteWithRetry(ctx context.Context, fl *flintlock.Client, uid string) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = fl.Delete(ctx, uid); err == nil {
			return nil
		}
		if attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * deleteBackoff):
		}
	}
	return err
}

// deleteVM deletes a VM prepare took or booted but cannot use, and frees its
// DHCP address right away (a leaked lease holds an address for 15 minutes).
func deleteVM(cfg config.Config, inst *vm.Instance) {
	fl, err := flintlock.Dial(cfg.Flintlock)
	if err != nil {
		return
	}
	defer fl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = vm.Destroy(ctx, cfg, fl, inst.ID, inst.UID)
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
